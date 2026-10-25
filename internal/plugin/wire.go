package plugin

import (
	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/sourcecontrol"
	"kandev-plugin-forgejo/internal/watches"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// This file is the composition root: the only one in this package that names
// a Forgejo adapter. Everything else reaches the instance through the ports
// the features declare.

// NewRuntime builds the runtime and its adapter graph. The Host is injected
// after construction, so every adapter resolves it lazily through the provider
// closure rather than capturing it here.
func NewRuntime() *Runtime {
	runtime := &Runtime{}
	hosts := kandev.Provider(func() pluginsdk.Host { return runtime.Host() })

	connection := forgejo.NewConnection(hosts)
	repositories := forgejo.NewRepositories(connection, hosts, ProviderID)
	associations := sourcecontrol.NewAssociations(hosts)
	changeRequests := forgejo.NewChangeRequests(connection, repositories)
	pulls := forgejo.NewPullRequests(connection)

	runtime.connection = connection
	runtime.repositories = repositories
	runtime.changeRequests = changeRequests
	runtime.associations = associations
	runtime.pulls = pulls
	runtime.ci = pulls
	runtime.gitCredentials = forgejo.NewGitCredentials(connection)
	runtime.pullActions = forgejo.NewPullActions(connection, repositories, associations, ProviderID)
	runtime.conditions = forgejo.NewConditionInstance(connection)
	runtime.webhooks = forgejo.Webhooks{}

	runtime.watchStore = watches.NewStore(hosts)
	runtime.watchPoller = watches.NewPoller(
		runtime.watchStore,
		hosts,
		forgejo.NewIssueSource(connection),
		runtime.integrationEnabled,
	).WithReviews(forgejo.NewReviewSource(connection))

	source := forgejo.NewAutomationSource(connection, associations)
	runtime.automationSource = source
	runtime.automation = runtime.newAutomationEngine(hosts, source)

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
func (r *Runtime) newAutomationEngine(hosts kandev.Provider, source *forgejo.AutomationSource) *prautomation.Engine {
	return prautomation.NewEngine(prautomation.Config{
		Store: prautomation.NewStore(func() prautomation.StateHost {
			host := hosts()
			if host == nil {
				return nil
			}
			return host
		}),
		Links:     source,
		Observer:  source,
		Merger:    automationMerger{runtime: r},
		Messenger: automationMessenger{hosts: hosts},
		// Fails closed: prompting an agent or merging in a workspace whose
		// operator turned the integration off is worse than waiting a tick.
		Gate:       r.integrationEnabledStrict,
		Workspaces: r.workspaceIDs,
		Describe:   safeMessage,
	})
}
