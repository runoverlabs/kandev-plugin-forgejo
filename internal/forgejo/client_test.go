package forgejo

import (
	"context"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/stretchr/testify/require"
)

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "strips trailing slash", input: "https://code.example.com/", want: "https://code.example.com"},
		{name: "keeps base path", input: "https://example.com/forge/", want: "https://example.com/forge"},
		{name: "drops query and fragment", input: "https://example.com/?a=1#x", want: "https://example.com"},
		{name: "allows http for lan instances", input: "http://forge.lan:3000", want: "http://forge.lan:3000"},
		{name: "rejects empty", input: "   ", wantErr: true},
		{name: "rejects non-http scheme", input: "ssh://example.com", wantErr: true},
		{name: "rejects missing host", input: "https://", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeBaseURL(testCase.input)
			if testCase.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestNewClientRequiresToken(t *testing.T) {
	t.Parallel()
	_, err := NewClient("https://example.com", "  ", nil)
	require.ErrorContains(t, err, "access token is required")
}

func TestClientSendsTokenSchemeAndMapsStatuses(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	api.handle(http.MethodGet, "/api/v1/repos/owner/missing", http.StatusNotFound, nil)
	api.handle(http.MethodGet, "/api/v1/repos/owner/forbidden", http.StatusForbidden, nil)
	api.handle(http.MethodGet, "/api/v1/repos/owner/broken", http.StatusInternalServerError, nil)

	client, err := NewClient(api.url(), "secret-token", nil)
	require.NoError(t, err)

	user, err := client.CurrentUser(context.Background())
	require.NoError(t, err)
	require.Equal(t, "kandev", user.Login)
	require.Equal(t, "token secret-token", api.authSeen[0], "both Forgejo and Gitea require the `token <value>` scheme")

	_, err = client.Repo(context.Background(), "owner", "missing")
	require.ErrorIs(t, err, scm.ErrNotFound)

	_, err = client.Repo(context.Background(), "owner", "forbidden")
	require.ErrorIs(t, err, scm.ErrUnauthorized)

	_, err = client.Repo(context.Background(), "owner", "broken")
	require.Error(t, err)
	require.NotErrorIs(t, err, scm.ErrNotFound)
}

// A transport error must not surface the presented credential.
func TestClientErrorsDoNotLeakToken(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/owner/broken", http.StatusBadGateway, map[string]any{
		"message": "upstream said token=super-secret-token",
	})
	client, err := NewClient(api.url(), "super-secret-token", nil)
	require.NoError(t, err)

	_, err = client.Repo(context.Background(), "owner", "broken")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "super-secret-token")
}

func TestClientHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	client, err := NewClient(api.url(), "token", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.CurrentUser(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// Owner and repository names are provider data and must be path-escaped.
func TestClientEscapesPathSegments(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	// The fake server routes on the DECODED path; the assertion below proves
	// the wire form stayed escaped.
	api.handle(http.MethodGet, "/api/v1/repos/owner/weird/name", http.StatusOK, map[string]any{"id": 1})
	client, err := NewClient(api.url(), "token", nil)
	require.NoError(t, err)

	_, err = client.Repo(context.Background(), "owner", "weird/name")
	require.NoError(t, err)
	require.Contains(t, api.requests[0].RequestURI, "weird%2Fname",
		"a slash in a repository name must stay escaped, not split into a new path segment")
}

// Flavor is display-only, but it must survive Forgejo dropping the
// "+gitea-<compat>" suffix it currently publishes.
func TestDetectFlavor(t *testing.T) {
	t.Parallel()

	t.Run("forgejo namespace present", func(t *testing.T) {
		t.Parallel()
		api := newAPIServer(t)
		api.handle(http.MethodGet, "/api/forgejo/v1/version", http.StatusOK, map[string]any{"version": "16.0.5+gitea-1.22.0"})
		client, err := NewClient(api.url(), "token", nil)
		require.NoError(t, err)
		require.Equal(t, FlavorForgejo, client.DetectFlavor(context.Background(), "16.0.5+gitea-1.22.0"))
	})

	t.Run("forgejo that no longer advertises gitea compatibility", func(t *testing.T) {
		t.Parallel()
		api := newAPIServer(t)
		api.handle(http.MethodGet, "/api/forgejo/v1/version", http.StatusOK, map[string]any{"version": "20.0.0"})
		client, err := NewClient(api.url(), "token", nil)
		require.NoError(t, err)
		require.Equal(t, FlavorForgejo, client.DetectFlavor(context.Background(), "20.0.0"),
			"the namespace probe must not depend on the version suffix")
	})

	t.Run("gitea does not serve the forgejo namespace", func(t *testing.T) {
		t.Parallel()
		api := newAPIServer(t)
		// /api/forgejo/v1/version intentionally unregistered -> 404.
		client, err := NewClient(api.url(), "token", nil)
		require.NoError(t, err)
		require.Equal(t, FlavorGitea, client.DetectFlavor(context.Background(), "1.24.7"))
	})

	t.Run("falls back to the version suffix when the namespace is unreachable", func(t *testing.T) {
		t.Parallel()
		api := newAPIServer(t)
		api.handle(http.MethodGet, "/api/forgejo/v1/version", http.StatusBadGateway, nil)
		client, err := NewClient(api.url(), "token", nil)
		require.NoError(t, err)
		require.Equal(t, FlavorForgejo, client.DetectFlavor(context.Background(), "16.0.5+gitea-1.22.0"))
		require.Equal(t, FlavorGitea, client.DetectFlavor(context.Background(), "1.24.7"))
	})

	t.Run("probe reports an unreachable namespace instead of guessing", func(t *testing.T) {
		t.Parallel()
		api := newAPIServer(t)
		api.handle(http.MethodGet, "/api/forgejo/v1/version", http.StatusBadGateway, nil)
		client, err := NewClient(api.url(), "token", nil)
		require.NoError(t, err)
		_, err = client.ProbeFlavor(context.Background(), "20.0.0")
		require.Error(t, err)
	})

	t.Run("probe treats a missing namespace as an answer", func(t *testing.T) {
		t.Parallel()
		api := newAPIServer(t)
		client, err := NewClient(api.url(), "token", nil)
		require.NoError(t, err)
		flavor, err := client.ProbeFlavor(context.Background(), "1.24.7")
		require.NoError(t, err)
		require.Equal(t, FlavorGitea, flavor)
	})
}
