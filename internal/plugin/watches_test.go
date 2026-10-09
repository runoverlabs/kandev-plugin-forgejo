package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
		{ID: "step-auto", WorkflowID: workflowID, Name: "Run", Position: 2, OnEnterActionTypes: []string{"auto_start_agent"}},
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
	decoded := decodeBody(t, response)
	if response.Status >= http.StatusBadRequest {
		// An operator-actionable refusal travels as a status and a message, not
		// as a Go error the host would mask; surface it as an error here.
		return nil, &refusedError{status: response.Status, message: fmt.Sprint(decoded["error"])}
	}
	return decoded, nil
}

type refusedError struct {
	status  int
	message string
}

func (e *refusedError) Error() string { return e.message }

func TestWatchActionsRoundTrip(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)

	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", fullWatchBody())
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

	// The pause toggle sends only `{id, enabled}`. Every other field must come
	// through untouched: this once erased the prompt, profiles, repository,
	// query and labels of any watch that was paused or resumed.
	updated, err := watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{
		"id":      id,
		"enabled": false,
	})
	require.NoError(t, err)
	changed := updated["watch"].(map[string]any)
	require.Equal(t, false, changed["enabled"])
	for key, want := range watch {
		if key == "enabled" || key == "updated_at" {
			continue
		}
		require.Equal(t, want, changed[key], "a partial update must leave %q alone", key)
	}

	deleted, err := watchAction(t, runtime, ActionWatchesDelete, "ws-1", map[string]any{"id": id})
	require.NoError(t, err)
	require.Equal(t, id, deleted["deleted"])

	empty, err := watchAction(t, runtime, ActionWatchesList, "ws-1", nil)
	require.NoError(t, err)
	require.Empty(t, empty["watches"])
}

// fullWatchBody sets every field a watch can carry, so a test can tell a field
// that survived from one that was never there.
func fullWatchBody() map[string]any {
	return map[string]any{
		"name":                  "Bugs",
		"workflow_id":           "wf-1",
		"workflow_step_id":      "step-inbox",
		"agent_profile_id":      "agent-1",
		"executor_profile_id":   "exec-1",
		"prompt":                "Investigate.",
		"start_agent":           true,
		"repository_id":         "repo-1",
		"base_branch":           "main",
		"repos":                 []string{"acme/app"},
		"labels":                []string{"bug"},
		"query":                 "crash",
		"poll_interval_seconds": 600,
		"max_inflight_tasks":    3,
	}
}

// An operator must still be able to clear a field on purpose: an explicit empty
// value is taken as written, while an absent field is not.
func TestWatchUpdateClearsAFieldOnlyWhenItIsSentExplicitly(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", fullWatchBody())
	require.NoError(t, err)
	before := created["watch"].(map[string]any)

	updated, err := watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{
		"id":            before["id"],
		"prompt":        "",
		"repository_id": "",
		"labels":        []string{},
		"start_agent":   false,
	})
	require.NoError(t, err)
	after := updated["watch"].(map[string]any)

	require.Equal(t, "", after["prompt"])
	require.Equal(t, "", after["repository_id"])
	require.Empty(t, after["labels"])
	require.Equal(t, false, after["start_agent"])
	// Fields the body did not mention are still as they were.
	require.Equal(t, before["agent_profile_id"], after["agent_profile_id"])
	require.Equal(t, before["executor_profile_id"], after["executor_profile_id"])
	require.Equal(t, before["base_branch"], after["base_branch"])
	require.Equal(t, before["query"], after["query"])
	require.Equal(t, before["repos"], after["repos"])
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
	require.Len(t, steps, 3)
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

func reviewBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"kind": "review", "name": "Reviews", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
	}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func createdWatch(t *testing.T, runtime *Runtime, body map[string]any) map[string]any {
	t.Helper()
	created, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", body)
	require.NoError(t, err)
	return created["watch"].(map[string]any)
}

