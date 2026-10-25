package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/prautomation"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// automationHost adds what pull request automation reads and writes on the
// Host: the workspace list, sessions and the message API.
type automationHost struct {
	*agentHost
	mu       sync.Mutex
	sessions []pluginsdk.Session
	sent     []string
	sendErr  error
}

func (h *automationHost) Workspaces() pluginsdk.WorkspaceReader { return automationWorkspaces{} }
func (h *automationHost) Sessions() pluginsdk.SessionReader     { return automationSessions{host: h} }
func (h *automationHost) Messages() pluginsdk.MessageReader     { return automationMessages{host: h} }

type automationWorkspaces struct{ pluginsdk.WorkspaceReader }

func (automationWorkspaces) List(context.Context, pluginsdk.Page) ([]pluginsdk.Workspace, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Workspace{{ID: "workspace-1"}}, &pluginsdk.PageInfo{}, nil
}

type automationSessions struct {
	pluginsdk.SessionReader
	host *automationHost
}

func (s automationSessions) List(_ context.Context, filter pluginsdk.SessionFilter, _ pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	var out []pluginsdk.Session
	for _, session := range s.host.sessions {
		for _, id := range filter.TaskIDs {
			if session.TaskID == id {
				out = append(out, session)
			}
		}
	}
	return out, &pluginsdk.PageInfo{}, nil
}

type automationMessages struct {
	pluginsdk.MessageReader
	host *automationHost
}

func (m automationMessages) Send(_ context.Context, taskID, sessionID, text string) (*pluginsdk.MessageDispatch, error) {
	m.host.mu.Lock()
	defer m.host.mu.Unlock()
	if m.host.sendErr != nil {
		return nil, m.host.sendErr
	}
	m.host.sent = append(m.host.sent, taskID+"|"+sessionID+"|"+text)
	return &pluginsdk.MessageDispatch{SessionID: sessionID, Status: "sent"}, nil
}

func (h *automationHost) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

// automationFixture is linkedPullFixture (PR 12 linked to task-1) with a host
// that has one idle session on the task, and CI failing on the head.
func automationFixture(t *testing.T) (*agentFixture, *automationHost) {
	t.Helper()
	fixture := linkedPullFixture(t)
	host := &automationHost{agentHost: fixture.host, sessions: []pluginsdk.Session{
		{ID: "session-1", TaskID: "task-1", State: "IDLE", StartedAt: "2026-10-25T10:00:00Z"},
	}}
	fixture.runtime.SetHost(host)
	t.Cleanup(fixture.runtime.Stop)
	fixture.json("GET", "/api/v1/repos/kandev/demo/commits/"+agentSHA+"/status",
		`{"state":"failure","statuses":[{"id":1,"status":"failure","context":"ci/test","target_url":"http://ci/1"}]}`)
	fixture.json("GET", "/api/v1/user/teams", `[]`)
	return fixture, host
}

func taskAction(key, body string) *pluginsdk.PluginActionRequest {
	return &pluginsdk.PluginActionRequest{
		ActionKey: key, Body: []byte(body),
		Context: pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1", TaskID: "task-1", ActorID: "user-7"},
	}
}

