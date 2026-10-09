package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// configHost is a Host that only serves plugin config; every other Host method
// keeps its unimplemented default.
type configHost struct {
	pluginsdk.UnimplementedHostData
	config map[string]any

	mu    sync.Mutex
	state map[string]map[string]any
}

func newConfigHost(config map[string]any) *configHost {
	return &configHost{config: config, state: map[string]map[string]any{}}
}

func (h *configHost) GetConfig(context.Context) (map[string]any, error) { return h.config, nil }
func (h *configHost) stateKey(scope, scopeID, key string) string {
	return scope + "/" + scopeID + "/" + key
}

func (h *configHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	value, ok := h.state[h.stateKey(scope, scopeID, key)]
	return value, ok, nil
}

func (h *configHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Host state round-trips through JSON: an int64 comes back as float64.
	encoded, _ := json.Marshal(value)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	if h.state == nil {
		h.state = map[string]map[string]any{}
	}
	h.state[h.stateKey(scope, scopeID, key)] = decoded
	return nil
}

func (h *configHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, h.stateKey(scope, scopeID, key))
	return nil
}
func (h *configHost) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	return nil, nil
}
func (h *configHost) RevealSecret(context.Context, string) (string, error) { return "", nil }
func (h *configHost) GetSecret(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (h *configHost) SetSecret(context.Context, string, string) error         { return nil }
func (h *configHost) DeleteSecret(context.Context, string) error              { return nil }
func (h *configHost) EmitEvent(context.Context, string, map[string]any) error { return nil }
func (h *configHost) InvokeUtilityAgent(context.Context, string, ...pluginsdk.UtilityAgentOptions) (string, error) {
	return "", nil
}

func decodeBody(t *testing.T, response *pluginsdk.PluginActionResponse) map[string]any {
	t.Helper()
	require.Equal(t, "application/json", response.Headers["Content-Type"])
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &decoded))
	return decoded
}

func TestConnectionGetReportsUnconfigured(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: ActionConnectionGet,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1"},
	})
	require.NoError(t, err)
	body := decodeBody(t, response)
	require.Equal(t, false, body["configured"])
	require.Equal(t, false, body["connected"])
	require.Contains(t, body["message"], "Settings > Plugins")
}

func TestConnectionTestReportsFlavorAndNeverLeaksToken(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name             string
		version          string
		forgejoNamespace bool
		wantFlavor       string
	}{
		{name: "forgejo", version: "16.0.5+gitea-1.22.0", forgejoNamespace: true, wantFlavor: "forgejo"},
		{name: "gitea", version: "1.24.7", wantFlavor: "gitea"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/user":
					_, _ = w.Write([]byte(`{"login":"kandev"}`))
				case "/api/v1/version":
					_ = json.NewEncoder(w).Encode(map[string]any{"version": testCase.version})
				case "/api/forgejo/v1/version":
					// Only Forgejo serves its own namespace.
					if !testCase.forgejoNamespace {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"version": testCase.version})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			runtime := NewRuntime()
			runtime.SetHost(newConfigHost(map[string]any{
				"base_url": server.URL, "api_token": "super-secret-token",
			}))

			response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
				ActionKey: ActionConnectionTest,
				Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1"},
			})
			require.NoError(t, err)
			body := decodeBody(t, response)
			require.Equal(t, true, body["configured"])
			require.Equal(t, true, body["connected"])
			require.Equal(t, "kandev", body["account"])
			require.Equal(t, testCase.wantFlavor, body["flavor"])
			require.NotContains(t, string(response.Body), "super-secret-token",
				"a status response must never echo the configured token")
		})
	}
}

func TestConnectionTestReportsRejectedToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token super-secret-token is invalid"}`))
	}))
	defer server.Close()

	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{"base_url": server.URL, "api_token": "super-secret-token"}))

	response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: ActionConnectionTest,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1"},
	})
	require.NoError(t, err)
	body := decodeBody(t, response)
	require.Equal(t, true, body["configured"])
	require.Equal(t, false, body["connected"])
	require.Contains(t, body["message"], "rejected the access token")
	require.NotContains(t, string(response.Body), "super-secret-token")
}

