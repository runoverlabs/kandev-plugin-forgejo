package watches

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// fakeHost is an in-memory Host covering the surface the watch feature uses:
// workspace state, workspace enumeration, and task create/list.
type fakeHost struct {
	pluginsdk.UnimplementedHostData

	mu    sync.Mutex
	state map[string]map[string]any

	workspaces []pluginsdk.Workspace
	tasks      []pluginsdk.Task
	created    []pluginsdk.CreateTaskInput

	steps     map[string][]pluginsdk.WorkflowStep
	updated   []pluginsdk.UpdateTaskInput
	stepsErr  error
	landAt    string // when set, created tasks end up in this step, as if automation moved them
	updateErr error

	nextTaskID int
	createErr  error
	listErr    error
	stateErr   error
}

func newFakeHost(workspaceIDs ...string) *fakeHost {
	host := &fakeHost{state: map[string]map[string]any{}}
	for _, id := range workspaceIDs {
		host.workspaces = append(host.workspaces, pluginsdk.Workspace{ID: id, Name: id})
	}
	return host
}

func (h *fakeHost) bucket(scope, scopeID string) string { return scope + "/" + scopeID }

func (h *fakeHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stateErr != nil {
		return nil, false, h.stateErr
	}
	value, found := h.state[h.bucket(scope, scopeID)][key]
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
	bucket := h.bucket(scope, scopeID)
	if h.state[bucket] == nil {
		h.state[bucket] = map[string]any{}
	}
	// Host state round-trips through JSON, so a number written as int comes
	// back as float64. Mirroring that here is what makes the decode paths
	// honest.
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
	delete(h.state[h.bucket(scope, scopeID)], key)
	return nil
}

func (h *fakeHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stateErr != nil {
		return nil, h.stateErr
	}
	entries := make([]pluginsdk.StateEntry, 0)
	for key, value := range h.state[h.bucket(scope, scopeID)] {
		entries = append(entries, pluginsdk.StateEntry{Key: key, Value: value.(map[string]any)})
	}
	return entries, nil
}

// stateKeys returns every key stored for a workspace, for assertions about the
// key layout itself.
func (h *fakeHost) stateKeys(workspaceID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0)
	for key := range h.state[h.bucket("workspace", workspaceID)] {
		keys = append(keys, key)
	}
	return keys
}

func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) { return map[string]any{}, nil }

// The secret and event surface is unused by watches but required by the Host
// interface.
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

func (h *fakeHost) Workspaces() pluginsdk.WorkspaceReader { return fakeWorkspaces{host: h} }
func (h *fakeHost) Tasks() pluginsdk.TaskReader           { return fakeTasks{host: h} }

type fakeWorkspaces struct{ host *fakeHost }

func (w fakeWorkspaces) List(context.Context, pluginsdk.Page) ([]pluginsdk.Workspace, *pluginsdk.PageInfo, error) {
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	return w.host.workspaces, &pluginsdk.PageInfo{}, nil
}

type fakeTasks struct {
	pluginsdk.TaskReader
	host *fakeHost
}

func (t fakeTasks) Create(_ context.Context, in pluginsdk.CreateTaskInput) (*pluginsdk.Task, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	if t.host.createErr != nil {
		return nil, t.host.createErr
	}
	t.host.nextTaskID++
	id := "task-" + strconv.Itoa(t.host.nextTaskID)
	task := pluginsdk.Task{
		ID:             id,
		WorkspaceID:    in.WorkspaceID,
		WorkflowID:     in.WorkflowID,
		Title:          in.Title,
		Description:    in.Description,
		WorkflowStepID: derefString(in.WorkflowStepID),
		Metadata:       in.Metadata,
	}
	if t.host.landAt != "" {
		task.WorkflowStepID = t.host.landAt
	}
	t.host.created = append(t.host.created, in)
	t.host.tasks = append(t.host.tasks, task)
	return &task, nil
}

func (t fakeTasks) List(_ context.Context, filter pluginsdk.TaskFilter, _ pluginsdk.Page) ([]pluginsdk.Task, *pluginsdk.PageInfo, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	if t.host.listErr != nil {
		return nil, nil, t.host.listErr
	}
	matched := make([]pluginsdk.Task, 0, len(t.host.tasks))
	for _, task := range t.host.tasks {
		if len(filter.WorkspaceIDs) > 0 && !contains(filter.WorkspaceIDs, task.WorkspaceID) {
			continue
		}
		matched = append(matched, task)
	}
	return matched, &pluginsdk.PageInfo{}, nil
}

// completeTask marks a created task done, so it stops counting against a
// watch's inflight budget.
func (h *fakeHost) completeTask(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	stamp := time.Now().UTC().Format(time.RFC3339)
	for i := range h.tasks {
		if h.tasks[i].ID == id {
			h.tasks[i].CompletedAt = &stamp
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// fakeIssues is a scripted IssueSource keyed by "owner/name".
type fakeIssues struct {
	mu       sync.Mutex
	byRepo   map[string][]Issue
	errs     map[string]error
	queries  []IssueQuery
	repoSeen []string
}

func newFakeIssues() *fakeIssues {
	return &fakeIssues{byRepo: map[string][]Issue{}, errs: map[string]error{}}
}

func (f *fakeIssues) set(repo string, issues ...Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byRepo[repo] = issues
}

func (f *fakeIssues) fail(repo string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[repo] = err
}

func (f *fakeIssues) ListIssues(_ context.Context, repo RepoRef, query IssueQuery) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	f.repoSeen = append(f.repoSeen, repo.FullName())
	if err, failing := f.errs[repo.FullName()]; failing {
		return nil, err
	}
	return f.byRepo[repo.FullName()], nil
}

// lastQuery returns the most recent query the poller issued.
func (f *fakeIssues) lastQuery() IssueQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		return IssueQuery{}
	}
	return f.queries[len(f.queries)-1]
}

