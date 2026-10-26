// Package plugin is the transport: it turns Kandev's RPCs into calls on the
// features' ports and their results back into SDK responses. It knows Kandev's
// shapes and nothing about Forgejo; wire.go is the composition root.
package plugin

import (
	"context"
	"errors"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/sourcecontrol"
	"kandev-plugin-forgejo/internal/watches"
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

// Runtime is the plugin value passed to pluginsdk.Serve. It embeds
// UnimplementedPlugin for no-op defaults, routes each RPC to the handler that
// owns it, and delegates the recipe's source-control RPCs to the extension.
// Each handler holds only the ports it uses; wire.go builds them.
type Runtime struct {
	pluginsdk.UnimplementedPlugin
	extension      *sourcecontrol.Extension
	switches       workspaceSwitches
	audit          *auditTrail
	gitCredentials gitCredentials

	connectionActions *connectionActions
	pullActions       *pullRequestActions
	watchActions      *watchActions
	automationActions *automationActions
	conditions        *conditionHandler
	tools             *agentTools

	// Background work, started once the Host arrives (see SetHost).
	watchPoller *watches.Poller
	automation  *prautomation.Engine
}

// taskLinks is the slice of the association store the agent tools and the
// automation actions use.
type taskLinks interface {
	Link(ctx context.Context, taskID string, identity sourcecontrol.ChangeRequestIdentity) error
	ListForTask(ctx context.Context, workspaceID, taskID string) ([]sourcecontrol.ChangeRequestIdentity, error)
}

// instanceInfo reports the configured instance without contacting it. Info
// fails with scm.ErrNotConfigured or scm.ErrInvalidConfig when the operator
// has not set a usable one.
type instanceInfo interface {
	Info(ctx context.Context) (sourcecontrol.ConnectionInfo, error)
}

// linkedPullRequests reads and writes a pull request linked to the verified task.
type linkedPullRequests interface {
	sourcecontrol.ChangeRequestDetailReader
	sourcecontrol.ChangeRequestActions
}

// gitCredentials issues and binds the HTTPS credentials git uses.
type gitCredentials interface {
	Resolve(ctx context.Context, request *pluginsdk.ResolveGitCredentialRequest) (*pluginsdk.ResolveGitCredentialResponse, error)
	Binding(ctx context.Context, request *pluginsdk.GitCredentialBindingRequest) (*pluginsdk.GitCredentialBindingResponse, error)
}

// automationPulls is what the automation actions ask of a linked pull request
// beyond the engine's own ports.
type automationPulls interface {
	IsFork(ctx context.Context, ref prautomation.PullRef) (bool, error)
	MergeLinked(ctx context.Context, ref prautomation.PullRef, headSHA string) error
}

var (
	_ pluginsdk.Plugin                    = (*Runtime)(nil)
	_ pluginsdk.ActionHandler             = (*Runtime)(nil)
	_ pluginsdk.EntityReferenceSearcher   = (*Runtime)(nil)
	_ pluginsdk.EntityReferenceAuthorizer = (*Runtime)(nil)
	_ pluginsdk.GitCredentialHandler      = (*Runtime)(nil)
	_ pluginsdk.AgentToolPlugin           = (*Runtime)(nil)
	_ pluginsdk.AutomationAdapter         = (*Runtime)(nil)
)

// HandleAction routes the actions this plugin owns and delegates every
// source-control action to the recipe extension.
func (r *Runtime) HandleAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if request == nil {
		return nil, errors.New("kandev-plugin-forgejo: action request is required")
	}
	switch request.ActionKey {
	case ActionConnectionGet:
		return r.connectionActions.connectionStatus(ctx, request.Context.WorkspaceID, false)
	case ActionConnectionTest:
		return r.connectionActions.connectionStatus(ctx, request.Context.WorkspaceID, true)
	case ActionConnectionSetEnabled:
		return r.connectionActions.setEnabled(ctx, request)
	case ActionConnectionSetAgentMerge:
		return r.connectionActions.setAgentMerge(ctx, request)
	case ActionConnectionAudit:
		return r.audit.list(ctx, request)
	case sourcecontrol.ActionChangeRequestsDetails, sourcecontrol.ActionChangeRequestsMerge,
		sourcecontrol.ActionChangeRequestsReview, sourcecontrol.ActionChangeRequestsRequestReviewers,
		sourcecontrol.ActionChangeRequestsUpdateBranch, sourcecontrol.ActionChangeRequestsComment:
		return r.pullActions.handlePullAction(ctx, request)
	case ActionWatchesList, ActionWatchesOptions, ActionWatchesCreate, ActionWatchesUpdate,
		ActionWatchesDelete, ActionWatchesRun, ActionWatchesReset, ActionWatchesCleanup:
		// Watch management stays reachable while the integration is off, so an
		// operator can see and edit what will resume when they turn it back on.
		// The poller and the manual run both refuse separately, which is where
		// the toggle has to bite.
		return r.watchActions.handleWatchAction(ctx, request)
	case ActionAutomationGet, ActionAutomationSet, ActionAutomationRetry,
		ActionAutomationSettings, ActionAutomationSettingsSet:
		// Reachable while the integration is off, like watch management: the
		// loop is what the switch stops.
		return r.automationActions.handleAutomationAction(ctx, request)
	default:
		// A disabled integration contributes nothing to its workspace. The
		// toggle would otherwise be decorative: it would move a badge while
		// repositories, reviews and references kept flowing.
		enabled, err := r.switches.Enabled(ctx, request.Context.WorkspaceID)
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
// starts the watch poller and the pull request automation loop.
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
	if r.automation != nil {
		r.automation.Start()
	}
}

// Stop halts background work. pluginsdk.Serve does not call this — kandev
// terminates the subprocess — but a test that starts a Runtime needs a way to
// leave no goroutine behind.
func (r *Runtime) Stop() {
	if r.watchPoller != nil {
		r.watchPoller.Stop()
	}
	if r.automation != nil {
		r.automation.Stop()
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

// InvokeAgentTool serves the MCP tools this plugin exposes to task agents.
func (r *Runtime) InvokeAgentTool(ctx context.Context, request *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	return r.tools.InvokeAgentTool(ctx, request)
}

// DescribeAutomationCondition reports whether a condition can be used.
func (r *Runtime) DescribeAutomationCondition(ctx context.Context, request *pluginsdk.AutomationConditionRequest) (*pluginsdk.AutomationConditionResponse, error) {
	return r.conditions.DescribeAutomationCondition(ctx, request)
}

// VerifyAutomationWebhook authenticates and classifies one delivery.
func (r *Runtime) VerifyAutomationWebhook(ctx context.Context, request *pluginsdk.AutomationWebhookRequest) (*pluginsdk.AutomationWebhookResponse, error) {
	return r.conditions.VerifyAutomationWebhook(ctx, request)
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
	enabled, err := r.switches.Enabled(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !enabled {
		return errGitCredentialWithheld
	}
	return nil
}

// disabledResponse answers a source-control action for a workspace where the
// operator turned this integration off. Reads return nothing so native
// surfaces render empty rather than erroring; mutations refuse, because
// silently dropping a create or a link would be worse than saying no.
func disabledResponse(actionKey string) (*pluginsdk.PluginActionResponse, error) {
	switch actionKey {
	case sourcecontrol.ActionRepositoriesList:
		return kandev.JSON(map[string]any{"repositories": []any{}})
	case sourcecontrol.ActionRepositoriesInspect:
		return kandev.JSON(map[string]any{"repository": nil})
	case sourcecontrol.ActionRepositoriesBranches:
		return kandev.JSON(map[string]any{"branches": []any{}})
	case sourcecontrol.ActionChangeRequestsGet:
		return kandev.JSON(map[string]any{"reviews": []any{}})
	case sourcecontrol.ActionChangeRequestAssociations:
		return kandev.JSON(map[string]any{"associations": []any{}})
	default:
		return nil, errors.New("kandev-plugin-forgejo: the Forgejo integration is turned off for this workspace")
	}
}
