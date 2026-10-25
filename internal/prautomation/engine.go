package prautomation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// Session states, as the Host reports them.
const (
	sessionCreated         = "CREATED"
	sessionStarting        = "STARTING"
	sessionRunning         = "RUNNING"
	sessionWaitingForInput = "WAITING_FOR_INPUT"
	sessionIdle            = "IDLE"
	sessionCompleted       = "COMPLETED"
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

// Tick runs one pass over every workspace. Exported so tests drive the loop
// without waiting on the clock.
func (e *Engine) Tick(ctx context.Context) {
	if e.workspaces == nil {
		return
	}
	workspaces, err := e.workspaces(ctx)
	if err != nil {
		e.logf("prautomation: list workspaces: %v", err)
		return
	}
	e.mu.Lock()
	e.meanwhile = map[string]trackedTask{}
	e.mu.Unlock()
	tracked := map[string]trackedTask{}
	for _, workspaceID := range workspaces {
		if ctx.Err() != nil {
			e.mu.Lock()
			e.meanwhile = nil
			e.mu.Unlock()
			return
		}
		e.tickWorkspace(ctx, workspaceID, tracked)
	}
	e.mu.Lock()
	for taskID, task := range e.meanwhile {
		for _, ref := range task.refs {
			tracked[taskID] = tracked[taskID].with(task.workspaceID, ref)
		}
	}
	e.tracked, e.meanwhile = tracked, nil
	e.mu.Unlock()
}

func (e *Engine) tickWorkspace(ctx context.Context, workspaceID string, tracked map[string]trackedTask) {
	if e.gate != nil {
		enabled, err := e.gate(ctx, workspaceID)
		if err != nil || !enabled {
			// Switched off: nothing is observed, prompted or merged. Records
			// stay as they are and resume when the switch comes back on.
			return
		}
	}
	// Records before links: a record the panel creates for a pull request
	// linked after the links were read must not be taken for an unlinked one.
	// In this order such a record is not in the list, so it is left alone.
	records, err := e.store.List(ctx, workspaceID)
	if err != nil {
		e.logf("prautomation: list records in %s: %v", workspaceID, err)
		return
	}
	links, err := e.links.Linked(ctx, workspaceID)
	if err != nil {
		e.logf("prautomation: list links in %s: %v", workspaceID, err)
		return
	}
	byKey := make(map[string]Record, len(records))
	for _, record := range records {
		byKey[recordKey(record.Ref())] = record
	}
	settings, err := e.store.Settings(ctx, workspaceID)
	if err != nil {
		e.logf("prautomation: settings for %s: %v", workspaceID, err)
		return
	}

	linked := make(map[string]bool, len(links))
	for _, ref := range links {
		key := recordKey(ref)
		linked[key] = true
		if _, ok := byKey[key]; ok {
			continue
		}
		// Upsert, not Put: the panel may have created the record with a
		// human's choice since the list was read.
		record, err := e.Upsert(ctx, workspaceID, ref, func(record *Record, created bool) error {
			if created && !settings.Defaults.Any() {
				// Nothing to copy, so nothing to learn about the pull request
				// first: the record starts with every switch off.
				record.Seeded = true
			}
			return nil
		})
		if err != nil {
			e.logf("prautomation: create record: %v", err)
			continue
		}
		byKey[key] = record
	}
	for key, record := range byKey {
		if !linked[key] {
			// Unlinked from the task: automation for it ends with the link.
			if err := e.remove(ctx, workspaceID, record.Ref()); err != nil {
				e.logf("prautomation: delete record: %v", err)
			}
			delete(byKey, key)
		}
	}

	now := e.now().UTC()
	var due []Record
	for _, record := range byKey {
		if record.Switches.Any() || !record.Seeded {
			tracked[record.TaskID] = tracked[record.TaskID].with(workspaceID, record.Ref())
		}
		if e.isDue(record, now) {
			due = append(due, record)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextDue.Before(due[j].NextDue) })
	if len(due) > budgetPerWorkspace {
		due = due[:budgetPerWorkspace]
	}
	for _, record := range due {
		if ctx.Err() != nil {
			return
		}
		e.evaluate(ctx, workspaceID, record.Ref(), &settings)
	}
}

func (e *Engine) isDue(record Record, now time.Time) bool {
	if record.Seeded && (!record.Switches.Any() || record.Terminal()) {
		return false
	}
	// An unseeded record is due until it is seeded, at the pace its last
	// observation set: a pull request that cannot be read waits errorInterval.
	return !record.NextDue.After(now)
}

// Evaluate observes one pull request and acts on it. It is exported for the
// panel's refresh and for tests.
func (e *Engine) Evaluate(ctx context.Context, workspaceID string, ref PullRef) {
	e.evaluate(ctx, workspaceID, ref, nil)
}

// evaluate is Evaluate with the workspace settings a tick has already read;
// nil reads them.
func (e *Engine) evaluate(ctx context.Context, workspaceID string, ref PullRef, loaded *Settings) {
	unlock, err := e.locks.lock(ctx, recordKey(ref))
	if err != nil {
		return
	}
	defer unlock()

	record, found, err := e.store.Get(ctx, workspaceID, ref)
	if err != nil || !found {
		return
	}
	var settings Settings
	if loaded != nil {
		settings = *loaded
	} else if settings, err = e.store.Settings(ctx, workspaceID); err != nil {
		return
	}
	now := e.now().UTC()

	// An unseeded record is about to take the workspace defaults.
	switches := record.Switches
	if !record.Seeded {
		switches = settings.Defaults
	}
	reads := Reads{
		Feedback:      switches.AutoFix || switches.AutoMerge,
		ReviewRequest: switches.OnReviewRequested,
	}
	obs, err := e.observer.Observe(ctx, workspaceID, ref, reads)
	if err != nil {
		record.Error = e.describe(err)
		record.Status = "Could not read the pull request."
		record.NextDue = now.Add(errorInterval)
		e.save(ctx, workspaceID, &record)
		return
	}
	record.Error = ""

	if !record.Seeded {
		next := settings.Defaults
		if obs.Fork {
			// A fork's code and comments are its author's. Running an agent
			// on them unattended has to be a human's explicit choice for this
			// pull request, never a workspace default.
			next.AutoFix, next.AutoMerge = false, false
		}
		record.ApplySwitches(next)
	}
	record.Repository, record.Title, record.URL = obs.Repository, obs.Title, obs.URL
	if obs.HeadSHA != record.HeadSHA || record.HeadSeenAt.IsZero() {
		record.HeadSeenAt = now
	}
	record.HeadSHA, record.Fork, record.Observed = obs.HeadSHA, obs.Fork, now

	run := &evaluation{engine: e, ctx: ctx, workspaceID: workspaceID, record: &record, obs: obs, settings: settings, now: now}
	run.lifecycle()
	run.autoFix()
	run.autoMerge()
	run.deliverPending()
	record.NextDue = run.nextDue()
	e.save(ctx, workspaceID, &record)
}

func (e *Engine) save(ctx context.Context, workspaceID string, record *Record) {
	if err := e.put(ctx, workspaceID, record); err != nil {
		e.logf("prautomation: save record: %v", err)
	}
}

func (e *Engine) put(ctx context.Context, workspaceID string, record *Record) error {
	record.UpdatedAt = e.now().UTC()
	return e.store.Put(ctx, workspaceID, *record)
}

// evaluation is one Evaluate's working set.
type evaluation struct {
	engine      *Engine
	ctx         context.Context
	workspaceID string
	record      *Record
	obs         Observation
	settings    Settings
	now         time.Time

	sessions       []Session
	sessionsLoaded bool
	sessionsErr    error
	statuses       []string
}

func (r *evaluation) status(text string) { r.statuses = append(r.statuses, text) }

func (r *evaluation) loadSessions() ([]Session, error) {
	if !r.sessionsLoaded {
		r.sessions, r.sessionsErr = r.engine.messenger.Sessions(r.ctx, r.record.TaskID)
		r.sessionsLoaded = true
	}
	return r.sessions, r.sessionsErr
}

// lifecycle records state transitions and queues the prompts they call for.
func (r *evaluation) lifecycle() {
	record, obs := r.record, r.obs
	state := obs.State
	if record.LastState == StateMerged {
		// Merged is final. An instance cannot reopen a merged pull request,
		// so a later "open" or "closed" reading is noise.
		state = StateMerged
	}
	if record.LastState == "" {
		record.LastState = state
		r.reviewRequest(state)
		switch state {
		case StateMerged:
			record.LastLifecycle = EventMerged
		case StateClosed:
			record.LastLifecycle = EventClosed
		}
		return
	}
	r.reviewRequest(state)

	switch state {
	case StateMerged:
		if record.LastLifecycle != EventMerged {
			record.LastLifecycle = EventMerged
			if record.OnMerged {
				r.queue(EventMerged)
			}
		}
	case StateClosed:
		if record.LastLifecycle != EventClosed && record.LastLifecycle != EventMerged {
			record.LastLifecycle = EventClosed
			if record.OnClosed {
				r.queue(EventClosed)
			}
		}
	case StateOpen:
		if record.LastLifecycle == EventClosed {
			// Reopened: the next close prompts again.
			record.LastLifecycle = ""
		}
	}
	record.LastState = state
}

// reviewRequest follows the operator's review request and prompts when a new
// one appears. It is only read while the prompt is on and the pull request is
// open: a closed one reports no request, which is not the request going away,
// and a request still pending after a reopen must not prompt twice.
func (r *evaluation) reviewRequest(state PullState) {
	record, obs := r.record, r.obs
	if !record.OnReviewRequested || state != StateOpen {
		return
	}
	if record.ReviewBaselineSet && obs.OperatorRequested && !record.ReviewRequested {
		r.queue(EventReviewRequested)
	}
	record.ReviewRequested = obs.OperatorRequested
	record.ReviewBaselineSet = true
}

func (r *evaluation) queue(event string) {
	for _, pending := range r.record.Pending {
		if pending.Event == event {
			return
		}
	}
	r.record.Pending = append(r.record.Pending, Pending{Event: event, ID: r.engine.newID()})
}

// deliverPending sends queued lifecycle prompts, oldest first. A prompt that
// cannot be delivered stays queued for the next evaluation.
func (r *evaluation) deliverPending() {
	for len(r.record.Pending) > 0 {
		pending := r.record.Pending[0]
		text := lifecycleText(pending, r.obs, *r.record)
		if !r.send(text, "") {
			return
		}
		r.record.Pending = r.record.Pending[1:]
	}
	if len(r.record.Pending) == 0 {
		r.record.Pending = nil
	}
}

// send delivers text to the best session. preferred is tried first when it
// can take a prompt.
func (r *evaluation) send(text, preferred string) bool {
	sessionID, ok := r.target(preferred)
	if !ok {
		return false
	}
	_, ok = r.deliver(sessionID, text)
	return ok
}

// target picks the session a prompt goes to.
func (r *evaluation) target(preferred string) (string, bool) {
	sessions, err := r.loadSessions()
	if err != nil {
		r.sendFailed(err)
		return "", false
	}
	sessionID, why := pickSession(sessions, preferred, r.record.Fork)
	if sessionID == "" {
		r.status(why)
		return "", false
	}
	return sessionID, true
}

// deliver sends text to a session and returns the Host's dispatch status.
func (r *evaluation) deliver(sessionID, text string) (string, bool) {
	status, err := r.engine.messenger.Send(r.ctx, r.record.TaskID, sessionID, text)
	if err != nil {
		r.sendFailed(err)
		return "", false
	}
	return status, true
}

// refused reports whether a Send error means the Host turned the prompt
// away, so nothing was queued.
func refused(err error) bool {
	return errors.Is(err, ErrNotApproved) || errors.Is(err, ErrNoSession) ||
		errors.Is(err, ErrTaskInactive) || errors.Is(err, ErrNoHost)
}

func (r *evaluation) sendFailed(err error) {
	switch {
	case errors.Is(err, ErrNotApproved):
		r.record.Error = "Kandev has not approved this plugin's capabilities since the last upgrade. An administrator needs to re-approve them before automation can prompt agents."
	case errors.Is(err, ErrNoSession):
		r.status("Waiting for a session on the task.")
	case errors.Is(err, ErrTaskInactive):
		r.status("The task no longer takes prompts (archived or inactive).")
	default:
		r.record.Error = "Kandev did not accept the prompt; it will be retried."
		r.engine.logf("prautomation: send: %v", err)
	}
}

// pickSession chooses the session to prompt: preferred when it can take a
// prompt, otherwise the most recently started session that can.
//
// A fork's task is held to sessions an agent has already run in. Kandev
// starts a never-launched session when it is messaged, and for a fork that
// would run its author's code with nobody having pressed start.
func pickSession(sessions []Session, preferred string, fork bool) (string, string) {
	var candidates []Session
	held := false
	for _, session := range sessions {
		if !promptable(session) {
			continue
		}
		if fork && !launched(session) {
			held = true
			continue
		}
		if session.ID == preferred && preferred != "" {
			return session.ID, ""
		}
		candidates = append(candidates, session)
	}
	if len(candidates) == 0 {
		if held {
			return "", "The task's agent has not been started. Automation never starts it for a pull request from a fork; start it to let automation prompt it."
		}
		return "", "Waiting for a session on the task."
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].StartedAt > candidates[j].StartedAt })
	return candidates[0].ID, ""
}

