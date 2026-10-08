package watches

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// State keys live in the workspace scope alongside the plugin's existing
// association and connection-cache entries, so every prefix here must stay
// distinct from those ("task:", "integration_enabled", "connection_status").
//
// Keys are restricted to the Host's [a-zA-Z0-9][a-zA-Z0-9._-]{0,127}
// vocabulary. A repository's owner/name can be 200 characters on its own, so
// the repository component of a dedup key is a short digest rather than the
// name itself; the human-readable name is carried in the entry's value, which
// has no such limit.
const (
	watchKeyPrefix = "watch."
	// watchTaskKeyPrefix records "this watch already made a task for this
	// issue" (DedupScopeWatch).
	watchTaskKeyPrefix = "wt."
	// issueTaskKeyPrefix records "this workspace already made a task for this
	// issue", regardless of which watch did it (DedupScopeWorkspace).
	issueTaskKeyPrefix = "wi."
)

// stateScope is the Host state scope every watch record lives in. Watches are
// workspace-scoped because that is the boundary kandev enforces on the actions
// that manage them, and because a workspace-scoped ListState is the only way
// to enumerate them.
const stateScope = "workspace"

// HostProvider defers Host resolution to call time. The Host is injected from
// a background goroutine after construction, so nothing may capture it
// eagerly.
type HostProvider func() pluginsdk.Host

// ErrNoHost means the plugin has not finished connecting back to kandev. It is
// transient: the broker dial happens once, shortly after start.
var ErrNoHost = errors.New("watches: kandev host is not available yet")

// ErrNotFound means no watch exists with the requested id in that workspace.
var ErrNotFound = errors.New("watches: watch not found")

// Store persists watches and the record of which issues already became tasks,
// in kandev's Host state.
//
// Every record is its own state entry keyed by an immutable tuple. The Host
// state API has no compare-and-swap, so a shared list value would lose a
// concurrent write — the same reason the association store is shaped this way.
type Store struct {
	hosts HostProvider
	now   func() time.Time
}

// NewStore returns a Host-state-backed store.
func NewStore(hosts HostProvider) *Store {
	return &Store{hosts: hosts, now: time.Now}
}

