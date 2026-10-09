package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// The audit trail answers one question the Forgejo side cannot: every write
// here acts as the operator's shared account, so Forgejo shows "kandev" whoever
// clicked. Each write records which Kandev actor did what, to which pull request
// number, and how it ended. It never holds a body, a title or a token.
//
// It is kept in Host state, not only logged: on Kandev v0.97.0 a plugin's
// stderr is dropped before it reaches the backend log (go-plugin skips parsing
// when the host's logger reports itself disabled), so a log line alone would
// leave the trail unreadable exactly when it is needed.
const (
	auditKeyPrefix = "audit:"
	// auditKeep bounds the trail per workspace; the oldest entries go first.
	auditKeep = 100
	// auditShown is how many entries connection.audit returns.
	auditShown = 50
	// ActionConnectionAudit lists the most recent writes in the workspace.
	ActionConnectionAudit = "connection.audit"
)

// auditRecord is one write.
type auditRecord struct {
	Action    string
	Actor     string
	Workspace string
	Task      string
	Number    int64
	Outcome   string
}

var auditMu sync.Mutex

// audit records a write. It never fails the write: a trail that cannot be
// stored is reported on stderr and otherwise ignored.
func (r *Runtime) audit(ctx context.Context, record auditRecord) {
	now := time.Now().UTC()
	if line, err := json.Marshal(map[string]any{
		"@level": "info", "@message": "pull request write", "@timestamp": now.Format(time.RFC3339Nano),
		"action": record.Action, "actor": record.Actor, "workspace": record.Workspace,
		"task": record.Task, "number": record.Number, "outcome": record.Outcome,
	}); err == nil {
		fmt.Fprintln(os.Stderr, string(line))
	}

	host := r.Host()
	if host == nil || strings.TrimSpace(record.Workspace) == "" {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	key := fmt.Sprintf("%s%020d", auditKeyPrefix, now.UnixNano())
	if err := host.SetState(ctx, "workspace", record.Workspace, key, map[string]any{
		"at": now.Format(time.RFC3339Nano), "action": record.Action, "actor": record.Actor,
		"task": record.Task, "number": record.Number, "outcome": record.Outcome,
	}); err != nil {
		return
	}
	entries, err := host.ListState(ctx, "workspace", record.Workspace)
	if err != nil {
		return
	}
	keys := auditKeys(entries)
	for len(keys) > auditKeep {
		_ = host.DeleteState(ctx, "workspace", record.Workspace, keys[0])
		keys = keys[1:]
	}
}

// auditKeys returns the audit keys oldest first.
func auditKeys(entries []pluginsdk.StateEntry) []string {
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Key, auditKeyPrefix) {
			keys = append(keys, entry.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// listAudit serves connection.audit: the most recent writes, newest first.
func (r *Runtime) listAudit(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	host := r.Host()
	if workspaceID == "" || host == nil {
		return jsonResponse(map[string]any{"entries": []any{}})
	}
	entries, err := host.ListState(ctx, "workspace", workspaceID)
	if err != nil {
		return domainResponse(502, "The audit trail could not be read.")
	}
	byKey := make(map[string]map[string]any, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = entry.Value
	}
	keys := auditKeys(entries)
	shown := make([]any, 0, auditShown)
	for i := len(keys) - 1; i >= 0 && len(shown) < auditShown; i-- {
		shown = append(shown, byKey[keys[i]])
	}
	return jsonResponse(map[string]any{"entries": shown})
}
