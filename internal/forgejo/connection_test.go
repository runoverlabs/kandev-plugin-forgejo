package forgejo

import (
	"context"
	"kandev-plugin-forgejo/internal/scm"
	"net/http"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestConnectionRequiresCompleteConfig(t *testing.T) {
	t.Parallel()
	for _, config := range []map[string]any{
		{},
		{"base_url": "https://forge.example.com"},
		{"api_token": "token"},
		{"base_url": "  ", "api_token": "token"},
		{"base_url": "https://forge.example.com", "api_token": 42},
	} {
		host := newFakeHost(config)
		_, err := NewConnection(func() pluginsdk.Host { return host }).Client(context.Background())
		require.ErrorIs(t, err, scm.ErrNotConfigured)
	}
}

func TestConnectionWithoutHostIsNotConfigured(t *testing.T) {
	t.Parallel()
	_, err := NewConnection(func() pluginsdk.Host { return nil }).Client(context.Background())
	require.ErrorIs(t, err, scm.ErrNotConfigured)
}

// A rotated token must not be served from the cache.
func TestConnectionRebuildsClientWhenConfigChanges(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	host := newFakeHost(map[string]any{"base_url": api.url(), "api_token": "first"})
	connection := NewConnection(func() pluginsdk.Host { return host })

	client, err := connection.Client(context.Background())
	require.NoError(t, err)
	_, err = client.CurrentUser(context.Background())
	require.NoError(t, err)
	require.Equal(t, "token first", api.authSeen[0])

	again, err := connection.Client(context.Background())
	require.NoError(t, err)
	require.Same(t, client, again, "unchanged config reuses the cached client")

	host.config = map[string]any{"base_url": api.url(), "api_token": "rotated"}
	rotated, err := connection.Client(context.Background())
	require.NoError(t, err)
	require.NotSame(t, client, rotated)
	_, err = rotated.CurrentUser(context.Background())
	require.NoError(t, err)
	require.Equal(t, "token rotated", api.authSeen[len(api.authSeen)-1])
}

func TestConnectionScopeIsNormalized(t *testing.T) {
	t.Parallel()
	host := newFakeHost(map[string]any{"base_url": "https://forge.example.com/", "api_token": "t"})
	scope, err := NewConnection(func() pluginsdk.Host { return host }).Scope(context.Background())
	require.NoError(t, err)
	require.Equal(t, "https://forge.example.com", scope)
}

func TestConnectionRejectsInvalidInstanceURL(t *testing.T) {
	t.Parallel()
	host := newFakeHost(map[string]any{"base_url": "ftp://forge.example.com", "api_token": "t"})
	_, err := NewConnection(func() pluginsdk.Host { return host }).Client(context.Background())
	require.ErrorContains(t, err, "must be http or https")
	require.ErrorIs(t, err, scm.ErrInvalidConfig, "a bad URL is fixed in settings, not by a retry")
}
