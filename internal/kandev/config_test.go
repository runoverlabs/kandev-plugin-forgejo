package kandev

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// configCounter serves GetConfig and counts the reads; nothing else is called.
type configCounter struct {
	pluginsdk.Host
	reads atomic.Int64
	err   error
}

func (c *configCounter) GetConfig(context.Context) (map[string]any, error) {
	c.reads.Add(1)
	return map[string]any{"base_url": "https://forgejo.example"}, c.err
}

func TestConfigIsReadOncePerSharedContext(t *testing.T) {
	t.Parallel()
	host := &configCounter{}
	ctx := ShareConfig(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			config, err := Config(ctx, host)
			require.NoError(t, err)
			require.Equal(t, "https://forgejo.example", config["base_url"])
		})
	}
	wg.Wait()
	require.Equal(t, int64(1), host.reads.Load())

	// Sharing again inside the same RPC keeps the one read.
	_, err := Config(ShareConfig(ctx), host)
	require.NoError(t, err)
	require.Equal(t, int64(1), host.reads.Load())
}

func TestConfigOutsideASharedContextReadsEveryTime(t *testing.T) {
	t.Parallel()
	host := &configCounter{}
	for range 3 {
		_, err := Config(context.Background(), host)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), host.reads.Load())
}

func TestConfigSharesAFailedRead(t *testing.T) {
	t.Parallel()
	host := &configCounter{err: errors.New("broker unavailable")}
	ctx := ShareConfig(context.Background())
	_, first := Config(ctx, host)
	_, second := Config(ctx, host)
	require.Error(t, first)
	require.Equal(t, first, second)
	require.Equal(t, int64(1), host.reads.Load())
}
