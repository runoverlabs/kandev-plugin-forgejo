package plugin

import (
	"context"
	"fmt"
	"strings"

	"kandev-plugin-forgejo/internal/kandev"
)

// enabledStateKey stores the per-workspace enable/disable choice. Kandev does
// not render an enable toggle for a plugin integration, so the plugin owns
// both the control and its meaning. Absent means enabled: installing the
// plugin is itself the opt-in.
const enabledStateKey = "integration_enabled"

// agentMergeStateKey stores the per-workspace switch that lets agents merge.
// Absent means off: an agent that can merge to the default branch is the one
// capability here whose default has to be "no". Review, comment and the other
// writes stay available, because they can be undone or ignored.
const agentMergeStateKey = "agent_merge_enabled"

// workspaceSwitches are the operator's per-workspace choices: whether the
// integration is on, and whether agents may merge. Both live in Host state.
type workspaceSwitches struct {
	hosts kandev.Provider
}

// Enabled reports whether this integration is on for a workspace.
// It fails open: a state read problem must not silently disable a working
// integration.
func (s workspaceSwitches) Enabled(ctx context.Context, workspaceID string) (bool, error) {
	enabled, err := s.EnabledStrict(ctx, workspaceID)
	if err != nil && ctx.Err() == nil {
		return true, nil
	}
	return enabled, err
}

// EnabledStrict is Enabled without the fail-open: a
// state read error is returned. Automations use it, because starting an agent
// run in a workspace whose operator turned the integration off is worse than
// failing the delivery.
func (s workspaceSwitches) EnabledStrict(ctx context.Context, workspaceID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	host := s.hosts()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return true, nil
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, enabledStateKey)
	if err != nil {
		return false, fmt.Errorf("kandev-plugin-forgejo: read enabled state: %w", err)
	}
	if !found || value == nil {
		return true, nil
	}
	enabled, ok := value["enabled"].(bool)
	return !ok || enabled, nil
}

// AgentMerge reports whether agents may merge in a workspace. Unlike
// Enabled it fails closed: if the state cannot be read, merging does
// not happen.
func (s workspaceSwitches) AgentMerge(ctx context.Context, workspaceID string) bool {
	host := s.hosts()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return false
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, agentMergeStateKey)
	if err != nil || !found || value == nil {
		return false
	}
	enabled, _ := value["enabled"].(bool)
	return enabled
}