var errBoom = errors.New("instance unreachable")

// newTestStore wires a store and poller over a fake host at a fixed clock.
func newTestStore(host *fakeHost) *Store {
	store := NewStore(func() pluginsdk.Host { return host })
	store.now = func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) }
	return store
}

// sampleWatch is a valid, saveable watch for the given workspace.
func sampleWatch(workspaceID string, repos ...string) Watch {
	refs := make([]RepoRef, 0, len(repos))
	for _, raw := range repos {
		ref, err := ParseRepoRef(raw)
		if err != nil {
			panic(err)
		}
		refs = append(refs, ref)
	}
	return Watch{
		ID:             "w1",
		WorkspaceID:    workspaceID,
		Name:           "Bugs",
		WorkflowID:     "wf-1",
		WorkflowStepID: "step-inbox",
		Repos:          refs,
		Labels:         []string{"bug"},
		Enabled:        true,
	}
}

// Review-watch support: workflow steps, task update and read, the exact
// archive command, and a scripted ReviewSource.

func (h *fakeHost) Workflows() pluginsdk.WorkflowReader { return fakeWorkflows{host: h} }

type fakeWorkflows struct {
	pluginsdk.WorkflowReader
	host *fakeHost
}

func (w fakeWorkflows) ListSteps(_ context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if w.host.stepsErr != nil {
		return nil, w.host.stepsErr
	}
	return w.host.steps[workflowID], nil
}

func (t fakeTasks) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	for i := range t.host.tasks {
		if t.host.tasks[i].ID == id {
			task := t.host.tasks[i]
			return &task, nil
		}
	}
	return nil, errors.New("no such task")
}

func (t fakeTasks) Update(_ context.Context, in pluginsdk.UpdateTaskInput) (*pluginsdk.Task, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	if t.host.updateErr != nil {
		return nil, t.host.updateErr
	}
	for i := range t.host.tasks {
		if t.host.tasks[i].ID == in.ID {
			if in.State != nil {
				t.host.tasks[i].State = *in.State
			}
			t.host.updated = append(t.host.updated, in)
			task := t.host.tasks[i]
			return &task, nil
		}
	}
	return nil, errors.New("no such task")
}

// fakeReviews is a scripted ReviewSource.
type fakeReviews struct {
	mu        sync.Mutex
	requests  []PullRequest
	details   map[string]PullDetail
	states    map[string]PullState
	errs      map[string]error
	searchErr error
	query     ReviewQuery
	detailed  []string
	checked   []string
}

func newFakeReviews() *fakeReviews {
	return &fakeReviews{details: map[string]PullDetail{}, states: map[string]PullState{}, errs: map[string]error{}}
}

func prKey(repo RepoRef, number int64) string {
	return repo.FullName() + "#" + strconv.FormatInt(number, 10)
}

// add scripts one pull request: the search hit and its detail.
func (f *fakeReviews) add(repo string, number int64, detail PullDetail) {
	ref, _ := ParseRepoRef(repo)
	f.requests = append(f.requests, PullRequest{Repo: ref, Number: number, Title: detail.Title, URL: detail.URL, Author: detail.Author})
	f.details[prKey(ref, number)] = detail
	f.states[prKey(ref, number)] = PullOpen
}

func (f *fakeReviews) SearchReviewRequests(_ context.Context, q ReviewQuery) ([]PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.query = q
	return f.requests, f.searchErr
}

func (f *fakeReviews) PullDetail(_ context.Context, repo RepoRef, number int64, _ ReviewScope) (PullDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := prKey(repo, number)
	f.detailed = append(f.detailed, key)
	if err := f.errs[key]; err != nil {
		return PullDetail{}, err
	}
	return f.details[key], nil
}

func (f *fakeReviews) PullState(_ context.Context, repo RepoRef, number int64) (PullState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := prKey(repo, number)
	f.checked = append(f.checked, key)
	if err := f.errs[key]; err != nil {
		return "", err
	}
	return f.states[key], nil
}

// openPR is a same-repository, non-draft pull request detail.
func openPR(title string) PullDetail {
	return PullDetail{
		Title: title, URL: "https://git.example/acme/web/pulls/1", Author: "alice",
		HeadBranch: "feature/x", BaseBranch: "main", Open: true, RequestedFromMe: true,
	}
}

func sampleReviewWatch(workspaceID string) Watch {
	watch := sampleWatch(workspaceID)
	watch.Kind = KindReview
	watch.Repos = nil
	watch.Labels = nil
	watch.RepositoryID = "repo-1"
	return watch
}
