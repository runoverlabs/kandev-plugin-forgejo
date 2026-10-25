package prautomation

import (
	"context"
	"sync"
)

// keyedMutex serializes read-modify-write cycles on one record: the loop, the
// panel's actions, the agent's outcome and event hints all write records, and
// the Host state API has no compare-and-swap.
//
// A lock is a one-slot channel rather than a sync.Mutex so that waiting for it
// gives up with the caller's context: an evaluation holds it across provider
// requests, and an event hint must not wait on that past its deadline.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	slot chan struct{}
	refs int
}

func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedLock{slot: make(chan struct{}, 1)}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	release := func() {
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
	return func() {
		<-entry.slot
		release()
	}, nil
}
