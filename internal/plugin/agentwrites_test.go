package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

const agentSHA = "abc1234def"

// linkedPullFixture links PR 12 to task-1 and serves a clean, open pull
// request with passing CI, so each test only changes what it is about.
func linkedPullFixture(t *testing.T) *agentFixture {
	t.Helper()
	fixture := newAgentFixture(t)
	require.NoError(t, fixture.host.SetState(context.Background(), "workspace", "workspace-1",
		"task:task-1:77:12", map[string]any{
			"task_id": "task-1", "connection_scope": fixture.server.URL, "repository_id": "77", "number": 12,
		}))
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12",
		`{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open","mergeable":true,
		  "head":{"ref":"feature","sha":"`+agentSHA+`"},"base":{"ref":"main"},"user":{"login":"bot"}}`)
	fixture.json("GET", "/api/v1/user", `{"login":"kandev"}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/branches/main", `{"name":"main","protected":false}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/commits/"+agentSHA+"/status", `{"state":"success","statuses":[]}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12/reviews", `[]`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/issues/12/comments", `[]`)
	return fixture
}

func (f *agentFixture) allowAgentMerge(t *testing.T) {
	t.Helper()
	require.NoError(t, f.host.SetState(context.Background(), "workspace", "workspace-1",
		agentMergeStateKey, map[string]any{"enabled": true}))
}

func TestAgentGetReportsTheHeadCommit(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	result := fixture.invoke(t, ToolPR, map[string]any{"op": "get"})
	require.False(t, result.IsError)
	require.Contains(t, result.Text, "sha="+agentSHA, "merge and review pin themselves to this")
}

// Decision 2: agents cannot merge unless the operator switched it on, and no
// request reaches the instance's merge endpoint until then.
func TestAgentMergeIsOffByDefault(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	fixture.json("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", ``)

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "merge", "sha": agentSHA})
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "may not merge")
	require.Zero(t, fixture.callCount("POST", "/api/v1/repos/kandev/demo/pulls/12/merge"))

	// Review and comment are not behind the switch.
	fixture.json("POST", "/api/v1/repos/kandev/demo/issues/12/comments", `{}`)
	comment := fixture.invoke(t, ToolPR, map[string]any{"op": "comment", "body": "hello"})
	require.False(t, comment.IsError, comment.Text)
}

func TestAgentMergeWhenAllowedPinsTheHeadAndDeletesTheBranch(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	fixture.allowAgentMerge(t)
	var sent map[string]any
	fixture.route("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
	})

	missing := fixture.invoke(t, ToolPR, map[string]any{"op": "merge"})
	require.True(t, missing.IsError)
	require.Contains(t, missing.Text, "sha is required")

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "merge", "sha": agentSHA, "style": "squash", "delete_branch": true})
	require.False(t, result.IsError, result.Text)
	require.Contains(t, result.Text, "merged #12")
	require.Equal(t, "squash", sent["Do"])
	require.Equal(t, agentSHA, sent["head_commit_id"])
	require.Equal(t, true, sent["delete_branch_after_merge"])

	stale := fixture.invoke(t, ToolPR, map[string]any{"op": "merge", "sha": "0000000"})
	require.True(t, stale.IsError)
	require.Contains(t, stale.Text, "changed")
}

// The checks rule is the switch's own, independent of repository protection:
// here the branch is unprotected, yet red or running CI still blocks an agent.
func TestAgentMergeRefusesFailingOrRunningChecks(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{"failure": "failing", "pending": "still running"} {
		fixture := linkedPullFixture(t)
		fixture.allowAgentMerge(t)
		fixture.json("GET", "/api/v1/repos/kandev/demo/commits/"+agentSHA+"/status",
			`{"state":"`+state+`","statuses":[{"id":1,"status":"`+state+`","context":"ci"}]}`)
		fixture.json("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", ``)

		result := fixture.invoke(t, ToolPR, map[string]any{"op": "merge", "sha": agentSHA})
		require.True(t, result.IsError, state)
		require.Contains(t, result.Text, want)
		require.Zero(t, fixture.callCount("POST", "/api/v1/repos/kandev/demo/pulls/12/merge"), "%s CI must not reach merge", state)
	}
}

