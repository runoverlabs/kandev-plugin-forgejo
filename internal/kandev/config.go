package kandev

import (
	"context"
	"maps"
	"sync"
)

// Config is the plugin config, read from the Host once per process. Kandev
// restarts a plugin's process whenever its config is saved, so a successful
// read stays current for the life of the process. A failed read is not kept:
// it may be the caller's own context ending, and the next caller asks again.
type Config struct {
	hosts Provider

	mu     sync.Mutex
	config map[string]any
}

// NewConfig returns a Config that reads through hosts.
func NewConfig(hosts Provider) *Config {
	return &Config{hosts: hosts}
}

// Get returns a copy of the plugin config, or ErrNoHost while the Host has not
// arrived.
// The lock is not held across the Host call, so callers racing the first read
// each make their own, under their own context, rather than queue behind one.
func (c *Config) Get(ctx context.Context) (map[string]any, error) {
	c.mu.Lock()
	config := c.config
	c.mu.Unlock()
	if config != nil {
		return maps.Clone(config), nil
	}
	host, err := c.hosts.Get()
	if err != nil {
		return nil, err
	}
	config, err = host.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	if config == nil {
		config = map[string]any{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config == nil {
		c.config = config
	}
	return maps.Clone(c.config), nil
}
