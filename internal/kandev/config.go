package kandev

import (
	"context"
	"sync"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// sharedConfig is one unit of work's plugin config: the first successful read
// from the Host, reused by every later caller.
type sharedConfig struct {
	mu     sync.Mutex
	config map[string]any
	read   bool
}

type sharedConfigKey struct{}

// ShareConfig returns a context in which Config reads the plugin config from
// the Host once and hands every later caller the same answer. The transport
// applies it per RPC and the background loops per pass: one unit of work
// reaches the instance through several adapter calls, and each would
// otherwise pay a GetConfig round trip of its own. Kandev restarts the plugin
// when its config changes, so an answer shared within one is never stale.
func ShareConfig(ctx context.Context) context.Context {
	if _, ok := ctx.Value(sharedConfigKey{}).(*sharedConfig); ok {
		return ctx
	}
	return context.WithValue(ctx, sharedConfigKey{}, &sharedConfig{})
}

// Config reads the plugin config through host, once per ShareConfig context.
// Outside one, every call reads it afresh. Only a successful read is shared:
// a failure, which may be the caller's own context ending, is that caller's
// alone, and the next caller asks the Host again.
func Config(ctx context.Context, host pluginsdk.Host) (map[string]any, error) {
	shared, ok := ctx.Value(sharedConfigKey{}).(*sharedConfig)
	if !ok {
		return host.GetConfig(ctx)
	}
	// Held across the read, so concurrent callers wait for it rather than
	// each issuing their own.
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.read {
		return shared.config, nil
	}
	config, err := host.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	shared.config, shared.read = config, true
	return config, nil
}