func TestCreateReviewWatchStoresItsFieldsAndDefaults(t *testing.T) {
	t.Parallel()
	runtime, host := newWatchRuntime(t)
	watch := createdWatch(t, runtime, reviewBody(map[string]any{
		"review_scope": "user", "include_drafts": true, "cleanup_policy": "when_closed",
		"fork_workflow_step_id": "step-doing", "prompt": "Review {{pr.number}}",
	}))
	require.Equal(t, "review", watch["kind"])
	require.Equal(t, "user", watch["review_scope"])
	require.Equal(t, true, watch["include_drafts"])
	require.Equal(t, "when_closed", watch["cleanup_policy"])
	require.Equal(t, "step-doing", watch["fork_workflow_step_id"])

	defaults := createdWatch(t, runtime, reviewBody(nil))
	require.Equal(t, "user_and_teams", defaults["review_scope"])
	require.Equal(t, "never", defaults["cleanup_policy"])
	require.EqualValues(t, 300, defaults["poll_interval_seconds"])

	keys := []string{}
	for key := range host.state["workspace/ws-1"] {
		keys = append(keys, key)
	}
	for _, key := range keys {
		require.True(t, strings.HasPrefix(key, "rwatch."), "review watches are stored under rwatch., got %s", key)
	}
}

func TestPausingAReviewWatchKeepsEveryField(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	original := createdWatch(t, runtime, reviewBody(map[string]any{
		"review_scope": "user", "include_drafts": true, "cleanup_policy": "when_closed",
		"fork_workflow_step_id": "step-doing", "prompt": "Review it", "agent_profile_id": "agent-1",
		"repos": []string{"acme/web"}, "labels": []string{"ui"}, "query": "export",
	}))
	updated, err := watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": original["id"], "enabled": false})
	require.NoError(t, err)
	paused := updated["watch"].(map[string]any)
	require.Equal(t, false, paused["enabled"])
	for _, key := range []string{"kind", "review_scope", "include_drafts", "cleanup_policy", "fork_workflow_step_id",
		"prompt", "agent_profile_id", "repos", "labels", "query"} {
		require.Equal(t, original[key], paused[key], "pausing erased %s", key)
	}
}

func TestReviewWatchFieldsCanBeClearedExplicitly(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	original := createdWatch(t, runtime, reviewBody(map[string]any{
		"include_drafts": true, "fork_workflow_step_id": "step-doing",
	}))
	updated, err := watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{
		"id": original["id"], "include_drafts": false, "fork_workflow_step_id": "",
	})
	require.NoError(t, err)
	cleared := updated["watch"].(map[string]any)
	require.NotContains(t, cleared, "include_drafts")
	require.NotContains(t, cleared, "fork_workflow_step_id")
}

func TestAWatchKindCannotBeChanged(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	review := createdWatch(t, runtime, reviewBody(nil))
	_, err := watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": review["id"], "kind": "issue"})
	require.Error(t, err)
	_, err = watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": review["id"], "kind": "review"})
	require.NoError(t, err, "restating the kind is harmless")

	issue := createdWatch(t, runtime, map[string]any{
		"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox", "repos": []string{"acme/app"},
	})
	_, err = watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": issue["id"], "kind": "review"})
	require.Error(t, err)
}

func TestListWatchesFiltersByKind(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	createdWatch(t, runtime, reviewBody(nil))
	createdWatch(t, runtime, map[string]any{
		"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox", "repos": []string{"acme/app"},
	})
	names := func(body any) []string {
		listed, err := watchAction(t, runtime, ActionWatchesList, "ws-1", body)
		require.NoError(t, err)
		var out []string
		for _, item := range listed["watches"].([]any) {
			out = append(out, item.(map[string]any)["name"].(string))
		}
		return out
	}
	require.ElementsMatch(t, []string{"Bugs", "Reviews"}, names(nil), "no filter returns everything")
	require.Equal(t, []string{"Reviews"}, names(map[string]any{"kind": "review"}))
	require.Equal(t, []string{"Bugs"}, names(map[string]any{"kind": "issue"}))
}

