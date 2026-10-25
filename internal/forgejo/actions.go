package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/scm"
)

// CI reporting is the one place where Forgejo and Gitea have genuinely
// diverged, so this file resolves a ref's CI state through three surfaces and
// takes the first that answers. Read from each release's own swagger.v1.json
// and confirmed against running instances:
//
//	endpoint                   G1.20  G1.24  G1.27  F7   F13  F16
//	/actions/runs              -      -      yes    -    yes  yes
//	/actions/runs/{id}/jobs    -      -      yes    -    -    yes
//	/actions/tasks             -      yes    yes    -    yes  yes
//	/actions/jobs/{id}/logs    -      yes    yes    -    -    yes
//	/commits/{ref}/status      yes    yes    yes    yes  yes  yes
//
// Forgejo 7 has no Actions API at all, and Forgejo 13 lists runs without
// serving their jobs. The combined
// commit status is last and present everywhere. It is not only a compatibility
// fallback: it is the only surface that sees CI running outside the forge,
// which on self-hosted Forgejo is common.
// actionTaskScanPages bounds the client-side scan of /actions/tasks. That
// endpoint cannot filter by ref on any release, so the newest few pages are
// scanned and anything older is treated as not present.
const actionTaskScanPages = 3

// actionTaskScanLimit is the page size used for that scan.
const actionTaskScanLimit = 50

// ActionRun mirrors one entry of /actions/runs. Forgejo reports a single
// `status` field rather than GitHub's status/conclusion pair.
type ActionRun struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CommitSHA string `json:"commit_sha"`
	PrettyRef string `json:"prettyref"`
	HTMLURL   string `json:"html_url"`
}

type actionRunList struct {
	TotalCount int64       `json:"total_count"`
	Runs       []ActionRun `json:"workflow_runs"`
}

// ActionRunJob mirrors one entry of /actions/runs/{id}/jobs. Its ID is the job
// id the log endpoint accepts.
type ActionRunJob struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	RunID  int64  `json:"run_id"`
}

// ActionTask mirrors one entry of /actions/tasks, the only Actions listing
// every supported release serves. Despite the envelope field name it is a job,
// not a run: one entry per workflow job.
type ActionTask struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	URL        string `json:"url"`
	RunNumber  int64  `json:"run_number"`
}

type actionTaskList struct {
	TotalCount int64        `json:"total_count"`
	Tasks      []ActionTask `json:"workflow_runs"`
}

// ActionRuns returns workflow runs for a ref, newest first. ref may be a branch
// name, a full git ref, or a commit SHA. Returns scm.ErrNotFound on a release that
// does not serve the endpoint.
func (c *Client) ActionRuns(ctx context.Context, owner, name, ref string, limit int) ([]ActionRun, error) {
	values := url.Values{}
	values.Set("limit", strconv.Itoa(limit))
	if isCommitSHA(ref) {
		values.Set("head_sha", ref)
	} else if trimmed := strings.TrimSpace(ref); trimmed != "" {
		values.Set("ref", gitRef(trimmed))
	}
	var payload actionRunList
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/actions/runs"
	if err := c.get(ctx, path, values, &payload); err != nil {
		return nil, err
	}
	return payload.Runs, nil
}

