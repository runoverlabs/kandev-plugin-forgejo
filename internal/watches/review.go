package watches

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// PullRequest is the provider-neutral view of one pull request as a review
// search returns it. It is deliberately thin: the search answer lacks the
// branches, the head repository and the draft flag, so those come from
// PullDetail, fetched only for a pull request the ledger has not seen.
type PullRequest struct {
	Repo      RepoRef
	Number    int64
	Title     string
	URL       string
	Author    string
	UpdatedAt string
}

// PullDetail is what a new pull request needs before a task is filed for it.
type PullDetail struct {
	Title      string
	URL        string
	Author     string
	HeadBranch string
	BaseBranch string
	Open       bool
	Draft      bool
	// Fork is true when the head lives in another repository, and also when the
	// adapter cannot tell. Unknown counts as a fork: the protection this guards
	// is worth more than the convenience of a checkout.
	Fork bool
	// RequestedFromMe is true when the token's user is named in the pull
	// request's reviewer list, as opposed to a team the user belongs to.
	RequestedFromMe bool
}

// PullState is where a pull request stands.
type PullState string

const (
	PullOpen   PullState = "open"
	PullClosed PullState = "closed"
	PullMerged PullState = "merged"
)

// ReviewQuery is one review watch's filter, as the adapter receives it.
type ReviewQuery struct {
	Repos  []RepoRef
	Labels []string
	Query  string
}

// ErrBackoff means the instance refused a request in a way that more requests
// would not cure: a rate limit, or a token that lost its access. A batch stops
// on it instead of spending the rest of its budget on the same refusal.
var ErrBackoff = errors.New("watches: the instance is refusing requests")

// ReviewSource is the provider port for review watches. internal/forgejo
// implements it.
type ReviewSource interface {
	// SearchReviewRequests returns the open pull requests awaiting the token
	// user's review, restricted to q.Repos when it names any.
	SearchReviewRequests(ctx context.Context, q ReviewQuery) ([]PullRequest, error)
	// PullDetail reads one pull request. scope says whether the adapter must
	// also work out if the user was named directly.
	PullDetail(ctx context.Context, repo RepoRef, number int64, scope ReviewScope) (PullDetail, error)
	// PullState reports whether a pull request is still open.
	PullState(ctx context.Context, repo RepoRef, number int64) (PullState, error)
}

// WithReviews gives the poller the review source. Review watches are skipped,
// with an error on the watch, until it is set.
func (p *Poller) WithReviews(source ReviewSource) *Poller {
	p.reviews = source
	return p
}

// autoStartAction is the workflow step action that launches an agent on entry.
const autoStartAction = "auto_start_agent"

// launchActions are the on_enter actions that can run an agent when a task
// enters a step. A step carrying any of them is not a safe place for a fork's
// task.
var launchActions = map[string]struct{}{
	autoStartAction:                  {},
	"queue_run":                      {},
	"queue_run_for_each_participant": {},
	"run_code_review":                {},
}

// StartsAgent reports whether any of a step's on_enter action types can launch
// an agent.
func StartsAgent(actions []string) bool {
	for _, action := range actions {
		if _, launches := launchActions[action]; launches {
			return true
		}
	}
	return false
}

// DefaultReviewPrompt is the task description for a review watch with no
// prompt of its own. It follows Kandev's own GitHub review watch.
const DefaultReviewPrompt = `Review Pull Request #{{pr.number}}: {{pr.title}}
Repository: {{pr.repo}}
PR: {{pr.link}}
Author: {{pr.author}}
Branch: {{pr.branch}} → {{pr.base_branch}}

To see ONLY the PR changes, use:
- git diff origin/{{pr.base_branch}}...HEAD (three-dot = only changes on the PR branch)
- git log --oneline origin/{{pr.base_branch}}..HEAD (list PR commits)
Do NOT review files outside this diff.`

