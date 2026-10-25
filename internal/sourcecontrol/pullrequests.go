package sourcecontrol

import (
	"context"

	"kandev-plugin-forgejo/internal/scm"
)

// PullRequestSummary is one pull request as the agent tools report it.
type PullRequestSummary struct {
	Number int64
	// State is open, closed or merged: merged is told apart from closed, which
	// the provider's own state field does not do. Unknown when it reports none.
	State   string
	URL     string
	Title   string
	Head    string
	Base    string
	HeadSHA string
	// Draft is the work-in-progress title marker.
	Draft bool
}

// PullRequestReader reads and readies pull requests in a resolved repository.
type PullRequestReader interface {
	PullRequest(ctx context.Context, repository Repository, number int64) (PullRequestSummary, error)
	// OpenPullRequestForHead finds the open pull request whose head is branch,
	// or nil when there is none.
	OpenPullRequestForHead(ctx context.Context, repository Repository, branch string) (*PullRequestSummary, error)
	// MarkReady clears pull's work-in-progress marker and reports whether
	// anything changed. A pull request that is already ready is returned as is.
	MarkReady(ctx context.Context, repository Repository, pull PullRequestSummary) (PullRequestSummary, bool, error)
}

// CIReader reports CI for a ref of a resolved repository.
type CIReader interface {
	CIStatus(ctx context.Context, repository Repository, ref string) (scm.CIStatus, error)
	// JobLogTail returns at most maxBytes from the end of a job's log, and
	// whether anything before it was cut.
	JobLogTail(ctx context.Context, repository Repository, jobID int64, maxBytes int) (string, bool, error)
}