// promptable mirrors Messages().Send, which refuses only FAILED and CANCELLED
// sessions: a COMPLETED one is prompted, resumed if its agent process is gone.
func promptable(session Session) bool {
	switch session.State {
	case sessionCreated, sessionStarting, sessionRunning, sessionWaitingForInput, sessionIdle, sessionCompleted:
		return true
	}
	return false
}

// launched reports whether an agent has run in the session. WAITING_FOR_INPUT
// is both a never-launched session and one whose agent finished its turn;
// Kandev tells them apart by an execution the Host does not list, and an
// agent that ran has an ACP session id.
func launched(session Session) bool {
	switch session.State {
	case sessionStarting, sessionRunning, sessionIdle, sessionCompleted:
		return true
	case sessionWaitingForInput:
		return session.ACPSessionID != ""
	}
	return false
}

// autoFix sends the agent new feedback, one attempt at a time.
func (r *evaluation) autoFix() {
	record, obs := r.record, r.obs
	if !record.AutoFix {
		return
	}
	if obs.State != StateOpen {
		record.Attempt = nil
		return
	}
	snapshot := BuildSnapshot(obs, r.settings.Trust)
	record.Ignored = snapshot.Ignored

	if attempt := record.Attempt; attempt != nil && attempt.State == AttemptDispatched {
		if !r.attemptEnded(*attempt) {
			r.status(fmt.Sprintf("Auto-fix round %d is with the agent.", attempt.Round))
			return
		}
		attempt.State = AttemptRetryable
	}
	if obs.CI.State == CIPending {
		r.status("Auto-fix is waiting for CI to finish.")
		return
	}

	retry := record.Attempt != nil && record.Attempt.State == AttemptRetryable
	if retry && r.now.Sub(record.Attempt.SentAt) < retryCooldown {
		r.status(fmt.Sprintf("Auto-fix round %d ended without an outcome; it is retried shortly.", record.Attempt.Round))
		return
	}
	// A retry resends the delta too: its own items were never checkpointed,
	// so they are in it unless they went away.
	items := Delta(snapshot.Items, record.Checkpoint)
	if retry && len(items) == 0 {
		record.Attempt = nil
	}
	if len(items) == 0 {
		r.status("Auto-fix: nothing new to fix.")
		return
	}
	if retry && r.sessionBusy(record.Attempt.SessionID) {
		// The turn that ended may not have been the prompt's: a message queued
		// ahead of it runs first, and the prompt can still be queued or
		// running. A resend now would stack on it.
		r.status(fmt.Sprintf("Auto-fix round %d ended without an outcome; it is resent once the agent's turn ends.", record.Attempt.Round))
		return
	}
	if record.HoldHead != "" {
		if record.HoldHead == obs.HeadSHA && r.now.Before(record.HoldUntil) {
			r.status("Auto-fix is waiting for the agent's change to show up.")
			return
		}
		record.HoldHead, record.HoldUntil = "", time.Time{}
	}
	if record.Rounds >= MaxRounds {
		// The retryable attempt is dropped, not kept: it would hold auto-merge
		// back with no round left to send it. Its items stay out of the
		// checkpoint, so a reset sends them again.
		record.Exhausted = true
		record.Attempt = nil
		r.status(fmt.Sprintf("Auto-fix paused after %d rounds. Reset the rounds to continue.", MaxRounds))
		return
	}

	preferred := ""
	if record.Attempt != nil {
		preferred = record.Attempt.SessionID
	}
	attempt := Attempt{
		ID:    r.engine.newID(),
		Round: record.Rounds + 1,
		State: AttemptDispatched,
	}
	text, shown := RenderAutoFix(r.settings.EffectiveTemplate(), promptObservation(obs, r.settings.Trust), attempt, items, retry)
	attempt.Signature, attempt.Keys = Signature(shown), itemKeys(shown)
	sessionID, ok := r.target(preferred)
	if !ok {
		return
	}
	attempt.SessionID = sessionID
	// Stamped right before sending, not at the start of the evaluation: a
	// turn the prompt starts begins after this, and a turn that was already
	// running when it was queued began before. The observation's requests
	// would otherwise leave a window for someone else's turn to start in.
	attempt.SentAt = r.engine.now().UTC()

	// The attempt is stored before the prompt is sent. The agent can report
	// its outcome as soon as the prompt reaches it, and the report is matched
	// against the stored attempt; and a prompt is never out without a stored
	// attempt, which a failed write would otherwise lead to a second one
	// stacked on.
	previous, rounds := record.Attempt, record.Rounds
	record.Attempt, record.Rounds = &attempt, rounds+1
	if err := r.engine.put(r.ctx, r.workspaceID, record); err != nil {
		record.Attempt, record.Rounds = previous, rounds
		record.Error = "Kandev's state could not be written; auto-fix will try again."
		r.engine.logf("prautomation: save attempt: %v", err)
		return
	}
	dispatch, err := r.engine.messenger.Send(r.ctx, record.TaskID, sessionID, text)
	if err != nil {
		r.sendFailed(err)
		// Only a refusal is undone. Any other failure (a timeout, the Host
		// going away mid-call) may have come after the prompt was queued;
		// the attempt stays dispatched, and if nothing ran it is resent once
		// its session has been idle past idleGrace.
		if refused(err) {
			record.Attempt, record.Rounds = previous, rounds
		}
		return
	}
	attempt.Dispatch = dispatch
	r.status(fmt.Sprintf("Auto-fix round %d sent to the agent (%s).", attempt.Round, dispatch))
}

