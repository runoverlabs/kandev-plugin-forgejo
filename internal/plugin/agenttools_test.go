package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// agentHost is a configHost that also serves the task and repository reads the
// agent tools depend on. An agent tool arrives with a task id and nothing else,
// so this is the whole resolution path under test.
type agentHost struct {
	*configHost
	mu    sync.Mutex
	tasks map[string]*pluginsdk.Task
	repos map[string][]pluginsdk.Repository
}

func newAgentHost(config map[string]any) *agentHost {
	return &agentHost{
		configHost: newConfigHost(config),
		tasks:      map[string]*pluginsdk.Task{},
		repos:      map[string][]pluginsdk.Repository{},
	}
}

func (h *agentHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.configHost.mu.Lock()
	defer h.configHost.mu.Unlock()
	prefix := scope + "/" + scopeID + "/"
	entries := make([]pluginsdk.StateEntry, 0)
	for key, value := range h.configHost.state {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entries = append(entries, pluginsdk.StateEntry{Key: strings.TrimPrefix(key, prefix), Value: value})
	}
	return entries, nil
}

func (h *agentHost) Tasks() pluginsdk.TaskReader { return agentTasks{host: h} }
func (h *agentHost) Repositories() pluginsdk.RepositoryReader {
	return agentRepos{host: h}
}

type agentTasks struct {
	pluginsdk.TaskReader
	host *agentHost
}

func (t agentTasks) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	return t.host.tasks[id], nil
}

type agentRepos struct{ host *agentHost }

func (r agentRepos) List(_ context.Context, workspaceID string, _ pluginsdk.Page) ([]pluginsdk.Repository, *pluginsdk.PageInfo, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	return r.host.repos[workspaceID], &pluginsdk.PageInfo{}, nil
}

// agentFixture is one wired plugin: a fake instance, a host holding one task
// with one attached Forgejo repository, and a runtime joining them.
type agentFixture struct {
	runtime *Runtime
	host    *agentHost
	server  *httptest.Server
	mu      sync.Mutex
	routes  map[string]http.HandlerFunc
	calls   map[string]*atomic.Int64
}

func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	fixture := &agentFixture{routes: map[string]http.HandlerFunc{}, calls: map[string]*atomic.Int64{}}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		fixture.mu.Lock()
		handler, ok := fixture.routes[key]
		counter, counted := fixture.calls[key]
		if !counted {
			counter = &atomic.Int64{}
			fixture.calls[key] = counter
		}
		fixture.mu.Unlock()
		counter.Add(1)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(fixture.server.Close)

	fixture.host = newAgentHost(map[string]any{"base_url": fixture.server.URL, "api_token": "secret"})
	fixture.host.tasks["task-1"] = &pluginsdk.Task{
		ID: "task-1", WorkspaceID: "workspace-1",
		Repositories: []pluginsdk.TaskRepository{{RepositoryID: "kandev-repo-1"}},
	}
	fixture.host.repos["workspace-1"] = []pluginsdk.Repository{{
		ID: "kandev-repo-1", WorkspaceID: "workspace-1", ProviderID: ProviderID,
		ProviderRepositoryID: "77", RemoteURL: fixture.server.URL + "/kandev/demo",
	}}
	fixture.route("GET", "/api/v1/repositories/77", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":77,"name":"demo","full_name":"kandev/demo",
			"owner":{"login":"kandev"},"clone_url":"` + fixture.server.URL + `/kandev/demo.git",
			"html_url":"` + fixture.server.URL + `/kandev/demo","default_branch":"main"}`))
	})

	fixture.runtime = NewRuntime()
	fixture.runtime.SetHost(fixture.host)
	return fixture
}

func (f *agentFixture) route(method, path string, handler http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = handler
}

func (f *agentFixture) json(method, path string, body string) {
	f.route(method, path, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

func (f *agentFixture) callCount(method, path string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if counter, ok := f.calls[method+" "+path]; ok {
		return counter.Load()
	}
	return 0
}

func (f *agentFixture) invoke(t *testing.T, name string, arguments map[string]any) *pluginsdk.AgentToolResult {
	t.Helper()
	result, err := f.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		InvocationID: "invocation-1",
		Name:         name,
		Arguments:    arguments,
		Context: pluginsdk.AgentToolContext{
			TaskID: "task-1", SessionID: "session-1", WorkspaceID: "workspace-1", Surface: "kanban-task",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	// The host rejects a result with empty text outright, so every path must
	// produce something readable.
	require.NotEmpty(t, strings.TrimSpace(result.Text))
	return result
}

func TestAgentToolCIReportsFailedJobsWithLogIDs(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs",
		`{"total_count":1,"workflow_runs":[{"id":7,"status":"failure","title":"build","commit_sha":"abc1234","html_url":"http://forge/run/7"}]}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs/7/jobs",
		`[{"id":41,"name":"lint","status":"success"},{"id":42,"name":"test","status":"failure"}]`)

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature"})
	require.False(t, result.IsError)
	require.Contains(t, result.Text, "failure kandev/demo@feature")
	require.Contains(t, result.Text, "failure test job=42")
	require.NotContains(t, result.Text, "lint", "a passing job is not worth the agent's context")
	require.Equal(t, "failure", result.StructuredContent["state"])
	require.Equal(t, "actions_runs", result.StructuredContent["source"])
}

