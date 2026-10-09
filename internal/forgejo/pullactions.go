package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Bounds for the read methods below. They keep a composite read inside the
// host's 15-second action budget and a response inside its 1 MiB reply cap.
const (
	// MaxPullDiffBytes caps the diff text a caller receives.
	MaxPullDiffBytes = 256 << 10
	// maxPullListPages bounds how many pages PullFiles and PullCommits walk.
	maxPullListPages = 10
	pullListPageSize = 50
)

// Merge styles, spelled as the merge endpoint's `Do` field expects them.
const (
	MergeStyleMerge           = "merge"
	MergeStyleRebase          = "rebase"
	MergeStyleRebaseMerge     = "rebase-merge"
	MergeStyleSquash          = "squash"
	MergeStyleFastForwardOnly = "fast-forward-only"
)

// MergeStyles lists the styles the repository reports as enabled, in the order
// a merge button should prefer them (squash, merge, rebase, then the rest). An
// empty result means "not reported": callers should let the backend choose
// rather than treat it as "nothing is allowed".
func (r Repo) MergeStyles() []string {
	var styles []string
	add := func(enabled bool, style string) {
		if enabled {
			styles = append(styles, style)
		}
	}
	add(r.AllowSquashMerge, MergeStyleSquash)
	add(r.AllowMergeCommits, MergeStyleMerge)
	add(r.AllowRebase, MergeStyleRebase)
	add(r.AllowRebaseExplicit, MergeStyleRebaseMerge)
	add(r.AllowFastForwardOnlyMerge, MergeStyleFastForwardOnly)
	return styles
}

// MergeInput is the body of a merge. HeadCommitID is the SHA the caller last
// saw; a mismatch makes the instance refuse, so a stale click cannot merge
// commits nobody looked at.
type MergeInput struct {
	Do                     string `json:"Do"`
	Title                  string `json:"MergeTitleField,omitempty"`
	Message                string `json:"MergeMessageField,omitempty"`
	HeadCommitID           string `json:"head_commit_id,omitempty"`
	DeleteBranch           bool   `json:"delete_branch_after_merge,omitempty"`
	MergeWhenChecksSucceed bool   `json:"merge_when_checks_succeed,omitempty"`
	ForceMerge             bool   `json:"force_merge,omitempty"`
}

func pullPath(owner, name string, number int64, suffix string) string {
	return fmt.Sprintf("/repos/%s/%s/pulls/%d%s", pathSegment(owner), pathSegment(name), number, suffix)
}

func issuePath(owner, name string, number int64, suffix string) string {
	return fmt.Sprintf("/repos/%s/%s/issues/%d%s", pathSegment(owner), pathSegment(name), number, suffix)
}

// MergePullRequest merges, or with MergeWhenChecksSucceed schedules a merge.
// Failures are *WriteError.
func (c *Client) MergePullRequest(ctx context.Context, owner, name string, number int64, input MergeInput) error {
	if input.Do == "" {
		return errors.New("forgejo: a merge style is required")
	}
	return c.write(ctx, http.MethodPost, pullPath(owner, name, number, "/merge"), nil, input, nil)
}

// CancelAutoMerge cancels a merge scheduled for when checks succeed.
func (c *Client) CancelAutoMerge(ctx context.Context, owner, name string, number int64) error {
	return c.delete(ctx, pullPath(owner, name, number, "/merge"), nil)
}

// ReviewEvent values the review endpoint accepts. The spelling is verified per
// instance in docs/development.md.
const (
	ReviewApprove        = "APPROVED"
	ReviewRequestChanges = "REQUEST_CHANGES"
	ReviewComment        = "COMMENT"
)

// InlineComment is one inline comment on a submitted review. NewPosition is a
// line in the new file, OldPosition a line in the old one.
type InlineComment struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	NewPosition int64  `json:"new_position,omitempty"`
	OldPosition int64  `json:"old_position,omitempty"`
}

// SubmitReviewInput is the body of a review submission.
type SubmitReviewInput struct {
	Event    string          `json:"event"`
	Body     string          `json:"body,omitempty"`
	CommitID string          `json:"commit_id,omitempty"`
	Comments []InlineComment `json:"comments,omitempty"`
}

// SubmitReview submits a review as the token's account.
func (c *Client) SubmitReview(ctx context.Context, owner, name string, number int64, input SubmitReviewInput) (Review, error) {
	var review Review
	if err := c.write(ctx, http.MethodPost, pullPath(owner, name, number, "/reviews"), nil, input, &review); err != nil {
		return Review{}, err
	}
	return review, nil
}

type reviewersBody struct {
	Reviewers []string `json:"reviewers"`
}

// RequestReviewers asks the named users to review.
func (c *Client) RequestReviewers(ctx context.Context, owner, name string, number int64, logins []string) error {
	return c.write(ctx, http.MethodPost, pullPath(owner, name, number, "/requested_reviewers"), nil, reviewersBody{Reviewers: logins}, nil)
}

// RemoveReviewRequest withdraws review requests.
func (c *Client) RemoveReviewRequest(ctx context.Context, owner, name string, number int64, logins []string) error {
	return c.delete(ctx, pullPath(owner, name, number, "/requested_reviewers"), reviewersBody{Reviewers: logins})
}

