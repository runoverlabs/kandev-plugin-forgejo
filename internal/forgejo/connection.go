package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// Connection turns the plugin's operator config into a ready REST client. The
// config is fixed for the life of the process (see kandev.Config) and a client
// holds nothing else, so one is built per call rather than cached.
type Connection struct {
	config     *kandev.Config
	httpClient *http.Client
}

// NewConnection returns a Connection that resolves config through hosts.
func NewConnection(hosts kandev.Provider) *Connection {
	return &Connection{
		config: kandev.NewConfig(hosts),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Client resolves the configured instance client.
func (c *Connection) Client(ctx context.Context) (*rest.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := c.config.Get(ctx)
	if errors.Is(err, kandev.ErrNoHost) {
		return nil, scm.ErrNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("forgejo: read plugin config: %w", err)
	}
	baseURL := configString(config, "base_url")
	token := configString(config, "api_token")
	if baseURL == "" || token == "" {
		return nil, scm.ErrNotConfigured
	}
	client, err := rest.NewClient(baseURL, token, c.httpClient)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", scm.ErrInvalidConfig, err)
	}
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
