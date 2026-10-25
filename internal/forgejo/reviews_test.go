package forgejo

import (
	"context"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestPullRequestState(t *testing.T) {
	t.Parallel()
	// A merged pull request reports state "closed" on both hosts, so the
	// merged flag has to win.
	require.Equal(t, "merged", pullRequestState(rest.PullRequest{State: "closed", Merged: true}))
	require.Equal(t, "closed", pullRequestState(rest.PullRequest{State: "closed"}))
	require.Equal(t, "draft", pullRequestState(rest.PullRequest{State: "open", Draft: true}))
	require.Equal(t, "draft", pullRequestState(rest.PullRequest{State: "open", Title: "WIP: later"}))
	require.Equal(t, "open", pullRequestState(rest.PullRequest{State: "open", Title: "Ready"}))
	// A merged draft is still merged.
	require.Equal(t, "merged", pullRequestState(rest.PullRequest{State: "closed", Merged: true, Draft: true}))
}

func TestPipelineState(t *testing.T) {
	t.Parallel()
	require.Equal(t, "success", pipelineState("success"))
	require.Equal(t, "failure", pipelineState("failure"))
	require.Equal(t, "failure", pipelineState("error"))
	require.Equal(t, "pending", pipelineState("pending"))
	require.Equal(t, "neutral", pipelineState(""))
	require.Equal(t, "neutral", pipelineState("unexpected"))
}

// The per-entry field is `status`; only the combined roll-up uses `state`.
func TestNormalizeChecksReadsStatusField(t *testing.T) {
	t.Parallel()
	checks := normalizeChecks([]rest.CommitStatus{
		{ID: 5, Status: "success", Context: "ci/build", Description: "ok", TargetURL: "https://ci/5"},
		{ID: 0, Status: "failure", Context: ""},
	})
	require.Len(t, checks, 2)
	require.Equal(t, "5", checks[0].ID)
	require.Equal(t, "ci/build", checks[0].Label)
	require.Equal(t, "success", checks[0].State)
	require.Equal(t, "https://ci/5", checks[0].URL)
	require.Equal(t, "check", checks[1].Label, "a context-less status still needs a label")
	require.Equal(t, "check", checks[1].ID)
}

func TestSummarizeReviews(t *testing.T) {
	t.Parallel()
	t.Run("no decisive reviews", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, summarizeReviews(nil))
		require.Nil(t, summarizeReviews([]rest.Review{{State: "COMMENT", User: rest.User{Login: "a"}}}))
		require.Nil(t, summarizeReviews([]rest.Review{{State: "PENDING", User: rest.User{Login: "a"}}}))
	})

	t.Run("ignores stale and dismissed", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, summarizeReviews([]rest.Review{
			{State: "APPROVED", Stale: true, User: rest.User{Login: "a"}},
			{State: "APPROVED", Dismissed: true, User: rest.User{Login: "b"}},
		}))
	})

	t.Run("counts one decision per reviewer", func(t *testing.T) {
		t.Parallel()
		summary := summarizeReviews([]rest.Review{
			{State: "APPROVED", User: rest.User{Login: "a"}},
			{State: "APPROVED", User: rest.User{Login: "a"}},
			{State: "APPROVED", User: rest.User{Login: "b"}},
		})
		require.NotNil(t, summary)
		require.Equal(t, "approved", summary.State)
		require.Equal(t, 2, summary.Approved, "a reviewer approving twice counts once")
	})

	t.Run("changes requested wins over approvals", func(t *testing.T) {
		t.Parallel()
		summary := summarizeReviews([]rest.Review{
			{State: "APPROVED", User: rest.User{Login: "a"}},
			{State: "REQUEST_CHANGES", User: rest.User{Login: "b"}},
		})
		require.NotNil(t, summary)
		require.Equal(t, "changes_requested", summary.State)
		require.Equal(t, 1, summary.Approved)
	})
}

func TestParseTimestampMillis(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(1755002400000), parseTimestampMillis("2025-08-12T12:40:00Z"))
	require.Zero(t, parseTimestampMillis(""))
	require.Zero(t, parseTimestampMillis("not-a-time"))
}

