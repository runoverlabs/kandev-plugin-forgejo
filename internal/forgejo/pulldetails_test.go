package forgejo

import (
	"context"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// detailsFixture links PRs 7 and 8 of repository 5 to task-1 and serves a
// complete set of responses for PR 7 on a protected base branch.
func detailsFixture(t *testing.T) (*PullActions, *apiServer, *Associations) {
	t.Helper()
	api := newAPIServer(t)
	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "ws-1"}
	hosts := func() pluginsdk.Host { return host }
	repositories := NewRepositories(connection, hosts, "forgejo")
	associations := NewAssociations(hosts)
	actions := NewPullActions(connection, repositories, associations, "forgejo")
	for _, number := range []int64{7, 8} {
		require.NoError(t, associations.Link(context.Background(), "task-1", identity(api.url(), "5", number)))
	}

	repo := map[string]any{
		"id": 5, "name": "r", "full_name": "o/r", "owner": map[string]any{"login": "o"},
		"clone_url": api.url() + "/o/r.git", "html_url": api.url() + "/o/r", "default_branch": "main",
		"allow_squash_merge": true, "allow_merge_commits": true, "default_merge_style": "squash",
	}
	api.handle(http.MethodGet, "/api/v1/repositories/5", 200, repo)
	api.handle(http.MethodGet, "/api/v1/repos/o/r", 200, repo)
	api.handle(http.MethodGet, "/api/v1/user", 200, map[string]any{"login": "kandev"})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7", 200, map[string]any{
		"number": 7, "title": "Add x", "state": "open", "mergeable": false,
		"user": map[string]any{"login": "alice"},
		"head": map[string]any{"ref": "feat", "sha": "abc"}, "base": map[string]any{"ref": "main"},
		"requested_reviewers": []map[string]any{{"login": "bob"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/branches/main", 200, map[string]any{
		"name": "main", "protected": true, "required_approvals": 1,
		"enable_status_check": true, "user_can_merge": true,
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/commits/abc/status", 200, map[string]any{
		"state": "failure", "statuses": []map[string]any{{"id": 1, "status": "failure", "context": "ci"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", 200, []map[string]any{
		{"id": 1, "state": "REQUEST_CHANGES", "user": map[string]any{"login": "bob"}, "body": "no"},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/issues/7/comments", 200, []map[string]any{
		{"id": 9, "body": "hi", "user": map[string]any{"login": "alice"}},
	})
	return actions, api, associations
}

func TestDetailsReturnsTheFullModel(t *testing.T) {
	t.Parallel()
	actions, _, _ := detailsFixture(t)

	got, err := actions.Details(context.Background(), "ws-1", "task-1", 7)
	require.NoError(t, err)
	require.Equal(t, "open", got.State)
	require.Equal(t, "alice", got.Author)
	require.Equal(t, "feat", got.SourceBranch)
	require.Equal(t, "main", got.TargetBranch)
	require.Equal(t, "abc", got.HeadSHA)
	require.Equal(t, "kandev", got.Actor)
	require.Equal(t, []string{"bob"}, got.RequestedReviewers)
	require.Equal(t, []string{"squash", "merge"}, got.Merge.Styles)
	require.Equal(t, "squash", got.Merge.DefaultStyle)
	require.True(t, got.Merge.ProtectionVisible)
	require.NotNil(t, got.Merge.CanMerge)
	require.Equal(t, "failure", got.PipelineState)
	require.Len(t, got.Reviews, 1)
	require.Equal(t, "REQUEST_CHANGES", got.Reviews[0].State)
	require.Len(t, got.Comments, 1)
	require.Nil(t, got.Additions, "an instance that omits line counts reports them as unknown")
	require.ElementsMatch(t, []string{
		sourcecontrol.BlockerConflicts, sourcecontrol.BlockerChecksFailing,
		sourcecontrol.BlockerChangesRequested, sourcecontrol.BlockerApprovalsRequired,
	}, got.Merge.Blockers)
}

// The authorization rule: a number is honoured only when the task links it,
// and which repository it means comes from the stored identity.
func TestDetailsRefusesAPullRequestTheTaskDoesNotLink(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	before := len(api.requests)

	_, err := actions.Details(context.Background(), "ws-1", "task-1", 99)
	require.ErrorIs(t, err, ErrNotLinked)
	_, err = actions.Details(context.Background(), "ws-1", "other-task", 7)
	require.ErrorIs(t, err, ErrNotLinked)
	_, err = actions.Details(context.Background(), "ws-1", "", 7)
	require.ErrorIs(t, err, ErrNotLinked)
	require.Len(t, api.requests, before, "a refused call must not reach the instance")
}

func TestDetailsNeedsANumberWhenSeveralAreLinked(t *testing.T) {
	t.Parallel()
	actions, _, _ := detailsFixture(t)
	_, err := actions.Details(context.Background(), "ws-1", "task-1", 0)
	require.ErrorIs(t, err, ErrAmbiguous)
}

// A token that cannot see protection, statuses, reviews or comments still gets
// the rest, and mergeability it was not told stays unknown rather than "no".
func TestDetailsDegradesWhenPartsAreNotVisible(t *testing.T) {
	t.Parallel()
	actions, api, associations := detailsFixture(t)
	require.NoError(t, associations.Unlink(context.Background(), "task-1", identity(api.url(), "5", 8)))
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7", 200, map[string]any{
		"number": 7, "title": "Add x", "state": "open",
		"head": map[string]any{"ref": "feat", "sha": "abc"}, "base": map[string]any{"ref": "main"},
	})
	for _, path := range []string{"/branches/main", "/commits/abc/status", "/pulls/7/reviews", "/issues/7/comments"} {
		api.handle(http.MethodGet, "/api/v1/repos/o/r"+path, 403, nil)
	}

	got, err := actions.Details(context.Background(), "ws-1", "task-1", 0)
	require.NoError(t, err)
	require.False(t, got.Merge.ProtectionVisible)
	require.Nil(t, got.Merge.RequiredApprovals)
	require.Nil(t, got.Merge.Mergeable)
	require.Empty(t, got.Merge.Blockers, "unknown mergeability must not read as blocked")
	require.Empty(t, got.Reviews)
	require.Equal(t, "neutral", got.PipelineState)
}

func TestMergeBlockersOnlyBlockWhenChecksAreRequired(t *testing.T) {
	t.Parallel()
	require.Empty(t, mergeBlockers("open", nil, "failure", false, nil, nil),
		"a failing check that protection does not require is not a blocker")
	require.Equal(t, []string{sourcecontrol.BlockerChecksPending},
		mergeBlockers("open", nil, "pending", true, nil, nil))
	require.Equal(t, []string{sourcecontrol.BlockerNotOpen},
		mergeBlockers("merged", nil, "success", true, nil, nil))
}
