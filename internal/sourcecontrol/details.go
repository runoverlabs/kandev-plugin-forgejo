package sourcecontrol

import "context"

// ActionChangeRequestsDetails is the read action behind the review panel: one
// bounded response with everything the panel shows, so opening it costs one
// round trip.
const ActionChangeRequestsDetails = "change_requests.details"

// Blocker codes explain why a merge is not possible. They are fixed strings so
// the panel can map them to its own wording; none carries provider text.
const (
	BlockerConflicts         = "conflicts"
	BlockerChecksFailing     = "checks_failing"
	BlockerChecksPending     = "checks_pending"
	BlockerApprovalsRequired = "approvals_required"
	BlockerChangesRequested  = "changes_requested"
	BlockerNotOpen           = "not_open"
)

// MergeStatus is what a caller needs to decide whether to offer a merge. A nil
// pointer means the instance did not say, which is not the same as "no".
type MergeStatus struct {
	// Styles lists the merge styles the repository allows. Empty means the
	// instance did not report any; callers let the backend choose.
	Styles       []string `json:"styles"`
	DefaultStyle string   `json:"default_style,omitempty"`
	// DeleteBranchDefault is the repository's default for deleting the head
	// branch after a merge.
	DeleteBranchDefault bool `json:"delete_branch_default"`
	// Mergeable is nil while the instance has not computed it.
	Mergeable *bool `json:"mergeable"`
	// CanMerge is the token's own merge permission on the base branch, nil
	// where the instance does not report it.
	CanMerge *bool `json:"can_merge"`
	// RequiredApprovals is nil when protection is not visible to this token.
	RequiredApprovals *int `json:"required_approvals"`
	Approvals         int  `json:"approvals"`
	// ProtectionVisible is false when the base branch's protection could not be
	// read, so the panel can say "unavailable to this token".
	ProtectionVisible bool     `json:"protection_visible"`
	Blockers          []string `json:"blockers"`
}

// DetailReview is one submitted review.
type DetailReview struct {
	ID          int64  `json:"id"`
	Author      string `json:"author"`
	State       string `json:"state"`
	Body        string `json:"body,omitempty"`
	CommitID    string `json:"commit_id,omitempty"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// DetailComment is one conversation comment.
type DetailComment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ChangeRequestDetails is the full panel model for one linked change request.
type ChangeRequestDetails struct {
	ProviderID string `json:"provider_id"`
	ReviewKey  string `json:"review_key"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Author     string `json:"author"`
	// Description is the pull request body, clipped.
	Description  string `json:"description,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	HeadSHA      string `json:"head_sha"`
	// Additions, Deletions and ChangedFiles are nil on instances that omit them.
	Additions     *int                    `json:"additions"`
	Deletions     *int                    `json:"deletions"`
	ChangedFiles  *int                    `json:"changed_files"`
	Merge         MergeStatus             `json:"merge"`
	PipelineState string                  `json:"pipeline_state"`
	Checks        []ReviewTaskStatusCheck `json:"checks"`
	Reviews       []DetailReview          `json:"reviews"`
	// RequestedReviewers are logins.
	RequestedReviewers []string        `json:"requested_reviewers"`
	Comments           []DetailComment `json:"comments"`
	// Truncated names any list cut to keep the response bounded.
	Truncated []string `json:"truncated"`
	// Actor is the Forgejo account every write acts as.
	Actor string `json:"actor,omitempty"`
}

// ChangeRequestDetailReader is the port behind ActionChangeRequestsDetails.
// It must resolve the pull request from the task's linked set and never from a
// number the caller supplies on its own.
type ChangeRequestDetailReader interface {
	Details(ctx context.Context, workspaceID, taskID string, number int64) (ChangeRequestDetails, error)
}
