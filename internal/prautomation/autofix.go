package prautomation

import (
	"fmt"
	"time"
)

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
	snapshot := r.readFeedback()
	if r.attemptInFlight() {
		return
	}
	if obs.CI.State == CIPending {
		r.status("Auto-fix is waiting for CI to finish.")
		return
	}
	items, retry, ok := r.nextRound(snapshot)
	if !ok {
		return
	}
	r.dispatch(items, retry)
}

// readFeedback builds the snapshot trust lets through and keeps the record's
// ignored count and checkpoint in step with it.
func (r *evaluation) readFeedback() Snapshot {
	record, obs := r.record, r.obs
	snapshot := BuildSnapshot(obs, r.settings.Trust)
	record.Ignored = snapshot.Ignored
	// Present at any trust level: narrowing it for a while must not make
	// handled feedback new again once it is widened.
	record.Checkpoint = pruneCheckpoint(record.Checkpoint, BuildSnapshot(obs, TrustEveryone).Items)
	return snapshot
}

// attemptInFlight reports whether the dispatched attempt is still with the
// agent. One whose turn ended without an outcome becomes retryable.
func (r *evaluation) attemptInFlight() bool {
	attempt := r.record.Attempt
	if attempt == nil || attempt.State != AttemptDispatched {
		return false
	}
	if !r.attemptEnded(*attempt) {
		r.status(fmt.Sprintf("Auto-fix round %d is with the agent.", attempt.Round))
		return true
	}
	attempt.State = AttemptRetryable
	return false
}

// nextRound returns the feedback the next round carries and whether it
// resends a retryable attempt, or false when no round is to be sent now.
func (r *evaluation) nextRound(snapshot Snapshot) ([]Item, bool, bool) {
	record := r.record
	retry := record.Attempt != nil && record.Attempt.State == AttemptRetryable
	if retry && r.now.Sub(record.Attempt.SentAt) < retryCooldown {
		r.status(fmt.Sprintf("Auto-fix round %d ended without an outcome; it is retried shortly.", record.Attempt.Round))
		return nil, false, false
	}
	// A retry resends the delta too: its own items were never checkpointed,
	// so they are in it unless they went away.
	items := Delta(snapshot.Items, record.Checkpoint)
	if len(items) == 0 {
		if retry {
			record.Attempt = nil
		}
		r.status("Auto-fix: nothing new to fix.")
		return nil, false, false
	}
	if retry && r.sessionBusy(record.Attempt.SessionID) {
		// The turn that ended may not have been the prompt's: a message queued
		// ahead of it runs first, and the prompt can still be queued or
		// running. A resend now would stack on it.
		r.status(fmt.Sprintf("Auto-fix round %d ended without an outcome; it is resent once the agent's turn ends.", record.Attempt.Round))
		return nil, false, false
	}
	if r.awaitingChange() {
		r.status("Auto-fix is waiting for the agent's change to show up.")
		return nil, false, false
	}
	if record.Rounds >= MaxRounds {
		// The retryable attempt is dropped, not kept: it would hold auto-merge
		// back with no round left to send it. Its items stay out of the
		// checkpoint, so a reset sends them again.
		record.Exhausted = true
		record.Attempt = nil
		r.status(fmt.Sprintf("Auto-fix paused after %d rounds. Reset the rounds to continue.", MaxRounds))
		return nil, false, false
	}
	return items, retry, true
}

// awaitingChange reports whether the agent's reported change is still due on
// the head it was reported on, and clears a hold that has run its course.
func (r *evaluation) awaitingChange() bool {
	record := r.record
	if record.HoldHead == "" {
		return false
	}
	if record.HoldHead == r.obs.HeadSHA && r.now.Before(record.HoldUntil) {
		return true
	}
	record.HoldHead, record.HoldUntil = "", time.Time{}
	return false
}

// dispatch stores a new attempt carrying items and sends its prompt.
func (r *evaluation) dispatch(items []Item, retry bool) {
	record, obs := r.record, r.obs
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

func itemKeys(items []Item) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	return keys
}
