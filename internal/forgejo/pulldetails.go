package forgejo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// ErrNotLinked means the requested pull request is not among those linked to
// the task. It is the authorization failure for every pull request action: a
// number is only ever honoured when the task already owns it.
var ErrNotLinked = errors.New("forgejo: pull request is not linked to this task")

// ErrAmbiguous means the task links several pull requests and the caller named
// none of them.
var ErrAmbiguous = errors.New("forgejo: task links more than one pull request")

// Bounds that keep one details response far below the host's 1 MiB cap.
const (
	maxDetailComments    = 50
	maxDetailReviews     = 50
	maxDetailTextRunes   = 2000
	truncatedMarker      = "…"
	truncatedComments    = "comments"
	truncatedReviews     = "reviews"
	defaultPipelineState = "neutral"
)

// PullActions implements the neutral detail port and, in later steps, the
// write ports, for pull requests linked to a task.
type PullActions struct {
	connection   *Connection
	repositories *Repositories
	associations *Associations
	providerID   string
	// now is the clock; tests replace it.
	now func() time.Time
}

var _ sourcecontrol.ChangeRequestDetailReader = (*PullActions)(nil)

// NewPullActions returns the pull request action adapter.
func NewPullActions(connection *Connection, repositories *Repositories, associations *Associations, providerID string) *PullActions {
	return &PullActions{connection: connection, repositories: repositories, associations: associations, providerID: providerID, now: time.Now}
}

// linked is a pull request resolved from the task's own associations.
type linked struct {
	client     *Client
	repository sourcecontrol.Repository
	identity   sourcecontrol.ChangeRequestIdentity
}

// resolveLinked finds the pull request a call may act on. number 0 means "the
// task's only pull request". The repository comes from the stored identity, so
// nothing the caller sends can redirect the call to another repository.
func (a *PullActions) resolveLinked(ctx context.Context, workspaceID, taskID string, number int64) (linked, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(taskID) == "" {
		return linked{}, ErrNotLinked
	}
	client, err := a.connection.Client(ctx)
	if err != nil {
		return linked{}, err
	}
	identities, err := a.associations.ListForTask(ctx, workspaceID, taskID)
	if err != nil {
		return linked{}, err
	}
	var matches []sourcecontrol.ChangeRequestIdentity
	for _, identity := range identities {
		if identity.ConnectionScope != client.Scope() {
			continue
		}
		if number == 0 || identity.Number == number {
			matches = append(matches, identity)
		}
	}
	switch {
	case len(matches) == 0:
		return linked{}, ErrNotLinked
	case len(matches) > 1:
		return linked{}, ErrAmbiguous
	}
	repository, err := a.repositories.Resolve(ctx, "", sourcecontrol.RepositoryIdentity{
		ConnectionScope: matches[0].ConnectionScope,
		RepositoryID:    matches[0].RepositoryID,
	})
	if err != nil {
		return linked{}, err
	}
	return linked{client: client, repository: repository, identity: matches[0]}, nil
}

