package sourcecontrol

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// associationKeyPrefix namespaces association entries inside the workspace
// state scope so future keys can coexist.
const associationKeyPrefix = "task:"

// Associations stores task-to-pull-request links in Kandev's Host state.
//
// Each link is its own state entry keyed by the full immutable tuple, so two
// links never read-modify-write the same value. The Host state API has no
// compare-and-swap, and a shared per-task list would lose a concurrent link.
type Associations struct {
	hosts kandev.Provider
}

var _ AssociationStore = (*Associations)(nil)

// NewAssociations returns the Host-state-backed association store.
func NewAssociations(hosts kandev.Provider) *Associations {
	return &Associations{hosts: hosts}
}

// Link records an association. It is idempotent: linking the same pull request
// to the same task twice writes the same key and leaves one entry.
func (a *Associations) Link(ctx context.Context, taskID string, identity ChangeRequestIdentity) error {
	host, workspaceID, err := a.resolveWorkspace(ctx, taskID)
	if err != nil {
		return err
	}
	value := map[string]any{
		"task_id":          taskID,
		"connection_scope": identity.ConnectionScope,
		"repository_id":    identity.RepositoryID,
		"number":           identity.Number,
	}
	if err := host.SetState(ctx, "workspace", workspaceID, associationKey(taskID, identity), value); err != nil {
		return fmt.Errorf("forgejo: store task association: %w", err)
	}
	return nil
}

// Unlink removes an association. Removing a missing link is not an error.
func (a *Associations) Unlink(ctx context.Context, taskID string, identity ChangeRequestIdentity) error {
	host, workspaceID, err := a.resolveWorkspace(ctx, taskID)
	if err != nil {
		return err
	}
	if err := host.DeleteState(ctx, "workspace", workspaceID, associationKey(taskID, identity)); err != nil {
		return fmt.Errorf("forgejo: remove task association: %w", err)
	}
	return nil
}

// ListForWorkspace returns every association recorded in a workspace.
func (a *Associations) ListForWorkspace(ctx context.Context, workspaceID string) ([]ChangeRequestIdentity, []string, error) {
	host := a.hosts()
	if host == nil {
		return nil, nil, scm.ErrNotConfigured
	}
	entries, err := host.ListState(ctx, "workspace", workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("forgejo: list task associations: %w", err)
	}
	identities := make([]ChangeRequestIdentity, 0, len(entries))
	taskIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, associationKeyPrefix) {
			continue
		}
		identity, taskID, ok := decodeAssociation(entry.Value)
		if !ok {
			// A corrupt or partially written entry is skipped rather than
			// failing the whole workspace refresh.
			continue
		}
		identities = append(identities, identity)
		taskIDs = append(taskIDs, taskID)
	}
	return identities, taskIDs, nil
}

// ListForTask returns the associations recorded for one task.
func (a *Associations) ListForTask(ctx context.Context, workspaceID, taskID string) ([]ChangeRequestIdentity, error) {
	identities, taskIDs, err := a.ListForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	filtered := make([]ChangeRequestIdentity, 0, len(identities))
	for i := range identities {
		if taskIDs[i] == taskID {
			filtered = append(filtered, identities[i])
		}
	}
	return filtered, nil
}

// resolveWorkspace maps a task to its workspace through the Host data API.
// The recipe's Link/Unlink ports carry only a task id, and associations are
// stored in workspace scope so a workspace refresh can enumerate them.
func (a *Associations) resolveWorkspace(ctx context.Context, taskID string) (pluginsdk.Host, string, error) {
	host := a.hosts()
	if host == nil {
		return nil, "", scm.ErrNotConfigured
	}
	if strings.TrimSpace(taskID) == "" {
		return nil, "", errors.New("forgejo: task id is required")
	}
	task, err := host.Tasks().Get(ctx, taskID)
	if err != nil {
		return nil, "", fmt.Errorf("forgejo: resolve task workspace: %w", err)
	}
	if task == nil || strings.TrimSpace(task.WorkspaceID) == "" {
		return nil, "", fmt.Errorf("forgejo: task %s has no workspace", taskID)
	}
	return host, task.WorkspaceID, nil
}

// associationKey builds the per-link state key from the immutable tuple.
func associationKey(taskID string, identity ChangeRequestIdentity) string {
	return fmt.Sprintf("%s%s:%s:%d", associationKeyPrefix, sanitizeKeySegment(taskID),
		sanitizeKeySegment(identity.RepositoryID), identity.Number)
}

// sanitizeKeySegment keeps state keys within the Host's
// [a-zA-Z0-9][a-zA-Z0-9._-]{0,127} key vocabulary.
func sanitizeKeySegment(value string) string {
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

// decodeAssociation reads a stored entry back into an identity. Host state
// round-trips through JSON, so a number arrives as float64.
func decodeAssociation(value map[string]any) (ChangeRequestIdentity, string, bool) {
	if value == nil {
		return ChangeRequestIdentity{}, "", false
	}
	scope, _ := value["connection_scope"].(string)
	repositoryID, _ := value["repository_id"].(string)
	taskID, _ := value["task_id"].(string)
	number, ok := decodeNumber(value["number"])
	if !ok || strings.TrimSpace(scope) == "" || strings.TrimSpace(repositoryID) == "" || strings.TrimSpace(taskID) == "" {
		return ChangeRequestIdentity{}, "", false
	}
	return ChangeRequestIdentity{
		ConnectionScope: scope,
		RepositoryID:    repositoryID,
		Number:          number,
	}, taskID, true
}

// decodeNumber accepts the numeric encodings Host state can return.
func decodeNumber(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		converted := int64(typed)
		return converted, float64(converted) == typed && converted > 0
	case int64:
		return typed, typed > 0
	case int:
		return int64(typed), typed > 0
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil && parsed > 0
	default:
		return 0, false
	}
}