// sessionBusy reports whether a session is running a turn, or might be: a
// session list that cannot be read counts as busy.
func (r *evaluation) sessionBusy(sessionID string) bool {
	sessions, err := r.loadSessions()
	if err != nil {
		return true
	}
	for _, session := range sessions {
		if session.ID == sessionID {
			return session.State == sessionRunning || session.State == sessionStarting
		}
	}
	return false
}

// attemptEnded reports whether a dispatched attempt's turn is over without an
// outcome: its session is gone, idle for longer than idleGrace since the
// prompt was sent, or still not started after startGrace.
func (r *evaluation) attemptEnded(attempt Attempt) bool {
	sessions, err := r.loadSessions()
	if err != nil {
		return false
	}
	for _, session := range sessions {
		if session.ID != attempt.SessionID {
			continue
		}
		switch session.State {
		case sessionRunning, sessionStarting:
			return false
		case sessionCreated:
			return r.now.Sub(attempt.SentAt) > startGrace
		case sessionIdle, sessionWaitingForInput, sessionCompleted:
			return r.now.Sub(attempt.SentAt) > idleGrace
		default:
			return true
		}
	}
	return true
}

// autoMerge merges when every gate passes, pinned to the observed head.
func (r *evaluation) autoMerge() {
	record, obs := r.record, r.obs
	if !record.AutoMerge || obs.State != StateOpen {
		return
	}
	if blocker := r.mergeBlocker(); blocker != "" {
		r.status("Auto-merge: " + blocker)
		return
	}
	signature := mergeSignature(obs)
	if record.MergeFailure != "" && record.MergeFailureSignature == signature && !record.MergeRetry {
		r.status("Auto-merge failed: " + record.MergeFailure + " Retry from the panel, or it tries again when the pull request changes.")
		return
	}
	err := r.engine.merger.Merge(r.ctx, r.workspaceID, record.Ref(), obs.HeadSHA)
	record.MergeRetry = false
	var refused *MergeError
	switch {
	case err == nil:
		record.MergedByPlugin = true
		record.MergeFailure, record.MergeFailureSignature = "", ""
		r.status("Auto-merge merged the pull request.")
	case errors.As(err, &refused) && refused.Transient:
		r.status("Auto-merge: the instance asked to try again; it will.")
	case errors.As(err, &refused):
		record.MergeFailure, record.MergeFailureSignature = refused.Message, signature
		r.status("Auto-merge failed: " + refused.Message)
	default:
		record.Error = r.engine.describe(err)
		r.status("Auto-merge could not reach the instance; it will try again.")
	}
}

