package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// branchPageLimit bounds a single branch page request.
const branchPageLimit = 100

// Repositories implements the recipe's RepositoryLister, RepositoryDetails and
// AttachedRepositoryResolver ports against one Forgejo/Gitea instance.
type Repositories struct {
	connection *Connection
	hosts      kandev.Provider
	providerID string
}

var (
	_ sourcecontrol.RepositoryLister           = (*Repositories)(nil)
	_ sourcecontrol.RepositoryDetails          = (*Repositories)(nil)
	_ sourcecontrol.AttachedRepositoryResolver = (*Repositories)(nil)
)

// NewRepositories returns the repository adapter for providerID.
func NewRepositories(connection *Connection, hosts kandev.Provider, providerID string) *Repositories {
	return &Repositories{connection: connection, hosts: hosts, providerID: providerID}
}

// ConnectionScope returns the normalized instance URL. The workspace argument
// is accepted for contract symmetry; this plugin holds one operator-level
// connection, so every workspace resolves to the same instance.
func (r *Repositories) ConnectionScope(ctx context.Context, _ string) (string, error) {
	return r.connection.Scope(ctx)
}

// List returns one page of repositories. The recipe owns cursor opacity and
// binds each cursor to the query and connection scope; this adapter only
// carries a page number through the cursor's provider-local Remote field.
func (r *Repositories) List(ctx context.Context, _ string, query string, cursor sourcecontrol.RepositoryCursor, limit int) (sourcecontrol.RepositoryPage, error) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.RepositoryPage{}, err
	}
	page := 1
	if trimmed := strings.TrimSpace(cursor.Remote); trimmed != "" {
		parsed, err := strconv.Atoi(trimmed)
		if err != nil || parsed < 1 {
			return sourcecontrol.RepositoryPage{}, errors.New("forgejo: invalid repository page cursor")
		}
		page = parsed
	}
	repos, err := client.SearchRepos(ctx, query, page, limit)
	if err != nil {
		return sourcecontrol.RepositoryPage{}, err
	}

	result := sourcecontrol.RepositoryPage{Repositories: make([]sourcecontrol.Repository, 0, len(repos))}
	for _, repo := range repos {
		converted, ok := r.toRepository(client, repo)
		if !ok {
			continue
		}
		result.Repositories = append(result.Repositories, converted)
	}
	// A full page implies there may be another. Forgejo's search endpoint has
	// no opaque continuation token, so the next page number is the cursor.
	if len(repos) == limit && limit > 0 {
		result.Next = sourcecontrol.RepositoryCursor{
			Remote: strconv.Itoa(page + 1),
		}
		if last := len(result.Repositories); last > 0 {
			result.Next.AfterRepositoryID = result.Repositories[last-1].RepositoryID
		}
	}
	return result, nil
}

// Inspect treats url as a cheap hint and then proves ownership with an
// authenticated lookup. It returns nil when this connection does not own the
// URL, which the recipe reports as "not this provider's repository".
func (r *Repositories) Inspect(ctx context.Context, _ string, rawURL string) (*sourcecontrol.Repository, error) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		if errors.Is(err, scm.ErrNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	owner, name, ok := parseRepositoryURL(client.Scope(), rawURL)
	if !ok {
		return nil, nil
	}
	repo, err := client.Repo(ctx, owner, name)
	if err != nil {
		// Not found or not visible to this token means "not owned here".
		if errors.Is(err, scm.ErrNotFound) || errors.Is(err, scm.ErrUnauthorized) {
			return nil, nil
		}
		return nil, err
	}
	converted, valid := r.toRepository(client, repo)
	if !valid {
		return nil, nil
	}
	return &converted, nil
}

// Resolve re-reads a repository by its immutable numeric id so a rename or
// transfer cannot break a stored identity.
func (r *Repositories) Resolve(ctx context.Context, _ string, identity sourcecontrol.RepositoryIdentity) (sourcecontrol.Repository, error) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.Repository{}, err
	}
	if strings.TrimSpace(identity.ConnectionScope) != client.Scope() {
		return sourcecontrol.Repository{}, fmt.Errorf("forgejo: repository identity belongs to another connection")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(identity.RepositoryID), 10, 64)
	if err != nil || id <= 0 {
		return sourcecontrol.Repository{}, errors.New("forgejo: repository identity is not a valid repository id")
	}
	repo, err := client.RepoByID(ctx, id)
	if err != nil {
		return sourcecontrol.Repository{}, err
	}
	converted, ok := r.toRepository(client, repo)
	if !ok {
		return sourcecontrol.Repository{}, errors.New("forgejo: repository is missing required identity fields")
	}
	return converted, nil
}

// ListBranches returns the branches of an already-resolved repository.
func (r *Repositories) ListBranches(ctx context.Context, _ string, repository sourcecontrol.Repository) ([]sourcecontrol.Branch, error) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		return nil, err
	}
	branches, err := client.Branches(ctx, repository.OwnerOrProject, repository.Name, 1, branchPageLimit)
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	result := make([]sourcecontrol.Branch, 0, len(branches))
	for _, branch := range branches {
		name := strings.TrimSpace(branch.Name)
		if name == "" {
			continue
		}
		result = append(result, sourcecontrol.Branch{
			Name:      name,
			Commit:    branch.Commit.ID,
			IsDefault: name == repository.DefaultBranch,
		})
	}
	return result, nil
}

