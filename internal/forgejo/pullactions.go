package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"kandev-plugin-forgejo/internal/scm"
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
// Failures are *scm.WriteError.
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
	var writeErr *scm.WriteError
	if errors.As(err, &writeErr) && writeErr.Reason == scm.ReasonUpToDate {
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

// pageNewest returns the newest limit items of a list endpoint that serves
// oldest first, still oldest first, and reports whether older ones were left
// out. X-Total-Count, which every supported host sends, locates the last pages;
// the page size is taken from the first answer, as the host may cap it below
// the one asked for.
//
// Not every list pages: an issue's conversation comments ignore page and limit
// (Forgejo 7 to 17, Gitea 1.27) and come back whole, which shows as a first
// page holding the total. Walking such a list page by page would repeat it.
func pageNewest[T any](ctx context.Context, c *Client, path string, limit int) ([]T, bool, error) {
	first, total, err := listPage[T](ctx, c, path, 1)
	if err != nil {
		return nil, false, err
	}
	if total < 0 {
		return pageNewestUncounted(ctx, c, path, limit, first)
	}
	if len(first) == 0 || len(first) >= total {
		items := newestItems(first, limit)
		return items, len(first) > len(items), nil
	}
	size := len(first)
	last := (total + size - 1) / size
	from := 1
	if total > limit {
		from = (total-limit)/size + 1
	}
	from = max(from, last-maxPullListPages+1)
	var all []T
	if from == 1 {
		all = first
	}
	for page := max(from, 2); page <= last; page++ {
		batch, _, err := listPage[T](ctx, c, path, page)
		if err != nil {
			return nil, false, err
		}
		all = append(all, batch...)
		if len(batch) < size {
			break
		}
	}
	items := newestItems(all, limit)
	return items, total > len(items), nil
}

// pageNewestUncounted is pageNewest for an answer without a count: it walks
// forward from the first page, keeping the newest limit items.
func pageNewestUncounted[T any](ctx context.Context, c *Client, path string, limit int, first []T) ([]T, bool, error) {
	all, dropped := first, false
	batch := first
	for page := 2; len(batch) == pullListPageSize && page <= maxPullListPages; page++ {
		var err error
		if batch, _, err = listPage[T](ctx, c, path, page); err != nil {
			return nil, false, err
		}
		all = append(all, batch...)
		if len(all) > limit {
			all, dropped = newestItems(all, limit), true
		}
	}
	items := newestItems(all, limit)
	return items, dropped || len(all) > len(items) || len(batch) == pullListPageSize, nil
}

func listPage[T any](ctx context.Context, c *Client, path string, page int) ([]T, int, error) {
	query := url.Values{}
	query.Set("page", strconv.Itoa(page))
	query.Set("limit", strconv.Itoa(pullListPageSize))
	var batch []T
	total, err := c.getCounted(ctx, path, query, &batch)
	return batch, total, err
}

func newestItems[T any](items []T, limit int) []T {
	if len(items) <= limit {
		return items
	}
	return append([]T(nil), items[len(items)-limit:]...)
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
// non-admin gets scm.ErrUnauthorized or scm.ErrNotFound, which callers report as
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

// IssueComments lists the newest limit conversation comments, oldest first;
// truncated reports older ones existed.
func (c *Client) IssueComments(ctx context.Context, owner, name string, number int64, limit int) ([]IssueComment, bool, error) {
	return pageNewest[IssueComment](ctx, c, issuePath(owner, name, number, "/comments"), limit)
}