func TestAgentMergeRefusesConflicts(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	fixture.allowAgentMerge(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12",
		`{"number":12,"title":"x","state":"open","mergeable":false,"updated_at":"2020-01-01T00:00:00Z",
		  "head":{"ref":"feature","sha":"`+agentSHA+`"},"base":{"ref":"main"}}`)
	fixture.json("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", ``)
	result := fixture.invoke(t, ToolPR, map[string]any{"op": "merge", "sha": agentSHA})
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "conflicts")
	require.Zero(t, fixture.callCount("POST", "/api/v1/repos/kandev/demo/pulls/12/merge"))
}

func TestAgentReviewAndRequestReviewAndUpdate(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	var review, reviewers map[string]any
	fixture.route("POST", "/api/v1/repos/kandev/demo/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&review))
		_, _ = w.Write([]byte(`{"id":1,"state":"REQUEST_CHANGES"}`))
	})
	fixture.route("POST", "/api/v1/repos/kandev/demo/pulls/12/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reviewers))
		w.WriteHeader(http.StatusCreated)
	})
	fixture.json("POST", "/api/v1/repos/kandev/demo/pulls/12/update", ``)

	bad := fixture.invoke(t, ToolPR, map[string]any{"op": "review", "event": "merge"})
	require.True(t, bad.IsError)

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "review", "event": "request_changes", "body": "fix it", "sha": agentSHA})
	require.False(t, result.IsError, result.Text)
	require.Contains(t, result.Text, "REQUEST_CHANGES")
	require.Equal(t, "REQUEST_CHANGES", review["event"])
	require.Contains(t, review["body"], "Posted via Kandev")

	asked := fixture.invoke(t, ToolPR, map[string]any{"op": "request_review", "reviewers": []any{"alice", "bob"}})
	require.False(t, asked.IsError, asked.Text)
	require.Equal(t, []any{"alice", "bob"}, reviewers["reviewers"])

	updated := fixture.invoke(t, ToolPR, map[string]any{"op": "update"})
	require.False(t, updated.IsError, updated.Text)
}

// Same rule as `ready`: with several linked pull requests the tool does not
// guess, for any write op.
func TestAgentWritesRefuseWhenSeveralPullRequestsAreLinked(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	fixture.allowAgentMerge(t)
	require.NoError(t, fixture.host.SetState(context.Background(), "workspace", "workspace-1",
		"task:task-1:77:13", map[string]any{
			"task_id": "task-1", "connection_scope": fixture.server.URL, "repository_id": "77", "number": 13,
		}))
	fixture.json("POST", "/api/v1/repos/kandev/demo/issues/12/comments", `{}`)
	fixture.json("POST", "/api/v1/repos/kandev/demo/issues/13/comments", `{}`)

	for _, args := range []map[string]any{
		{"op": "comment", "body": "x"}, {"op": "update"}, {"op": "request_review", "reviewers": []any{"a"}},
		{"op": "review", "event": "approve"}, {"op": "merge", "sha": agentSHA},
	} {
		result := fixture.invoke(t, ToolPR, args)
		require.True(t, result.IsError, "%v", args)
		require.Contains(t, result.Text, "more than one", "%v", args)
	}
	require.Zero(t, fixture.callCount("POST", "/api/v1/repos/kandev/demo/issues/12/comments"))
	require.Zero(t, fixture.callCount("POST", "/api/v1/repos/kandev/demo/issues/13/comments"))
}

func TestSetAgentMergeIsWorkspaceScopedAndFailsClosed(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	ctx := context.Background()
	require.False(t, fixture.runtime.agentMergeEnabled(ctx, "workspace-1"), "off until an operator says otherwise")

	response, err := fixture.runtime.HandleAction(ctx, pluginRequest(ActionConnectionSetAgentMerge, "workspace-1", `{"enabled":true}`))
	require.NoError(t, err)
	require.Contains(t, string(response.Body), `"agent_merge":true`)
	require.True(t, fixture.runtime.agentMergeEnabled(ctx, "workspace-1"))
	require.False(t, fixture.runtime.agentMergeEnabled(ctx, "workspace-2"), "another workspace is unaffected")

	bad, err := fixture.runtime.HandleAction(ctx, pluginRequest(ActionConnectionSetAgentMerge, "workspace-1", `{}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, bad.Status)
}

func pluginRequest(key, workspaceID, body string) *pluginsdk.PluginActionRequest {
	return &pluginsdk.PluginActionRequest{
		ActionKey: key, Body: []byte(body),
		Context: pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID},
	}
}