// mergeBlocker returns why auto-merge waits, or "" when it may merge.
func (r *evaluation) mergeBlocker() string {
	record, obs := r.record, r.obs
	switch {
	case obs.Draft:
		return "the pull request is a draft."
	case obs.CI.State == CIPending:
		return "waiting for CI."
	case obs.CI.State == CIFailure:
		return "CI is failing."
	case len(obs.CI.Missing) > 0:
		return "waiting for required checks to report: " + strings.Join(obs.CI.Missing, ", ") + "."
	case obs.CI.State == CINone && r.now.Sub(record.HeadSeenAt) < noCIGrace:
		return "waiting for CI to report on the new head."
	case obs.Mergeable == nil:
		return "waiting for the instance to compute mergeability."
	case !*obs.Mergeable:
		return "the branch has conflicts."
	case record.Attempt != nil:
		return "an auto-fix round is in progress."
	case record.HoldHead == obs.HeadSHA && r.now.Before(record.HoldUntil):
		// The agent reported a change: the head from before it is not the
		// one to merge until a new head shows or the window passes.
		return "waiting for the agent's change to show up."
	case record.BlockedHead == obs.HeadSHA && r.now.Before(record.BlockedUntil):
		return "the agent reported that a change is needed but blocked."
	}
	if obs.ReviewsIncomplete {
		return "the pull request has more reviews than automation reads; merge it by hand."
	}
	changes, approvals := reviewVerdicts(obs.Reviews, obs.Operator)
	if changes > 0 {
		return "changes are requested."
	}
	if required := obs.RequiredApprovals; required != nil && approvals < *required {
		return fmt.Sprintf("%d of %d required approvals.", approvals, *required)
	}
	for _, review := range obs.Reviews {
		for _, comment := range review.Comments {
			if !comment.Resolved && !review.Dismissed {
				return "review comments are unresolved."
			}
		}
	}
	return ""
}