func TestAgentToolCIRequiresRef(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	result := fixture.invoke(t, ToolCI, map[string]any{})
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "ref is required")
}

func TestAgentToolCIInlinesFailedJobLogs(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs",
		`{"total_count":1,"workflow_runs":[{"id":7,"status":"failure","title":"build","commit_sha":"abc1234"}]}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs/7/jobs",
		`[{"id":41,"name":"lint","status":"success"},{"id":42,"name":"test","status":"failure"}]`)
	var builder strings.Builder
	for i := 0; i < 500; i++ {
		builder.WriteString("log line\n")
	}
	builder.WriteString("FINAL FAILURE\n")
	fixture.route("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(builder.String()))
	})

	// Without logs the cheap read stays cheap and fetches nothing.
	summary := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature"})
	require.False(t, summary.IsError)
	require.NotContains(t, summary.Text, "FINAL FAILURE")
	require.Zero(t, fixture.callCount("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs"))

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature", "logs": float64(5)})
	require.False(t, result.IsError)
	require.Contains(t, result.Text, "--- test (job 42) ---")
	require.Contains(t, result.Text, "FINAL FAILURE")
	require.Contains(t, result.Text, "earlier output omitted")
	require.NotContains(t, result.Text, "--- lint", "a passing job has nothing to explain")

	// An absurd request is clamped to the ceiling rather than overflowing or
	// being honoured: 500 lines fit, so nothing is omitted.
	huge := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature", "logs": float64(9_000_000_000_000)})
	require.False(t, huge.IsError)
	require.Contains(t, huge.Text, "FINAL FAILURE")
	require.NotContains(t, huge.Text, "earlier output omitted")

	jobs := result.StructuredContent["jobs"].([]any)
	failed := jobs[1].(map[string]any)
	require.Equal(t, true, failed["log_attached"])
	require.Equal(t, true, failed["log_truncated"])
	// The log body belongs to Text alone: the host counts Text and the encoded
	// structured content against one ceiling.
	require.NotContains(t, failed, "log")
}

// TestAgentToolCILogsBoundedByJobCount keeps one pathological run from
// spending the whole invocation on log fetches.
func TestAgentToolCILogsBoundedByJobCount(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	jobs := make([]string, 0, 8)
	for id := 41; id <= 48; id++ {
		jobs = append(jobs, fmt.Sprintf(`{"id":%d,"name":"job-%d","status":"failure"}`, id, id))
		fixture.json("GET", fmt.Sprintf("/api/v1/repos/kandev/demo/actions/jobs/%d/logs", id), "boom\n")
	}
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs",
		`{"total_count":1,"workflow_runs":[{"id":7,"status":"failure","title":"build","commit_sha":"abc"}]}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs/7/jobs", "["+strings.Join(jobs, ",")+"]")

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature", "logs": float64(50)})
	require.False(t, result.IsError)
	require.Equal(t, ciLogMaxJobs, strings.Count(result.Text, "--- job-"))
	require.Contains(t, result.Text, "more failed jobs have logs")
}