func TestForkStepThatStartsAgentsIsRefusedAtSaveTime(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	_, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", reviewBody(map[string]any{"fork_workflow_step_id": "step-auto"}))
	require.ErrorContains(t, err, "starts an agent")
	_, err = watchAction(t, runtime, ActionWatchesCreate, "ws-1", reviewBody(map[string]any{"fork_workflow_step_id": "step-gone"}))
	require.ErrorContains(t, err, "not in the watch's workflow")

	safe := createdWatch(t, runtime, reviewBody(map[string]any{"fork_workflow_step_id": "step-doing"}))
	// Editing the watch later is checked too, not only creation.
	_, err = watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": safe["id"], "fork_workflow_step_id": "step-auto"})
	require.ErrorContains(t, err, "starts an agent")
}

func TestReviewWatchesNeedNoRepository(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	createdWatch(t, runtime, reviewBody(nil))
	_, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", map[string]any{
		"name": "Bugs", "workflow_id": "wf-1", "workflow_step_id": "step-inbox",
	})
	require.Error(t, err, "an issue watch still needs a repository")
}

func TestWatchOptionsDescribeReviewWatches(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	options, err := watchAction(t, runtime, ActionWatchesOptions, "ws-1", nil)
	require.NoError(t, err)
	require.EqualValues(t, 60, options["min_review_interval"])
	require.Contains(t, options["default_review_prompt"], "{{pr.number}}")
	require.Equal(t, false, options["archive_granted"], "a host without the v2 contract grants nothing")
	steps := options["workflows"].([]any)[0].(map[string]any)["steps"].([]any)
	autos := map[string]bool{}
	for _, step := range steps {
		item := step.(map[string]any)
		autos[item["id"].(string)] = item["auto_starts_agent"].(bool)
	}
	require.Equal(t, map[string]bool{"step-inbox": false, "step-doing": false, "step-auto": true}, autos)
}

func TestCleanupActionRefusesAWatchWithoutAPolicy(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	review := createdWatch(t, runtime, reviewBody(nil))
	_, err := watchAction(t, runtime, ActionWatchesCleanup, "ws-1", map[string]any{"id": review["id"]})
	require.Error(t, err)
	_, err = watchAction(t, runtime, ActionWatchesCleanup, "ws-1", map[string]any{"id": "missing"})
	require.Error(t, err)
}

// The host replaces any Go error with "plugin action unavailable", so an
// operator-actionable failure must travel as a status and its own message.
func TestWatchRefusalsCarryAStatusAndAMessage(t *testing.T) {
	t.Parallel()
	runtime, _ := newWatchRuntime(t)
	review := createdWatch(t, runtime, reviewBody(nil))

	statusOf := func(err error) (int, string) {
		var refused *refusedError
		require.ErrorAs(t, err, &refused)
		require.NotContains(t, refused.message, "plugin action unavailable")
		return refused.status, refused.message
	}
	_, err := watchAction(t, runtime, ActionWatchesCreate, "ws-1", map[string]any{"name": "x"})
	status, message := statusOf(err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Contains(t, message, "a workflow is required")

	_, err = watchAction(t, runtime, ActionWatchesCreate, "ws-1", reviewBody(map[string]any{"fork_workflow_step_id": "step-auto"}))
	status, message = statusOf(err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Contains(t, message, "starts an agent")

	_, err = watchAction(t, runtime, ActionWatchesUpdate, "ws-1", map[string]any{"id": review["id"], "kind": "issue"})
	status, _ = statusOf(err)
	require.Equal(t, http.StatusUnprocessableEntity, status)

	_, err = watchAction(t, runtime, ActionWatchesDelete, "ws-1", map[string]any{"id": "missing"})
	status, message = statusOf(err)
	require.Equal(t, http.StatusNotFound, status)
	require.Contains(t, message, "no such watch")

	_, err = watchAction(t, runtime, ActionWatchesCleanup, "ws-1", map[string]any{"id": review["id"]})
	status, _ = statusOf(err)
	require.Equal(t, http.StatusConflict, status)

	_, err = watchAction(t, runtime, ActionWatchesCreate, "ws-1", reviewBody(map[string]any{"repos": []string{"not-a-repo"}}))
	status, message = statusOf(err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Contains(t, message, "owner/name")
}