// UpdateBranch brings the head branch up to date with its base. style is
// "merge" or "rebase".
func (c *Client) UpdateBranch(ctx context.Context, owner, name string, number int64, style string) error {
	if style != "merge" && style != "rebase" {
		return errors.New("forgejo: update style must be merge or rebase")
	}
	query := url.Values{"style": []string{style}}
	err := c.write(ctx, http.MethodPost, pullPath(owner, name, number, "/update"), query, nil, nil)
	var writeErr *WriteError
	if errors.As(err, &writeErr) && writeErr.Reason == ReasonUpToDate {
		// Forgejo 16 answers 200 for a no-op; Gitea and Forgejo 7 answer 500.
		// Both mean the branch is current, so the call is idempotent.
		return nil
	}
	return err
}

// ChangedFile is one entry of a pull request's file list.
type ChangedFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// PullCommit is one commit of a pull request.
type PullCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

// pageAll walks a paginated list endpoint up to limit items or maxPullListPages
// pages, and reports whether more existed.
func pageAll[T any](ctx context.Context, c *Client, path string, limit int) ([]T, bool, error) {
	var all []T
	for page := 1; page <= maxPullListPages; page++ {
		query := url.Values{}
		query.Set("page", strconv.Itoa(page))
		query.Set("limit", strconv.Itoa(pullListPageSize))
		var batch []T
		if err := c.get(ctx, path, query, &batch); err != nil {
			return nil, false, err
		}
		all = append(all, batch...)
		if len(all) >= limit {
			return all[:limit], len(all) > limit || len(batch) == pullListPageSize, nil
		}
		if len(batch) < pullListPageSize {
			return all, false, nil
		}
	}
	return all, true, nil
}

// PullFiles lists changed files, up to limit; truncated reports more existed.
func (c *Client) PullFiles(ctx context.Context, owner, name string, number int64, limit int) ([]ChangedFile, bool, error) {
	return pageAll[ChangedFile](ctx, c, pullPath(owner, name, number, "/files"), limit)
}

// PullCommits lists the commits, up to limit; truncated reports more existed.
func (c *Client) PullCommits(ctx context.Context, owner, name string, number int64, limit int) ([]PullCommit, bool, error) {
	return pageAll[PullCommit](ctx, c, pullPath(owner, name, number, "/commits"), limit)
}

// PullDiff returns the start of the unified diff, at most maxBytes (capped at
// MaxPullDiffBytes), and whether it was cut.
func (c *Client) PullDiff(ctx context.Context, owner, name string, number int64, maxBytes int) (string, bool, error) {
	if maxBytes <= 0 || maxBytes > MaxPullDiffBytes {
		maxBytes = MaxPullDiffBytes
	}
	return c.getHead(ctx, pullPath(owner, name, number, ".diff"), nil, maxBytes)
}

// CreateComment adds a conversation comment to a pull request.
func (c *Client) CreateComment(ctx context.Context, owner, name string, number int64, body string) error {
	return c.write(ctx, http.MethodPost, issuePath(owner, name, number, "/comments"), nil, map[string]string{"body": body}, nil)
}

// RepoLabels lists repository labels, so names can be resolved to the numeric
// ids every supported version accepts (names are newer).
func (c *Client) RepoLabels(ctx context.Context, owner, name string) ([]Label, error) {
	labels, _, err := pageAll[Label](ctx, c, "/repos/"+pathSegment(owner)+"/"+pathSegment(name)+"/labels", pullListPageSize*maxPullListPages)
	return labels, err
}

// SetLabels replaces the labels on a pull request with the given label ids.
func (c *Client) SetLabels(ctx context.Context, owner, name string, number int64, ids []int64) error {
	if ids == nil {
		ids = []int64{}
	}
	return c.put(ctx, issuePath(owner, name, number, "/labels"), map[string][]int64{"labels": ids}, nil)
}

// SetAssignees replaces the assignees. An empty list clears them, which the
// omit-when-empty EditPullRequest body cannot express.
func (c *Client) SetAssignees(ctx context.Context, owner, name string, number int64, logins []string) error {
	if logins == nil {
		logins = []string{}
	}
	return c.write(ctx, http.MethodPatch, issuePath(owner, name, number, ""), nil, map[string][]string{"assignees": logins}, nil)
}

// BranchDetail fetches one branch with the protection facts the instance shows
// a non-admin token.
func (c *Client) BranchDetail(ctx context.Context, owner, name, branch string) (Branch, error) {
	var detail Branch
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/branches/" + pathSegment(branch)
	if err := c.get(ctx, path, nil, &detail); err != nil {
		return Branch{}, err
	}
	return detail, nil
}

// BranchProtection is the full protection rule. Only admins can read it; a
// non-admin gets ErrUnauthorized or ErrNotFound, which callers report as
// "unavailable to this token" rather than as a failure.
type BranchProtection struct {
	RuleName            string   `json:"rule_name"`
	RequiredApprovals   int64    `json:"required_approvals"`
	EnableStatusCheck   bool     `json:"enable_status_check"`
	StatusCheckContexts []string `json:"status_check_contexts"`
}

// BranchProtectionRule reads the protection rule for a branch name.
func (c *Client) BranchProtectionRule(ctx context.Context, owner, name, branch string) (BranchProtection, error) {
	var rule BranchProtection
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/branch_protections/" + pathSegment(branch)
	if err := c.get(ctx, path, nil, &rule); err != nil {
		return BranchProtection{}, err
	}
	return rule, nil
}

// IssueComment is one conversation comment on a pull request.
type IssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      User   `json:"user"`
}

// IssueComments lists conversation comments, oldest first, up to limit;
// truncated reports more existed.
func (c *Client) IssueComments(ctx context.Context, owner, name string, number int64, limit int) ([]IssueComment, bool, error) {
	return pageAll[IssueComment](ctx, c, issuePath(owner, name, number, "/comments"), limit)
}
