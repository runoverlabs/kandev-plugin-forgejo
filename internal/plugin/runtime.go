// Package plugin wires the concrete Forgejo adapters into the provider-neutral
// source-control extension and exposes the value pluginsdk.Serve runs.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/sourcecontrol"
	"kandev-plugin-forgejo/internal/watches"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	// ProviderID is the repository provider this plugin owns. It must match
	// manifest repository_providers and the id the UI bundle registers.
	ProviderID = "forgejo"

	// ReferenceSource must match the manifest reference_sources entry exactly.
	ReferenceSource = "forgejo_pull_requests"

	// ActionConnectionGet reports connection status to the settings surface.
	ActionConnectionGet = "connection.get"
	// ActionConnectionTest validates the configured URL and token live.
	ActionConnectionTest = "connection.test"
	// ActionConnectionSetEnabled records the operator's per-workspace
	// enable/disable choice for this integration.
	ActionConnectionSetEnabled = "connection.set_enabled"
	// ActionConnectionSetAgentMerge records whether agents may merge in a
	// workspace. Off unless an operator turns it on.
	ActionConnectionSetAgentMerge = "connection.set_agent_merge"
)

// enabledStateKey stores the per-workspace enable/disable choice. Kandev does
// not render an enable toggle for a plugin integration, so the plugin owns
// both the control and its meaning. Absent means enabled: installing the
// plugin is itself the opt-in.
const enabledStateKey = "integration_enabled"

// Runtime is the plugin value passed to pluginsdk.Serve. It embeds
// UnimplementedPlugin for no-op event/webhook defaults and delegates the
// source-control action, search, and authorize RPCs to the recipe extension.
type Runtime struct {
	pluginsdk.UnimplementedPlugin
	extension  *sourcecontrol.Extension
	connection *forgejo.Connection
	// The adapters below are also reachable through extension, but the agent
	// tools call them directly: an agent tool has a task, not the
	// VerifiedActionContext the recipe's action path is built around.
	repositories   *forgejo.Repositories
	changeRequests *forgejo.ChangeRequests
	associations   *forgejo.Associations
	gitCredentials *forgejo.GitCredentials
	pullActions    *forgejo.PullActions

	// Issue watches. The store is reachable from the action handlers; the
	// poller owns the background goroutine and is started once the Host
	// arrives (see SetHost).
	watchStore  *watches.Store
	watchPoller *watches.Poller
}

var (
	_ pluginsdk.Plugin                    = (*Runtime)(nil)
	_ pluginsdk.ActionHandler             = (*Runtime)(nil)
	_ pluginsdk.EntityReferenceSearcher   = (*Runtime)(nil)
	_ pluginsdk.EntityReferenceAuthorizer = (*Runtime)(nil)
	_ pluginsdk.GitCredentialHandler      = (*Runtime)(nil)
)

// NewRuntime builds the runtime and its adapter graph. The Host is injected
// after construction, so every adapter resolves it lazily through the provider
// closure rather than capturing it here.
func NewRuntime() *Runtime {
	runtime := &Runtime{}
	hosts := forgejo.HostProvider(func() pluginsdk.Host { return runtime.Host() })

	connection := forgejo.NewConnection(hosts)
	repositories := forgejo.NewRepositories(connection, hosts, ProviderID)
	associations := forgejo.NewAssociations(hosts)

	changeRequests := forgejo.NewChangeRequests(connection, repositories)

	runtime.connection = connection
	runtime.repositories = repositories
	runtime.changeRequests = changeRequests
	runtime.associations = associations
	runtime.gitCredentials = forgejo.NewGitCredentials(connection)
	runtime.pullActions = forgejo.NewPullActions(connection, repositories, associations, ProviderID)

	runtime.watchStore = watches.NewStore(watches.HostProvider(hosts))
	runtime.watchPoller = watches.NewPoller(
		runtime.watchStore,
		watches.HostProvider(hosts),
		forgejo.NewIssueSource(connection),
		runtime.integrationEnabled,
	).WithReviews(forgejo.NewReviewSource(connection))
	runtime.extension = &sourcecontrol.Extension{
		ProviderID:           ProviderID,
		ReferenceSource:      ReferenceSource,
		Repositories:         repositories,
		RepositoryDetails:    repositories,
		AttachedRepositories: repositories,
		ChangeRequests:       changeRequests,
		Associations:         associations,
		Reviews:              forgejo.NewReviews(connection, repositories, associations, ProviderID),
		References:           forgejo.NewReferences(connection),
	}
	return runtime
}

