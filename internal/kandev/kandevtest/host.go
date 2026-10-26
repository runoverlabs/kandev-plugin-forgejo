// Package kandevtest is an in-memory Kandev host for tests: plugin state,
// config and tasks, with the same JSON round trip the real host applies.
package kandevtest

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Host is a minimal in-memory pluginsdk.Host. Embed it to add the readers a
// test needs; every method it does not implement panics through the embedded
// UnimplementedHostData, which names the missing call.
type Host struct {
	pluginsdk.UnimplementedHostData

	mu     sync.Mutex
	config map[string]any
	state  map[string]map[string]any // scope/scopeID -> key -> value
	tasks  map[string]*pluginsdk.Task

	// StateErr, when set, fails every state write and list.
	StateErr error
	// TaskErr, when set, fails every task read.
	TaskErr error
}

// NewHost returns a host serving config, with no state and no tasks.
func NewHost(config map[string]any) *Host {
	return &Host{config: config, state: map[string]map[string]any{}, tasks: map[string]*pluginsdk.Task{}}
}

// AddTask makes a task readable through Tasks().Get.
func (h *Host) AddTask(task pluginsdk.Task) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tasks[task.ID] = &task
}

// GetConfig returns the config the host was built with.
func (h *Host) GetConfig(context.Context) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.config, nil
}

func bucket(scope, scopeID string) string { return scope + "/" + scopeID }

// GetState reads one entry.
func (h *Host) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	value, found := h.state[bucket(scope, scopeID)][key]
	if !found {
		return nil, false, nil
	}
	read, err := roundTrip(value.(map[string]any))
	return read, err == nil, err
}

// SetState writes one entry. Host state round-trips through JSON, so this
// does too: an int64 written comes back as float64.
func (h *Host) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.StateErr != nil {
		return h.StateErr
	}
	name := bucket(scope, scopeID)
	if h.state[name] == nil {
		h.state[name] = map[string]any{}
	}
	decoded, err := roundTrip(value)
	if err != nil {
		return err
	}
	h.state[name][key] = decoded
	return nil
}

// roundTrip copies value through JSON, as the host's wire does. A read hands
// out a copy too, so a caller mutating what it read changes nothing stored.
func roundTrip(value map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// DeleteState removes one entry; a missing one is not an error.
func (h *Host) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.StateErr != nil {
		return h.StateErr
	}
	delete(h.state[bucket(scope, scopeID)], key)
	return nil
}

// ListState returns every entry in a scope.
func (h *Host) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.StateErr != nil {
		return nil, h.StateErr
	}
	entries := make([]pluginsdk.StateEntry, 0)
	for key, value := range h.state[bucket(scope, scopeID)] {
		read, err := roundTrip(value.(map[string]any))
		if err != nil {
			return nil, err
		}
		entries = append(entries, pluginsdk.StateEntry{Key: key, Value: read})
	}
	return entries, nil
}

// Tasks serves the tasks added with AddTask.
func (h *Host) Tasks() pluginsdk.TaskReader { return tasks{host: h} }

type tasks struct {
	pluginsdk.TaskReader
	host *Host
}

func (t tasks) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	t.host.mu.Lock()
	defer t.host.mu.Unlock()
	if t.host.TaskErr != nil {
		return nil, t.host.TaskErr
	}
	return t.host.tasks[id], nil
}

// The secret, event and utility agent calls are inert: nothing under test
// depends on them.

func (h *Host) RevealSecret(context.Context, string) (string, error)    { return "", nil }
func (h *Host) GetSecret(context.Context, string) (string, bool, error) { return "", false, nil }
func (h *Host) SetSecret(context.Context, string, string) error         { return nil }
func (h *Host) DeleteSecret(context.Context, string) error              { return nil }
func (h *Host) EmitEvent(context.Context, string, map[string]any) error { return nil }
func (h *Host) InvokeUtilityAgent(context.Context, string, ...pluginsdk.UtilityAgentOptions) (string, error) {
	return "", nil
}

var _ pluginsdk.Host = (*Host)(nil)
