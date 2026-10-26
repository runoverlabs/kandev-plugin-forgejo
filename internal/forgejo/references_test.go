package forgejo

import (
	"context"
	"net/http"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestSearchBuildsImmutableCandidateIDs(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/issues/search", http.StatusOK, []map[string]any{
		{"number": 42, "title": "Add widgets", "state": "open", "html_url": "https://forge/pulls/42",
			"repository": map[string]any{"id": 7, "owner": "acme", "name": "widgets", "full_name": "acme/widgets"}},
		{"number": 9, "title": "Merged one", "state": "closed", "html_url": "https://forge/pulls/9",
			"repository": map[string]any{"id": 8, "full_name": "acme/other"}, "pull_request": map[string]any{"merged": true}},
		// No repository -> unusable identity, must be dropped.
		{"number": 1, "title": "orphan"},
	})
	connection, _ := newTestConnection(t, api, "token")
	references := NewReferences(connection)

	candidates, err := references.Search(context.Background(), "workspace-1", "widgets", 10)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	require.Equal(t, "7:42", candidates[0].ProviderLocalID, "candidate ids use immutable numeric ids")
	require.Equal(t, "Add widgets", candidates[0].Title)
	require.Equal(t, "acme/widgets#42", candidates[0].Attributes["label"])
	require.Equal(t, "open", candidates[0].Attributes["state"])
	// owner/name recovered from full_name when the flat fields are absent.
	require.Equal(t, "acme/other#9", candidates[1].Attributes["label"])
	require.Equal(t, "merged", candidates[1].Attributes["state"])
}

func TestSearchRespectsLimit(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/issues/search", http.StatusOK, []map[string]any{
		{"number": 1, "title": "a", "repository": map[string]any{"id": 7, "owner": "o", "name": "r"}},
		{"number": 2, "title": "b", "repository": map[string]any{"id": 7, "owner": "o", "name": "r"}},
	})
	connection, _ := newTestConnection(t, api, "token")

	candidates, err := NewReferences(connection).Search(context.Background(), "workspace-1", "", 1)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
}

func TestSearchIsQuietWhenUnconfiguredOrUnauthorized(t *testing.T) {
	t.Parallel()
	host := newFakeHost(map[string]any{})
	unconfigured := NewReferences(NewConnection(func() pluginsdk.Host { return host }))
	candidates, err := unconfigured.Search(context.Background(), "workspace-1", "", 10)
	require.NoError(t, err)
	require.Empty(t, candidates)

	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/issues/search", http.StatusForbidden, nil)
	connection, _ := newTestConnection(t, api, "token")
	candidates, err = NewReferences(connection).Search(context.Background(), "workspace-1", "", 10)
	require.NoError(t, err)
	require.Empty(t, candidates)
}

// Authorization is a live check and fails closed for every denial path.
func TestAuthorizeFailsClosed(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "widgets"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusOK, map[string]any{"number": 42})
	api.Handle(http.MethodGet, "/api/v1/repositories/8", http.StatusNotFound, nil)
	api.Handle(http.MethodGet, "/api/v1/repositories/9", http.StatusOK, repoPayload(9, "acme", "gone"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/gone/pulls/1", http.StatusNotFound, nil)
	connection, _ := newTestConnection(t, api, "token")
	references := NewReferences(connection)

	allowed, err := references.Authorize(context.Background(), "workspace-1", "submission", map[string]any{"id": "7:42"})
	require.NoError(t, err)
	require.True(t, allowed)

	for _, testCase := range []struct {
		name      string
		reference map[string]any
	}{
		{name: "nil reference", reference: nil},
		{name: "missing id", reference: map[string]any{"title": "x"}},
		{name: "unparseable id", reference: map[string]any{"id": "not-an-id"}},
		{name: "zero ids", reference: map[string]any{"id": "0:0"}},
		{name: "repository no longer visible", reference: map[string]any{"id": "8:1"}},
		{name: "pull request deleted", reference: map[string]any{"id": "9:1"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			allowed, err := references.Authorize(context.Background(), "workspace-1", "submission", testCase.reference)
			require.NoError(t, err)
			require.False(t, allowed)
		})
	}
}

// Kandev echoes the plugin's own ProviderLocalID back under `key` as well.
func TestAuthorizeAcceptsKeyField(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repositories/7", http.StatusOK, repoPayload(7, "acme", "widgets"))
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusOK, map[string]any{"number": 42})
	connection, _ := newTestConnection(t, api, "token")

	allowed, err := NewReferences(connection).Authorize(context.Background(), "workspace-1", "search", map[string]any{"key": "7:42"})
	require.NoError(t, err)
	require.True(t, allowed)
}
