package prautomation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Timing. The loop wakes every tick and evaluates the records that are due;
// each record sets its own next due time from what it is waiting for.
const (
	defaultTick = 15 * time.Second
	// activeInterval: an attempt in flight, auto-merge armed, or CI running
	// under auto-fix. Short enough that a green build merges within a tick or
	// two.
	activeInterval = 20 * time.Second
	idleInterval   = 60 * time.Second
	// closedInterval: a closed pull request can reopen, which re-arms the
	// closed prompt, but nothing else happens to it.
	closedInterval = 10 * time.Minute
	errorInterval  = 5 * time.Minute
	// pendingInterval: a lifecycle prompt waiting for a session.
	pendingInterval = 30 * time.Second
	// progressWindow: after action_taken, how long the agent's change has to
	// show (a new head) before new feedback is sent anyway. Kandev's GitHub
	// integration uses the same two minutes.
	progressWindow = 2 * time.Minute
	// idleGrace: a dispatched attempt whose session is no longer running and
	// that reported nothing for this long ended without an outcome. The
	// turn.completed event usually says so sooner; this is the fallback when
	// an event is lost.
	idleGrace = 2 * time.Minute
	// startGrace: Kandev starts a never-launched session when it is messaged.
	// One still CREATED this long after the prompt never started; the launch
	// is best-effort and its failure is not reported.
	startGrace = 5 * time.Minute
	// retryCooldown: the least time between an auto-fix round and its resend
	// after the turn ended without an outcome. Without it an agent that never
	// reports (the tool unavailable, say) would use up every round in a few
	// minutes.
	retryCooldown = 2 * time.Minute
	// noCIGrace: how long a head with no checks at all must stay that way
	// before auto-merge takes the repository for one without CI. Right after
	// a push, CI has often not reported yet, and "nothing reported" must not
	// read as "nothing to wait for".
	noCIGrace = 2 * time.Minute
	// blockedHold: after blocked, how long auto-merge waits on the same head.
	blockedHold = time.Hour
	// budgetPerWorkspace bounds the observations one tick makes in one
	// workspace, so a large board cannot hammer the instance.
	budgetPerWorkspace = 20
	maxSummaryBytes    = 4096
	// hintTimeout bounds an event hint, most of it spent waiting for a record
	// an evaluation holds. The hint only brings work forward; the loop does it
	// anyway.
	hintTimeout = 30 * time.Second
)

// Errors the outcome report can fail with. Their text is shown to the agent.
var (
	ErrNoAttempt        = errors.New("no auto-fix attempt is waiting for an outcome on this task")
	ErrAmbiguousAttempt = errors.New("several linked pull requests are waiting for an auto-fix outcome; pass repo to pick one")
	ErrNotFound         = errors.New("this pull request is not linked to the task")
)

// Engine runs pull request automation for every workspace.
type Engine struct {
	store     *Store
	links     Links
	observer  Observer
	merger    Merger
	messenger Messenger
	// gate reports whether the integration is switched on for a workspace.
	gate func(ctx context.Context, workspaceID string) (bool, error)
	// workspaces lists the workspaces to serve.
	workspaces func(ctx context.Context) ([]string, error)
	// describe turns an error into fixed, operator-facing text. The provider
	// adapter's errors can carry anything; only this text is stored.
	describe func(error) string

	tick  time.Duration
	now   func() time.Time
	newID func() string
	logf  func(format string, args ...any)

	locks keyedMutex

	mu      sync.Mutex
	tracked map[string]trackedTask // by task id, rebuilt every tick
	// meanwhile holds what track adds while a tick rebuilds tracked, which
	// the rebuilt index would otherwise drop. Nil between ticks.
	meanwhile map[string]trackedTask
	running   context.Context
	cancel    context.CancelFunc
	stopped   chan struct{}
	hints     sync.WaitGroup
}

// Config wires an Engine.
type Config struct {
	Store      *Store
	Links      Links
	Observer   Observer
	Merger     Merger
	Messenger  Messenger
	Gate       func(ctx context.Context, workspaceID string) (bool, error)
	Workspaces func(ctx context.Context) ([]string, error)
	Describe   func(error) string
	Logf       func(format string, args ...any)
}

// NewEngine returns an engine. Start begins its loop.
func NewEngine(config Config) *Engine {
	engine := &Engine{
		store:      config.Store,
		links:      config.Links,
		observer:   config.Observer,
		merger:     config.Merger,
		messenger:  config.Messenger,
		gate:       config.Gate,
		workspaces: config.Workspaces,
		describe:   config.Describe,
		tick:       defaultTick,
		now:        time.Now,
		newID:      randomID,
		logf:       config.Logf,
		tracked:    map[string]trackedTask{},
	}
	if engine.describe == nil {
		engine.describe = func(error) string { return "The instance could not be read." }
	}
	if engine.logf == nil {
		engine.logf = func(string, ...any) {}
	}
	return engine
}

// Store returns the engine's store, for the action handlers.
func (e *Engine) Store() *Store { return e.store }

// Start begins the loop. A second call while running is a no-op.
func (e *Engine) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	e.running, e.cancel, e.stopped = ctx, cancel, stopped
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(e.tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.Tick(ctx)
			}
		}
	}()
}

// Stop halts the loop and waits for the tick in progress.
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel, stopped := e.cancel, e.stopped
	e.running, e.cancel, e.stopped = nil, nil, nil
	e.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-stopped
	e.hints.Wait()
}

// Hint runs an event handler off the caller's path, bounded by hintTimeout
// and the engine's lifetime. The host waits on event delivery with a short
// deadline and drops events when its queue backs up, while a handler can wait
// on a record an evaluation holds across provider requests. A hint while the
// engine is stopped is dropped: nothing would act on it.
func (e *Engine) Hint(handle func(ctx context.Context)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running == nil {
		return
	}
	ctx, cancel := context.WithTimeout(e.running, hintTimeout)
	e.hints.Add(1)
	go func() {
		defer e.hints.Done()
		defer cancel()
		handle(ctx)
	}()
}

func randomID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