// HandleAction routes the connection actions this plugin owns and delegates
// every source-control action to the recipe extension.
func (r *Runtime) HandleAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if request == nil {
		return nil, errors.New("kandev-plugin-forgejo: action request is required")
	}
	switch request.ActionKey {
	case ActionConnectionGet:
		return r.connectionStatus(ctx, request.Context.WorkspaceID, false)
	case ActionConnectionTest:
		return r.connectionStatus(ctx, request.Context.WorkspaceID, true)
	case ActionConnectionSetEnabled:
		return r.setEnabled(ctx, request)
	case ActionConnectionSetAgentMerge:
		return r.setAgentMerge(ctx, request)
	case ActionConnectionAudit:
		return r.listAudit(ctx, request)
	case sourcecontrol.ActionChangeRequestsDetails, sourcecontrol.ActionChangeRequestsMerge,
		sourcecontrol.ActionChangeRequestsReview, sourcecontrol.ActionChangeRequestsRequestReviewers,
		sourcecontrol.ActionChangeRequestsUpdateBranch, sourcecontrol.ActionChangeRequestsComment:
		return r.handlePullAction(ctx, request)
	case ActionWatchesList, ActionWatchesOptions, ActionWatchesCreate, ActionWatchesUpdate,
		ActionWatchesDelete, ActionWatchesRun, ActionWatchesReset, ActionWatchesCleanup:
		// Watch management stays reachable while the integration is off, so an
		// operator can see and edit what will resume when they turn it back on.
		// The poller and the manual run both refuse separately, which is where
		// the toggle has to bite.
		return r.handleWatchAction(ctx, request)
	default:
		// A disabled integration contributes nothing to its workspace. The
		// toggle would otherwise be decorative: it would move a badge while
		// repositories, reviews and references kept flowing.
		enabled, err := r.integrationEnabled(ctx, request.Context.WorkspaceID)
		if err != nil {
			return nil, err
		}
		if !enabled {
			return disabledResponse(request.ActionKey)
		}
		return r.extension.HandleAction(ctx, request)
	}
}

// SetHost receives the Host once kandev's broker connection is established and
// starts the watch poller.
//
// This is the only lifecycle hook the SDK offers: Serve dials the host from a
// background goroutine after the plugin is already serving, so there is no
// earlier point at which a poll could reach kandev. Start is idempotent, so a
// re-injection does not leave two loops competing for the same watches.
func (r *Runtime) SetHost(host pluginsdk.Host) {
	r.UnimplementedPlugin.SetHost(host)
	if r.watchPoller != nil {
		r.watchPoller.Start()
	}
}

// Stop halts background work. pluginsdk.Serve does not call this — kandev
// terminates the subprocess — but a test that starts a Runtime needs a way to
// leave no goroutine behind.
func (r *Runtime) Stop() {
	if r.watchPoller != nil {
		r.watchPoller.Stop()
	}
}

// SearchEntityReferences delegates the composer `#` search.
func (r *Runtime) SearchEntityReferences(ctx context.Context, request *pluginsdk.SearchEntityReferencesRequest) (*pluginsdk.SearchEntityReferencesResponse, error) {
	return r.extension.SearchEntityReferences(ctx, request)
}

// AuthorizeEntityReference delegates live reference authorization.
func (r *Runtime) AuthorizeEntityReference(ctx context.Context, request *pluginsdk.AuthorizeEntityReferenceRequest) (*pluginsdk.AuthorizeEntityReferenceResponse, error) {
	return r.extension.AuthorizeEntityReference(ctx, request)
}

// errGitCredentialWithheld is returned when this plugin does not own the
// provider or the workspace has the integration switched off.
var errGitCredentialWithheld = errors.New("kandev-plugin-forgejo: git credential withheld for this provider or workspace")

