package rest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, api *apiServer) *Client {
	t.Helper()
	client, err := NewClient(api.URL(), "token-value", nil)
	require.NoError(t, err)
	return client
}

// TestCIStatusPrefersWorkflowRuns covers current Forgejo, where runs and their
// jobs are both served and the job ids are what ci_log needs.
func TestCIStatusPrefersWorkflowRuns(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		// A bare branch name must be expanded: the endpoint filters on a full
		// git ref and silently ignores anything it does not recognize.
		require.Equal(t, "refs/heads/feature", r.URL.Query().Get("ref"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[
			{"id":7,"title":"build","status":"failure","commit_sha":"abc123def4567","html_url":"http://forge/run/7"}]}`))
	})
	api.Handle("GET", "/api/v1/repos/kandev/demo/actions/runs/7/jobs", http.StatusOK, []map[string]any{
		{"id": 41, "name": "lint", "status": "success", "run_id": 7},
		{"id": 42, "name": "test", "status": "failure", "run_id": 7},
	})

	status, err := newTestClient(t, api).CIStatusFor(context.Background(), "kandev", "demo", "feature")
	require.NoError(t, err)
	require.Equal(t, scm.CISourceRuns, status.Source)
	require.Equal(t, scm.CIStateFailure, status.State)
	require.Equal(t, "abc123def4567", status.SHA)
	require.Len(t, status.Jobs, 2)
	require.Equal(t, scm.CIJob{ID: 42, Name: "test", Status: scm.CIStateFailure}, status.Jobs[1])
}

// TestCIStatusRunsWithoutJobsEndpoint covers Forgejo 13, which lists runs but
// serves no per-run jobs. The run itself must still be reported.
func TestCIStatusRunsWithoutJobsEndpoint(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle("GET", "/api/v1/repos/kandev/demo/actions/runs", http.StatusOK, map[string]any{
		"total_count": 1,
		"workflow_runs": []map[string]any{
			{"id": 7, "title": "build", "status": "running", "commit_sha": "abc", "html_url": "http://forge/run/7"},
		},
	})
	// /actions/runs/{id}/jobs is absent on this release.

	status, err := newTestClient(t, api).CIStatusFor(context.Background(), "kandev", "demo", "feature")
	require.NoError(t, err)
	require.Equal(t, scm.CISourceRuns, status.Source)
	require.Equal(t, scm.CIStateRunning, status.State)
	require.Len(t, status.Jobs, 1)
	require.Equal(t, "build", status.Jobs[0].Name)
	require.Zero(t, status.Jobs[0].ID, "a run is not a job and must not offer a log id")
}

// TestCIStatusFallsBackToActionsTasks covers Gitea, which serves no
// /actions/runs at all. The tasks listing cannot filter, so the match is local.
func TestCIStatusFallsBackToActionsTasks(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle("GET", "/api/v1/repos/kandev/demo/actions/tasks", http.StatusOK, map[string]any{
		"total_count": 3,
		"workflow_runs": []map[string]any{
			{"id": 91, "name": "test", "head_branch": "feature", "head_sha": "aaa", "status": "failure", "run_number": 12, "url": "http://forge/t/91"},
			{"id": 90, "name": "lint", "head_branch": "feature", "head_sha": "aaa", "status": "success", "run_number": 12},
			{"id": 80, "name": "test", "head_branch": "feature", "head_sha": "old", "status": "success", "run_number": 11},
			{"id": 70, "name": "test", "head_branch": "other", "head_sha": "zzz", "status": "failure", "run_number": 10},
		},
	})

	status, err := newTestClient(t, api).CIStatusFor(context.Background(), "kandev", "demo", "feature")
	require.NoError(t, err)
	require.Equal(t, scm.CISourceTasks, status.Source)
	require.Equal(t, scm.CIStateFailure, status.State)
	require.Len(t, status.Jobs, 2, "only the newest run for this branch is current state")
	require.Equal(t, int64(91), status.Jobs[0].ID)
	require.Equal(t, int64(90), status.Jobs[1].ID)
}

// TestCIStatusFallsBackToCommitStatus covers both the supported floor and any
// CI running outside the forge, which no Actions endpoint can see.
func TestCIStatusFallsBackToCommitStatus(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle("GET", "/api/v1/repos/kandev/demo/commits/feature/status", http.StatusOK, map[string]any{
		"state": "failure",
		"sha":   "deadbeef",
		"statuses": []map[string]any{
			{"status": "success", "context": "woodpecker/lint", "target_url": "http://ci/1"},
			{"status": "failure", "context": "woodpecker/test", "target_url": "http://ci/2"},
		},
	})

	status, err := newTestClient(t, api).CIStatusFor(context.Background(), "kandev", "demo", "feature")
	require.NoError(t, err)
	require.Equal(t, scm.CISourceStatus, status.Source)
	require.Equal(t, scm.CIStateFailure, status.State)
	require.Equal(t, "deadbeef", status.SHA)
	require.Len(t, status.Jobs, 2)
	require.Equal(t, "woodpecker/test", status.Jobs[1].Name)
	require.Zero(t, status.Jobs[1].ID, "a commit status has no log to fetch")
}

// TestCIStatusReportsNoneRatherThanFailing keeps "this ref has no CI" distinct
// from "the lookup failed", because an agent must not treat the first as red.
func TestCIStatusReportsNoneRatherThanFailing(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle("GET", "/api/v1/repos/kandev/demo/actions/runs", http.StatusOK, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	api.Handle("GET", "/api/v1/repos/kandev/demo/actions/tasks", http.StatusOK, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	api.Handle("GET", "/api/v1/repos/kandev/demo/commits/feature/status", http.StatusOK, map[string]any{"state": "", "statuses": []any{}})

	status, err := newTestClient(t, api).CIStatusFor(context.Background(), "kandev", "demo", "feature")
	require.NoError(t, err)
	require.Equal(t, scm.CISourceNone, status.Source)
	require.Equal(t, scm.CIStateNone, status.State)
	require.Empty(t, status.Jobs)
}

// TestCIStatusQueriesByCommitSHA covers the other ref form the tools accept.
func TestCIStatusQueriesByCommitSHA(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	sha := "a1b2c3d4e5f6"
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, sha, r.URL.Query().Get("head_sha"))
		require.Empty(t, r.URL.Query().Get("ref"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":3,"title":"b","status":"success","commit_sha":%q}]}`, sha)
	})

	status, err := newTestClient(t, api).CIStatusFor(context.Background(), "kandev", "demo", sha)
	require.NoError(t, err)
	require.Equal(t, scm.CIStateSuccess, status.State)
}

func TestIsCommitSHA(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"a1b2c3d", true},
		{strings.Repeat("a", 40), true},
		{"main", false},
		{"deadbee", true},
		{"feat/cafe", false},
		{"cafe", false}, // hex, but shorter than git's own abbreviation floor
		{"", false},
	} {
		require.Equalf(t, testCase.want, isCommitSHA(testCase.value), "value %q", testCase.value)
	}
}

func TestRollUpStates(t *testing.T) {
	t.Parallel()
	require.Equal(t, scm.CIStateFailure, rollUpStates([]string{scm.CIStateSuccess, scm.CIStateRunning, scm.CIStateFailure}))
	require.Equal(t, scm.CIStateRunning, rollUpStates([]string{scm.CIStateSuccess, scm.CIStateRunning, scm.CIStatePending}))
	require.Equal(t, scm.CIStatePending, rollUpStates([]string{scm.CIStateSuccess, scm.CIStatePending}))
	require.Equal(t, scm.CIStateSuccess, rollUpStates([]string{scm.CIStateSuccess, scm.CIStateSuccess}))
	require.Equal(t, scm.CIStateNone, rollUpStates(nil))
}
