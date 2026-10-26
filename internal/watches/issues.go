package watches

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Issue is the provider-neutral view of one tracker issue. The adapter maps a
// Forgejo issue onto this; nothing below this line knows what a Forgejo issue
// looks like.
type Issue struct {
	Number    int64
	Title     string
	Body      string
	State     string
	URL       string
	Labels    []string
	Author    string
	UpdatedAt string
}

// IssueQuery is one watch's filter, as the adapter receives it.
type IssueQuery struct {
	State  string
	Labels []string
	Query  string
	// Since bounds results to issues updated at or after this instant. The
	// poller passes the previous successful poll time so a steady-state tick
	// transfers almost nothing.
	Since time.Time
}

// IssueSource is the provider port. internal/forgejo implements it.
type IssueSource interface {
	ListIssues(ctx context.Context, repo scm.RepoRef, query IssueQuery) ([]Issue, error)
}

// maxTitleRunes bounds a generated task title. A tracker allows far longer
// titles than a board column can show.
const maxTitleRunes = 160

// maxBodyRunes bounds how much of an issue body is copied into the task
// description. The full issue is one click away through the link, and an
// agent's first read of a task should not be forty screens of bug report.
const maxBodyRunes = 6000

// pollIssues files a task per new issue and returns the failures it met.
func (p *Poller) pollIssues(ctx context.Context, host pluginsdk.Host, watch Watch, result *Result) []string {
	since := watchSince(watch)
	var failures []string
	for _, repo := range watch.Repos {
		if ctx.Err() != nil {
			return failures
		}
		issues, err := p.issues.ListIssues(ctx, repo, IssueQuery{
			State:  watch.State,
			Labels: watch.Labels,
			Query:  watch.Query,
			Since:  since,
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", repo.FullName(), err))
			continue
		}
		for _, issue := range issues {
			result.Matched++
			failures = append(failures, p.fileIssue(ctx, host, watch, repo, issue, result)...)
		}
	}
	return failures
}

// fileIssue creates the task for one matched issue unless the ledger has it or
// the inflight budget is full.
func (p *Poller) fileIssue(ctx context.Context, host pluginsdk.Host, watch Watch, repo scm.RepoRef, issue Issue, result *Result) []string {
	if _, seen, err := p.store.Seen(ctx, watch, repo, issue.Number); err != nil {
		return []string{fmt.Sprintf("%s#%d: %s", repo.FullName(), issue.Number, err)}
	} else if seen {
		result.Duplicates++
		return nil
	}
	if result.Inflight+result.Created >= watch.MaxInflightTasks {
		result.Throttled++
		return nil
	}
	taskID, err := p.createTask(ctx, host, watch, repo, issue)
	if err != nil {
		return []string{fmt.Sprintf("%s#%d: %s", repo.FullName(), issue.Number, err)}
	}
	var failures []string
	if err := p.store.Record(ctx, watch, repo, issue.Number, issue.URL, taskID); err != nil {
		// The task exists but the ledger does not know it. Say so
		// loudly: the next poll will create a second card, and an
		// operator who sees this can reconcile deliberately.
		failures = append(failures, fmt.Sprintf(
			"%s#%d: task %s was created but could not be recorded; it may be created again: %s",
			repo.FullName(), issue.Number, taskID, err))
	}
	result.Created++
	return failures
}

// watchSince is the incremental bound for a poll.
//
// A watch that has polled before asks only for issues updated since then. That
// is safe for discovery as well as for changes: a newly created issue has an
// update time at or after its creation, and an issue that only starts matching
// because a label was added has its update time bumped by that edit.
//
// The bound is rewound by one interval. Clock skew between this process and
// the Forgejo instance is real, and re-reading a small overlap costs one
// already-deduplicated comparison, while missing an issue costs a card that
// never appears.
func watchSince(watch Watch) time.Time {
	last, err := time.Parse(time.RFC3339, watch.LastPolledAt)
	if err != nil {
		return time.Time{}
	}
	return last.Add(-watch.PollInterval()).UTC()
}

// createTask turns one issue into one kandev task.
func (p *Poller) createTask(ctx context.Context, host pluginsdk.Host, watch Watch, repo scm.RepoRef, issue Issue) (string, error) {
	stepID := watch.WorkflowStepID
	input := pluginsdk.CreateTaskInput{
		WorkspaceID:    watch.WorkspaceID,
		WorkflowID:     watch.WorkflowID,
		WorkflowStepID: &stepID,
		Title:          taskTitle(issue),
		Description:    taskDescription(repo, issue),
		StartAgent:     watch.StartAgent,
		// Metadata is the machine-readable half of the same provenance the
		// description carries in prose. kandev stamps source="plugin:<id>"
		// itself, so this says which issue, not which plugin.
		Metadata: map[string]any{
			"forgejo_repo":         repo.FullName(),
			"forgejo_issue_number": issue.Number,
			"forgejo_issue_url":    issue.URL,
			"forgejo_watch_id":     watch.ID,
		},
	}
	if watch.RepositoryID != "" {
		attached := pluginsdk.PluginTaskRepository{RepositoryID: watch.RepositoryID}
		if watch.BaseBranch != "" {
			base := watch.BaseBranch
			attached.BaseBranch = &base
		}
		input.Repositories = []pluginsdk.PluginTaskRepository{attached}
	}
	if launch := taskLaunch(watch); launch != nil {
		input.Launch = launch
	}

	created, err := host.Tasks().Create(ctx, input)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	if created == nil || strings.TrimSpace(created.ID) == "" {
		return "", errors.New("create task: the host returned no task")
	}
	return created.ID, nil
}

// taskLaunch renders the watch's launch settings, or nil when the operator set
// none. A nil Launch leaves kandev's own defaults in charge, which is the
// right behaviour for a watch that only files cards.
func taskLaunch(watch Watch) *pluginsdk.PluginTaskLaunchOptions {
	launch := profileLaunch(watch)
	if watch.Prompt == "" {
		return launch
	}
	if launch == nil {
		launch = &pluginsdk.PluginTaskLaunchOptions{}
	}
	prompt := watch.Prompt
	launch.Prompt = &prompt
	return launch
}

// profileLaunch renders the watch's agent and executor profiles, or nil when
// it names neither.
func profileLaunch(watch Watch) *pluginsdk.PluginTaskLaunchOptions {
	if watch.AgentProfileID == "" && watch.ExecutorProfileID == "" {
		return nil
	}
	launch := &pluginsdk.PluginTaskLaunchOptions{}
	if watch.AgentProfileID != "" {
		id := watch.AgentProfileID
		launch.AgentProfileID = &id
	}
	if watch.ExecutorProfileID != "" {
		id := watch.ExecutorProfileID
		launch.ExecutorProfileID = &id
	}
	return launch
}

// taskTitle renders the board card's title: the issue number first, so two
// issues that share a title are still distinguishable in a column.
func taskTitle(issue Issue) string {
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = "Untitled issue"
	}
	return truncateRunes(fmt.Sprintf("#%d %s", issue.Number, title), maxTitleRunes)
}

// taskDescription renders the task body: provenance first, then the issue's
// own text. An agent reading this task needs to know where the work came from
// before it reads what the work is.
func taskDescription(repo scm.RepoRef, issue Issue) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Forgejo issue [%s#%d](%s)", repo.FullName(), issue.Number, issue.URL)
	if author := strings.TrimSpace(issue.Author); author != "" {
		fmt.Fprintf(&builder, " opened by %s", author)
	}
	builder.WriteString("\n")
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&builder, "\nLabels: %s\n", strings.Join(issue.Labels, ", "))
	}
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		builder.WriteString("\n_The issue has no description._\n")
		return builder.String()
	}
	builder.WriteString("\n---\n\n")
	builder.WriteString(truncateRunes(body, maxBodyRunes))
	return builder.String()
}

// truncateRunes cuts on a rune boundary so a multi-byte character is never
// split, and marks that it cut.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return strings.TrimRight(string(runes[:limit-1]), " \t\n") + "…"
}