func TestSourceControlActionsAreDelegated(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	// Delegation is proven by the recipe's own guard firing, rather than the
	// runtime's "unsupported action" path.
	_, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: sourcecontrol.ActionRepositoriesList,
		Body:      []byte(`{}`),
	})
	require.ErrorContains(t, err, "requires a verified workspace")

	_, err = runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: "does.not.exist",
	})
	require.ErrorContains(t, err, "unsupported action")

	_, err = runtime.HandleAction(context.Background(), nil)
	require.ErrorContains(t, err, "action request is required")
}

func TestReferenceRPCsAreDelegatedAndFailClosed(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	// A search for a source this plugin does not own must be rejected.
	_, err := runtime.SearchEntityReferences(context.Background(), &pluginsdk.SearchEntityReferencesRequest{
		Source: "someone_elses_source", WorkspaceID: "workspace-1",
	})
	require.Error(t, err)

	authorized, err := runtime.AuthorizeEntityReference(context.Background(), &pluginsdk.AuthorizeEntityReferenceRequest{
		Source: ReferenceSource, WorkspaceID: "workspace-1", Purpose: "not-a-purpose",
	})
	require.NoError(t, err)
	require.False(t, authorized.Allowed)

	// Correct source and purpose, but the plugin is unconfigured: still denied.
	authorized, err = runtime.AuthorizeEntityReference(context.Background(), &pluginsdk.AuthorizeEntityReferenceRequest{
		Source: ReferenceSource, WorkspaceID: "workspace-1", Purpose: "submission",
		Reference: map[string]any{"id": "7:42"},
	})
	require.NoError(t, err)
	require.False(t, authorized.Allowed)
}

// The Host is injected after construction; nothing may panic before then.
func TestRuntimeToleratesMissingHost(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: ActionConnectionGet,
	})
	require.NoError(t, err)
	require.Equal(t, false, decodeBody(t, response)["configured"])
}

func TestHandleActionHonorsCancellation(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runtime.HandleAction(ctx, &pluginsdk.PluginActionRequest{
		ActionKey: sourcecontrol.ActionRepositoriesList,
	})
	require.ErrorIs(t, err, context.Canceled)
}

// probeCountingServer is a fake instance that records how many live probes it
// served, so a cached answer is distinguishable from a fresh one.
func probeCountingServer(t *testing.T, version string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var probes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/user":
			probes.Add(1)
			_, _ = w.Write([]byte(`{"login":"kandev"}`))
		case "/api/v1/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version})
		case "/api/forgejo/v1/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &probes
}

func connectionAction(t *testing.T, runtime *Runtime, key, workspaceID string) map[string]any {
	t.Helper()
	response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: key,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID},
	})
	require.NoError(t, err)
	return decodeBody(t, response)
}

// Regression: connection.get used to report connected:false unconditionally, so
// the panel showed "not connected" on every mount even seconds after a
// successful connection.test, and reverted on each refresh.
func TestConnectionGetReportsLiveState(t *testing.T) {
	t.Parallel()
	server, probes := probeCountingServer(t, "16.0.5+gitea-1.22.0")
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{"base_url": server.URL, "api_token": "t"}))

	body := connectionAction(t, runtime, ActionConnectionGet, "workspace-1")
	require.Equal(t, true, body["configured"])
	require.Equal(t, true, body["connected"], "get must report the real connection state")
	require.Equal(t, "kandev", body["account"])
	require.Equal(t, int64(1), probes.Load())
}

