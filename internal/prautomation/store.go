package prautomation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"kandev-plugin-forgejo/internal/kandev"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// State keys share the workspace scope with the plugin's associations
// ("task:"), audit ("audit:"), watches ("watch.", "rwatch.", "wt.", "wi.")
// and single keys; "pra." and "pra-settings" collide with none of them.
const (
	recordKeyPrefix = "pra."
	settingsKey     = "pra-settings"
	stateScope      = "workspace"
)

// StateHost is the slice of the Host this package stores through;
// pluginsdk.Host satisfies it.
type StateHost interface {
	GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error)
	SetState(ctx context.Context, scope, scopeID, key string, value map[string]any) error
	DeleteState(ctx context.Context, scope, scopeID, key string) error
	ListState(ctx context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error)
}

// Store persists records and settings in Kandev's Host state. Each pull
// request is its own entry, so concurrent writers never share a value.
type Store struct {
	hosts func() StateHost
}

// NewStore returns a Host-state-backed store.
func NewStore(hosts func() StateHost) *Store {
	return &Store{hosts: hosts}
}

func (s *Store) host() (StateHost, error) {
	if s.hosts == nil {
		return nil, kandev.ErrNoHost
	}
	host := s.hosts()
	if host == nil {
		return nil, kandev.ErrNoHost
	}
	return host, nil
}

// recordKey hashes the reference: a task id and a repository id together can
// exceed the Host's 128-character key limit, and truncating could collide.
func recordKey(ref PullRef) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", ref.TaskID, ref.RepositoryID, ref.Number)))
	return recordKeyPrefix + hex.EncodeToString(sum[:16])
}

// Get loads one record.
func (s *Store) Get(ctx context.Context, workspaceID string, ref PullRef) (Record, bool, error) {
	host, err := s.host()
	if err != nil {
		return Record{}, false, err
	}
	value, found, err := host.GetState(ctx, stateScope, workspaceID, recordKey(ref))
	if err != nil || !found {
		return Record{}, false, err
	}
	var record Record
	if !fromMap(value, &record) {
		return Record{}, false, nil
	}
	return record, true, nil
}

// Put stores one record.
func (s *Store) Put(ctx context.Context, workspaceID string, record Record) error {
	host, err := s.host()
	if err != nil {
		return err
	}
	value, err := toMap(record)
	if err != nil {
		return err
	}
	return host.SetState(ctx, stateScope, workspaceID, recordKey(record.Ref()), value)
}

// Delete removes a record. A missing one is not an error.
func (s *Store) Delete(ctx context.Context, workspaceID string, ref PullRef) error {
	host, err := s.host()
	if err != nil {
		return err
	}
	return host.DeleteState(ctx, stateScope, workspaceID, recordKey(ref))
}

// List returns every record in a workspace. A corrupt entry is skipped.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Record, error) {
	host, err := s.host()
	if err != nil {
		return nil, err
	}
	entries, err := host.ListState(ctx, stateScope, workspaceID)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, recordKeyPrefix) {
			continue
		}
		var record Record
		if fromMap(entry.Value, &record) && record.TaskID != "" {
			records = append(records, record)
		}
	}
	return records, nil
}

// Settings loads the workspace's settings, normalized.
func (s *Store) Settings(ctx context.Context, workspaceID string) (Settings, error) {
	host, err := s.host()
	if err != nil {
		return Settings{}, err
	}
	value, found, err := host.GetState(ctx, stateScope, workspaceID, settingsKey)
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if found {
		fromMap(value, &settings)
	}
	return settings.Normalize(), nil
}

// PutSettings stores the workspace's settings.
func (s *Store) PutSettings(ctx context.Context, workspaceID string, settings Settings) error {
	host, err := s.host()
	if err != nil {
		return err
	}
	value, err := toMap(settings)
	if err != nil {
		return err
	}
	return host.SetState(ctx, stateScope, workspaceID, settingsKey, value)
}

// toMap and fromMap round-trip through JSON, which is also what the Host
// does: an int written comes back as float64, and decoding into the struct
// absorbs that.
func toMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("prautomation: encode: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, fmt.Errorf("prautomation: encode: %w", err)
	}
	return decoded, nil
}

func fromMap(value map[string]any, out any) bool {
	if value == nil {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return json.Unmarshal(encoded, out) == nil
}