// reviewVerdicts reads each reviewer's latest decisive review: how many
// request changes, and how many official reviewers approve. Reviews arrive
// oldest first from the instance.
//
// The operator's approval is not counted: agents post as that account, so it
// could be an agent approving its own work. Its request for changes still
// holds the merge back.
func reviewVerdicts(reviews []Review, operator string) (changes, approvals int) {
	operator = strings.ToLower(strings.TrimSpace(operator))
	latest := map[string]Review{}
	for _, review := range reviews {
		if review.Dismissed || (review.State != ReviewApproved && review.State != ReviewChangesRequested) {
			continue
		}
		latest[strings.ToLower(strings.TrimSpace(review.Author))] = review
	}
	for author, review := range latest {
		switch {
		case review.State == ReviewChangesRequested:
			changes++
		case review.Official && (operator == "" || author != operator):
			approvals++
		}
	}
	return changes, approvals
}

// mergeSignature identifies the state a merge was attempted in. A failure is
// not retried on its own until this changes.
func mergeSignature(obs Observation) string {
	items := []Item{{Key: "head:" + obs.HeadSHA}, {Key: "ci:" + string(obs.CI.State)}}
	for _, missing := range obs.CI.Missing {
		items = append(items, Item{Key: "missing:" + missing})
	}
	for _, review := range obs.Reviews {
		items = append(items, Item{Key: fmt.Sprintf("review:%d:%s:%t", review.ID, review.State, review.Dismissed)})
	}
	if obs.RequiredApprovals != nil {
		items = append(items, Item{Key: fmt.Sprintf("required:%d", *obs.RequiredApprovals)})
	}
	return Signature(items)
}