func TestConnectionGetServesCacheThenTestForcesProbe(t *testing.T) {
	t.Parallel()
	server, probes := probeCountingServer(t, "1.27.3")
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{"base_url": server.URL, "api_token": "t"}))

	require.Equal(t, true, connectionAction(t, runtime, ActionConnectionGet, "workspace-1")["connected"])
	require.Equal(t, int64(1), probes.Load())

	// Within the TTL a second get is served from cache.
	cached := connectionAction(t, runtime, ActionConnectionGet, "workspace-1")
	require.Equal(t, true, cached["connected"])
	require.Equal(t, true, cached["cached"])
	require.Equal(t, "kandev", cached["account"])
	require.Equal(t, int64(1), probes.Load(), "a cached get must not hit the instance")

	// An explicit test always re-probes and is never marked cached.
	fresh := connectionAction(t, runtime, ActionConnectionTest, "workspace-1")
	require.Equal(t, true, fresh["connected"])
	require.Nil(t, fresh["cached"])
	require.Equal(t, int64(2), probes.Load())
}

// A cache entry written for one instance must not answer for another.
func TestConnectionCacheIsScopedToTheConfiguredInstance(t *testing.T) {
	t.Parallel()
	first, firstProbes := probeCountingServer(t, "1.27.3")
	second, secondProbes := probeCountingServer(t, "16.0.5+gitea-1.22.0")
	host := newConfigHost(map[string]any{"base_url": first.URL, "api_token": "t"})
	runtime := NewRuntime()
	runtime.SetHost(host)

	require.Equal(t, true, connectionAction(t, runtime, ActionConnectionGet, "workspace-1")["connected"])
	require.Equal(t, int64(1), firstProbes.Load())

	host.config = map[string]any{"base_url": second.URL, "api_token": "t"}
	body := connectionAction(t, runtime, ActionConnectionGet, "workspace-1")
	require.Equal(t, second.URL, body["instance_url"])
	require.Nil(t, body["cached"], "a cache entry for the old instance must not be reused")
	require.Equal(t, int64(1), secondProbes.Load())
}

