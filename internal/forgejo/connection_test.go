package forgejo

import (
	"context"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/scm"

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

// Kandev restarts the plugin when its config is saved, so a process reads it
// once.
func TestConnectionReadsConfigOncePerProcess(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/user", http.StatusOK, map[string]any{"login": "kandev"})
	host := newFakeHost(map[string]any{"base_url": api.URL(), "api_token": "first"})
	connection := NewConnection(func() pluginsdk.Host { return host })

	client, err := connection.Client(context.Background())
	require.NoError(t, err)
	_, err = client.CurrentUser(context.Background())
	require.NoError(t, err)
	require.Equal(t, "token first", api.AuthSeen()[0])

	host.mu.Lock()
	host.config = map[string]any{"base_url": api.URL(), "api_token": "unread"}
	host.mu.Unlock()
	again, err := connection.Client(context.Background())
	require.NoError(t, err)
	_, err = again.CurrentUser(context.Background())
	require.NoError(t, err)
	require.Equal(t, "token first", api.AuthSeen()[1])
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