// ResolveGitCredential supplies HTTPS clone and push credentials for a
// repository on the configured instance.
func (r *Runtime) ResolveGitCredential(ctx context.Context, request *pluginsdk.ResolveGitCredentialRequest) (*pluginsdk.ResolveGitCredentialResponse, error) {
	if request == nil {
		return nil, errGitCredentialWithheld
	}
	if err := r.gitCredentialAllowed(ctx, request.ProviderID, request.WorkspaceID); err != nil {
		return nil, err
	}
	return r.gitCredentials.Resolve(ctx, request)
}

// GetGitCredentialBinding reports the non-secret credential revision Kandev
// uses to revoke helper leases after rotation or disconnect.
func (r *Runtime) GetGitCredentialBinding(ctx context.Context, request *pluginsdk.GitCredentialBindingRequest) (*pluginsdk.GitCredentialBindingResponse, error) {
	if request == nil {
		return nil, errGitCredentialWithheld
	}
	if err := r.gitCredentialAllowed(ctx, request.ProviderID, request.WorkspaceID); err != nil {
		return nil, err
	}
	return r.gitCredentials.Binding(ctx, request)
}

func (r *Runtime) gitCredentialAllowed(ctx context.Context, providerID, workspaceID string) error {
	if !strings.EqualFold(strings.TrimSpace(providerID), ProviderID) {
		return errGitCredentialWithheld
	}
	enabled, err := r.integrationEnabled(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !enabled {
		return errGitCredentialWithheld
	}
	return nil
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
func (r *Runtime) connectionStatus(ctx context.Context, workspaceID string, force bool) (*pluginsdk.PluginActionResponse, error) {
	status := map[string]any{"provider": ProviderID, "configured": false, "connected": false}
	if enabled, err := r.integrationEnabled(ctx, workspaceID); err == nil {
		status["enabled"] = enabled
		status["agent_merge"] = r.agentMergeEnabled(ctx, workspaceID)
	}

	client, err := r.connection.Client(ctx)
	if err != nil {
		if errors.Is(err, forgejo.ErrNotConfigured) {
			status["message"] = "Set the instance URL and access token in Settings > Plugins > Forgejo."
		} else {
			status["message"] = safeMessage(err)
		}
		r.clearConnectionCache(ctx, workspaceID)
		return jsonResponse(status)
	}
	status["configured"] = true
	status["instance_url"] = client.Scope()
	status["host"] = client.Host()

	if !force {
		if cached, ok := r.cachedConnectionStatus(ctx, workspaceID, client.Scope()); ok {
			for key, value := range cached {
				status[key] = value
			}
			status["cached"] = true
			return jsonResponse(status)
		}
	}

	probed, err := r.probeConnection(ctx, client)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		status["message"] = safeMessage(err)
		r.clearConnectionCache(ctx, workspaceID)
		return jsonResponse(status)
	}
	for key, value := range probed {
		status[key] = value
	}
	r.storeConnectionCache(ctx, workspaceID, client.Scope(), probed)
	return jsonResponse(status)
}

// setEnabled records the operator's choice for one workspace.
func (r *Runtime) setEnabled(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
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
	host := r.Host()
	if host == nil {
		return nil, errors.New("kandev-plugin-forgejo: host is unavailable")
	}
	if err := host.SetState(ctx, "workspace", workspaceID, enabledStateKey, map[string]any{
		"enabled": *input.Enabled,
	}); err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: store enabled state: %w", err)
	}
	return jsonResponse(map[string]any{"provider": ProviderID, "enabled": *input.Enabled})
}

// integrationEnabled reports whether this integration is on for a workspace.
// It fails open: a state read problem must not silently disable a working
// integration.
func (r *Runtime) integrationEnabled(ctx context.Context, workspaceID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	host := r.Host()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return true, nil
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, enabledStateKey)
	if err != nil || !found || value == nil {
		return true, nil
	}
	enabled, ok := value["enabled"].(bool)
	return !ok || enabled, nil
}