// ResolveAttached resolves the repository Kandev verified for this action
// through the Host data API. The browser never supplies create authority — the
// repository id comes from VerifiedActionContext.
func (r *Repositories) ResolveAttached(ctx context.Context, action pluginsdk.VerifiedActionContext) (sourcecontrol.Repository, error) {
	host := r.hosts()
	if host == nil {
		return sourcecontrol.Repository{}, scm.ErrNotConfigured
	}
	client, err := r.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.Repository{}, err
	}

	wanted := strings.TrimSpace(action.RepositoryID)
	if wanted == "" {
		return sourcecontrol.Repository{}, errors.New("forgejo: verified action context has no repository")
	}

	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return sourcecontrol.Repository{}, err
		}
		repositories, pageInfo, err := host.Repositories().List(ctx, action.WorkspaceID, pluginsdk.Page{Limit: 100, Cursor: cursor})
		if err != nil {
			return sourcecontrol.Repository{}, fmt.Errorf("forgejo: list workspace repositories: %w", err)
		}
		for _, repository := range repositories {
			if repository.ID != wanted {
				continue
			}
			if repository.ProviderID != "" && repository.ProviderID != r.providerID {
				return sourcecontrol.Repository{}, fmt.Errorf("forgejo: repository %s is not a %s repository", wanted, r.providerID)
			}
			return r.resolveHostRepository(ctx, client, repository)
		}
		if pageInfo == nil || !pageInfo.HasMore || strings.TrimSpace(pageInfo.NextCursor) == "" {
			break
		}
		cursor = pageInfo.NextCursor
	}
	return sourcecontrol.Repository{}, fmt.Errorf("forgejo: repository %s is not attached to workspace %s", wanted, action.WorkspaceID)
}

// resolveHostRepository converts a Kandev repository record into a provider
// repository, re-reading it from the instance so identity and clone URL are
// provider truth rather than stored display data.
func (r *Repositories) resolveHostRepository(ctx context.Context, client *Client, repository pluginsdk.Repository) (sourcecontrol.Repository, error) {
	if id := strings.TrimSpace(repository.ProviderRepositoryID); id != "" {
		parsed, err := strconv.ParseInt(id, 10, 64)
		if err == nil && parsed > 0 {
			repo, err := client.RepoByID(ctx, parsed)
			if err == nil {
				if converted, ok := r.toRepository(client, repo); ok {
					return converted, nil
				}
			} else if !errors.Is(err, scm.ErrNotFound) {
				return sourcecontrol.Repository{}, err
			}
		}
	}
	// Fall back to the remote URL when Kandev has no provider id recorded,
	// e.g. a repository added before this plugin was installed.
	owner, name, ok := parseRepositoryURL(client.Scope(), repository.RemoteURL)
	if !ok {
		return sourcecontrol.Repository{}, fmt.Errorf("forgejo: repository %s does not belong to %s", repository.ID, client.Scope())
	}
	repo, err := client.Repo(ctx, owner, name)
	if err != nil {
		return sourcecontrol.Repository{}, err
	}
	converted, valid := r.toRepository(client, repo)
	if !valid {
		return sourcecontrol.Repository{}, errors.New("forgejo: repository is missing required identity fields")
	}
	return converted, nil
}

// toRepository converts a provider repo into the credential-free descriptor
// the host renders. It drops records missing identity fields rather than
// emitting a half-formed entry the picker cannot act on.
func (r *Repositories) toRepository(client *Client, repo Repo) (sourcecontrol.Repository, bool) {
	owner := repo.OwnerLogin()
	name := strings.TrimSpace(repo.Name)
	cloneURL := strings.TrimSpace(repo.CloneURL)
	if repo.ID <= 0 || owner == "" || name == "" || cloneURL == "" {
		return sourcecontrol.Repository{}, false
	}
	return sourcecontrol.Repository{
		ProviderID:      r.providerID,
		ProviderHost:    client.Host(),
		ConnectionScope: client.Scope(),
		RepositoryID:    strconv.FormatInt(repo.ID, 10),
		OwnerOrProject:  owner,
		Name:            name,
		CloneURL:        cloneURL,
		DefaultBranch:   strings.TrimSpace(repo.DefaultBranch),
	}, true
}

// parseRepositoryURL extracts owner and repository name from a web or clone
// URL belonging to scope. It returns false for any other host, so a URL from a
// different instance is never probed with this connection's token.
func parseRepositoryURL(scope, rawURL string) (string, string, bool) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", "", false
	}
	// Normalize scp-style SSH remotes (git@host:owner/repo.git) into a URL.
	if !strings.Contains(trimmed, "://") {
		if at := strings.Index(trimmed, "@"); at >= 0 {
			if colon := strings.Index(trimmed[at:], ":"); colon >= 0 {
				host := trimmed[at+1 : at+colon]
				path := trimmed[at+colon+1:]
				trimmed = "ssh://" + host + "/" + strings.TrimPrefix(path, "/")
			}
		}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", "", false
	}
	scopeURL, err := url.Parse(scope)
	if err != nil {
		return "", "", false
	}
	if !strings.EqualFold(parsed.Hostname(), scopeURL.Hostname()) {
		return "", "", false
	}

	path := strings.Trim(parsed.Path, "/")
	// Drop any base path the instance is mounted under.
	if base := strings.Trim(scopeURL.Path, "/"); base != "" {
		if !strings.HasPrefix(path, base+"/") {
			return "", "", false
		}
		path = strings.TrimPrefix(path, base+"/")
	}
	segments := strings.Split(path, "/")
	if len(segments) < 2 {
		return "", "", false
	}
	owner := strings.TrimSpace(segments[0])
	name := strings.TrimSuffix(strings.TrimSpace(segments[1]), ".git")
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
