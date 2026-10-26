package watches

import (
	"context"
	"errors"
	"fmt"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// PullRequest is the provider-neutral view of one pull request as a review
// search returns it. It is deliberately thin: the search answer lacks the
// branches, the head repository and the draft flag, so those come from
// PullDetail, fetched only for a pull request the ledger has not seen.
type PullRequest struct {
	Repo      scm.RepoRef
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
	Repos  []scm.RepoRef
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
	PullDetail(ctx context.Context, repo scm.RepoRef, number int64, scope ReviewScope) (PullDetail, error)
	// PullState reports whether a pull request is still open.
	PullState(ctx context.Context, repo scm.RepoRef, number int64) (PullState, error)
}

// WithReviews gives the poller the review source. Review watches are skipped,
// with an error on the watch, until it is set.
func (p *Poller) WithReviews(source ReviewSource) *Poller {
	p.reviews = source
	return p
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
		met, backoff := p.fileReview(ctx, host, watch, candidate, resolved, result)
		failures = append(failures, met...)
		if backoff {
			break
		}
	}
	return append(failures, p.cleanup(ctx, host, watch, result)...)
}

// fileReview creates the task for one pull request awaiting review, unless the
// ledger has it, the budget is full, or its detail rules it out. backoff is
// true when the instance refused in a way that should stop the batch.
func (p *Poller) fileReview(ctx context.Context, host pluginsdk.Host, watch Watch, candidate PullRequest, resolved *steps, result *Result) (failures []string, backoff bool) {
	if _, seen, err := p.store.Seen(ctx, watch, candidate.Repo, candidate.Number); err != nil {
		return []string{pullFailure(candidate, err)}, false
	} else if seen {
		result.Duplicates++
		return nil, false
	}
	if result.Inflight+result.Created >= watch.MaxInflightTasks {
		result.Throttled++
		return nil, false
	}
	detail, err := p.reviews.PullDetail(ctx, candidate.Repo, candidate.Number, watch.ReviewScope)
	if err != nil {
		return []string{pullFailure(candidate, err)}, errors.Is(err, ErrBackoff)
	}
	if excluded, draft := pullExcluded(watch, detail); excluded {
		if draft {
			result.Drafts++
		}
		return nil, false
	}
	where, ok := p.place(ctx, host, watch, detail, resolved)
	if !ok {
		result.SkippedForks++
		return nil, false
	}
	taskID, err := p.createReviewTask(ctx, host, watch, candidate, detail, where)
	if err != nil {
		return []string{pullFailure(candidate, err)}, false
	}
	if err := p.store.Record(ctx, watch, candidate.Repo, candidate.Number, detail.URL, taskID); err != nil {
		failures = append(failures, fmt.Sprintf(
			"%s#%d: task %s was created but could not be recorded; it may be created again: %s",
			candidate.Repo.FullName(), candidate.Number, taskID, err))
	}
	result.Created++
	// The placement only knows the step's own actions. Workflow automation can
	// still move a card on (an on_enter chain, a rule), so read a fork's task
	// back and say so if it is not where we put it.
	if detail.Fork && movedOut(ctx, host, taskID, where.stepID) {
		failures = append(failures, fmt.Sprintf(
			"%s#%d: the fork task %s was moved by workflow automation out of the column it was filed in; check that column's rules",
			candidate.Repo.FullName(), candidate.Number, taskID))
	}
	return failures, false
}

// pullExcluded reports whether a pull request's detail rules it out, and
// whether the reason is that it is a draft, which the run counts.
func pullExcluded(watch Watch, detail PullDetail) (excluded, draft bool) {
	switch {
	case !detail.Open:
		return true, false
	case detail.Draft && !watch.IncludeDrafts:
		return true, true
	case watch.ReviewScope == ReviewScopeUser && !detail.RequestedFromMe:
		return true, false
	}
	return false, false
}

// movedOut reports whether a task read back sits in a step other than stepID.
// A task that cannot be read back is not reported.
func movedOut(ctx context.Context, host pluginsdk.Host, taskID, stepID string) bool {
	landed, err := host.Tasks().Get(ctx, taskID)
	return err == nil && landed != nil && landed.WorkflowStepID != "" && landed.WorkflowStepID != stepID
}

func pullFailure(candidate PullRequest, err error) string {
	return fmt.Sprintf("%s#%d: %s", candidate.Repo.FullName(), candidate.Number, err)
}