// A cached success must not survive the connection breaking.
func TestConnectionCacheClearedWhenProbeFails(t *testing.T) {
	t.Parallel()
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/user" {
			_, _ = w.Write([]byte(`{"login":"kandev"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	host := newConfigHost(map[string]any{"base_url": server.URL, "api_token": "t"})
	runtime := NewRuntime()
	runtime.SetHost(host)
	require.Equal(t, true, connectionAction(t, runtime, ActionConnectionGet, "workspace-1")["connected"])

	reject.Store(true)
	failed := connectionAction(t, runtime, ActionConnectionTest, "workspace-1")
	require.Equal(t, false, failed["connected"])
	require.Contains(t, failed["message"], "rejected the access token")

	// The next get must not resurrect the cached success.
	after := connectionAction(t, runtime, ActionConnectionGet, "workspace-1")
	require.Equal(t, false, after["connected"])
}

// Caching is keyed by workspace; without one the probe still answers.
func TestConnectionStatusWorksWithoutAWorkspace(t *testing.T) {
	t.Parallel()
	server, probes := probeCountingServer(t, "1.27.3")
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{"base_url": server.URL, "api_token": "t"}))

	for range 2 {
		body := connectionAction(t, runtime, ActionConnectionGet, "")
		require.Equal(t, true, body["connected"])
		require.Nil(t, body["cached"])
	}
	require.Equal(t, int64(2), probes.Load(), "with no workspace there is nowhere to cache")
}

func setEnabled(t *testing.T, runtime *Runtime, workspaceID string, enabled bool) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"enabled": enabled})
	response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: ActionConnectionSetEnabled,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID},
		Body:      body,
	})
	require.NoError(t, err)
	return decodeBody(t, response)
}

// Installing the plugin is the opt-in: a workspace with no stored choice is on.
func TestIntegrationIsEnabledByDefault(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	body := connectionAction(t, runtime, ActionConnectionGet, "workspace-1")
	require.Equal(t, true, body["enabled"])
}

func TestSetEnabledRoundTrips(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	require.Equal(t, false, setEnabled(t, runtime, "workspace-1", false)["enabled"])
	require.Equal(t, false, connectionAction(t, runtime, ActionConnectionGet, "workspace-1")["enabled"])

	require.Equal(t, true, setEnabled(t, runtime, "workspace-1", true)["enabled"])
	require.Equal(t, true, connectionAction(t, runtime, ActionConnectionGet, "workspace-1")["enabled"])
}

// The choice is per workspace: turning it off in one must not affect another.
func TestEnabledStateIsPerWorkspace(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	setEnabled(t, runtime, "workspace-1", false)
	require.Equal(t, false, connectionAction(t, runtime, ActionConnectionGet, "workspace-1")["enabled"])
	require.Equal(t, true, connectionAction(t, runtime, ActionConnectionGet, "workspace-2")["enabled"])
}

// The toggle must withdraw the integration, not just move a badge.
func TestDisabledWorkspaceWithdrawsSourceControl(t *testing.T) {
	t.Parallel()
	server, probes := probeCountingServer(t, "1.27.3")
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{"base_url": server.URL, "api_token": "t"}))
	setEnabled(t, runtime, "workspace-1", false)
	before := probes.Load()

	for _, testCase := range []struct {
		action string
		field  string
	}{
		{action: sourcecontrol.ActionRepositoriesList, field: "repositories"},
		{action: sourcecontrol.ActionRepositoriesBranches, field: "branches"},
		{action: sourcecontrol.ActionChangeRequestsGet, field: "reviews"},
		{action: sourcecontrol.ActionChangeRequestAssociations, field: "associations"},
	} {
		response, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
			ActionKey: testCase.action,
			Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1", TaskID: "task-1"},
			Body:      []byte(`{}`),
		})
		require.NoError(t, err, testCase.action)
		require.Empty(t, decodeBody(t, response)[testCase.field], testCase.action)
	}

	inspect, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: sourcecontrol.ActionRepositoriesInspect,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1"},
		Body:      []byte(`{"url":"https://example.com/a/b"}`),
	})
	require.NoError(t, err)
	require.Nil(t, decodeBody(t, inspect)["repository"])

	require.Equal(t, before, probes.Load(), "a disabled workspace must not reach the instance")
}

// Reads go quiet, but a mutation says no rather than silently doing nothing.
func TestDisabledWorkspaceRefusesMutations(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))
	setEnabled(t, runtime, "workspace-1", false)

	for _, action := range []string{
		sourcecontrol.ActionChangeRequestsCreate,
		sourcecontrol.ActionChangeRequestsLink,
		sourcecontrol.ActionChangeRequestsUnlink,
	} {
		_, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
			ActionKey: action,
			Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1", TaskID: "task-1"},
			Body:      []byte(`{}`),
		})
		require.ErrorContains(t, err, "turned off for this workspace", action)
	}
}

// Re-enabling restores the provider.
func TestReEnablingRestoresSourceControl(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))
	setEnabled(t, runtime, "workspace-1", false)
	setEnabled(t, runtime, "workspace-1", true)

	// Enabled but unconfigured: the recipe's own guard fires, which proves the
	// request reached the extension instead of the disabled short-circuit.
	_, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: sourcecontrol.ActionRepositoriesList,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1"},
		Body:      []byte(`{}`),
	})
	require.ErrorContains(t, err, "resolve connection scope")
}

func TestSetEnabledValidatesInput(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{}))

	_, err := runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: ActionConnectionSetEnabled,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "workspace-1"},
		Body:      []byte(`{}`),
	})
	require.ErrorContains(t, err, "enabled is required")

	_, err = runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: ActionConnectionSetEnabled,
		Body:      []byte(`{"enabled":true}`),
	})
	require.ErrorContains(t, err, "requires a verified workspace")
}

func TestSafeMessageDoesNotCallWriteForbiddenABadToken(t *testing.T) {
	t.Parallel()
	forbidden := safeMessage(&forgejo.WriteError{Status: 403, Reason: forgejo.ReasonForbidden})
	require.NotContains(t, forbidden, "rejected the access token")
	require.Contains(t, forbidden, "permission")
	require.Contains(t, safeMessage(&forgejo.WriteError{Status: 409, Reason: forgejo.ReasonConflict}), "conflicts")
}
