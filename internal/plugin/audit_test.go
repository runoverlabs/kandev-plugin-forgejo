package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func auditEntries(t *testing.T, fixture *agentFixture, workspace string) []map[string]any {
	t.Helper()
	response, err := fixture.runtime.HandleAction(context.Background(), pluginRequest(ActionConnectionAudit, workspace, `{}`))
	require.NoError(t, err)
	var decoded struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &decoded))
	return decoded.Entries
}

// The trail is what ties a write by the shared Forgejo account back to a Kandev
// actor, so a browser write and an agent write both leave one entry.
func TestWritesLeaveAnAuditEntryNamingTheActor(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	fixture.json("POST", "/api/v1/repos/kandev/demo/issues/12/comments", `{}`)
	ctx := context.Background()

	// A browser write by a verified user.
	response, err := fixture.runtime.HandleAction(ctx, actorRequest("user-7", `{"number":12,"body":"top secret text"}`))
	require.NoError(t, err)
	require.Equal(t, 0, response.Status)

	// An agent write.
	result := fixture.invoke(t, ToolPR, map[string]any{"op": "comment", "body": "agent words"})
	require.False(t, result.IsError, result.Text)

	entries := auditEntries(t, fixture, "workspace-1")
	require.Len(t, entries, 2)
	require.Equal(t, "agent.pr.comment", entries[0]["action"], "newest first")
	require.Equal(t, "agent:session-1", entries[0]["actor"])
	require.Equal(t, "change_requests.comment", entries[1]["action"])
	require.Equal(t, "user-7", entries[1]["actor"])
	require.Equal(t, float64(12), entries[1]["number"])
	require.Equal(t, "ok", entries[1]["outcome"])

	encoded, _ := json.Marshal(entries)
	require.NotContains(t, string(encoded), "top secret text", "the trail never holds a body")
	require.NotContains(t, string(encoded), "agent words")
	require.NotContains(t, string(encoded), "secret", "nor the token")
}

func TestRefusedWritesAreAuditedToo(t *testing.T) {
	t.Parallel()
	fixture := linkedPullFixture(t)
	fixture.json("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", ``)

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "merge", "sha": agentSHA})
	require.True(t, result.IsError, "agent merge is off")
	entries := auditEntries(t, fixture, "workspace-1")
	require.Len(t, entries, 1)
	require.Equal(t, "agent.pr.merge", entries[0]["action"])
	require.Equal(t, "refused", entries[0]["outcome"])
}

func TestAuditTrailIsBoundedAndScopedToTheWorkspace(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	ctx := context.Background()
	for i := 0; i < auditKeep+30; i++ {
		fixture.runtime.audit(ctx, auditRecord{Action: "change_requests.comment", Actor: "u", Workspace: "workspace-1", Number: int64(i), Outcome: "ok"})
	}
	fixture.runtime.audit(ctx, auditRecord{Action: "change_requests.merge", Actor: "u", Workspace: "workspace-2", Number: 1, Outcome: "ok"})

	stored, err := fixture.host.ListState(ctx, "workspace", "workspace-1")
	require.NoError(t, err)
	require.Len(t, auditKeys(stored), auditKeep, "the oldest entries are dropped")

	shown := auditEntries(t, fixture, "workspace-1")
	require.Len(t, shown, auditShown)
	require.Equal(t, float64(auditKeep+29), shown[0]["number"], "newest first")
	require.Len(t, auditEntries(t, fixture, "workspace-2"), 1)
}

func actorRequest(actor, body string) *pluginsdk.PluginActionRequest {
	request := pluginRequest("change_requests.comment", "workspace-1", body)
	request.Context.ActorID = actor
	request.Context.TaskID = "task-1"
	return request
}
