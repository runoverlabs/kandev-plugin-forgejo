package scm

// CI states, normalized across every surface a provider reports CI through.
const (
	// CIStateSuccess means every known check passed.
	CIStateSuccess = "success"
	// CIStateFailure means at least one check failed, errored, or was cancelled.
	CIStateFailure = "failure"
	// CIStateRunning means work is in progress.
	CIStateRunning = "running"
	// CIStatePending means work is accepted but not started.
	CIStatePending = "pending"
	// CIStateNone means the ref has no CI at all.
	CIStateNone = "none"
)

// CI source identifiers, reported so a caller can tell a complete answer from
// a degraded one.
const (
	CISourceRuns   = "actions_runs"
	CISourceTasks  = "actions_tasks"
	CISourceStatus = "commit_status"
	CISourceNone   = "none"
)

// CIJob is one unit of CI work: an Actions job, an Actions task, or a commit
// status entry, normalized into one shape.
type CIJob struct {
	// ID addresses the job log. Zero means this source has no log to fetch.
	ID     int64  `json:"id,omitempty"`
	Name   string `json:"name"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}

// CIStatus is the resolved CI picture for one ref.
type CIStatus struct {
	Ref    string  `json:"ref"`
	SHA    string  `json:"sha,omitempty"`
	Source string  `json:"source"`
	State  string  `json:"state"`
	URL    string  `json:"url,omitempty"`
	Jobs   []CIJob `json:"jobs"`
}