func (r *evaluation) nextDue() time.Time {
	record, obs := r.record, r.obs
	if len(r.statuses) > 0 {
		record.Status = strings.Join(r.statuses, " ")
	} else {
		record.Status = idleStatus(*record)
	}
	switch {
	case record.Terminal():
		return time.Time{}
	case len(record.Pending) > 0:
		return r.now.Add(pendingInterval)
	case obs.State == StateClosed:
		return r.now.Add(closedInterval)
	case record.Attempt != nil, record.AutoMerge, record.AutoFix && obs.CI.State == CIPending:
		return r.now.Add(activeInterval)
	default:
		return r.now.Add(idleInterval)
	}
}

func idleStatus(record Record) string {
	switch {
	case record.LastState == StateMerged:
		return "Merged."
	case record.LastState == StateClosed:
		return "Closed."
	case record.Switches.Any():
		return "Watching."
	default:
		return ""
	}
}

// Outcome records the agent's report for the attempt in flight on a task.
// repository narrows the choice when several linked pull requests have one;
// sessionID, when known, must be the session the attempt went to.
func (e *Engine) Outcome(ctx context.Context, workspaceID, taskID, sessionID, repository, outcome, summary string) (Record, error) {
	records, err := e.store.List(ctx, workspaceID)
	if err != nil {
		return Record{}, err
	}
	var matches []Record
	for _, record := range records {
		if record.TaskID != taskID || record.Attempt == nil {
			continue
		}
		if repository != "" && !strings.EqualFold(record.Repository, repository) {
			continue
		}
		matches = append(matches, record)
	}
	switch {
	case len(matches) == 0:
		return Record{}, ErrNoAttempt
	case len(matches) > 1:
		return Record{}, ErrAmbiguousAttempt
	}

	ref := matches[0].Ref()
	unlock, err := e.locks.lock(ctx, recordKey(ref))
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	record, found, err := e.store.Get(ctx, workspaceID, ref)
	if err != nil {
		return Record{}, err
	}
	if !found || record.Attempt == nil {
		return Record{}, ErrNoAttempt
	}
	attempt := *record.Attempt
	if sessionID != "" && attempt.SessionID != "" && sessionID != attempt.SessionID {
		return Record{}, ErrNoAttempt
	}
	now := e.now().UTC()
	record.Checkpoint = mergeKeys(record.Checkpoint, attempt.Keys)
	switch outcome {
	case OutcomeActionTaken:
		record.HoldHead, record.HoldUntil = record.HeadSHA, now.Add(progressWindow)
	case OutcomeBlocked:
		record.BlockedHead, record.BlockedUntil = record.HeadSHA, now.Add(blockedHold)
	}
	record.Attempt = nil
	record.LastOutcome = outcome
	record.LastSummary = clipBytes(strings.TrimSpace(summary), maxSummaryBytes)
	record.LastOutcomeAt = now
	record.Status = fmt.Sprintf("Auto-fix round %d: the agent reported %s.", attempt.Round, strings.ReplaceAll(outcome, "_", " "))
	record.NextDue = now
	e.save(ctx, workspaceID, &record)
	return record, nil
}