// disabledResponse answers a source-control action for a workspace where the
// operator turned this integration off. Reads return nothing so native
// surfaces render empty rather than erroring; mutations refuse, because
// silently dropping a create or a link would be worse than saying no.
func disabledResponse(actionKey string) (*pluginsdk.PluginActionResponse, error) {
	switch actionKey {
	case sourcecontrol.ActionRepositoriesList:
		return jsonResponse(map[string]any{"repositories": []any{}})
	case sourcecontrol.ActionRepositoriesInspect:
		return jsonResponse(map[string]any{"repository": nil})
	case sourcecontrol.ActionRepositoriesBranches:
		return jsonResponse(map[string]any{"branches": []any{}})
	case sourcecontrol.ActionChangeRequestsGet:
		return jsonResponse(map[string]any{"reviews": []any{}})
	case sourcecontrol.ActionChangeRequestAssociations:
		return jsonResponse(map[string]any{"associations": []any{}})
	default:
		return nil, errors.New("kandev-plugin-forgejo: the Forgejo integration is turned off for this workspace")
	}
}

// probeConnection performs the live check: who the token acts as, and what the
// instance is running.
func (r *Runtime) probeConnection(ctx context.Context, client *forgejo.Client) (map[string]any, error) {
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	probed := map[string]any{"connected": true, "account": user.Login}
	if version, err := client.Version(ctx); err == nil && strings.TrimSpace(version) != "" {
		probed["instance_version"] = version
		probed["flavor"] = string(client.DetectFlavor(ctx, version))
	}
	return probed, nil
}

// cachedConnectionStatus returns a probe result that is still fresh and still
// describes the currently configured instance.
func (r *Runtime) cachedConnectionStatus(ctx context.Context, workspaceID, scope string) (map[string]any, bool) {
	host := r.Host()
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
func (r *Runtime) storeConnectionCache(ctx context.Context, workspaceID, scope string, probed map[string]any) {
	host := r.Host()
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
func (r *Runtime) clearConnectionCache(ctx context.Context, workspaceID string) {
	host := r.Host()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return
	}
	_ = host.DeleteState(ctx, "workspace", workspaceID, connectionStatusCacheKey)
}

// safeMessage maps an adapter error onto an operator-facing message. Provider
// error bodies are never forwarded: on some deployments they echo the token.
func safeMessage(err error) string {
	var writeErr *forgejo.WriteError
	switch {
	case errors.As(err, &writeErr):
		return writeReasonMessage(writeErr.Reason)
	case errors.Is(err, forgejo.ErrUnauthorized):
		return "The instance rejected the access token. Check that it is valid and has repository scope."
	case errors.Is(err, forgejo.ErrNotFound):
		return "The instance did not expose the expected REST v1 API at this URL."
	case errors.Is(err, forgejo.ErrNotConfigured):
		return "Set the instance URL and access token in Settings > Plugins > Forgejo."
	default:
		return "Could not reach the configured instance."
	}
}

// writeReasonMessage maps a classified write failure onto a fixed message. A
// 403 on a write is a permission or protection problem, never a bad token.
func writeReasonMessage(reason forgejo.Reason) string {
	switch reason {
	case forgejo.ReasonNotMergeable:
		return "The pull request cannot be merged right now."
	case forgejo.ReasonChecking:
		return "The instance is still checking whether this can be merged. Try again in a moment."
	case forgejo.ReasonConflict:
		return "The pull request has merge conflicts."
	case forgejo.ReasonOutOfDate:
		return "The branch is out of date with its base. Update it first."
	case forgejo.ReasonHeadChanged:
		return "The pull request changed since it was last read. Refresh and try again."
	case forgejo.ReasonBlockedByProtection:
		return "Branch protection blocks this: required approvals or checks are missing."
	case forgejo.ReasonSelfReview:
		return "An account cannot review its own pull request."
	case forgejo.ReasonAlreadyMerged:
		return "The pull request is already merged."
	case forgejo.ReasonForbidden:
		return "The access token lacks permission for this action. It needs write access to the repository."
	default:
		return "The instance rejected the request."
	}
}

func jsonResponse(value any) (*pluginsdk.PluginActionResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: encode action response: %w", err)
	}
	return &pluginsdk.PluginActionResponse{
		Body:    body,
		Headers: map[string]string{"Content-Type": "application/json"},
	}, nil
}
