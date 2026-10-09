package sourcecontrol

import "context"

// Write actions on a change request. Every one acts on a change request the
// verified task has already linked, and every one that depends on what the
// caller last saw carries the head SHA it saw.
const (
	ActionChangeRequestsMerge            = "change_requests.merge"
	ActionChangeRequestsReview           = "change_requests.review"
	ActionChangeRequestsRequestReviewers = "change_requests.request_reviewers"
	ActionChangeRequestsUpdateBranch     = "change_requests.update_branch"
	ActionChangeRequestsComment          = "change_requests.comment"
)

// Review events, spelled neutrally; each provider maps them to its own.
const (
	ReviewEventApprove        = "approve"
	ReviewEventRequestChanges = "request_changes"
	ReviewEventComment        = "comment"
)

// Merge outcomes.
const (
	MergeOutcomeMerged    = "merged"
	MergeOutcomeScheduled = "scheduled"
	MergeOutcomeCancelled = "cancelled"
)

// Limits shared by the actions and the agent tools, so one rule bounds both.
const (
	MaxReviewComments    = 50
	MaxReviewers         = 20
	MaxCommentBodyRunes  = 16000
	MaxReviewBodyRunes   = 16000
	MaxInlineCommentRune = 4000
)

// MergeRequest merges, schedules a merge, or cancels a scheduled one.
type MergeRequest struct {
	// Number names the change request; zero means the task's only one.
	Number int64
	// HeadSHA is required: the commit the caller last saw. A merge of anything
	// else is refused, so nobody merges commits they have not looked at.
	HeadSHA string
	// Style is optional; empty lets the repository's own default decide.
	Style             string
	DeleteBranch      bool
	WhenChecksSucceed bool
	// CancelScheduled cancels a scheduled merge instead of merging.
	CancelScheduled bool
}

// MergeResult reports what a merge request did.
type MergeResult struct {
	Number  int64  `json:"number"`
	Outcome string `json:"outcome"`
	Style   string `json:"style,omitempty"`
}

// InlineComment is a comment on one line of the diff.
type InlineComment struct {
	Path string `json:"path"`
	Body string `json:"body"`
	// Line is the line number in the new version of the file.
	Line int64 `json:"line"`
}

// ReviewRequest submits a review.
type ReviewRequest struct {
	Number int64
	Event  string
	Body   string
	// HeadSHA, when given, pins the review to the commit that was read.
	HeadSHA  string
	Comments []InlineComment
}

// ReviewResult reports the submitted review.
type ReviewResult struct {
	Number int64  `json:"number"`
	State  string `json:"state"`
}

// ReviewersRequest changes the requested reviewers.
type ReviewersRequest struct {
	Number int64
	Add    []string
	Remove []string
}

// UpdateBranchRequest brings the head branch up to date with its base.
type UpdateBranchRequest struct {
	Number int64
	// Style is "merge" or "rebase"; empty means merge.
	Style string
}

// CommentRequest adds a conversation comment.
type CommentRequest struct {
	Number int64
	Body   string
}

// ActionResult is the reply to an action with nothing more to report.
type ActionResult struct {
	Number int64 `json:"number"`
	OK     bool  `json:"ok"`
}

// ChangeRequestActions is the port behind the write actions. Implementations
// resolve the change request from the task's linked set and never from a
// number the caller supplies on its own, and surface failures as typed errors
// with fixed reasons.
type ChangeRequestActions interface {
	Merge(ctx context.Context, workspaceID, taskID string, request MergeRequest) (MergeResult, error)
	Review(ctx context.Context, workspaceID, taskID string, request ReviewRequest) (ReviewResult, error)
	RequestReviewers(ctx context.Context, workspaceID, taskID string, request ReviewersRequest) (ActionResult, error)
	UpdateBranch(ctx context.Context, workspaceID, taskID string, request UpdateBranchRequest) (ActionResult, error)
	Comment(ctx context.Context, workspaceID, taskID string, request CommentRequest) (ActionResult, error)
}