// ActionRunJobs returns the jobs of one run. Returns scm.ErrNotFound on a release
// that does not serve the endpoint.
func (c *Client) ActionRunJobs(ctx context.Context, owner, name string, runID int64) ([]ActionRunJob, error) {
	var jobs []ActionRunJob
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs", pathSegment(owner), pathSegment(name), runID)
	if err := c.get(ctx, path, nil, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// ActionTasks returns one page of Actions tasks, newest first.
func (c *Client) ActionTasks(ctx context.Context, owner, name string, page, limit int) ([]ActionTask, error) {
	if page < 1 {
		page = 1
	}
	values := url.Values{}
	values.Set("page", strconv.Itoa(page))
	values.Set("limit", strconv.Itoa(limit))
	var payload actionTaskList
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/actions/tasks"
	if err := c.get(ctx, path, values, &payload); err != nil {
		return nil, err
	}
	return payload.Tasks, nil
}

// JobLogTail returns the last maxBytes of a job's log and whether earlier
// output was dropped. Returns scm.ErrNotFound on a release that does not serve job
// logs, and for a job whose log is gone or never existed.
//
// Gitea answers an unknown job id with 500 rather than 404 — measured on
// 1.24.7, which returns {"message":"run job with id 1: resource does not
// exist"} with that status, where Forgejo 16.0.5 returns 404 for the same
// request. Reporting that as a server fault would send an agent chasing an
// outage instead of a stale job id, so it is normalized here, on this endpoint
// only.
func (c *Client) JobLogTail(ctx context.Context, owner, name string, jobID int64, maxBytes int) (string, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", pathSegment(owner), pathSegment(name), jobID)
	tail, truncated, err := c.getTail(ctx, path, nil, maxBytes)
	if err != nil {
		var status *StatusError
		if errors.As(err, &status) && status.Status >= 500 {
			return "", false, scm.ErrNotFound
		}
		return "", false, err
	}
	return tail, truncated, nil
}

// CIStatusFor resolves the CI picture for a ref, trying each surface in turn.
// A ref with no CI anywhere is not an error: it reports scm.CIStateNone.
func (c *Client) CIStatusFor(ctx context.Context, owner, name, ref string) (scm.CIStatus, error) {
	status := scm.CIStatus{Ref: ref, Source: scm.CISourceNone, State: scm.CIStateNone, Jobs: []scm.CIJob{}}
	if isCommitSHA(ref) {
		status.SHA = ref
	}

	if resolved, ok, err := c.ciFromRuns(ctx, owner, name, ref); err != nil {
		return scm.CIStatus{}, err
	} else if ok {
		return resolved, nil
	}
	if resolved, ok, err := c.ciFromTasks(ctx, owner, name, ref); err != nil {
		return scm.CIStatus{}, err
	} else if ok {
		return resolved, nil
	}
	if resolved, ok, err := c.ciFromCommitStatus(ctx, owner, name, ref); err != nil {
		return scm.CIStatus{}, err
	} else if ok {
		return resolved, nil
	}
	return status, nil
}

// ciFromRuns reads the newest workflow run for a ref and, where the release
// serves them, its jobs. A release without /actions/runs reports not-ok rather
// than failing, so the caller falls through to the next surface.
func (c *Client) ciFromRuns(ctx context.Context, owner, name, ref string) (scm.CIStatus, bool, error) {
	runs, err := c.ActionRuns(ctx, owner, name, ref, 10)
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) {
			return scm.CIStatus{}, false, nil
		}
		return scm.CIStatus{}, false, err
	}
	if len(runs) == 0 {
		return scm.CIStatus{}, false, nil
	}
	run := runs[0]
	status := scm.CIStatus{
		Ref:    ref,
		SHA:    run.CommitSHA,
		Source: scm.CISourceRuns,
		State:  normalizeActionStatus(run.Status),
		URL:    run.HTMLURL,
		Jobs:   []scm.CIJob{},
	}

	jobs, err := c.ActionRunJobs(ctx, owner, name, run.ID)
	if err != nil && !errors.Is(err, scm.ErrNotFound) {
		return scm.CIStatus{}, false, err
	}
	for _, job := range jobs {
		status.Jobs = append(status.Jobs, scm.CIJob{
			ID:     job.ID,
			Name:   strings.TrimSpace(job.Name),
			Status: normalizeActionStatus(job.Status),
		})
	}
	// A release that lists runs but not their jobs still reports the run
	// itself, so the caller always has something to name.
	if len(status.Jobs) == 0 {
		status.Jobs = append(status.Jobs, scm.CIJob{
			Name:   firstNonEmpty(run.Title, "workflow run"),
			Status: status.State,
			URL:    run.HTMLURL,
		})
	}
	return status, true, nil
}

// ciFromTasks scans the newest Actions tasks and keeps those matching ref.
// The endpoint cannot filter, so the scan is bounded and a ref older than the
// scan window reads as "no CI here" rather than stalling.
func (c *Client) ciFromTasks(ctx context.Context, owner, name, ref string) (scm.CIStatus, bool, error) {
	var matched []ActionTask
	newestRun := int64(-1)
	for page := 1; page <= actionTaskScanPages; page++ {
		tasks, err := c.ActionTasks(ctx, owner, name, page, actionTaskScanLimit)
		if err != nil {
			if errors.Is(err, scm.ErrNotFound) {
				return scm.CIStatus{}, false, nil
			}
			return scm.CIStatus{}, false, err
		}
		for _, task := range tasks {
			if !taskMatchesRef(task, ref) {
				continue
			}
			// Tasks arrive newest first, so the first match fixes the run
			// whose jobs are reported; older runs for the same ref are
			// history, not current state.
			if newestRun < 0 {
				newestRun = task.RunNumber
			}
			if task.RunNumber == newestRun {
				matched = append(matched, task)
			}
		}
		if len(tasks) < actionTaskScanLimit {
			break
		}
		if newestRun >= 0 {
			break
		}
	}
	if len(matched) == 0 {
		return scm.CIStatus{}, false, nil
	}

	status := scm.CIStatus{
		Ref:    ref,
		SHA:    matched[0].HeadSHA,
		Source: scm.CISourceTasks,
		URL:    matched[0].URL,
		Jobs:   make([]scm.CIJob, 0, len(matched)),
	}
	states := make([]string, 0, len(matched))
	for _, task := range matched {
		state := normalizeActionStatus(task.Status)
		states = append(states, state)
		status.Jobs = append(status.Jobs, scm.CIJob{
			ID:     task.ID,
			Name:   strings.TrimSpace(task.Name),
			Status: state,
			URL:    task.URL,
		})
	}
	status.State = rollUpStates(states)
	return status, true, nil
}

