package prautomation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// fakeState is an in-memory Host state that round-trips values through JSON,
// as the real Host does: an int written comes back as float64.
type fakeState struct {
	mu      sync.Mutex
	entries map[string]map[string]map[string]any // scopeID → key → value
	// onGet, when set, runs before a read of key, outside the lock: a hook to
	// interleave a concurrent write.
	onGet func(key string)
	// setErr, when set, fails every write.
	setErr error
}

func newFakeState() *fakeState { return &fakeState{entries: map[string]map[string]map[string]any{}} }

func (f *fakeState) GetState(_ context.Context, _, scopeID, key string) (map[string]any, bool, error) {
	if f.onGet != nil {
		f.onGet(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.entries[scopeID][key]
	return value, ok, nil
}

func (f *fakeState) SetState(_ context.Context, _, scopeID, key string, value map[string]any) error {
	if f.setErr != nil {
		return f.setErr
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries[scopeID] == nil {
		f.entries[scopeID] = map[string]map[string]any{}
	}
	f.entries[scopeID][key] = decoded
	return nil
}

func (f *fakeState) DeleteState(_ context.Context, _, scopeID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries[scopeID], key)
	return nil
}

func (f *fakeState) ListState(_ context.Context, _, scopeID string) ([]pluginsdk.StateEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var entries []pluginsdk.StateEntry
	for key, value := range f.entries[scopeID] {
		entries = append(entries, pluginsdk.StateEntry{Key: key, Value: value})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

type fakeLinks struct {
	refs map[string][]PullRef
	// afterLinked, when set, runs once the links have been read: a hook to
	// link a pull request the caller's answer does not include.
	afterLinked func()
}

func (f *fakeLinks) Linked(_ context.Context, workspaceID string) ([]PullRef, error) {
	refs := f.refs[workspaceID]
	if f.afterLinked != nil {
		f.afterLinked()
	}
	return refs, nil
}

type fakeObserver struct {
	mu    sync.Mutex
	obs   map[PullRef]Observation
	err   error
	calls int
	reads Reads // of the last call
}

func (f *fakeObserver) Observe(_ context.Context, _ string, ref PullRef, reads Reads) (Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.reads = reads
	if f.err != nil {
		return Observation{}, f.err
	}
	obs, ok := f.obs[ref]
	if !ok {
		return Observation{}, errors.New("no such pull request")
	}
	return obs, nil
}

type mergeCall struct {
	ref  PullRef
	head string
}

type fakeMerger struct {
	calls []mergeCall
	err   error
}

func (f *fakeMerger) Merge(_ context.Context, _ string, ref PullRef, headSHA string) error {
	f.calls = append(f.calls, mergeCall{ref: ref, head: headSHA})
	return f.err
}

type sent struct {
	taskID, sessionID, text string
}

type fakeMessenger struct {
	sessions    map[string][]Session
	sessionsErr error
	sendErr     error
	status      string
	sent        []sent
	// onSend, when set, runs before a prompt is delivered.
	onSend func()
}

func (f *fakeMessenger) Sessions(_ context.Context, taskID string) ([]Session, error) {
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	return f.sessions[taskID], nil
}

func (f *fakeMessenger) Send(_ context.Context, taskID, sessionID, text string) (string, error) {
	if f.onSend != nil {
		f.onSend()
	}
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sent = append(f.sent, sent{taskID: taskID, sessionID: sessionID, text: text})
	if f.status == "" {
		return "sent", nil
	}
	return f.status, nil
}

const testWorkspace = "ws-1"

var testRef = PullRef{TaskID: "task-1", RepositoryID: "42", Number: 7}

// harness is an engine over fakes with a controllable clock.
type harness struct {
	t         *testing.T
	engine    *Engine
	state     *fakeState
	links     *fakeLinks
	observer  *fakeObserver
	merger    *fakeMerger
	messenger *fakeMessenger
	now       time.Time
	enabled   bool
	ids       int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		state:     newFakeState(),
		links:     &fakeLinks{refs: map[string][]PullRef{testWorkspace: {testRef}}},
		observer:  &fakeObserver{obs: map[PullRef]Observation{testRef: openObservation()}},
		merger:    &fakeMerger{},
		messenger: &fakeMessenger{sessions: map[string][]Session{testRef.TaskID: {{ID: "s-1", State: sessionIdle, StartedAt: "2026-10-25T10:00:00Z"}}}},
		now:       time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
		enabled:   true,
	}
	h.engine = NewEngine(Config{
		Store:     NewStore(func() StateHost { return h.state }),
		Links:     h.links,
		Observer:  h.observer,
		Merger:    h.merger,
		Messenger: h.messenger,
		Gate: func(context.Context, string) (bool, error) {
			return h.enabled, nil
		},
		Workspaces: func(context.Context) ([]string, error) { return []string{testWorkspace}, nil },
		Describe:   func(error) string { return "described" },
	})
	h.engine.now = func() time.Time { return h.now }
	h.engine.newID = func() string {
		h.ids++
		return fmt.Sprintf("id%d", h.ids)
	}
	return h
}

func openObservation() Observation {
	mergeable := true
	return Observation{
		Number:     7,
		State:      StateOpen,
		Title:      "Add the thing",
		URL:        "https://forgejo.example/o/r/pulls/7",
		Repository: "o/r",
		BaseBranch: "main",
		HeadBranch: "feature",
		HeadSHA:    "aaaaaaaaaaaa",
		Mergeable:  &mergeable,
		CI:         CI{State: CISuccess},
		Operator:   "kandev-bot",
	}
}

func (h *harness) setObs(change func(*Observation)) {
	h.observer.mu.Lock()
	defer h.observer.mu.Unlock()
	obs := h.observer.obs[testRef]
	change(&obs)
	h.observer.obs[testRef] = obs
}

func (h *harness) settings(s Settings) {
	h.t.Helper()
	if err := h.engine.store.PutSettings(context.Background(), testWorkspace, s); err != nil {
		h.t.Fatal(err)
	}
}

// switches seeds the record with the given switches, as the panel would.
func (h *harness) switches(s Switches) {
	h.t.Helper()
	ctx := context.Background()
	h.engine.Tick(ctx) // creates and seeds the record
	if _, err := h.engine.Update(ctx, testWorkspace, testRef, func(r *Record) error {
		r.ApplySwitches(s)
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) record() Record {
	h.t.Helper()
	record, found, err := h.engine.store.Get(context.Background(), testWorkspace, testRef)
	if err != nil || !found {
		h.t.Fatalf("record: found=%v err=%v", found, err)
	}
	return record
}

// evaluate runs one evaluation of the test pull request.
func (h *harness) evaluate() Record {
	h.engine.Evaluate(context.Background(), testWorkspace, testRef)
	return h.record()
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }
