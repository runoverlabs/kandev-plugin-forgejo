package kandev

import (
	"context"
	"sync"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// sharedConfig is one RPC's plugin config, read from the Host at most once.
type sharedConfig struct {
	once   sync.Once
	config map[string]any
	err    error
}

type sharedConfigKey struct{}

// ShareConfig returns a context in which Config reads the plugin config from
// the Host once and hands every later caller the same answer. The transport
// applies it per RPC: one RPC reaches the instance through several adapter
// calls, and each would otherwise pay a GetConfig round trip of its own.
// Kandev restarts the plugin when its config changes, so an answer shared
// within one RPC is never stale.
func ShareConfig(ctx context.Context) context.Context {
	if _, ok := ctx.Value(sharedConfigKey{}).(*sharedConfig); ok {
		return ctx
	}
	return context.WithValue(ctx, sharedConfigKey{}, &sharedConfig{})
}

// Config reads the plugin config through host, once per ShareConfig context.
// Outside one, every call reads it afresh. A failed read is shared too: the
// RPC fails once rather than retrying the Host in every adapter call.
func Config(ctx context.Context, host pluginsdk.Host) (map[string]any, error) {
	shared, ok := ctx.Value(sharedConfigKey{}).(*sharedConfig)
	if !ok {
		return host.GetConfig(ctx)
	}
	shared.once.Do(func() { shared.config, shared.err = host.GetConfig(ctx) })
	return shared.config, shared.err
}
