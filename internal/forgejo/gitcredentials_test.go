package forgejo

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// credentialScope is a complete, host-verified lease scope for the fake
// instance: the shape Kandev sends for an initial host-side clone.
func credentialScope(t *testing.T, api *apiServer) (string, string, string) {
	t.Helper()
	parsed, err := url.Parse(api.URL())
	require.NoError(t, err)
	return parsed.Host, "/tools/widgets.git", "repository-1"
}

func resolveRequest(host, path, repositoryID string) *pluginsdk.ResolveGitCredentialRequest {
	return &pluginsdk.ResolveGitCredentialRequest{
		ProviderID: "forgejo", WorkspaceID: "workspace-1", TaskID: "task-1",
		SessionID: "session-1", RepositoryID: repositoryID, Host: host, Path: path,
	}
}

func bindingRequest(host, path, repositoryID string) *pluginsdk.GitCredentialBindingRequest {
	return &pluginsdk.GitCredentialBindingRequest{
		ProviderID: "forgejo", WorkspaceID: "workspace-1", TaskID: "task-1",
		SessionID: "session-1", RepositoryID: repositoryID, Host: host, Path: path,
	}
}

func TestGitCredentialsResolveReturnsTokenOwnerAndToken(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	connection, _ := newTestConnection(t, api, "secret-token")
	host, path, repositoryID := credentialScope(t, api)

	response, err := NewGitCredentials(connection).Resolve(context.Background(), resolveRequest(host, path, repositoryID))
	require.NoError(t, err)
	require.Equal(t, "kandev", response.Username)
	require.Equal(t, "secret-token", response.Secret)
}

// Kandev's contract: reject missing task, session, or repository identity
// rather than weakening authorization to workspace-only access.
func TestGitCredentialsResolveRejectsIncompleteScope(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	connection, _ := newTestConnection(t, api, "secret-token")
	host, path, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	for name, mutate := range map[string]func(*pluginsdk.ResolveGitCredentialRequest){
		"workspace":  func(r *pluginsdk.ResolveGitCredentialRequest) { r.WorkspaceID = "" },
		"task":       func(r *pluginsdk.ResolveGitCredentialRequest) { r.TaskID = "" },
		"session":    func(r *pluginsdk.ResolveGitCredentialRequest) { r.SessionID = " " },
		"repository": func(r *pluginsdk.ResolveGitCredentialRequest) { r.RepositoryID = "" },
	} {
		request := resolveRequest(host, path, repositoryID)
		mutate(request)
		response, err := credentials.Resolve(context.Background(), request)
		require.Error(t, err, name)
		require.Nil(t, response, name)
	}
	require.Empty(t, api.Requests(), "an incomplete scope must fail before any instance I/O")
}

func TestGitCredentialsResolveRejectsForeignHost(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	connection, _ := newTestConnection(t, api, "secret-token")
	_, path, repositoryID := credentialScope(t, api)

	response, err := NewGitCredentials(connection).Resolve(context.Background(), resolveRequest("github.com", path, repositoryID))
	require.Error(t, err)
	require.Nil(t, response)
}

func TestGitCredentialsResolveRejectsNonRepositoryPath(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	connection, _ := newTestConnection(t, api, "secret-token")
	host, _, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	for _, path := range []string{"", "/", "/tools", "/tools/widgets/extra", "/tools/../widgets", "tools/widgets"} {
		response, err := credentials.Resolve(context.Background(), resolveRequest(host, path, repositoryID))
		require.Error(t, err, path)
		require.Nil(t, response, path)
	}
}

func TestGitCredentialsResolveWithoutConfigIsNotConfigured(t *testing.T) {
	t.Parallel()
	host := newFakeHost(map[string]any{})
	connection := NewConnection(func() pluginsdk.Host { return host })

	_, err := NewGitCredentials(connection).Resolve(context.Background(),
		resolveRequest("forge.example.com", "/tools/widgets.git", "repository-1"))
	require.ErrorIs(t, err, scm.ErrNotConfigured)
}

func TestGitCredentialsBindingIsStableAndOpaque(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, _ := newTestConnection(t, api, "secret-token")
	host, path, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	first, err := credentials.Binding(context.Background(), bindingRequest(host, path, repositoryID))
	require.NoError(t, err)
	second, err := credentials.Binding(context.Background(), bindingRequest(host, path, repositoryID))
	require.NoError(t, err)
	require.NotEmpty(t, first.Binding)
	require.Equal(t, first.Binding, second.Binding)
	require.NotContains(t, first.Binding, "secret-token")
	require.Empty(t, api.Requests(), "a binding lookup is non-secret and needs no instance I/O")
}

// Rotating the token must invalidate an already-issued helper lease.
func TestGitCredentialsBindingChangesWhenTokenRotates(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, fake := newTestConnection(t, api, "first-token")
	host, path, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	before, err := credentials.Binding(context.Background(), bindingRequest(host, path, repositoryID))
	require.NoError(t, err)
	fake.mu.Lock()
	fake.config = map[string]any{"base_url": api.URL(), "api_token": "second-token"}
	fake.mu.Unlock()
	after, err := credentials.Binding(context.Background(), bindingRequest(host, path, repositoryID))
	require.NoError(t, err)
	require.NotEqual(t, before.Binding, after.Binding)
}

func TestGitCredentialsBindingDiffersPerScope(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, _ := newTestConnection(t, api, "secret-token")
	host, path, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	one, err := credentials.Binding(context.Background(), bindingRequest(host, path, repositoryID))
	require.NoError(t, err)
	other, err := credentials.Binding(context.Background(), bindingRequest(host, "/tools/gadgets.git", repositoryID))
	require.NoError(t, err)
	require.NotEqual(t, one.Binding, other.Binding)
}

func TestGitCredentialsBindingRejectsForeignHostAndIncompleteScope(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, _ := newTestConnection(t, api, "secret-token")
	host, path, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	_, err := credentials.Binding(context.Background(), bindingRequest("github.com", path, repositoryID))
	require.Error(t, err)
	_, err = credentials.Binding(context.Background(), bindingRequest(host, path, ""))
	require.Error(t, err)
	_, err = credentials.Binding(context.Background(), bindingRequest(host, "/tools", repositoryID))
	require.Error(t, err)
}

// A base URL served under a sub-path only authorizes repositories below it.
func TestGitCredentialsHonorInstanceSubPath(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/forge/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	fake := newFakeHost(map[string]any{"base_url": api.URL() + "/forge", "api_token": "secret-token"})
	connection := NewConnection(func() pluginsdk.Host { return fake })
	host, _, repositoryID := credentialScope(t, api)
	credentials := NewGitCredentials(connection)

	response, err := credentials.Resolve(context.Background(), resolveRequest(host, "/forge/tools/widgets.git", repositoryID))
	require.NoError(t, err)
	require.Equal(t, "secret-token", response.Secret)

	_, err = credentials.Resolve(context.Background(), resolveRequest(host, "/tools/widgets.git", repositoryID))
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "secret-token"))
}
