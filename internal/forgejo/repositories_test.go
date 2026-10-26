package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func repoPayload(id int64, owner, name string) map[string]any {
	return map[string]any{
		"id":             id,
		"name":           name,
		"full_name":      owner + "/" + name,
		"owner":          map[string]any{"login": owner},
		"clone_url":      "https://forge.example.com/" + owner + "/" + name + ".git",
		"html_url":       "https://forge.example.com/" + owner + "/" + name,
		"default_branch": "main",
	}
}

func TestParseRepositoryURL(t *testing.T) {
	t.Parallel()
	const scope = "https://forge.example.com"
	for _, testCase := range []struct {
		name      string
		scope     string
		input     string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{name: "https web url", scope: scope, input: "https://forge.example.com/acme/widgets", wantOwner: "acme", wantName: "widgets", wantOK: true},
		{name: "clone url drops .git", scope: scope, input: "https://forge.example.com/acme/widgets.git", wantOwner: "acme", wantName: "widgets", wantOK: true},
		{name: "scp style ssh remote", scope: scope, input: "git@forge.example.com:acme/widgets.git", wantOwner: "acme", wantName: "widgets", wantOK: true},
		{name: "host is case insensitive", scope: scope, input: "https://FORGE.example.com/acme/widgets", wantOwner: "acme", wantName: "widgets", wantOK: true},
		{name: "extra path segments ignored", scope: scope, input: "https://forge.example.com/acme/widgets/src/branch/main", wantOwner: "acme", wantName: "widgets", wantOK: true},
		{name: "different host rejected", scope: scope, input: "https://github.com/acme/widgets", wantOK: false},
		{name: "missing repo segment rejected", scope: scope, input: "https://forge.example.com/acme", wantOK: false},
		{name: "subpath instance honored", scope: "https://example.com/forge", input: "https://example.com/forge/acme/widgets", wantOwner: "acme", wantName: "widgets", wantOK: true},
		{name: "subpath instance rejects root path", scope: "https://example.com/forge", input: "https://example.com/acme/widgets", wantOK: false},
		{name: "empty rejected", scope: scope, input: "  ", wantOK: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			owner, name, ok := parseRepositoryURL(testCase.scope, testCase.input)
			require.Equal(t, testCase.wantOK, ok)
			if testCase.wantOK {
				require.Equal(t, testCase.wantOwner, owner)
				require.Equal(t, testCase.wantName, name)
			}
		})
	}
}

func TestListPagesWithCursorAndDropsIncompleteRecords(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.HandleFunc(http.MethodGet, "/api/v1/repos/search", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		if page == "2" {
			_, _ = w.Write([]byte(`{"ok":true,"data":[]}`))
			return
		}
		// The second record has no id and must be dropped rather than
		// rendered as an unusable picker row.
		_, _ = w.Write([]byte(`{"ok":true,"data":[
			{"id":7,"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"},"clone_url":"https://x/acme/widgets.git","default_branch":"main"},
			{"id":0,"name":"broken","full_name":"acme/broken","owner":{"login":"acme"},"clone_url":"https://x/acme/broken.git"}
		]}`))
	})
	connection, _ := newTestConnection(t, api, "token")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo")

	page, err := repositories.List(context.Background(), "workspace-1", "", sourcecontrol.RepositoryCursor{}, 2)
	require.NoError(t, err)
	require.Len(t, page.Repositories, 1)
	require.Equal(t, "7", page.Repositories[0].RepositoryID)
	require.Equal(t, "acme", page.Repositories[0].OwnerOrProject)
	require.Equal(t, api.URL(), page.Repositories[0].ConnectionScope)
	// A full page (2 records returned for limit 2) implies another page.
	require.Equal(t, "2", page.Next.Remote)

	empty, err := repositories.List(context.Background(), "workspace-1", "", page.Next, 2)
	require.NoError(t, err)
	require.Empty(t, empty.Repositories)
	require.Empty(t, empty.Next.Remote, "a short page must not advertise another page")
}

func TestListRejectsMalformedCursor(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, _ := newTestConnection(t, api, "token")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo")

	_, err := repositories.List(context.Background(), "workspace-1", "", sourcecontrol.RepositoryCursor{Remote: "not-a-page"}, 10)
	require.ErrorContains(t, err, "invalid repository page cursor")
}

// matchesURL is a hint; inspectURL is the ownership authority and must return
// nil (not an error) for anything this connection does not own.
func TestInspectReturnsNilForUnownedURLs(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets", http.StatusOK, repoPayload(7, "acme", "widgets"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/secret", http.StatusNotFound, nil)
	connection, _ := newTestConnection(t, api, "token")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo")

	owned, err := repositories.Inspect(context.Background(), "workspace-1", api.URL()+"/acme/widgets")
	require.NoError(t, err)
	require.NotNil(t, owned)
	require.Equal(t, "7", owned.RepositoryID)
	require.Equal(t, "forgejo", owned.ProviderID)

	foreign, err := repositories.Inspect(context.Background(), "workspace-1", "https://github.com/acme/widgets")
	require.NoError(t, err)
	require.Nil(t, foreign, "a URL on another host is not this provider's repository")

	invisible, err := repositories.Inspect(context.Background(), "workspace-1", api.URL()+"/acme/secret")
	require.NoError(t, err)
	require.Nil(t, invisible, "a repository the token cannot see is reported as unowned, not as an error")
}