// The per-task snapshot and the workspace association map must agree on
// reviewKey, or the sidebar glyph cannot resolve to its review.
func TestReviewKeyMatchesAcrossTaskAndWorkspaceRefresh(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "widgets"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusOK, map[string]any{
		"number": 42, "title": "Add widgets", "state": "open", "html_url": "https://forge/pulls/42",
		"head": map[string]any{"sha": "abc", "ref": "feature"}, "updated_at": "2025-08-12T12:40:00Z",
		"review_comments": 3,
	})
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/commits/abc/status", http.StatusOK, map[string]any{
		"state": "success", "statuses": []map[string]any{{"id": 1, "status": "success", "context": "ci/build"}},
	})
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42/reviews", http.StatusOK, []map[string]any{
		{"id": 1, "state": "APPROVED", "user": map[string]any{"login": "reviewer"}},
	})

	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "workspace-1"}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")
	associations := sourcecontrol.NewAssociations(func() pluginsdk.Host { return host })
	reviews := NewReviews(connection, repositories, associations, "forgejo")

	require.NoError(t, associations.Link(context.Background(), "task-1", identity(api.URL(), "7", 42)))

	forTask, err := reviews.ForTask(context.Background(), "workspace-1", "task-1")
	require.NoError(t, err)
	require.Len(t, forTask, 1)
	require.Equal(t, "open", forTask[0].State)
	require.Equal(t, "success", forTask[0].TaskStatus.PipelineState)
	require.Len(t, forTask[0].TaskStatus.Checks, 1)
	require.Equal(t, 3, forTask[0].TaskStatus.UnresolvedComments)
	require.Equal(t, "approved", forTask[0].TaskStatus.Review.State)
	require.Equal(t, int64(1755002400000), forTask[0].TaskStatus.UpdatedAt)

	workspace, err := reviews.Associations(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Len(t, workspace, 1)
	require.Equal(t, forTask[0].ReviewKey, workspace[0].ReviewKey,
		"the snapshot and association reviewKey must be identical")
	require.Equal(t, "task-1", workspace[0].TaskID)
}

// Missing CI or hidden reviews are normal, not failures.
func TestForTaskToleratesMissingStatusAndReviews(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "widgets"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusOK, map[string]any{
		"number": 42, "title": "t", "state": "open", "head": map[string]any{"sha": "abc"},
	})
	// status + reviews endpoints intentionally unregistered -> 404
	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "workspace-1"}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")
	associations := sourcecontrol.NewAssociations(func() pluginsdk.Host { return host })
	reviews := NewReviews(connection, repositories, associations, "forgejo")
	require.NoError(t, associations.Link(context.Background(), "task-1", identity(api.URL(), "7", 42)))

	forTask, err := reviews.ForTask(context.Background(), "workspace-1", "task-1")
	require.NoError(t, err)
	require.Len(t, forTask, 1)
	require.Equal(t, "neutral", forTask[0].TaskStatus.PipelineState)
	require.Empty(t, forTask[0].TaskStatus.Checks)
	require.Nil(t, forTask[0].TaskStatus.Review)
}

// A deleted pull request must not break the whole task refresh.
func TestForTaskSkipsDeletedPullRequests(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "widgets"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusNotFound, nil)
	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "workspace-1"}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")
	associations := sourcecontrol.NewAssociations(func() pluginsdk.Host { return host })
	reviews := NewReviews(connection, repositories, associations, "forgejo")
	require.NoError(t, associations.Link(context.Background(), "task-1", identity(api.URL(), "7", 42)))

	forTask, err := reviews.ForTask(context.Background(), "workspace-1", "task-1")
	require.NoError(t, err)
	require.Empty(t, forTask)
}

// Links recorded against a different instance are not this connection's to report.
func TestForTaskIgnoresOtherConnectionScopes(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "workspace-1"}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")
	associations := sourcecontrol.NewAssociations(func() pluginsdk.Host { return host })
	reviews := NewReviews(connection, repositories, associations, "forgejo")
	require.NoError(t, associations.Link(context.Background(), "task-1", identity("https://other.example.com", "7", 42)))

	forTask, err := reviews.ForTask(context.Background(), "workspace-1", "task-1")
	require.NoError(t, err)
	require.Empty(t, forTask)
}

// The workspace glyph refresh must never call the provider.
func TestAssociationsDoesNotCallProvider(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "workspace-1"}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")
	associations := sourcecontrol.NewAssociations(func() pluginsdk.Host { return host })
	reviews := NewReviews(connection, repositories, associations, "forgejo")
	require.NoError(t, associations.Link(context.Background(), "task-1", identity(api.URL(), "7", 42)))

	result, err := reviews.Associations(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.Empty(t, api.Requests(), "association refresh must be served from stored state alone")
}