// interpolate fills the {{pr.*}} placeholders. Anything else in braces is left
// alone, so an operator's own braces survive. The values are pull request
// metadata an author controls, so they are substituted as text and never
// evaluated.
func interpolate(template string, repo RepoRef, number int64, detail PullDetail) string {
	return strings.NewReplacer(
		"{{pr.link}}", detail.URL,
		"{{pr.number}}", strconv.FormatInt(number, 10),
		"{{pr.title}}", detail.Title,
		"{{pr.author}}", detail.Author,
		"{{pr.repo}}", repo.FullName(),
		"{{pr.branch}}", detail.HeadBranch,
		"{{pr.base_branch}}", detail.BaseBranch,
	).Replace(template)
}

// safeRef accepts a branch name that is safe to hand to a checkout: no leading
// dash (an option), no "..", no control characters, no whitespace. A name that
// fails creates the task without a checkout rather than failing the watch.
var safeRef = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+@-]{0,199}$`)

func safeBranch(name string) bool {
	return safeRef.MatchString(name) && !strings.Contains(name, "..") &&
		!strings.HasSuffix(name, ".lock") && !strings.HasSuffix(name, "/")
}

// placement is where a pull request's task goes and whether it may start.
type placement struct {
	stepID     string
	startAgent bool
	checkout   bool
}

// steps resolves the watch's workflow steps once per run.
type steps struct {
	loaded bool
	byID   map[string][]string
	err    error
}

func (s *steps) autoStarts(ctx context.Context, host pluginsdk.Host, workflowID, stepID string) (starts, known bool) {
	if !s.loaded {
		s.loaded = true
		listed, err := host.Workflows().ListSteps(ctx, workflowID)
		if err != nil {
			s.err = err
			return false, false
		}
		s.byID = make(map[string][]string, len(listed))
		for _, step := range listed {
			s.byID[step.ID] = step.OnEnterActionTypes
		}
	}
	actions, found := s.byID[stepID]
	if !found {
		return false, false
	}
	return StartsAgent(actions), true
}

// place decides where a pull request's task lands.
//
// A fork's head is content its author controls, and a task that checks it out
// and starts an agent runs it with the operator's executor environment.
// GitHub's watches mark such a task so the host refuses an automatic start; a
// plugin cannot set that marker, so the protection here is a placement that
// cannot start an agent: no checkout, no start flag, and a step that does not
// start one on entry. If no such step exists the pull request is skipped.
func (p *Poller) place(ctx context.Context, host pluginsdk.Host, watch Watch, detail PullDetail, resolved *steps) (placement, bool) {
	if !detail.Fork {
		return placement{
			stepID:     watch.WorkflowStepID,
			startAgent: watch.StartAgent,
			checkout:   safeBranch(detail.HeadBranch),
		}, true
	}
	target := watch.WorkflowStepID
	if watch.ForkWorkflowStepID != "" {
		target = watch.ForkWorkflowStepID
	}
	// Checked on every run, not only at save time: the step can be edited later.
	starts, known := resolved.autoStarts(ctx, host, watch.WorkflowID, target)
	if !known || starts {
		return placement{}, false
	}
	return placement{stepID: target}, true
}

// pollReviews files a task per new pull request awaiting review, then runs
// cleanup, and returns the failures it met.
func (p *Poller) pollReviews(ctx context.Context, host pluginsdk.Host, watch Watch, result *Result) []string {
	if p.reviews == nil {
		return []string{"review watches are not available"}
	}
	var failures []string
	candidates, err := p.reviews.SearchReviewRequests(ctx, ReviewQuery{
		Repos: watch.Repos, Labels: watch.Labels, Query: watch.Query,
	})
	if err != nil {
		failures = append(failures, err.Error())
	}
	resolved := &steps{}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return failures
		}
		result.Matched++
		if _, seen, err := p.store.Seen(ctx, watch, candidate.Repo, candidate.Number); err != nil {
			failures = append(failures, fmt.Sprintf("%s#%d: %s", candidate.Repo.FullName(), candidate.Number, err))
			continue
		} else if seen {
			result.Duplicates++
			continue
		}
		if result.Inflight+result.Created >= watch.MaxInflightTasks {
			result.Throttled++
			continue
		}
		detail, err := p.reviews.PullDetail(ctx, candidate.Repo, candidate.Number, watch.ReviewScope)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s#%d: %s", candidate.Repo.FullName(), candidate.Number, err))
			if errors.Is(err, ErrBackoff) {
				break
			}
			continue
		}
		switch {
		case !detail.Open:
			continue
		case detail.Draft && !watch.IncludeDrafts:
			result.Drafts++
			continue
		case watch.ReviewScope == ReviewScopeUser && !detail.RequestedFromMe:
			continue
		}
		where, ok := p.place(ctx, host, watch, detail, resolved)
		if !ok {
			result.SkippedForks++
			continue
		}
		taskID, err := p.createReviewTask(ctx, host, watch, candidate, detail, where)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s#%d: %s", candidate.Repo.FullName(), candidate.Number, err))
			continue
		}
		if err := p.store.Record(ctx, watch, candidate.Repo, candidate.Number, detail.URL, taskID); err != nil {
			failures = append(failures, fmt.Sprintf(
				"%s#%d: task %s was created but could not be recorded; it may be created again: %s",
				candidate.Repo.FullName(), candidate.Number, taskID, err))
		}
		result.Created++
		if detail.Fork {
			// The placement above only knows the step's own actions. Workflow
			// automation can still move a card on (an on_enter chain, a rule),
			// so read the task back and say so if it is not where we put it.
			if landed, err := host.Tasks().Get(ctx, taskID); err == nil && landed != nil && landed.WorkflowStepID != "" && landed.WorkflowStepID != where.stepID {
				failures = append(failures, fmt.Sprintf(
					"%s#%d: the fork task %s was moved by workflow automation out of the column it was filed in; check that column's rules",
					candidate.Repo.FullName(), candidate.Number, taskID))
			}
		}
	}
	return append(failures, p.cleanup(ctx, host, watch, result)...)
}

// createReviewTask turns one pull request into one task.
func (p *Poller) createReviewTask(ctx context.Context, host pluginsdk.Host, watch Watch, candidate PullRequest, detail PullDetail, where placement) (string, error) {
	template := watch.Prompt
	if template == "" {
		template = DefaultReviewPrompt
	}
	stepID := where.stepID
	metadata := map[string]any{
		"forgejo_repo":      candidate.Repo.FullName(),
		"forgejo_pr_number": candidate.Number,
		"forgejo_pr_url":    detail.URL,
		"forgejo_watch_id":  watch.ID,
		"forgejo_author":    detail.Author,
		"forgejo_head":      detail.HeadBranch,
		"forgejo_base":      detail.BaseBranch,
	}
	if detail.Fork {
		metadata["forgejo_fork"] = true
	}
	input := pluginsdk.CreateTaskInput{
		WorkspaceID:    watch.WorkspaceID,
		WorkflowID:     watch.WorkflowID,
		WorkflowStepID: &stepID,
		Title:          truncateRunes(fmt.Sprintf("PR #%d: %s", candidate.Number, strings.TrimSpace(detail.Title)), maxTitleRunes),
		Description:    interpolate(template, candidate.Repo, candidate.Number, detail),
		StartAgent:     where.startAgent,
		Metadata:       metadata,
	}
	if watch.RepositoryID != "" {
		attached := pluginsdk.PluginTaskRepository{RepositoryID: watch.RepositoryID}
		base := detail.BaseBranch
		if base == "" {
			base = watch.BaseBranch
		}
		if base != "" {
			attached.BaseBranch = &base
		}
		if where.checkout {
			head, number := detail.HeadBranch, candidate.Number
			attached.CheckoutBranch, attached.PullRequestNumber = &head, &number
		}
		input.Repositories = []pluginsdk.PluginTaskRepository{attached}
	}
	if watch.AgentProfileID != "" || watch.ExecutorProfileID != "" {
		launch := &pluginsdk.PluginTaskLaunchOptions{}
		if watch.AgentProfileID != "" {
			id := watch.AgentProfileID
			launch.AgentProfileID = &id
		}
		if watch.ExecutorProfileID != "" {
			id := watch.ExecutorProfileID
			launch.ExecutorProfileID = &id
		}
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
