package plugin

import (
	"context"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/sourcecontrol"
	"kandev-plugin-forgejo/internal/watches"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// This file is the composition root: the only one in this package that names
// a Forgejo adapter. Everything else reaches the instance through ports: the
// features' own, and the handlers' where no feature consumes one.

// NewRuntime builds the runtime and its adapter graph. The Host is injected
// after construction, so every adapter resolves it lazily through the provider
// closure rather than capturing it here.
func NewRuntime() *Runtime {
	runtime := &Runtime{}
	hosts := kandev.Provider(func() pluginsdk.Host { return runtime.Host() })
	switches := workspaceSwitches{hosts: hosts}
	audit := &auditTrail{hosts: hosts}

	connection := forgejo.NewConnection(hosts)
	repositories := forgejo.NewRepositories(connection, hosts, ProviderID)
	associations := sourcecontrol.NewAssociations(hosts)
	changeRequests := forgejo.NewChangeRequests(connection, repositories)
	pulls := forgejo.NewPullRequests(connection)
	linkedPulls := forgejo.NewPullActions(connection, repositories, associations, ProviderID)
	source := forgejo.NewAutomationSource(connection, associations)

	watchStore := watches.NewStore(hosts)
	watchPoller := watches.NewPoller(watchStore, hosts, forgejo.NewIssueSource(connection), switches.Enabled).
		WithReviews(forgejo.NewReviewSource(connection))
	engine := newAutomationEngine(hosts, switches, audit, source)

	runtime.switches = switches
	runtime.audit = audit
	runtime.gitCredentials = forgejo.NewGitCredentials(connection)
	runtime.watchPoller = watchPoller
	runtime.automation = engine

	runtime.connectionActions = &connectionActions{
		hosts: hosts, connection: connection, switches: switches, audit: audit,
	}
	runtime.pullActions = &pullRequestActions{pullActions: linkedPulls, switches: switches, audit: audit}
	runtime.watchActions = &watchActions{
		hosts: hosts, watchStore: watchStore, watchPoller: watchPoller, switches: switches,
	}
	runtime.automationActions = &automationActions{
		automation: engine, automationSource: source, connection: connection,
		associations: associations, switches: switches, audit: audit,
	}
	runtime.conditions = &conditionHandler{
		conditions: forgejo.NewConditionInstance(connection), webhooks: forgejo.Webhooks{}, switches: switches,
	}
	runtime.tools = &agentTools{
		hosts: hosts, switches: switches, audit: audit,
		connection: connection, repositories: repositories, changeRequests: changeRequests,
		associations: associations, pulls: pulls, ci: pulls, pullActions: linkedPulls,
		automation: engine,
	}

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

// newAutomationEngine wires the engine to the Host and the instance.
func newAutomationEngine(hosts kandev.Provider, switches workspaceSwitches, audit *auditTrail, source *forgejo.AutomationSource) *prautomation.Engine {
	return prautomation.NewEngine(prautomation.Config{
		// A nil Host converts to a nil StateHost: interface to interface.
		Store:     prautomation.NewStore(func() prautomation.StateHost { return hosts() }),
		Links:     source,
		Observer:  source,
		Merger:    automationMerger{source: source, audit: audit},
		Messenger: automationMessenger{hosts: hosts},
		// Fails closed: prompting an agent or merging in a workspace whose
		// operator turned the integration off is worse than waiting a tick.
		Gate: switches.EnabledStrict,
		Workspaces: func(ctx context.Context) ([]string, error) {
			host, err := hosts.Get()
			if err != nil {
				return nil, err
			}
			return kandev.WorkspaceIDs(ctx, host)
		},
		Describe: safeMessage,
	})
}