func TestInspectReturnsNilWhenUnconfigured(t *testing.T) {
	t.Parallel()
	host := newFakeHost(map[string]any{})
	connection := NewConnection(func() pluginsdk.Host { return host })
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")

	result, err := repositories.Inspect(context.Background(), "workspace-1", "https://forge.example.com/a/b")
	require.NoError(t, err)
	require.Nil(t, result)
}

// Identity is the numeric id, so a rename must not break resolution.
func TestResolveUsesImmutableIDAndRejectsForeignScope(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "renamed-widgets"))
	connection, _ := newTestConnection(t, api, "token")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo")

	resolved, err := repositories.Resolve(context.Background(), "workspace-1", sourcecontrol.RepositoryIdentity{
		ConnectionScope: api.URL(),
		RepositoryID:    "7",
	})
	require.NoError(t, err)
	require.Equal(t, "renamed-widgets", resolved.Name)

	_, err = repositories.Resolve(context.Background(), "workspace-1", sourcecontrol.RepositoryIdentity{
		ConnectionScope: "https://other.example.com",
		RepositoryID:    "7",
	})
	require.ErrorContains(t, err, "another connection")

	_, err = repositories.Resolve(context.Background(), "workspace-1", sourcecontrol.RepositoryIdentity{
		ConnectionScope: api.URL(),
		RepositoryID:    "not-a-number",
	})
	require.ErrorContains(t, err, "not a valid repository id")
}

func TestListBranchesMarksDefault(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/branches", http.StatusOK, []map[string]any{
		{"name": "main", "commit": map[string]any{"id": "abc"}},
		{"name": "feature/x", "commit": map[string]any{"id": "def"}},
		{"name": "  "},
	})
	connection, _ := newTestConnection(t, api, "token")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo")

	branches, err := repositories.ListBranches(context.Background(), "workspace-1", sourcecontrol.Repository{
		OwnerOrProject: "acme", Name: "widgets", DefaultBranch: "main",
	})
	require.NoError(t, err)
	require.Len(t, branches, 2)
	require.True(t, branches[0].IsDefault)
	require.False(t, branches[1].IsDefault)
}

// Create authority comes from the verified context, resolved through the Host.
func TestResolveAttachedUsesVerifiedRepositoryID(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "widgets"))
	connection, host := newTestConnection(t, api, "token")
	host.repos["workspace-1"] = []pluginsdk.Repository{
		{ID: "other", ProviderID: "forgejo", ProviderRepositoryID: "9"},
		{ID: "repo-1", ProviderID: "forgejo", ProviderRepositoryID: "7"},
	}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")

	resolved, err := repositories.ResolveAttached(context.Background(), pluginsdk.VerifiedActionContext{
		WorkspaceID: "workspace-1", RepositoryID: "repo-1",
	})
	require.NoError(t, err)
	require.Equal(t, "7", resolved.RepositoryID)
	require.Equal(t, "widgets", resolved.Name)
}

func TestResolveAttachedRejectsUnattachedAndForeignProvider(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, host := newTestConnection(t, api, "token")
	host.repos["workspace-1"] = []pluginsdk.Repository{
		{ID: "repo-github", ProviderID: "github", ProviderRepositoryID: "1"},
	}
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")

	_, err := repositories.ResolveAttached(context.Background(), pluginsdk.VerifiedActionContext{
		WorkspaceID: "workspace-1", RepositoryID: "repo-github",
	})
	require.ErrorContains(t, err, "not a forgejo repository")

	_, err = repositories.ResolveAttached(context.Background(), pluginsdk.VerifiedActionContext{
		WorkspaceID: "workspace-1", RepositoryID: "missing",
	})
	require.ErrorContains(t, err, "not attached to workspace")

	_, err = repositories.ResolveAttached(context.Background(), pluginsdk.VerifiedActionContext{
		WorkspaceID: "workspace-1",
	})
	require.ErrorContains(t, err, "no repository")
}

func TestResolveAttachedPropagatesHostErrors(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, host := newTestConnection(t, api, "token")
	host.repoErr = errors.New("host unavailable")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")

	_, err := repositories.ResolveAttached(context.Background(), pluginsdk.VerifiedActionContext{
		WorkspaceID: "workspace-1", RepositoryID: "repo-1",
	})
	require.ErrorContains(t, err, "list workspace repositories")
}
