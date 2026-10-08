package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// watchHost serves the Host surface the watch actions use: workspace state
// with a real ListState, plus workflow and agent-profile reads.
type watchHost struct {
	pluginsdk.UnimplementedHostData

	mu    sync.Mutex
	state map[string]map[string]map[string]any // scope/scopeID -> key -> value
}

func newWatchHost() *watchHost {
	return &watchHost{state: map[string]map[string]map[string]any{}}
}

func (h *watchHost) bucket(scope, scopeID string) string { return scope + "/" + scopeID }

func (h *watchHost) GetConfig(context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func (h *watchHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	value, ok := h.state[h.bucket(scope, scopeID)][key]
	return value, ok, nil
}

func (h *watchHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	bucket := h.bucket(scope, scopeID)
	if h.state[bucket] == nil {
		h.state[bucket] = map[string]map[string]any{}
	}
	encoded, _ := json.Marshal(value)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	h.state[bucket][key] = decoded
	return nil
}

func (h *watchHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state[h.bucket(scope, scopeID)], key)
	return nil
}

func (h *watchHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := make([]pluginsdk.StateEntry, 0)
	for key, value := range h.state[h.bucket(scope, scopeID)] {
		entries = append(entries, pluginsdk.StateEntry{Key: key, Value: value})
	}
	return entries, nil
}

func (h *watchHost) RevealSecret(context.Context, string) (string, error) { return "", nil }
func (h *watchHost) GetSecret(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (h *watchHost) SetSecret(context.Context, string, string) error         { return nil }
func (h *watchHost) DeleteSecret(context.Context, string) error              { return nil }
func (h *watchHost) EmitEvent(context.Context, string, map[string]any) error { return nil }
func (h *watchHost) InvokeUtilityAgent(context.Context, string, ...pluginsdk.UtilityAgentOptions) (string, error) {
	return "", nil
}

func (h *watchHost) Workflows() pluginsdk.WorkflowReader         { return fakeWorkflows{} }
func (h *watchHost) AgentProfiles() pluginsdk.AgentProfileReader { return fakeAgentProfiles{} }

type fakeWorkflows struct{}

func (fakeWorkflows) List(_ context.Context, workspaceID string, _ pluginsdk.Page) ([]pluginsdk.Workflow, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Workflow{{ID: "wf-1", WorkspaceID: workspaceID, Name: "Autopilot"}}, &pluginsdk.PageInfo{}, nil
}

func (fakeWorkflows) ListSteps(_ context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	return []pluginsdk.WorkflowStep{
		{ID: "step-inbox", WorkflowID: workflowID, Name: "Inbox", Position: 0, IsStartStep: true},
		{ID: "step-doing", WorkflowID: workflowID, Name: "Doing", Position: 1},
	}, nil
}

type fakeAgentProfiles struct{}

func (fakeAgentProfiles) List(context.Context, pluginsdk.Page) ([]pluginsdk.AgentProfile, *pluginsdk.PageInfo, error) {
	return []pluginsdk.AgentProfile{{ID: "agent-1", DisplayName: "Builder", Model: "claude-opus-5"}}, &pluginsdk.PageInfo{}, nil
}

// newWatchRuntime returns a Runtime wired to a watch host, without starting the
// poller: these tests drive the action surface, not the loop.
func newWatchRuntime(t *testing.T) (*Runtime, *watchHost) {
	t.Helper()
	runtime := NewRuntime()
	host := newWatchHost()
	runtime.UnimplementedPlugin.SetHost(host)
	t.Cleanup(runtime.Stop)
	return runtime, host
}

func watchAction(t *testing.T, runtime *Runtime, key, workspaceID string, body any) (map[string]any, error) {
	t.Helper()
	encoded := []byte(nil)
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		require.NoError(t, err)
	}
	response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: key,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID},
		Body:      encoded,
	})
	if err != nil {
		return nil, err
	}
	return decodeBody(t, response), nil
}

func TestWatchActionsRoundTrip(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)

	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", map[string]any{
		"name":             "Bugs",
		"workflow_id":      "wf-1",
		"workflow_step_id": "step-inbox",
		"repos":            []string{"acme/app"},
		"labels":           []string{"bug"},
		"prompt":           "Investigate.",
	})
	require.NoError(t, err)
	watch := created["watch"].(map[string]any)
	id := watch["id"].(string)
	require.NotEmpty(t, id)
	require.Equal(t, true, watch["enabled"], "a new watch is enabled by default")
	require.Equal(t, "open", watch["state"])
	require.Equal(t, "watch", watch["dedup_scope"])

	listed, err := watchAction(t, runtime, ActionWatchesList, "ws-1", nil)
	require.NoError(t, err)
	require.Len(t, listed["watches"], 1)

	// An update carries only the fields the form changed; the rest survive.
	updated, err := watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{
		"id":      id,
		"enabled": false,
	})
	require.NoError(t, err)
	changed := updated["watch"].(map[string]any)
	require.Equal(t, false, changed["enabled"])
	require.Equal(t, "Bugs", changed["name"], "an omitted name keeps its stored value")
	require.Equal(t, "wf-1", changed["workflow_id"])

	deleted, err := watchAction(t, runtime, ActionWatchesDelete, "ws-1", map[string]any{"id": id})
	require.NoError(t, err)
	require.Equal(t, id, deleted["deleted"])

	empty, err := watchAction(t, runtime, ActionWatchesList, "ws-1", nil)
	require.NoError(t, err)
	require.Empty(t, empty["watches"])
}

