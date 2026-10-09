package watches

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
	ListIssues(ctx context.Context, repo RepoRef, query IssueQuery) ([]Issue, error)
}

// WorkspaceGate reports whether this integration is switched on for a
// workspace. A watch in a workspace where the operator turned the integration
// off must stop polling, not merely stop rendering — otherwise the toggle is
// decorative while tasks keep appearing.
type WorkspaceGate func(ctx context.Context, workspaceID string) (bool, error)

// defaultTick is how often the loop looks for due watches. It is not the poll
// interval: each watch has its own, and the loop only wakes often enough to
// honour the shortest one with reasonable granularity.
const defaultTick = 30 * time.Second

// maxTitleRunes bounds a generated task title. A tracker allows far longer
// titles than a board column can show.
const maxTitleRunes = 160

// maxBodyRunes bounds how much of an issue body is copied into the task
// description. The full issue is one click away through the link, and an
// agent's first read of a task should not be forty screens of bug report.
const maxBodyRunes = 6000

// maxTaskPages bounds the inflight scan. A workspace with more live tasks than
// this is past the point where a watch's budget is the interesting constraint.
const maxTaskPages = 20

// taskPageSize is the page size for the inflight scan.
const taskPageSize = 200

// Poller runs due watches on an interval.
//
// One goroutine serves every workspace: the work is IO-bound on the Forgejo
// instance and the Host RPC, and a goroutine per watch would multiply
// rate-limit pressure without finishing sooner.
type Poller struct {
	store   *Store
	hosts   HostProvider
	issues  IssueSource
	reviews ReviewSource
	gate    WorkspaceGate
	tick    time.Duration
	now     func() time.Time
	logf    func(format string, args ...any)

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

// NewPoller wires a poller. gate and logf may be nil.
func NewPoller(store *Store, hosts HostProvider, issues IssueSource, gate WorkspaceGate) *Poller {
	return &Poller{
		store:  store,
		hosts:  hosts,
		issues: issues,
		gate:   gate,
		tick:   defaultTick,
		now:    time.Now,
		logf:   func(string, ...any) {},
	}
}

// Start begins the loop. It is idempotent: a second call while running is a
// no-op, because SetHost is the only caller and a host re-injection must not
// leave two loops racing for the same watches.
func (p *Poller) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	p.cancel, p.stopped = cancel, stopped
	go func() {
		defer close(stopped)
		p.run(ctx)
	}()
}

// Stop halts the loop and waits for the in-flight tick to finish.
func (p *Poller) Stop() {
	p.mu.Lock()
	cancel, stopped := p.cancel, p.stopped
	p.cancel, p.stopped = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-stopped
}

func (p *Poller) run(ctx context.Context) {
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// One slow tick must not stack up behind the next. The ticker
			// drops intervening fires while this is running, which is the
			// behaviour we want: a watch that is late is polled once, not
			// once per missed tick.
			p.Tick(ctx)
		}
	}
}

// Tick runs one pass over every workspace, polling the watches that are due.
// It is exported so a test can drive the loop deterministically instead of
// waiting on wall-clock ticks.
func (p *Poller) Tick(ctx context.Context) {
	host, err := p.store.host()
	if err != nil {
		// The broker dial has not finished. The next tick will find it.
		return
	}
	workspaces, err := p.workspaceIDs(ctx, host)
	if err != nil {
		p.logf("watches: list workspaces: %v", err)
		return
	}
	for _, workspaceID := range workspaces {
		if ctx.Err() != nil {
			return
		}
		p.tickWorkspace(ctx, workspaceID)
	}
}

func (p *Poller) tickWorkspace(ctx context.Context, workspaceID string) {
	if p.gate != nil {
		enabled, err := p.gate(ctx, workspaceID)
		if err != nil {
			p.logf("watches: enabled check for %s: %v", workspaceID, err)
			return
		}
		if !enabled {
			return
		}
	}
	watches, err := p.store.List(ctx, workspaceID)
	if err != nil {
		p.logf("watches: list watches for %s: %v", workspaceID, err)
		return
	}
	now := p.now().UTC()
	for _, watch := range watches {
		if ctx.Err() != nil {
			return
		}
		if !watch.Enabled || !watch.Due(now) {
			continue
		}
		if _, err := p.RunWatch(ctx, watch); err != nil {
			p.logf("watches: run %s: %v", watch.ID, err)
		}
	}
}

// workspaceIDs enumerates the workspaces this host serves.
func (p *Poller) workspaceIDs(ctx context.Context, host pluginsdk.Host) ([]string, error) {
	workspaces, _, err := host.Workspaces().List(ctx, pluginsdk.Page{Limit: taskPageSize})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		if strings.TrimSpace(workspace.ID) != "" {
			ids = append(ids, workspace.ID)
		}
	}
	return ids, nil
}

// Result reports what one watch run did. It is returned to the panel's manual
// "run now" action so an operator gets an answer rather than a spinner.
type Result struct {
	WatchID string `json:"watch_id"`
	// Matched is how many non-pull-request issues the filters returned.
	Matched int `json:"matched"`
	// Created is how many became new tasks.
	Created int `json:"created"`
	// Duplicates is how many were already in the ledger.
	Duplicates int `json:"duplicates"`
	// Throttled is how many matched but were left for a later run because the
	// inflight budget was full.
	Throttled int `json:"throttled"`
	// Inflight is the budget reading taken at the start of the run.
	Inflight int      `json:"inflight"`
	Budget   int      `json:"budget"`
	Errors   []string `json:"errors,omitempty"`

	// Review watches only. Drafts counts pull requests left out because they
	// are drafts. SkippedForks counts fork pull requests left out because no
	// placement for them was safe. Cleaned and Completed report the cleanup
	// pass that follows discovery.
	Drafts       int `json:"drafts,omitempty"`
	SkippedForks int `json:"skipped_forks,omitempty"`
	Archived     int `json:"archived,omitempty"`
	Completed    int `json:"completed,omitempty"`
	// CleanupNote says why tasks were completed rather than archived.
	CleanupNote string `json:"cleanup_note,omitempty"`
}