// TestAgentToolCILogUnavailableIsLocalToTheJob covers Forgejo 13, which lists
// runs but serves no job logs, and Gitea's 500 for an unknown id. Neither may
// turn the whole CI read into a failure.
func TestAgentToolCILogUnavailableIsLocalToTheJob(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs",
		`{"total_count":1,"workflow_runs":[{"id":7,"status":"failure","title":"build","commit_sha":"abc"}]}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs/7/jobs",
		`[{"id":42,"name":"test","status":"failure"}]`)
	// No log route registered: the fake instance answers 404.

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature", "logs": float64(20)})
	require.False(t, result.IsError, "a missing log is not a failed CI read")
	require.Contains(t, result.Text, "failure test job=42")
	require.Contains(t, result.Text, "(no log available)")
}

// TestAgentToolCIWithoutLogIDsAsksForNothing covers the commit-status source,
// where checks are real but have no fetchable log.
func TestAgentToolCIWithoutLogIDsAsksForNothing(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/commits/feature/status",
		`{"state":"failure","sha":"dead","statuses":[{"status":"failure","context":"woodpecker/test","target_url":"http://ci/2"}]}`)

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature", "logs": float64(20)})
	require.False(t, result.IsError)
	require.Contains(t, result.Text, "failure woodpecker/test")
	require.NotContains(t, result.Text, "job=")
	require.NotContains(t, result.Text, "---")
}

func TestAgentToolPROpenIsIdempotent(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	created := int64(0)
	fixture.route("POST", "/api/v1/repos/kandev/demo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&created, 1)
		_, _ = w.Write([]byte(`{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open",
			"head":{"ref":"feature"},"base":{"ref":"main"}}`))
	})
	open := `[{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open",
		"head":{"ref":"feature"},"base":{"ref":"main"}}]`
	first := true
	fixture.route("GET", "/api/v1/repos/kandev/demo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		// Before the create there is nothing; afterwards the pull request is
		// listed, which is exactly the state a repeated call would see.
		if first {
			first = false
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(open))
	})
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12", open[1:len(open)-1])

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "open", "head": "feature", "title": "Add thing"})
	require.False(t, result.IsError)
	require.Contains(t, result.Text, "#12 open")

	again := fixture.invoke(t, ToolPR, map[string]any{"op": "open", "head": "feature", "title": "Add thing"})
	require.False(t, again.IsError)
	require.Contains(t, again.Text, "already open")
	require.EqualValues(t, 1, atomic.LoadInt64(&created),
		"kandev never retries an agent tool, so opening twice must not open twice")
}

// TestAgentToolPROpenLinksTheTask is what makes an agent-opened pull request
// show up in Kandev's own review sidebar rather than only on the forge.
func TestAgentToolPROpenLinksTheTask(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls", `[]`)
	fixture.json("POST", "/api/v1/repos/kandev/demo/pulls",
		`{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open","head":{"ref":"feature"},"base":{"ref":"main"}}`)
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12",
		`{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open","head":{"ref":"feature"},"base":{"ref":"main"}}`)

	fixture.invoke(t, ToolPR, map[string]any{"op": "open", "head": "feature", "title": "Add thing"})

	entries, err := fixture.host.ListState(context.Background(), "workspace", "workspace-1")
	require.NoError(t, err)
	var linked map[string]any
	for _, entry := range entries {
		if strings.HasPrefix(entry.Key, "task:") {
			linked = entry.Value
		}
	}
	require.NotNil(t, linked, "the association store is how kandev finds the pull request")
	require.Equal(t, "task-1", linked["task_id"])
	require.Equal(t, "77", linked["repository_id"])
	require.EqualValues(t, 12, linked["number"])
}

func TestAgentToolPRReadyClearsWorkInProgressPrefix(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	require.NoError(t, fixture.host.SetState(context.Background(), "workspace", "workspace-1",
		"task:task-1:77:12", map[string]any{
			"task_id": "task-1", "connection_scope": fixture.server.URL,
			"repository_id": "77", "number": 12,
		}))
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12",
		`{"number":12,"title":"WIP: Add thing","html_url":"http://forge/pr/12","state":"open","head":{"ref":"feature"}}`)
	var sent map[string]any
	fixture.route("PATCH", "/api/v1/repos/kandev/demo/pulls/12", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
		_, _ = w.Write([]byte(`{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open","head":{"ref":"feature"}}`))
	})

	result := fixture.invoke(t, ToolPR, map[string]any{"op": "ready"})
	require.False(t, result.IsError)
	require.Equal(t, "Add thing", sent["title"])
	require.Contains(t, result.Text, "ready")

	// Readying an already-ready pull request must not send a second edit.
	fixture.json("GET", "/api/v1/repos/kandev/demo/pulls/12",
		`{"number":12,"title":"Add thing","html_url":"http://forge/pr/12","state":"open","head":{"ref":"feature"}}`)
	second := fixture.invoke(t, ToolPR, map[string]any{"op": "ready"})
	require.False(t, second.IsError)
	require.Contains(t, second.Text, "already ready")
	require.EqualValues(t, 1, fixture.callCount("PATCH", "/api/v1/repos/kandev/demo/pulls/12"))
}