// The workspace comes from the verified context, never the body. Without it
// there is nothing to scope the read to.
func TestWatchActionsRequireAVerifiedWorkspace(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	_, err := watchAction(t, runtime, ActionWatchesList, "", nil)
	require.ErrorContains(t, err, "verified workspace")
}

// A watch in one workspace is invisible and unreachable from another — the
// property #3681 was about.
func TestWatchActionsAreScopedToTheirWorkspace(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)

	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", map[string]any{
		"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
		"repos": []string{"acme/app"},
	})
	require.NoError(t, err)
	id := created["watch"].(map[string]any)["id"].(string)

	other, err := watchAction(t, runtime, ActionWatchesList, "ws-2", nil)
	require.NoError(t, err)
	require.Empty(t, other["watches"])

	for _, key := range []string{ActionWatchesUpdate, ActionWatchesDelete, ActionWatchesRun, ActionWatchesReset} {
		_, err := watchAction(t, runtime, key, "ws-2", map[string]any{"id": id})
		require.ErrorContains(t, err, "no such watch in this workspace", "action %q", key)
	}
}

func TestWatchCreateRejectsBadInput(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)

	for name, body := range map[string]map[string]any{
		"malformed repository": {
			"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
			"repos": []string{"not-a-repo"},
		},
		"no repositories": {
			"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
		},
		"no workflow step": {
			"name": "Bugs", "workflow_id": "wf-1", "repos": []string{"acme/app"},
		},
		"auto-start without an agent": {
			"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
			"repos": []string{"acme/app"}, "start_agent": true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", body)
			require.Error(t, err)
		})
	}
}

// A manual run ignores the interval but not the enable switches.
func TestWatchRunRespectsBothToggles(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)

	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", map[string]any{
		"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
		"repos": []string{"acme/app"}, "enabled": false,
	})
	require.NoError(t, err)
	id := created["watch"].(map[string]any)["id"].(string)

	_, err = watchAction(t, runtime, ActionWatchesRun, "ws-1", map[string]any{"id": id})
	require.ErrorContains(t, err, "paused")

	// Re-enable the watch, then turn the whole integration off.
	_, err = watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": id, "enabled": true})
	require.NoError(t, err)
	_, err = watchAction(t, runtime, ActionConnectionSetEnabled, "ws-1", map[string]any{"enabled": false})
	require.NoError(t, err)

	_, err = watchAction(t, runtime, ActionWatchesRun, "ws-1", map[string]any{"id": id})
	require.ErrorContains(t, err, "turned off")
}

// Watch management stays reachable while the integration is off, so an
// operator can see and edit what will resume when they turn it back on.
func TestWatchListSurvivesADisabledIntegration(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	_, err := watchAction(t, runtime, ActionConnectionSetEnabled, "ws-1", map[string]any{"enabled": false})
	require.NoError(t, err)

	_, err = watchAction(t, runtime, ActionWatchesList, "ws-1", nil)
	require.NoError(t, err)
}

// Without these the configuration form has no way to name a workflow step or a
// profile, and a watch has nowhere to put the task it creates.
func TestWatchOptionsServesTheConfigurationForm(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)

	options, err := watchAction(t, runtime, ActionWatchesOptions, "ws-1", nil)
	require.NoError(t, err)

	workflows := options["workflows"].([]any)
	require.Len(t, workflows, 1)
	workflow := workflows[0].(map[string]any)
	require.Equal(t, "Autopilot", workflow["name"])
	steps := workflow["steps"].([]any)
	require.Len(t, steps, 2)
	require.Equal(t, "Inbox", steps[0].(map[string]any)["name"])

	profiles := options["agent_profiles"].([]any)
	require.Len(t, profiles, 1)
	require.Equal(t, "Builder", profiles[0].(map[string]any)["name"])

	require.EqualValues(t, 300, options["default_interval"])
	require.EqualValues(t, 30, options["min_interval"])
}

func TestWatchResetReportsWhatItForgot(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", map[string]any{
		"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
		"repos": []string{"acme/app"},
	})
	require.NoError(t, err)
	id := created["watch"].(map[string]any)["id"].(string)

	reset, err := watchAction(t, runtime, ActionWatchesReset, "ws-1", map[string]any{"id": id})
	require.NoError(t, err)
	require.EqualValues(t, 0, reset["forgotten"])
	require.Equal(t, id, reset["watch_id"])
}

// Watch action keys must not collide with the source-control keys the recipe
// routes, or one handler would shadow the other.
func TestWatchActionKeysAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for _, key := range []string{
		ActionWatchesList, ActionWatchesOptions, ActionWatchesCreate, ActionWatchesUpdate,
		ActionWatchesDelete, ActionWatchesRun, ActionWatchesReset,
		ActionConnectionGet, ActionConnectionTest, ActionConnectionSetEnabled,
	} {
		_, duplicate := seen[key]
		require.Falsef(t, duplicate, "duplicate action key %q", key)
		seen[key] = struct{}{}
		require.True(t, strings.HasPrefix(key, "watches.") || strings.HasPrefix(key, "connection."))
	}
}