// TaskRecord is the ledger entry proving one issue already became one task.
// It is what makes a re-poll idempotent, and it is the single most important
// piece of state this feature keeps: losing it means duplicate cards.
type TaskRecord struct {
	WatchID  string `json:"watch_id"`
	Repo     string `json:"repo"`
	Number   int64  `json:"number"`
	IssueURL string `json:"issue_url"`
	TaskID   string `json:"task_id"`
	// Key is the state key this record was read from. It is not persisted; the
	// store fills it so a caller can delete the entry without rebuilding the
	// key from a scope it may not know.
	Key       string `json:"-"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) host() (pluginsdk.Host, error) {
	if s.hosts == nil {
		return nil, ErrNoHost
	}
	host := s.hosts()
	if host == nil {
		return nil, ErrNoHost
	}
	return host, nil
}

// NewID returns an opaque identifier for a new watch. It is hex so it is
// always a legal state key component.
func NewID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("watches: generate id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// List returns every watch in a workspace, ordered by name so the panel and
// the poller both see a stable sequence. Host state has no ordering of its
// own.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Watch, error) {
	host, err := s.host()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspaceID) == "" {
		return nil, errors.New("watches: a workspace is required")
	}
	entries, err := host.ListState(ctx, stateScope, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("watches: list state: %w", err)
	}
	found := make([]Watch, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, watchKeyPrefix) {
			continue
		}
		watch, ok := decodeWatch(entry.Value)
		if !ok {
			// A corrupt entry is skipped rather than failing the whole
			// workspace: one bad record must not take the panel down.
			continue
		}
		watch.WorkspaceID = workspaceID
		found = append(found, watch)
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Name != found[j].Name {
			return found[i].Name < found[j].Name
		}
		return found[i].ID < found[j].ID
	})
	return found, nil
}

// Get returns one watch.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (Watch, error) {
	host, err := s.host()
	if err != nil {
		return Watch{}, err
	}
	value, found, err := host.GetState(ctx, stateScope, workspaceID, watchKey(id))
	if err != nil {
		return Watch{}, fmt.Errorf("watches: read watch: %w", err)
	}
	if !found {
		return Watch{}, ErrNotFound
	}
	watch, ok := decodeWatch(value)
	if !ok {
		return Watch{}, fmt.Errorf("watches: watch %s is unreadable", id)
	}
	watch.WorkspaceID = workspaceID
	return watch, nil
}

// Put writes a watch, normalizing and validating it first. It stamps
// CreatedAt on first write and UpdatedAt on every write.
func (s *Store) Put(ctx context.Context, watch Watch) (Watch, error) {
	host, err := s.host()
	if err != nil {
		return Watch{}, err
	}
	watch.Normalize()
	if err := watch.Validate(); err != nil {
		return Watch{}, err
	}
	if strings.TrimSpace(watch.ID) == "" {
		return Watch{}, errors.New("watches: an id is required")
	}
	stamp := s.now().UTC().Format(time.RFC3339)
	if strings.TrimSpace(watch.CreatedAt) == "" {
		watch.CreatedAt = stamp
	}
	watch.UpdatedAt = stamp

	value, err := encodeWatch(watch)
	if err != nil {
		return Watch{}, err
	}
	if err := host.SetState(ctx, stateScope, watch.WorkspaceID, watchKey(watch.ID), value); err != nil {
		return Watch{}, fmt.Errorf("watches: store watch: %w", err)
	}
	return watch, nil
}

// Delete removes a watch and every dedup record it owns.
//
// The records go too because they are meaningless without the watch, and
// leaving them would silently suppress a later watch over the same issues if
// the operator re-created one with the same id. Records under the workspace
// dedup scope are NOT removed: they belong to the workspace, not to this
// watch, and another watch may still be relying on them.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	host, err := s.host()
	if err != nil {
		return err
	}
	records, err := s.records(ctx, workspaceID, watchTaskKeyPrefix+sanitize(id)+".")
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := host.DeleteState(ctx, stateScope, workspaceID, record.Key); err != nil {
			return fmt.Errorf("watches: remove watch record: %w", err)
		}
	}
	if err := host.DeleteState(ctx, stateScope, workspaceID, watchKey(id)); err != nil {
		return fmt.Errorf("watches: remove watch: %w", err)
	}
	return nil
}

// Seen reports whether an issue already produced a task under this watch's
// dedup scope, and which task it was.
func (s *Store) Seen(ctx context.Context, watch Watch, repo RepoRef, number int64) (TaskRecord, bool, error) {
	host, err := s.host()
	if err != nil {
		return TaskRecord{}, false, err
	}
	value, found, err := host.GetState(ctx, stateScope, watch.WorkspaceID, dedupKey(watch, repo, number))
	if err != nil {
		return TaskRecord{}, false, fmt.Errorf("watches: read task record: %w", err)
	}
	if !found {
		return TaskRecord{}, false, nil
	}
	record, ok := decodeRecord(value)
	if !ok {
		return TaskRecord{}, false, nil
	}
	return record, true, nil
}

// Record writes the ledger entry for an issue that just became a task.
//
// It is written AFTER the task is created. The alternative — reserving the key
// first — would avoid a duplicate on a crash between create and record, but
// would permanently suppress the issue if task creation then failed, which is
// the worse failure: a duplicate card is visible and fixable, a silently
// skipped issue is neither.
func (s *Store) Record(ctx context.Context, watch Watch, repo RepoRef, number int64, issueURL, taskID string) error {
	host, err := s.host()
	if err != nil {
		return err
	}
	record := TaskRecord{
		WatchID:   watch.ID,
		Repo:      repo.FullName(),
		Number:    number,
		IssueURL:  issueURL,
		TaskID:    taskID,
		CreatedAt: s.now().UTC().Format(time.RFC3339),
	}
	value, err := encodeRecord(record)
	if err != nil {
		return err
	}
	if err := host.SetState(ctx, stateScope, watch.WorkspaceID, dedupKey(watch, repo, number), value); err != nil {
		return fmt.Errorf("watches: store task record: %w", err)
	}
	return nil
}

// Records returns every ledger entry a watch owns, under whichever dedup scope
// it is configured for. It backs both the inflight count and the panel's
// "issues picked up" list.
func (s *Store) Records(ctx context.Context, watch Watch) ([]TaskRecord, error) {
	prefix := watchTaskKeyPrefix + sanitize(watch.ID) + "."
	if watch.DedupScope == DedupScopeWorkspace {
		prefix = issueTaskKeyPrefix
	}
	records, err := s.records(ctx, watch.WorkspaceID, prefix)
	if err != nil {
		return nil, err
	}
	if watch.DedupScope != DedupScopeWorkspace {
		return records, nil
	}
	// Workspace-scoped entries are shared, so attribute them back to this
	// watch before counting them against its inflight budget.
	owned := make([]TaskRecord, 0, len(records))
	for _, record := range records {
		if record.WatchID == watch.ID {
			owned = append(owned, record)
		}
	}
	return owned, nil
}

// records enumerates ledger entries under one key prefix.
func (s *Store) records(ctx context.Context, workspaceID, prefix string) ([]TaskRecord, error) {
	host, err := s.host()
	if err != nil {
		return nil, err
	}
	entries, err := host.ListState(ctx, stateScope, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("watches: list task records: %w", err)
	}
	records := make([]TaskRecord, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, prefix) {
			continue
		}
		record, ok := decodeRecord(entry.Value)
		if !ok {
			continue
		}
		record.Key = entry.Key
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records, nil
}

// Forget removes a watch's ledger entries without removing the watch, so the
// next poll re-creates tasks for issues it already handled. It backs the
// panel's reset control.
func (s *Store) Forget(ctx context.Context, watch Watch) (int, error) {
	host, err := s.host()
	if err != nil {
		return 0, err
	}
	records, err := s.Records(ctx, watch)
	if err != nil {
		return 0, err
	}
	for _, record := range records {
		if err := host.DeleteState(ctx, stateScope, watch.WorkspaceID, record.Key); err != nil {
			return 0, fmt.Errorf("watches: remove task record: %w", err)
		}
	}
	return len(records), nil
}

// MarkPolled records a successful poll and clears any previous error.
func (s *Store) MarkPolled(ctx context.Context, watch Watch) error {
	watch.LastPolledAt = s.now().UTC().Format(time.RFC3339)
	watch.LastError, watch.LastErrorAt = "", ""
	_, err := s.Put(ctx, watch)
	return err
}

// MarkError records a failed poll. The poll time is advanced too: a watch
// against an unreachable instance must back off to its normal interval rather
// than retry every tick.
func (s *Store) MarkError(ctx context.Context, watch Watch, message string) error {
	stamp := s.now().UTC().Format(time.RFC3339)
	watch.LastPolledAt = stamp
	watch.LastError = message
	watch.LastErrorAt = stamp
	_, err := s.Put(ctx, watch)
	return err
}

// watchKey is the state key holding one watch's configuration.
func watchKey(id string) string { return watchKeyPrefix + sanitize(id) }

// dedupKey is the ledger key for one issue under a watch's dedup scope.
func dedupKey(watch Watch, repo RepoRef, number int64) string {
	issue := repoDigest(repo) + "." + strconv.FormatInt(number, 10)
	if watch.DedupScope == DedupScopeWorkspace {
		return issueTaskKeyPrefix + issue
	}
	return watchTaskKeyPrefix + sanitize(watch.ID) + "." + issue
}

// repoDigest reduces "owner/name" to a short, stable, key-legal component.
// Repository names are long enough that including them raw would breach the
// Host's 128-character key limit once combined with a watch id.
func repoDigest(repo RepoRef) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repo.FullName())))
	return hex.EncodeToString(sum[:6])
}

// sanitize keeps a key component inside the Host's key vocabulary.
func sanitize(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	return builder.String()
}

// Host state round-trips through JSON, so the model is marshalled into a
// map rather than assembled field by field. That keeps the stored shape and
// the wire shape identical and removes a class of "field added to the struct,
// forgotten in the encoder" bug.

func encodeWatch(watch Watch) (map[string]any, error) {
	return toMap(watch, "watch")
}

func decodeWatch(value map[string]any) (Watch, bool) {
	var watch Watch
	if !fromMap(value, &watch) || strings.TrimSpace(watch.ID) == "" {
		return Watch{}, false
	}
	return watch, true
}

func encodeRecord(record TaskRecord) (map[string]any, error) {
	return toMap(record, "task record")
}

func decodeRecord(value map[string]any) (TaskRecord, bool) {
	var record TaskRecord
	if !fromMap(value, &record) || strings.TrimSpace(record.TaskID) == "" {
		return TaskRecord{}, false
	}
	return record, true
}

func toMap(value any, what string) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("watches: encode %s: %w", what, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, fmt.Errorf("watches: encode %s: %w", what, err)
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