// ciFromCommitStatus reads the combined commit status. This is the portable
// floor and the only surface that sees CI reported by an external system.
func (c *Client) ciFromCommitStatus(ctx context.Context, owner, name, ref string) (scm.CIStatus, bool, error) {
	combined, err := c.CombinedStatus(ctx, owner, name, ref)
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) {
			return scm.CIStatus{}, false, nil
		}
		return scm.CIStatus{}, false, err
	}
	if len(combined.Statuses) == 0 {
		return scm.CIStatus{}, false, nil
	}
	status := scm.CIStatus{
		Ref:    ref,
		SHA:    combined.Sha,
		Source: scm.CISourceStatus,
		State:  normalizeCommitState(combined.State),
		Jobs:   make([]scm.CIJob, 0, len(combined.Statuses)),
	}
	states := make([]string, 0, len(combined.Statuses))
	for _, entry := range combined.Statuses {
		state := normalizeCommitState(entry.Status)
		states = append(states, state)
		status.Jobs = append(status.Jobs, scm.CIJob{
			Name:   firstNonEmpty(entry.Context, entry.Description),
			Status: state,
			URL:    entry.TargetURL,
		})
	}
	// Some releases leave the roll-up empty while the entries are populated.
	if status.State == scm.CIStateNone {
		status.State = rollUpStates(states)
	}
	return status, true, nil
}

// normalizeActionStatus maps the Actions vocabulary
// (unknown/waiting/running/success/failure/cancelled/skipped/blocked) onto the
// five states this plugin reports.
func normalizeActionStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "skipped":
		return scm.CIStateSuccess
	case "failure", "cancelled", "canceled":
		return scm.CIStateFailure
	case "running":
		return scm.CIStateRunning
	case "waiting", "blocked", "pending", "queued":
		return scm.CIStatePending
	default:
		return scm.CIStateNone
	}
}

// normalizeCommitState maps the commit-status vocabulary onto the same five
// states. "warning" is deliberately not a failure: it is advisory on both
// hosts.
func normalizeCommitState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return scm.CIStateSuccess
	case "failure", "error":
		return scm.CIStateFailure
	case "pending":
		return scm.CIStateRunning
	case "warning":
		return scm.CIStateSuccess
	default:
		return scm.CIStateNone
	}
}

// rollUpStates reduces per-job states to one verdict: any failure loses, then
// anything still moving, then success.
func rollUpStates(states []string) string {
	verdict := scm.CIStateNone
	for _, state := range states {
		switch state {
		case scm.CIStateFailure:
			return scm.CIStateFailure
		case scm.CIStateRunning:
			verdict = scm.CIStateRunning
		case scm.CIStatePending:
			if verdict != scm.CIStateRunning {
				verdict = scm.CIStatePending
			}
		case scm.CIStateSuccess:
			if verdict == scm.CIStateNone {
				verdict = scm.CIStateSuccess
			}
		}
	}
	return verdict
}

// taskMatchesRef reports whether an Actions task belongs to ref, accepting a
// branch name or a full or abbreviated commit SHA.
func taskMatchesRef(task ActionTask, ref string) bool {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(task.HeadBranch), strings.TrimPrefix(trimmed, "refs/heads/")) {
		return true
	}
	if isCommitSHA(trimmed) {
		return strings.HasPrefix(strings.ToLower(task.HeadSHA), strings.ToLower(trimmed))
	}
	return false
}

// isCommitSHA reports whether ref looks like an abbreviated or full commit id
// rather than a branch name. Seven hex characters is git's own abbreviation
// floor; a shorter hex string is far more likely to be a branch.
func isCommitSHA(ref string) bool {
	trimmed := strings.TrimSpace(ref)
	if len(trimmed) < 7 || len(trimmed) > 64 {
		return false
	}
	for _, r := range trimmed {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// gitRef expands a bare branch name into the full ref the runs endpoint
// expects, leaving an already-qualified ref alone.
func gitRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
