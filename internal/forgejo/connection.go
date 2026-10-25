package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// Connection turns the plugin's operator config into a ready REST client. The
// resolved client is cached and rebuilt whenever the operator changes the
// instance URL or token. Kandev restarts a plugin on config change, so the
// cache exists to avoid a GetConfig round trip per action, not to survive
// rotation.
type Connection struct {
	hosts      kandev.Provider
	httpClient *http.Client

	mu     sync.Mutex
	client *rest.Client
	key    string
}

// NewConnection returns a Connection that resolves config through hosts.
func NewConnection(hosts kandev.Provider) *Connection {
	return &Connection{
		hosts: hosts,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Client resolves the configured instance client, rebuilding it when the
// operator config has changed since the last call.
func (c *Connection) Client(ctx context.Context) (*rest.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host := c.hosts()
	if host == nil {
		return nil, scm.ErrNotConfigured
	}
	config, err := host.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("forgejo: read plugin config: %w", err)
	}
	baseURL := configString(config, "base_url")
	token := configString(config, "api_token")
	if baseURL == "" || token == "" {
		return nil, scm.ErrNotConfigured
	}

	// The cache key includes the token so a rotated credential is never
	// served from cache. It stays in memory and is never logged.
	key := baseURL + "\x00" + token

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil && c.key == key {
		return c.client, nil
	}
	client, err := rest.NewClient(baseURL, token, c.httpClient)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", scm.ErrInvalidConfig, err)
	}
	c.client, c.key = client, key
	return client, nil
}

// Scope returns the connection scope (the normalized instance URL) for the
// current configuration. It is the value stored alongside every association.
func (c *Connection) Scope(ctx context.Context) (string, error) {
	client, err := c.Client(ctx)
	if err != nil {
		return "", err
	}
	return client.Scope(), nil
}

// Info reports the configured instance without contacting it.
func (c *Connection) Info(ctx context.Context) (sourcecontrol.ConnectionInfo, error) {
	client, err := c.Client(ctx)
	if err != nil {
		return sourcecontrol.ConnectionInfo{}, err
	}
	return sourcecontrol.ConnectionInfo{URL: client.Scope(), Host: client.Host()}, nil
}

// Probe performs the live check: who the token acts as, and what the instance
// is running. Only the account is required; a hidden version is not a failure.
func (c *Connection) Probe(ctx context.Context) (sourcecontrol.ConnectionProbe, error) {
	client, err := c.Client(ctx)
	if err != nil {
		return sourcecontrol.ConnectionProbe{}, err
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return sourcecontrol.ConnectionProbe{}, err
	}
	probe := sourcecontrol.ConnectionProbe{Account: user.Login}
	if version, err := client.Version(ctx); err == nil && strings.TrimSpace(version) != "" {
		probe.Version = version
		probe.Flavor = string(client.DetectFlavor(ctx, version))
	}
	return probe, nil
}

// configString reads a trimmed string value out of a plugin config map.
func configString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	value, ok := config[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
