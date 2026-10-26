package forgejo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

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
	associations *sourcecontrol.Associations
	providerID   string
	// now is the clock; tests replace it.
	now func() time.Time
}

var _ sourcecontrol.ChangeRequestDetailReader = (*PullActions)(nil)

// NewPullActions returns the pull request action adapter.
func NewPullActions(connection *Connection, repositories *Repositories, associations *sourcecontrol.Associations, providerID string) *PullActions {
	return &PullActions{connection: connection, repositories: repositories, associations: associations, providerID: providerID, now: time.Now}
}

// linked is a pull request resolved from the task's own associations.
type linked struct {
	client     *rest.Client
	repository sourcecontrol.Repository
	identity   sourcecontrol.ChangeRequestIdentity
}

// resolveLinked finds the pull request a call may act on. number 0 means "the
// task's only pull request". The repository comes from the stored identity, so
// nothing the caller sends can redirect the call to another repository.
func (a *PullActions) resolveLinked(ctx context.Context, workspaceID, taskID string, number int64) (linked, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(taskID) == "" {
		return linked{}, scm.ErrNotLinked
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
		return linked{}, scm.ErrNotLinked
	case len(matches) > 1:
		return linked{}, scm.ErrAmbiguous
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
	read := detailReads{client: target.client, owner: target.repository.OwnerOrProject, name: target.repository.Name, number: target.identity.Number}

	pull, err := read.client.PullRequest(ctx, read.owner, read.name, read.number)
	if err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	details := a.pullDetails(read, target.identity, pull)
	if me, err := read.client.CurrentUser(ctx); err == nil {
		details.Actor = me.Login
	}

	mergeable := a.settledMergeable(pull)
	merge := sourcecontrol.MergeStatus{Styles: []string{}, Blockers: []string{}, Mergeable: mergeable}
	if err := read.mergeSettings(ctx, target.identity.RepositoryID, &merge); err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	statusCheckRequired, err := read.protection(ctx, pull.Base.Ref, &merge)
	if err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	if err := read.checks(ctx, pull.Head.Sha, &details); err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	summary, err := read.reviews(ctx, &details)
	if err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}
	if summary != nil {
		merge.Approvals = summary.Approved
	}
	if err := read.comments(ctx, &details); err != nil {
		return sourcecontrol.ChangeRequestDetails{}, err
	}

	merge.Blockers = mergeBlockers(details.State, mergeable, details.PipelineState, statusCheckRequired, summary, merge.RequiredApprovals)
	details.Merge = merge
	return details, nil
}

// pullDetails maps the pull request itself, with every list empty rather than
// nil.
func (a *PullActions) pullDetails(read detailReads, identity sourcecontrol.ChangeRequestIdentity, pull rest.PullRequest) sourcecontrol.ChangeRequestDetails {
	details := sourcecontrol.ChangeRequestDetails{
		ProviderID:         a.providerID,
		ReviewKey:          identityReviewKey(identity),
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
		details.URL = PullRequestURL(read.client.Scope(), read.owner, read.name, pull.Number)
	}
	for _, requested := range pull.Requested {
		if login := strings.TrimSpace(requested.Login); login != "" {
			details.RequestedReviewers = append(details.RequestedReviewers, login)
		}
	}
	return details
}

// detailReads are the reads behind the details of one pull request. Each
// leaves its part as it was on a soft failure.
type detailReads struct {
	client *rest.Client
	owner  string
	name   string
	number int64
}

func (r detailReads) mergeSettings(ctx context.Context, repositoryID string, merge *sourcecontrol.MergeStatus) error {
	id, err := parseRepositoryID(repositoryID)
	if err != nil {
		return nil
	}
	repo, err := r.client.RepoByID(ctx, id)
	switch {
	case err == nil:
		merge.Styles = nonNil(repo.MergeStyles())
		merge.DefaultStyle = repo.DefaultMergeStyle
		merge.DeleteBranchDefault = repo.DefaultDeleteBranch
	case !softFailure(err):
		return err
	}
	return nil
}

// protection reads the base branch's protection and reports whether it
// requires status checks.
func (r detailReads) protection(ctx context.Context, base string, merge *sourcecontrol.MergeStatus) (bool, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return false, nil
	}
	branch, err := r.client.BranchDetail(ctx, r.owner, r.name, base)
	switch {
	case err == nil:
		merge.ProtectionVisible = true
		required := int(branch.RequiredApprovals)
		merge.RequiredApprovals = &required
		merge.CanMerge = userCanMerge(branch)
		return branch.EnableStatusCheck, nil
	case !softFailure(err):
		return false, err
	}
	return false, nil
}

func (r detailReads) checks(ctx context.Context, sha string, details *sourcecontrol.ChangeRequestDetails) error {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return nil
	}
	combined, err := r.client.CombinedStatus(ctx, r.owner, r.name, sha)
	switch {
	case err == nil:
		details.PipelineState = pipelineState(combined.State)
		details.Checks = normalizeChecks(combined.Statuses)
	case !softFailure(err):
		return err
	}
	return nil
}

// reviews reads the reviews and returns their summary, nil when they could not
// be read.
func (r detailReads) reviews(ctx context.Context, details *sourcecontrol.ChangeRequestDetails) (*sourcecontrol.ReviewTaskReview, error) {
	reviews, err := r.client.Reviews(ctx, r.owner, r.name, r.number)
	switch {
	case err == nil:
		details.Reviews = detailReviews(reviews, &details.Truncated)
		return summarizeReviews(reviews), nil
	case !softFailure(err):
		return nil, err
	}
	return nil, nil
}

func (r detailReads) comments(ctx context.Context, details *sourcecontrol.ChangeRequestDetails) error {
	comments, more, err := r.client.IssueComments(ctx, r.owner, r.name, r.number, maxDetailComments)
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
		return err
	}
	return nil
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
func (a *PullActions) settledMergeable(pull rest.PullRequest) *bool {
	return settleMergeable(pull, a.now())
}

// userCanMerge reports the branch's per-user merge permission. The field is
// absent on instances that do not report it, and a plain bool cannot say so,
// so presence is inferred from the branch being protected or the field being
// true: false on an unprotected branch is indistinguishable from "unreported".
func userCanMerge(branch rest.Branch) *bool {
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

func detailReviews(reviews []rest.Review, truncated *[]string) []sourcecontrol.DetailReview {
	if len(reviews) > maxDetailReviews {
		*truncated = append(*truncated, truncatedReviews)
		reviews = reviews[len(reviews)-maxDetailReviews:]
	}
	out := make([]sourcecontrol.DetailReview, 0, len(reviews))
	for _, review := range reviews {
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
	return errors.Is(err, scm.ErrNotFound) || errors.Is(err, scm.ErrUnauthorized)
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
