package watches

import (
	"context"
	"strings"
	"sync"
	"time"

	"kandev-plugin-forgejo/internal/kandev"
)

// WorkspaceGate reports whether this integration is switched on for a
// workspace. A watch in a workspace where the operator turned the integration
// off must stop polling, not merely stop rendering — otherwise the toggle is
// decorative while tasks keep appearing.
type WorkspaceGate func(ctx context.Context, workspaceID string) (bool, error)

// defaultTick is how often the loop looks for due watches. It is not the poll
// interval: each watch has its own, and the loop only wakes often enough to
// honour the shortest one with reasonable granularity.
const defaultTick = 30 * time.Second

// Poller runs due watches on an interval.
//
// One goroutine serves every workspace: the work is IO-bound on the Forgejo
// instance and the Host RPC, and a goroutine per watch would multiply
// rate-limit pressure without finishing sooner.
type Poller struct {
	store   *Store
	hosts   kandev.Provider
	issues  IssueSource
	reviews ReviewSource
	gate    WorkspaceGate
	tick    time.Duration
	now     func() time.Time
	logf    func(format string, args ...any)

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

// NewPoller wires a poller. gate and logf may be nil.
func NewPoller(store *Store, hosts kandev.Provider, issues IssueSource, gate WorkspaceGate) *Poller {
	return &Poller{
		store:  store,
		hosts:  hosts,
		issues: issues,
		gate:   gate,
		tick:   defaultTick,
		now:    time.Now,
		logf:   func(string, ...any) {},
	}
}

// Start begins the loop. It is idempotent: a second call while running is a
// no-op, because SetHost is the only caller and a host re-injection must not
// leave two loops racing for the same watches.
func (p *Poller) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	p.cancel, p.stopped = cancel, stopped
	go func() {
		defer close(stopped)
		p.run(ctx)
	}()
}

// Stop halts the loop and waits for the in-flight tick to finish.
func (p *Poller) Stop() {
	p.mu.Lock()
	cancel, stopped := p.cancel, p.stopped
	p.cancel, p.stopped = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-stopped
}

func (p *Poller) run(ctx context.Context) {
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// One slow tick must not stack up behind the next. The ticker
			// drops intervening fires while this is running, which is the
			// behaviour we want: a watch that is late is polled once, not
			// once per missed tick.
			p.Tick(ctx)
		}
	}
}

// Tick runs one pass over every workspace, polling the watches that are due.
// It is exported so a test can drive the loop deterministically instead of
// waiting on wall-clock ticks.
func (p *Poller) Tick(ctx context.Context) {
	host, err := p.store.host()
	if err != nil {
		// The broker dial has not finished. The next tick will find it.
		return
	}
	workspaces, err := kandev.WorkspaceIDs(ctx, host)
	if err != nil {
		p.logf("watches: list workspaces: %v", err)
		return
	}
	for _, workspaceID := range workspaces {
		if ctx.Err() != nil {
			return
		}
		p.tickWorkspace(ctx, workspaceID)
	}
}

func (p *Poller) tickWorkspace(ctx context.Context, workspaceID string) {
	if p.gate != nil {
		enabled, err := p.gate(ctx, workspaceID)
		if err != nil {
			p.logf("watches: enabled check for %s: %v", workspaceID, err)
			return
		}
		if !enabled {
			return
		}
	}
	watches, err := p.store.List(ctx, workspaceID)
	if err != nil {
		p.logf("watches: list watches for %s: %v", workspaceID, err)
		return
	}
	now := p.now().UTC()
	for _, watch := range watches {
		if ctx.Err() != nil {
			return
		}
		if !watch.Enabled || !watch.Due(now) {
			continue
		}
		if _, err := p.RunWatch(ctx, watch); err != nil {
			p.logf("watches: run %s: %v", watch.ID, err)
		}
	}
}

// Result reports what one watch run did. It is returned to the panel's manual
// "run now" action so an operator gets an answer rather than a spinner.
type Result struct {
	WatchID string `json:"watch_id"`
	// Matched is how many non-pull-request issues the filters returned.
	Matched int `json:"matched"`
	// Created is how many became new tasks.
	Created int `json:"created"`
	// Duplicates is how many were already in the ledger.
	Duplicates int `json:"duplicates"`
	// Throttled is how many matched but were left for a later run because the
	// inflight budget was full.
	Throttled int `json:"throttled"`
	// Inflight is the budget reading taken at the start of the run.
	Inflight int      `json:"inflight"`
	Budget   int      `json:"budget"`
	Errors   []string `json:"errors,omitempty"`

	// Review watches only. Drafts counts pull requests left out because they
	// are drafts. SkippedForks counts fork pull requests left out because no
	// placement for them was safe. Cleaned and Completed report the cleanup
	// pass that follows discovery.
	Drafts       int `json:"drafts,omitempty"`
	SkippedForks int `json:"skipped_forks,omitempty"`
	Archived     int `json:"archived,omitempty"`
	Completed    int `json:"completed,omitempty"`
	// CleanupNote says why tasks were completed rather than archived.
	CleanupNote string `json:"cleanup_note,omitempty"`
}

// RunWatch polls one watch once and creates tasks for issues it has not seen.
//
// A per-repository failure is collected rather than fatal: one unreachable
// repository in a five-repository watch must not stop the other four. The
// watch is only marked failed if every repository failed, because a watch
// showing a persistent error while quietly working is worse than one that
// reports the repository it cannot read.
func (p *Poller) RunWatch(ctx context.Context, watch Watch) (Result, error) {
	result := Result{WatchID: watch.ID, Budget: watch.MaxInflightTasks}
	host, err := p.store.host()
	if err != nil {
		return result, err
	}

	inflight, err := p.inflight(ctx, host, watch)
	if err != nil {
		// A budget that cannot be read is treated as exhausted. Creating
		// tasks with an unknown inflight count is exactly the failure the
		// budget exists to prevent.
		_ = p.store.MarkError(ctx, watch, "Could not read the current task count for this watch.")
		return result, err
	}
	result.Inflight = inflight

	var failures []string
	if watch.IsReview() {
		failures = p.pollReviews(ctx, host, watch, &result)
	} else {
		failures = p.pollIssues(ctx, host, watch, &result)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// Either way the poll clock advances, so a watch against an unreachable
	// instance backs off to its interval instead of retrying every tick. The
	// difference is only whether the panel has an error to show.
	result.Errors = failures
	if watch.IsReview() && watch.CleanupPolicy == CleanupWhenClosed {
		// The note follows the latest pass that did something: it clears once
		// archiving works, and appears once it stops.
		switch {
		case result.CleanupNote != "":
			watch.LastCleanupNote = result.CleanupNote
		case result.Archived > 0:
			watch.LastCleanupNote = ""
		}
	}
	var mark error
	if len(failures) == 0 {
		mark = p.store.MarkPolled(ctx, watch)
	} else {
		mark = p.store.MarkError(ctx, watch, strings.Join(failures, "; "))
	}
	if mark != nil {
		return result, mark
	}
	return result, nil
}
