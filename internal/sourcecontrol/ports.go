package sourcecontrol

import (
	"context"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// The ports the recipe consumes; internal/forgejo implements them.

type RepositoryLister interface {
	ConnectionScope(context.Context, string) (string, error)
	List(context.Context, string, string, RepositoryCursor, int) (RepositoryPage, error)
}

type RepositoryDetails interface {
	Inspect(context.Context, string, string) (*Repository, error)
	Resolve(context.Context, string, RepositoryIdentity) (Repository, error)
	ListBranches(context.Context, string, Repository) ([]Branch, error)
}

// AttachedRepositoryResolver must resolve through Host Tasks/Repositories APIs.
// Browser descriptors are never authority for create operations.
type AttachedRepositoryResolver interface {
	ResolveAttached(context.Context, pluginsdk.VerifiedActionContext) (Repository, error)
}

type ChangeRequestService interface {
	ResolveReference(context.Context, string, string) (ChangeRequest, error)
	Create(context.Context, Repository, string, CreateChangeRequestInput) (ChangeRequest, error)
}

type AssociationStore interface {
	Link(context.Context, string, ChangeRequestIdentity) error
	Unlink(context.Context, string, ChangeRequestIdentity) error
}

type ReviewReader interface {
	ForTask(context.Context, string, string) ([]ReviewSummary, error)
	Associations(context.Context, string) ([]ReviewAssociation, error)
}