// RunWatch polls one watch once and creates tasks for issues it has not seen.
//
// A per-repository failure is collected rather than fatal: one unreachable
// repository in a five-repository watch must not stop the other four. The
// watch is only marked failed if every repository failed, because a watch
// showing a persistent error while quietly working is worse than one that
// reports the repository it cannot read.
func (p *Poller) RunWatch(ctx context.Context, watch Watch) (Result, error) {
	result := Result{WatchID: watch.ID, Budget: watch.MaxInflightTasks}
	host, err := p.store.host()
	if err != nil {
		return result, err
	}

	inflight, err := p.inflight(ctx, host, watch)
	if err != nil {
		// A budget that cannot be read is treated as exhausted. Creating
		// tasks with an unknown inflight count is exactly the failure the
		// budget exists to prevent.
		_ = p.store.MarkError(ctx, watch, "Could not read the current task count for this watch.")
		return result, err
	}
	result.Inflight = inflight

	var failures []string
	if watch.IsReview() {
		failures = p.pollReviews(ctx, host, watch, &result)
	} else {
		failures = p.pollIssues(ctx, host, watch, &result)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// Either way the poll clock advances, so a watch against an unreachable
	// instance backs off to its interval instead of retrying every tick. The
	// difference is only whether the panel has an error to show.
	result.Errors = failures
	if watch.IsReview() && watch.CleanupPolicy == CleanupWhenClosed {
		// The note follows the latest pass that did something: it clears once
		// archiving works, and appears once it stops.
		switch {
		case result.CleanupNote != "":
			watch.LastCleanupNote = result.CleanupNote
		case result.Archived > 0:
			watch.LastCleanupNote = ""
		}
	}
	var mark error
	if len(failures) == 0 {
		mark = p.store.MarkPolled(ctx, watch)
	} else {
		mark = p.store.MarkError(ctx, watch, strings.Join(failures, "; "))
	}
	if mark != nil {
		return result, mark
	}
	return result, nil
}

// pollIssues files a task per new issue and returns the failures it met.
func (p *Poller) pollIssues(ctx context.Context, host pluginsdk.Host, watch Watch, result *Result) []string {
	since := watchSince(watch)
	inflight := result.Inflight
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
			if _, seen, err := p.store.Seen(ctx, watch, repo, issue.Number); err != nil {
				failures = append(failures, fmt.Sprintf("%s#%d: %s", repo.FullName(), issue.Number, err))
				continue
			} else if seen {
				result.Duplicates++
				continue
			}
			if inflight+result.Created >= watch.MaxInflightTasks {
				result.Throttled++
				continue
			}
			taskID, err := p.createTask(ctx, host, watch, repo, issue)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s#%d: %s", repo.FullName(), issue.Number, err))
				continue
			}
			if err := p.store.Record(ctx, watch, repo, issue.Number, issue.URL, taskID); err != nil {
				// The task exists but the ledger does not know it. Say so
				// loudly: the next poll will create a second card, and an
				// operator who sees this can reconcile deliberately.
				failures = append(failures, fmt.Sprintf(
					"%s#%d: task %s was created but could not be recorded; it may be created again: %s",
					repo.FullName(), issue.Number, taskID, err))
			}
			result.Created++
		}
	}
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

// inflight counts this watch's tasks that are still open.
//
// It reads the ledger, then one page-walk of the workspace's live tasks, and
// intersects them — rather than a Get per recorded task, which would be one
// RPC per issue this watch has ever handled. A task kandev has completed or
// archived no longer counts against the budget, which is what makes the budget
// a throttle rather than a lifetime cap.
func (p *Poller) inflight(ctx context.Context, host pluginsdk.Host, watch Watch) (int, error) {
	records, err := p.store.Records(ctx, watch)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	owned := make(map[string]struct{}, len(records))
	for _, record := range records {
		owned[record.TaskID] = struct{}{}
	}

	count := 0
	cursor := ""
	for page := 0; page < maxTaskPages; page++ {
		tasks, info, err := host.Tasks().List(ctx, pluginsdk.TaskFilter{
			WorkspaceIDs: []string{watch.WorkspaceID},
		}, pluginsdk.Page{Limit: taskPageSize, Cursor: cursor})
		if err != nil {
			return 0, fmt.Errorf("watches: list tasks: %w", err)
		}
		for _, task := range tasks {
			if _, mine := owned[task.ID]; !mine {
				continue
			}
			// CompletedAt and ArchivedAt are the two fields that mean "no
			// longer occupying the board". Task state vocabulary is richer
			// than this plugin needs to know about.
			if task.CompletedAt == nil && task.ArchivedAt == nil {
				count++
			}
		}
		if info == nil || !info.HasMore || strings.TrimSpace(info.NextCursor) == "" {
			break
		}
		cursor = info.NextCursor
	}
	return count, nil
}

// createTask turns one issue into one kandev task.
func (p *Poller) createTask(ctx context.Context, host pluginsdk.Host, watch Watch, repo RepoRef, issue Issue) (string, error) {
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
	if watch.AgentProfileID == "" && watch.ExecutorProfileID == "" && watch.Prompt == "" {
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
	if watch.Prompt != "" {
		prompt := watch.Prompt
		launch.Prompt = &prompt
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
func taskDescription(repo RepoRef, issue Issue) string {
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
