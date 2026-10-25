package forgejo

import (
	"context"
	"encoding/json"
	"kandev-plugin-forgejo/internal/sourcecontrol"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// fakeHost is a minimal in-memory Host. It records every state write so tests
// can assert on stored association shape, and it captures the Authorization
// header the client presents.
type fakeHost struct {
	pluginsdk.UnimplementedHostData

	mu     sync.Mutex
	config map[string]any
	state  map[string]map[string]any // scope/scopeID -> key -> value

	tasks     map[string]*pluginsdk.Task
	repos     map[string][]pluginsdk.Repository
	taskErr   error
	repoErr   error
	stateErr  error
	listCalls int
}

func newFakeHost(config map[string]any) *fakeHost {
	return &fakeHost{
		config: config,
		state:  map[string]map[string]any{},
		tasks:  map[string]*pluginsdk.Task{},
		repos:  map[string][]pluginsdk.Repository{},
	}
}

func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.config, nil
}

func (h *fakeHost) scopeKey(scope, scopeID string) string { return scope + "/" + scopeID }

func (h *fakeHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, ok := h.state[h.scopeKey(scope, scopeID)]
	if !ok {
		return nil, false, nil
	}
	value, found := entries[key]
	if !found {
		return nil, false, nil
	}
	return value.(map[string]any), true, nil
}

func (h *fakeHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stateErr != nil {
		return h.stateErr
	}
	bucket := h.scopeKey(scope, scopeID)
	if h.state[bucket] == nil {
		h.state[bucket] = map[string]any{}
	}
	// Host state round-trips through JSON, so mirror that here: a number
	// written as int64 comes back as float64.
	encoded, _ := json.Marshal(value)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	h.state[bucket][key] = decoded
	return nil
}

func (h *fakeHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stateErr != nil {
		return h.stateErr
	}
	if entries, ok := h.state[h.scopeKey(scope, scopeID)]; ok {
		delete(entries, key)
	}
	return nil
}

func (h *fakeHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stateErr != nil {
		return nil, h.stateErr
	}
	entries := make([]pluginsdk.StateEntry, 0)
	for key, value := range h.state[h.scopeKey(scope, scopeID)] {
		entries = append(entries, pluginsdk.StateEntry{Key: key, Value: value.(map[string]any)})
	}
	return entries, nil
}

func (h *fakeHost) RevealSecret(context.Context, string) (string, error) { return "", nil }
func (h *fakeHost) GetSecret(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (h *fakeHost) SetSecret(context.Context, string, string) error         { return nil }
func (h *fakeHost) DeleteSecret(context.Context, string) error              { return nil }
func (h *fakeHost) EmitEvent(context.Context, string, map[string]any) error { return nil }
func (h *fakeHost) InvokeUtilityAgent(context.Context, string, ...pluginsdk.UtilityAgentOptions) (string, error) {
	return "", nil
}

func (h *fakeHost) Tasks() pluginsdk.TaskReader { return fakeTasks{host: h} }
func (h *fakeHost) Repositories() pluginsdk.RepositoryReader {
	return fakeRepositories{host: h}
}

type fakeTasks struct {
	pluginsdk.TaskReader
	host *fakeHost
}

func (t fakeTasks) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	if t.host.taskErr != nil {
		return nil, t.host.taskErr
	}
	return t.host.tasks[id], nil
}

type fakeRepositories struct {
	host *fakeHost
}

func (r fakeRepositories) List(_ context.Context, workspaceID string, _ pluginsdk.Page) ([]pluginsdk.Repository, *pluginsdk.PageInfo, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	r.host.listCalls++
	if r.host.repoErr != nil {
		return nil, nil, r.host.repoErr
	}
	return r.host.repos[workspaceID], &pluginsdk.PageInfo{}, nil
}

// apiServer is a fake Forgejo instance. Handlers are keyed by "METHOD /path".
type apiServer struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []*http.Request
	authSeen []string
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	api := &apiServer{t: t, handlers: map[string]http.HandlerFunc{}}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		handler, ok := api.handlers[r.Method+" "+r.URL.Path]
		api.requests = append(api.requests, r)
		api.authSeen = append(api.authSeen, r.Header.Get("Authorization"))
		api.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(api.server.Close)
	return api
}

// handle registers a JSON response for one method/path pair.
func (a *apiServer) handle(method, path string, status int, body any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

// handleFunc registers a custom handler, e.g. to assert on the request body.
func (a *apiServer) handleFunc(method, path string, handler http.HandlerFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[method+" "+path] = handler
}

func (a *apiServer) url() string { return a.server.URL }

// newTestConnection wires a Connection at the fake instance with a fake Host.
func newTestConnection(t *testing.T, api *apiServer, token string) (*Connection, *fakeHost) {
	t.Helper()
	host := newFakeHost(map[string]any{"base_url": api.url(), "api_token": token})
	connection := NewConnection(func() pluginsdk.Host { return host })
	return connection, host
}

func identity(scope, repositoryID string, number int64) sourcecontrol.ChangeRequestIdentity {
	return sourcecontrol.ChangeRequestIdentity{ConnectionScope: scope, RepositoryID: repositoryID, Number: number}
}