func TestAutomationActionsActOnlyOnLinkedPullRequests(t *testing.T) {
	fixture, _ := automationFixture(t)
	ctx := context.Background()

	response, err := fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationGet, `{"number":99}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.Status)

	unverified := taskAction(ActionAutomationSet, `{"number":12,"auto_fix":true}`)
	unverified.Context.TaskID = ""
	response, err = fixture.runtime.HandleAction(ctx, unverified)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.Status)

	response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationGet, `{"number":12}`))
	require.NoError(t, err)
	body := decodeBody(t, response)
	require.Equal(t, false, body["auto_fix"])
	require.Equal(t, false, body["seeded"])

	response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationRetry, `{"number":12,"what":"merge"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.Status, "nothing to retry before automation has a record")

	response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, `{"number":12,"auto_fix":true,"on_merged":true}`))
	require.NoError(t, err)
	body = decodeBody(t, response)
	require.Equal(t, true, body["auto_fix"])
	require.Equal(t, true, body["on_merged"])
	require.Equal(t, false, body["auto_merge"])

	entries := auditEntries(t, fixture, "workspace-1")
	require.NotEmpty(t, entries)
	last := entries[len(entries)-1]
	require.Equal(t, ActionAutomationSet, last["action"])
	require.Equal(t, "user-7", last["actor"])
	require.Equal(t, "created,auto_fix=true,on_merged=true", last["outcome"], "the trail says what was turned on")

	response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationRetry, `{"number":12,"what":"everything"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, response.Status)
}

// Auto-fix and auto-merge on a fork need the panel's confirmation, enforced
// here: the panel may not know yet that it is a fork.
func TestAutomationSetRefusesAForkWithoutConfirmation(t *testing.T) {
	fixture, _ := automationFixture(t)
	ctx := context.Background()
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12",
		`{"number":12,"title":"Add thing","state":"open","mergeable":true,
		  "head":{"ref":"feature","sha":"`+agentSHA+`","repo_id":78},"base":{"ref":"main","repo_id":77},"user":{"login":"bot"}}`)

	response, err := fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationGet, `{"number":12}`))
	require.NoError(t, err)
	require.Equal(t, true, decodeBody(t, response)["fork"], "known before the loop observed it")

	for _, body := range []string{`{"number":12,"auto_fix":true}`, `{"number":12,"auto_merge":true}`} {
		response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, body))
		require.NoError(t, err)
		require.Equal(t, http.StatusConflict, response.Status, body)
	}
	_, found, err := fixture.runtime.automation.Store().Get(ctx, "workspace-1",
		prautomation.PullRef{TaskID: "task-1", RepositoryID: "77", Number: 12})
	require.NoError(t, err)
	require.False(t, found, "a refused switch stores nothing")

	response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, `{"number":12,"on_merged":true}`))
	require.NoError(t, err)
	require.Equal(t, true, decodeBody(t, response)["on_merged"], "a lifecycle prompt needs no confirmation")

	response, err = fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, `{"number":12,"auto_fix":true,"confirm_fork":true}`))
	require.NoError(t, err)
	require.Equal(t, true, decodeBody(t, response)["auto_fix"])
}

func TestAutomationSettingsRoundTripAndValidate(t *testing.T) {
	fixture, _ := automationFixture(t)
	ctx := context.Background()

	response, err := fixture.runtime.HandleAction(ctx, pluginRequest(ActionAutomationSettings, "workspace-1", `{}`))
	require.NoError(t, err)
	body := decodeBody(t, response)
	require.Equal(t, "everyone", body["trust"], "GitHub parity is the default")
	require.Equal(t, prautomation.DefaultTemplate, body["default_template"])

	response, err = fixture.runtime.HandleAction(ctx, pluginRequest(ActionAutomationSettingsSet, "workspace-1",
		`{"trust":"official","defaults":{"auto_merge":true},"template":"Fix it: {{pr.feedback}}"}`))
	require.NoError(t, err)
	body = decodeBody(t, response)
	require.Equal(t, "official", body["trust"])
	require.Equal(t, true, body["defaults"].(map[string]any)["auto_merge"])

	response, err = fixture.runtime.HandleAction(ctx, pluginRequest(ActionAutomationSettingsSet, "workspace-1", `{"trust":"friends"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, response.Status)

	response, err = fixture.runtime.HandleAction(ctx, pluginRequest(ActionAutomationSettings, "workspace-2", `{}`))
	require.NoError(t, err)
	require.Equal(t, "everyone", decodeBody(t, response)["trust"], "settings are per workspace")
}

// The loop end to end against a fake instance and host: a failing check is
// sent to the idle session, the turn ends without a report and the event makes
// the attempt retryable, and the agent's outcome closes it.
func TestAutomationLoopPromptsAndTakesTheOutcome(t *testing.T) {
	fixture, host := automationFixture(t)
	ctx := context.Background()
	_, err := fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, `{"number":12,"auto_fix":true}`))
	require.NoError(t, err)

	fixture.runtime.automation.Tick(ctx)
	sent := host.messages()
	require.Len(t, sent, 1)
	require.True(t, strings.HasPrefix(sent[0], "task-1|session-1|[Forgejo auto-fix · kandev/demo#12 · round 1/10"))
	require.Contains(t, sent[0], "ci/test (failure) http://ci/1")

	ref := prautomation.PullRef{TaskID: "task-1", RepositoryID: "77", Number: 12}
	record, found, err := fixture.runtime.automation.Store().Get(ctx, "workspace-1", ref)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, record.Attempt)

	require.NoError(t, fixture.runtime.OnEvent(ctx, &pluginsdk.Event{
		EventType: "turn.completed",
		Payload: map[string]any{
			"task_id": "task-1", "session_id": "session-1",
			"started_at": record.Attempt.SentAt.Add(time.Second).Format(time.RFC3339Nano),
		},
	}))
	record, _, _ = fixture.runtime.automation.Store().Get(ctx, "workspace-1", ref)
	require.Equal(t, prautomation.AttemptRetryable, record.Attempt.State)

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "outcome", "outcome": "blocked", "body": "needs a secret"})
	require.False(t, result.IsError, result.Text)
	require.Contains(t, result.Text, "Recorded blocked for kandev/demo#12")

	record, _, _ = fixture.runtime.automation.Store().Get(ctx, "workspace-1", ref)
	require.Nil(t, record.Attempt)
	require.Equal(t, "needs a secret", record.LastSummary)

	result = fixture.invoke(t, ToolPR, map[string]any{"op": "outcome", "outcome": "blocked"})
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "no auto-fix attempt")

	result = fixture.invoke(t, ToolPR, map[string]any{"op": "outcome", "outcome": "done"})
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "action_taken, non_actionable, or blocked")
}

