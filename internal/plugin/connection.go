package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// connectionActions serve the connection.* actions: the settings panel's
// status, live test, and the two workspace switches.
type connectionActions struct {
	hosts      kandev.Provider
	connection instanceConnection
	switches   workspaceSwitches
	audit      *auditTrail
}

// instanceConnection is the configured instance: what the config says, and
// what a live check learns.
type instanceConnection interface {
	instanceInfo
	Probe(ctx context.Context) (sourcecontrol.ConnectionProbe, error)
}

// connectionStatusCacheKey holds the last probe result per workspace so
// connection.get can answer without hitting the instance on every panel mount.
const connectionStatusCacheKey = "connection_status"

// connectionStatusTTL bounds how long a cached probe result is trusted.
const connectionStatusTTL = 60 * time.Second

// connectionStatus reports whether the plugin is configured and whether the
// instance accepts the configured token.
//
// Both connection.get and connection.test answer from here. get serves a cached
// probe result while it is fresh and probes when it is not, so the panel shows
// real state on mount instead of reporting "not connected" until an operator
// clicks a button. test always probes and refreshes the cache. The response
// never includes the token or any credential-bearing URL.
func (h *connectionActions) connectionStatus(ctx context.Context, workspaceID string, force bool) (*pluginsdk.PluginActionResponse, error) {
	status := map[string]any{"provider": ProviderID, "configured": false, "connected": false}
	if enabled, err := h.switches.Enabled(ctx, workspaceID); err == nil {
		status["enabled"] = enabled
		status["agent_merge"] = h.switches.AgentMerge(ctx, workspaceID)
	}

	info, err := h.connection.Info(ctx)
	if err != nil {
		if errors.Is(err, scm.ErrNotConfigured) {
			status["message"] = "Set the instance URL and access token in Settings > Plugins > Forgejo."
		} else {
			status["message"] = safeMessage(err)
		}
		h.clearConnectionCache(ctx, workspaceID)
		return kandev.JSON(status)
	}
	status["configured"] = true
	status["instance_url"] = info.URL
	status["host"] = info.Host

	if !force {
		if cached, ok := h.cachedConnectionStatus(ctx, workspaceID, info.URL); ok {
			for key, value := range cached {
				status[key] = value
			}
			status["cached"] = true
			return kandev.JSON(status)
		}
	}

	probed, err := h.probeConnection(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		status["message"] = safeMessage(err)
		h.clearConnectionCache(ctx, workspaceID)
		return kandev.JSON(status)
	}
	for key, value := range probed {
		status[key] = value
	}
	h.storeConnectionCache(ctx, workspaceID, info.URL, probed)
	return kandev.JSON(status)
}

// setEnabled records the operator's choice for one workspace.
func (h *connectionActions) setEnabled(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return nil, errors.New("kandev-plugin-forgejo: enabling requires a verified workspace")
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(request.Body, &input); err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: decode enabled body: %w", err)
	}
	if input.Enabled == nil {
		return nil, errors.New("kandev-plugin-forgejo: enabled is required")
	}
	host := h.hosts()
	if host == nil {
		return nil, errors.New("kandev-plugin-forgejo: host is unavailable")
	}
	if err := host.SetState(ctx, "workspace", workspaceID, enabledStateKey, map[string]any{
		"enabled": *input.Enabled,
	}); err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: store enabled state: %w", err)
	}
	return kandev.JSON(map[string]any{"provider": ProviderID, "enabled": *input.Enabled})
}

// setAgentMerge records the operator's choice. It is a workspace-scoped action,
// so the workspace is the verified one and never comes from the body.
func (h *connectionActions) setAgentMerge(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return nil, errors.New("kandev-plugin-forgejo: this setting requires a verified workspace")
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if bad := kandev.DecodeBody(request, &input); bad != nil || input.Enabled == nil {
		return kandev.Refuse(422, "enabled is required.")
	}
	host := h.hosts()
	if host == nil {
		return nil, errors.New("kandev-plugin-forgejo: host is unavailable")
	}
	if err := host.SetState(ctx, "workspace", workspaceID, agentMergeStateKey, map[string]any{"enabled": *input.Enabled}); err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: store agent merge setting: %w", err)
	}
	h.audit.Record(ctx, auditRecord{
		Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: workspaceID,
		Outcome: fmt.Sprintf("agent_merge=%t", *input.Enabled),
	})
	return kandev.JSON(map[string]any{"agent_merge": *input.Enabled})
}

// probeConnection performs the live check: who the token acts as, and what the
// instance is running.
func (h *connectionActions) probeConnection(ctx context.Context) (map[string]any, error) {
	probe, err := h.connection.Probe(ctx)
	if err != nil {
		return nil, err
	}
	probed := map[string]any{"connected": true, "account": probe.Account}
	if probe.Version != "" {
		probed["instance_version"] = probe.Version
		probed["flavor"] = probe.Flavor
	}
	return probed, nil
}

// cachedConnectionStatus returns a probe result that is still fresh and still
// describes the currently configured instance.
func (h *connectionActions) cachedConnectionStatus(ctx context.Context, workspaceID, scope string) (map[string]any, bool) {
	host := h.hosts()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return nil, false
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, connectionStatusCacheKey)
	if err != nil || !found || value == nil {
		return nil, false
	}
	// A cache entry for a different instance is stale by definition: the
	// operator changed base_url since it was written.
	if cachedScope, _ := value["instance_url"].(string); cachedScope != scope {
		return nil, false
	}
	checkedAt, ok := value["checked_at"].(float64)
	if !ok || time.Since(time.UnixMilli(int64(checkedAt))) > connectionStatusTTL {
		return nil, false
	}
	result := map[string]any{"connected": value["connected"] == true}
	for _, key := range []string{"account", "instance_version", "flavor"} {
		if text, ok := value[key].(string); ok && text != "" {
			result[key] = text
		}
	}
	return result, true
}

// storeConnectionCache records a probe result. A failure to cache is not a
// failure to connect, so the error is dropped rather than surfaced.
func (h *connectionActions) storeConnectionCache(ctx context.Context, workspaceID, scope string, probed map[string]any) {
	host := h.hosts()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return
	}
	value := map[string]any{
		"instance_url": scope,
		"checked_at":   time.Now().UnixMilli(),
	}
	for key, item := range probed {
		value[key] = item
	}
	_ = host.SetState(ctx, "workspace", workspaceID, connectionStatusCacheKey, value)
}

// clearConnectionCache drops a cached result once the connection stops working,
// so a later get cannot report a stale success.
func (h *connectionActions) clearConnectionCache(ctx context.Context, workspaceID string) {
	host := h.hosts()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return
	}
	_ = host.DeleteState(ctx, "workspace", workspaceID, connectionStatusCacheKey)
}
