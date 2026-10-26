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

func TestConfigIsReadOncePerProcess(t *testing.T) {
	t.Parallel()
	host := &configCounter{}
	config := NewConfig(func() pluginsdk.Host { return host })
	_, err := config.Get(context.Background())
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			read, err := config.Get(context.Background())
			require.NoError(t, err)
			require.Equal(t, "https://forgejo.example", read["base_url"])
		})
	}
	wg.Wait()
	require.Equal(t, int64(1), host.reads.Load())
}

func TestConfigDoesNotKeepAFailedRead(t *testing.T) {
	t.Parallel()
	host := &configCounter{err: errors.New("broker unavailable")}
	config := NewConfig(func() pluginsdk.Host { return host })
	_, err := config.Get(context.Background())
	require.Error(t, err)

	// One caller's failure, such as its own context ending, is not the next
	// caller's answer.
	host.err = nil
	read, err := config.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, "https://forgejo.example", read["base_url"])
	_, err = config.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), host.reads.Load())
}

func TestConfigWithoutHost(t *testing.T) {
	t.Parallel()
	_, err := NewConfig(func() pluginsdk.Host { return nil }).Get(context.Background())
	require.ErrorIs(t, err, ErrNoHost)
}

func TestConfigHandsOutCopies(t *testing.T) {
	t.Parallel()
	config := NewConfig(func() pluginsdk.Host { return &configCounter{} })
	read, err := config.Get(context.Background())
	require.NoError(t, err)
	read["base_url"] = "https://changed.example"
	again, err := config.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, "https://forgejo.example", again["base_url"])
}