func TestAutomationLoopRespectsTheIntegrationSwitch(t *testing.T) {
	fixture, host := automationFixture(t)
	ctx := context.Background()
	_, err := fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, `{"number":12,"auto_fix":true}`))
	require.NoError(t, err)
	require.NoError(t, host.SetState(ctx, "workspace", "workspace-1", enabledStateKey, map[string]any{"enabled": false}))
	// Turning auto-fix on read the pull request once, to check for a fork.
	reads := fixture.callCount("GET", "/api/v1/repos/kandev/demo/pulls/12")
	fixture.runtime.automation.Tick(ctx)
	require.Empty(t, host.messages())
	require.Equal(t, reads, fixture.callCount("GET", "/api/v1/repos/kandev/demo/pulls/12"))
}

func TestAutomationReportsMissingApproval(t *testing.T) {
	fixture, host := automationFixture(t)
	ctx := context.Background()
	host.sendErr = status.Error(codes.PermissionDenied, "capability not approved")
	_, err := fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationSet, `{"number":12,"auto_fix":true}`))
	require.NoError(t, err)
	fixture.runtime.automation.Tick(ctx)

	response, err := fixture.runtime.HandleAction(ctx, taskAction(ActionAutomationGet, `{"number":12}`))
	require.NoError(t, err)
	body := decodeBody(t, response)
	require.Contains(t, body["error"], "re-approve")
	require.EqualValues(t, 0, body["rounds"])
}

func TestHostMessagingErrorMapping(t *testing.T) {
	for code, want := range map[codes.Code]error{
		codes.PermissionDenied:   prautomation.ErrNotApproved,
		codes.NotFound:           prautomation.ErrNoSession,
		codes.FailedPrecondition: prautomation.ErrTaskInactive,
		codes.Aborted:            prautomation.ErrTaskInactive,
	} {
		require.ErrorIs(t, hostMessagingError(status.Error(code, "x")), want, code.String())
	}
	plain := errors.New("boom")
	require.Equal(t, plain, hostMessagingError(plain))
}

func TestAutomationMergerClassifiesRefusals(t *testing.T) {
	fixture, _ := automationFixture(t)
	ctx := context.Background()
	merger := automationMerger{runtime: fixture.runtime}
	ref := prautomation.PullRef{TaskID: "task-1", RepositoryID: "77", Number: 12}

	var body map[string]any
	fixture.route("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"head out of date"}`))
	})
	var refused *prautomation.MergeError
	require.ErrorAs(t, merger.Merge(ctx, "workspace-1", ref, agentSHA), &refused)
	require.True(t, refused.Transient, "the head moved: evaluate the new one")
	require.Equal(t, agentSHA, body["head_commit_id"])
	require.Equal(t, "merge", body["Do"])
	require.Nil(t, body["merge_when_checks_succeed"], "never the instance's scheduler")

	fixture.route("POST", "/api/v1/repos/kandev/demo/pulls/12/merge", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	require.ErrorAs(t, merger.Merge(ctx, "workspace-1", ref, agentSHA), &refused)
	require.False(t, refused.Transient)
	require.NotContains(t, refused.Message, "secret")

	entries := auditEntries(t, fixture, "workspace-1")
	require.Equal(t, "automation.merge", entries[len(entries)-1]["action"])
	require.Equal(t, "auto-merge", entries[len(entries)-1]["actor"])
}