// TurnCompleted handles a turn.completed event: an attempt whose turn ended
// without an outcome becomes retryable at once instead of after idleGrace.
// The event is a hint — delivery is best-effort — so nothing depends on it.
func (e *Engine) TurnCompleted(ctx context.Context, taskID, sessionID string, startedAt time.Time) {
	e.hintRecords(ctx, taskID, func(record *Record) {
		if attempt := record.Attempt; attempt != nil && attempt.State == AttemptDispatched &&
			attempt.SessionID == sessionID && !startedAt.Before(attempt.SentAt) {
			attempt.State = AttemptRetryable
		}
	})
}

// SessionChanged handles a session state change on a task: anything waiting
// for a session is looked at on the next tick.
func (e *Engine) SessionChanged(ctx context.Context, taskID string) {
	e.hintRecords(ctx, taskID, func(*Record) {})
}

// errNothingWaits leaves a record a hint has nothing to do for unwritten.
var errNothingWaits = errors.New("prautomation: nothing waits on the agent")

// hintRecords applies change to the task's records that wait on the agent (an
// attempt in flight, or a prompt queued for a session) and makes them due.
// The records come from the task index, so a hint reads only them.
func (e *Engine) hintRecords(ctx context.Context, taskID string, change func(*Record)) {
	e.mu.Lock()
	task, ok := e.tracked[taskID]
	e.mu.Unlock()
	if !ok {
		return
	}
	for _, ref := range task.refs {
		_, _ = e.Update(ctx, task.workspaceID, ref, func(record *Record) error {
			if record.Attempt == nil && len(record.Pending) == 0 {
				return errNothingWaits
			}
			change(record)
			record.NextDue = time.Time{}
			return nil
		})
	}
}