// TestAgentToolsRespectTheWorkspaceToggle keeps the enable switch honest: an
// agent must not reach the instance through a door the operator closed.
func TestAgentToolsRespectTheWorkspaceToggle(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs", `{"total_count":0,"workflow_runs":[]}`)
	require.NoError(t, fixture.host.SetState(context.Background(), "workspace", "workspace-1",
		enabledStateKey, map[string]any{"enabled": false}))

	for _, name := range []string{ToolCI, ToolPR} {
		result := fixture.invoke(t, name, map[string]any{"ref": "feature", "op": "get"})
		require.Truef(t, result.IsError, "tool %s", name)
		require.Contains(t, result.Text, "turned off")
	}
	require.Zero(t, fixture.callCount("GET", "/api/v1/repos/kandev/demo/actions/runs"))
}

func TestAgentToolAmbiguousRepositoryAsksForOne(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.host.tasks["task-1"].Repositories = append(fixture.host.tasks["task-1"].Repositories,
		pluginsdk.TaskRepository{RepositoryID: "kandev-repo-2"})
	fixture.host.repos["workspace-1"] = append(fixture.host.repos["workspace-1"], pluginsdk.Repository{
		ID: "kandev-repo-2", WorkspaceID: "workspace-1", ProviderID: ProviderID,
		ProviderRepositoryID: "78", RemoteURL: fixture.server.URL + "/kandev/other",
	})
	fixture.json("GET", "/api/v1/repositories/78",
		`{"id":78,"name":"other","full_name":"kandev/other","owner":{"login":"kandev"},
		  "clone_url":"http://forge/kandev/other.git","default_branch":"main"}`)

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature"})
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "pass repo")
	require.Contains(t, result.Text, "kandev/demo")
	require.Contains(t, result.Text, "kandev/other")

	// Naming one resolves it.
	fixture.json("GET", "/api/v1/repos/kandev/other/actions/runs",
		`{"total_count":1,"workflow_runs":[{"id":9,"status":"success","title":"build","commit_sha":"abc"}]}`)
	chosen := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature", "repo": "kandev/other"})
	require.False(t, chosen.IsError)
	require.Equal(t, "success", chosen.StructuredContent["state"])
}

// TestAgentToolSkipsNonForgejoRepositories covers a task that mixes providers:
// the other provider's repository is a filter, not a failure.
func TestAgentToolSkipsNonForgejoRepositories(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	fixture.host.tasks["task-1"].Repositories = append([]pluginsdk.TaskRepository{{RepositoryID: "github-repo"}},
		fixture.host.tasks["task-1"].Repositories...)
	fixture.host.repos["workspace-1"] = append(fixture.host.repos["workspace-1"], pluginsdk.Repository{
		ID: "github-repo", WorkspaceID: "workspace-1", ProviderID: "github",
		ProviderRepositoryID: "1", RemoteURL: "https://github.com/kandev/demo",
	})
	fixture.json("GET", "/api/v1/repos/kandev/demo/actions/runs",
		`{"total_count":1,"workflow_runs":[{"id":9,"status":"success","title":"build","commit_sha":"abc"}]}`)

	result := fixture.invoke(t, ToolCI, map[string]any{"ref": "feature"})
	require.False(t, result.IsError)
	require.Equal(t, "kandev/demo", result.StructuredContent["repo"])
}

func TestAgentToolPRGetWithNothingLinked(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	result := fixture.invoke(t, ToolPR, map[string]any{"op": "get"})
	require.False(t, result.IsError, "no pull request yet is an answer, not a failure")
	require.Contains(t, result.Text, "No pull request")
	require.Empty(t, result.StructuredContent["pulls"])
}

func TestAgentToolUnknownNameIsAnError(t *testing.T) {
	t.Parallel()
	fixture := newAgentFixture(t)
	_, err := fixture.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    "nope",
		Context: pluginsdk.AgentToolContext{TaskID: "task-1", WorkspaceID: "workspace-1", Surface: "kanban-task"},
	})
	require.Error(t, err)
}