// Details loads everything the review panel shows for one linked pull request.
// Reads that the token may not be allowed (protection, statuses, reviews) leave
// their part empty instead of failing the whole response.
func (a *PullActions) Details(ctx context.Context, workspaceID, taskID string, number int64) (sourcecontrol.ChangeRequestDetails, error) {
	target, err := a.resolveLinked(ctx, workspaceID, taskID, number)
	if err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	client, owner, name := target.client, target.repository.OwnerOrProject, target.repository.Name

	pull, err := client.PullRequest(ctx, owner, name, target.identity.Number)
	if err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	details := sourcecontrol.ChangeRequestDetails{
		ProviderID:         a.providerID,
		ReviewKey:          identityReviewKey(target.identity),
		Number:             pull.Number,
		Title:              strings.TrimSpace(pull.Title),
		URL:                strings.TrimSpace(pull.HTMLURL),
		State:              pullRequestState(pull),
		Author:             strings.TrimSpace(pull.User.Login),
		Description:        clipText(pull.Body),
		CreatedAt:          pull.CreatedAt,
		SourceBranch:       pull.Head.Ref,
		TargetBranch:       pull.Base.Ref,
		HeadSHA:            pull.Head.Sha,
		Additions:          pull.Additions,
		Deletions:          pull.Deletions,
		ChangedFiles:       pull.ChangedFiles,
		PipelineState:      defaultPipelineState,
		Checks:             []sourcecontrol.ReviewTaskStatusCheck{},
		Reviews:            []sourcecontrol.DetailReview{},
		RequestedReviewers: []string{},
		Comments:           []sourcecontrol.DetailComment{},
		Truncated:          []string{},
	}
	if details.URL == "" {
		details.URL = PullRequestURL(client.Scope(), owner, name, pull.Number)
	}
	for _, requested := range pull.Requested {
		if login := strings.TrimSpace(requested.Login); login != "" {
			details.RequestedReviewers = append(details.RequestedReviewers, login)
		}
	}
	if me, err := client.CurrentUser(ctx); err == nil {
		details.Actor = me.Login
	}

	mergeable := a.settledMergeable(pull)
	merge := sourcecontrol.MergeStatus{Styles: []string{}, Blockers: []string{}, Mergeable: mergeable}
	if id, err := parseRepositoryID(target.identity.RepositoryID); err == nil {
		if repo, err := client.RepoByID(ctx, id); err == nil {
			merge.Styles = nonNil(repo.MergeStyles())
			merge.DefaultStyle = repo.DefaultMergeStyle
			merge.DeleteBranchDefault = repo.DefaultDeleteBranch
		} else if !softFailure(err) {
			return sourcecontrol.ChangeRequestDetails{}, err
		}
	}

	var statusCheckRequired bool
	if base := strings.TrimSpace(pull.Base.Ref); base != "" {
		branch, err := client.BranchDetail(ctx, owner, name, base)
		switch {
		case err == nil:
			merge.ProtectionVisible = true
			required := int(branch.RequiredApprovals)
			merge.RequiredApprovals = &required
			merge.CanMerge = userCanMerge(branch)
			statusCheckRequired = branch.EnableStatusCheck
		case !softFailure(err):
			return sourcecontrol.ChangeRequestDetails{}, err
		}
	}

	if sha := strings.TrimSpace(pull.Head.Sha); sha != "" {
		combined, err := client.CombinedStatus(ctx, owner, name, sha)
		switch {
		case err == nil:
			details.PipelineState = pipelineState(combined.State)
			details.Checks = normalizeChecks(combined.Statuses)
		case !softFailure(err):
			return sourcecontrol.ChangeRequestDetails{}, err
		}
	}

	var summary *sourcecontrol.ReviewTaskReview
	reviews, err := client.Reviews(ctx, owner, name, target.identity.Number)
	switch {
	case err == nil:
		summary = summarizeReviews(reviews)
		details.Reviews = detailReviews(reviews, &details.Truncated)
	case !softFailure(err):
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	if summary != nil {
		merge.Approvals = summary.Approved
	}

	comments, more, err := client.IssueComments(ctx, owner, name, target.identity.Number, maxDetailComments)
	switch {
	case err == nil:
		for _, comment := range comments {
			details.Comments = append(details.Comments, sourcecontrol.DetailComment{
				ID:        comment.ID,
				Author:    strings.TrimSpace(comment.User.Login),
				Body:      clipText(comment.Body),
				CreatedAt: comment.CreatedAt,
			})
		}
		if more {
			details.Truncated = append(details.Truncated, truncatedComments)
		}
	case !softFailure(err):
		return sourcecontrol.ChangeRequestDetails{}, err
	}

	merge.Blockers = mergeBlockers(details.State, mergeable, details.PipelineState, statusCheckRequired, summary, merge.RequiredApprovals)
	details.Merge = merge
	return details, nil
}

// mergeableSettle is how long after a pull request last changed a "not
// mergeable" answer is not yet trusted. Probed on Gitea 1.20 and 1.27, the flag
// reads false for up to a second or two after a PR is opened, and Gitea 1.27 was
// seen flipping true, false, true within three seconds.
const mergeableSettle = 30 * time.Second

// settledMergeable returns the pull request's mergeable flag, or nil (unknown)
// when it says false but the pull request changed too recently to believe it.
// A true answer is always passed through: a stale true is caught by the head SHA
// precondition on merge, whereas a stale false would wrongly block the user.
func (a *PullActions) settledMergeable(pull PullRequest) *bool {
	if pull.Mergeable == nil || *pull.Mergeable {
		return pull.Mergeable
	}
	updated := parseTimestampMillis(pull.UpdatedAt)
	if updated > 0 && a.now().Sub(time.UnixMilli(updated)) < mergeableSettle {
		return nil
	}
	return pull.Mergeable
}

// userCanMerge reports the branch's per-user merge permission. The field is
// absent on instances that do not report it, and a plain bool cannot say so,
// so presence is inferred from the branch being protected or the field being
// true: false on an unprotected branch is indistinguishable from "unreported".
func userCanMerge(branch Branch) *bool {
	if branch.UserCanMerge || branch.Protected {
		value := branch.UserCanMerge
		return &value
	}
	return nil
}

// mergeBlockers derives fixed reason codes. A reason is only given when the
// instance actually said so: an unknown mergeable flag adds nothing, so the
// panel stays quiet instead of flashing "not mergeable" right after a push.
func mergeBlockers(state string, mergeable *bool, pipeline string, checksRequired bool, review *sourcecontrol.ReviewTaskReview, required *int) []string {
	blockers := []string{}
	if state != "open" && state != "draft" {
		return append(blockers, sourcecontrol.BlockerNotOpen)
	}
	if mergeable != nil && !*mergeable {
		blockers = append(blockers, sourcecontrol.BlockerConflicts)
	}
	if checksRequired {
		switch pipeline {
		case "failure":
			blockers = append(blockers, sourcecontrol.BlockerChecksFailing)
		case "pending":
			blockers = append(blockers, sourcecontrol.BlockerChecksPending)
		}
	}
	if review != nil && review.State == "changes_requested" {
		blockers = append(blockers, sourcecontrol.BlockerChangesRequested)
	}
	if required != nil && *required > 0 {
		approved := 0
		if review != nil {
			approved = review.Approved
		}
		if approved < *required {
			blockers = append(blockers, sourcecontrol.BlockerApprovalsRequired)
		}
	}
	return blockers
}

func detailReviews(reviews []Review, truncated *[]string) []sourcecontrol.DetailReview {
	out := make([]sourcecontrol.DetailReview, 0, len(reviews))
	for _, review := range reviews {
		if len(out) == maxDetailReviews {
			*truncated = append(*truncated, truncatedReviews)
			break
		}
		out = append(out, sourcecontrol.DetailReview{
			ID:          review.ID,
			Author:      strings.TrimSpace(review.User.Login),
			State:       strings.ToUpper(strings.TrimSpace(review.State)),
			Body:        clipText(review.Body),
			CommitID:    review.CommitID,
			SubmittedAt: review.SubmittedAt,
		})
	}
	return out
}

// softFailure reports an error that means "this token cannot see that", which
// leaves a part of the response empty instead of failing the whole read.
func softFailure(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnauthorized)
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func clipText(text string) string {
	runes := []rune(text)
	if len(runes) <= maxDetailTextRunes {
		return text
	}
	return string(runes[:maxDetailTextRunes]) + truncatedMarker
}

func parseRepositoryID(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}