// trackedTask is a task with automation on: its workspace, which events do
// not carry, and its records.
type trackedTask struct {
	workspaceID string
	refs        []PullRef
}

func (t trackedTask) with(workspaceID string, ref PullRef) trackedTask {
	for _, known := range t.refs {
		if known == ref {
			return t
		}
	}
	return trackedTask{workspaceID: workspaceID, refs: append(t.refs[:len(t.refs):len(t.refs)], ref)}
}

// track adds a record switched on between ticks to the task index, so the
// events of its first round are not dropped until the next tick rebuilds it.
func (e *Engine) track(workspaceID string, record Record) {
	if !record.Switches.Any() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tracked[record.TaskID] = e.tracked[record.TaskID].with(workspaceID, record.Ref())
	if e.meanwhile != nil {
		e.meanwhile[record.TaskID] = e.meanwhile[record.TaskID].with(workspaceID, record.Ref())
	}
}

// Update applies change to one record under its lock and stores it. A change
// that returns an error stores nothing.
func (e *Engine) Update(ctx context.Context, workspaceID string, ref PullRef, change func(*Record) error) (Record, error) {
	return e.modify(ctx, workspaceID, ref, false, func(record *Record, _ bool) error { return change(record) })
}

// Upsert is Update that starts a record for ref when there is none; created
// tells change which happened.
func (e *Engine) Upsert(ctx context.Context, workspaceID string, ref PullRef, change func(record *Record, created bool) error) (Record, error) {
	return e.modify(ctx, workspaceID, ref, true, change)
}

func (e *Engine) modify(ctx context.Context, workspaceID string, ref PullRef, create bool, change func(*Record, bool) error) (Record, error) {
	unlock, err := e.locks.lock(ctx, recordKey(ref))
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	record, found, err := e.store.Get(ctx, workspaceID, ref)
	if err != nil {
		return Record{}, err
	}
	if !found {
		if !create {
			return Record{}, ErrNotFound
		}
		record = Record{TaskID: ref.TaskID, RepositoryID: ref.RepositoryID, Number: ref.Number}
	}
	if err := change(&record, !found); err != nil {
		return Record{}, err
	}
	record.UpdatedAt = e.now().UTC()
	if err := e.store.Put(ctx, workspaceID, record); err != nil {
		return Record{}, err
	}
	e.track(workspaceID, record)
	return record, nil
}

func (e *Engine) remove(ctx context.Context, workspaceID string, ref PullRef) error {
	unlock, err := e.locks.lock(ctx, recordKey(ref))
	if err != nil {
		return err
	}
	defer unlock()
	return e.store.Delete(ctx, workspaceID, ref)
}

func itemKeys(items []Item) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	return keys
}

func mergeKeys(checkpoint, keys []string) []string {
	items := make([]Item, 0, len(keys))
	for _, key := range keys {
		items = append(items, Item{Key: key})
	}
	return mergeCheckpoint(checkpoint, items)
}

func clipBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "")
}

func randomID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

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
