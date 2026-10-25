package prautomation

import "time"

// MaxRounds bounds auto-fix prompts per pull request, as Kandev's GitHub
// integration does. Turning auto-fix off and on, or the panel's reset, starts
// the count again.
const MaxRounds = 10

// Outcomes the agent reports through the pr tool.
const (
	OutcomeActionTaken   = "action_taken"
	OutcomeNonActionable = "non_actionable"
	OutcomeBlocked       = "blocked"
)

// ValidOutcome reports whether outcome is one the agent may report.
func ValidOutcome(outcome string) bool {
	return outcome == OutcomeActionTaken || outcome == OutcomeNonActionable || outcome == OutcomeBlocked
}

// Attempt states.
const (
	// AttemptDispatched: the prompt reached Kandev and no outcome is known yet.
	AttemptDispatched = "dispatched"
	// AttemptRetryable: the turn ended without an outcome; the next evaluation
	// sends the whole current feedback again.
	AttemptRetryable = "retryable"
)

// Attempt is the auto-fix prompt in flight. There is at most one: the host's
// message API cannot replace a queued prompt, so a second one would stack.
type Attempt struct {
	ID        string    `json:"id"`
	Round     int       `json:"round"`
	State     string    `json:"state"`
	Signature string    `json:"signature"`
	Keys      []string  `json:"keys"`
	SessionID string    `json:"session_id"`
	Dispatch  string    `json:"dispatch"`
	SentAt    time.Time `json:"sent_at"`
}

// Lifecycle events.
const (
	EventReviewRequested = "review_requested"
	EventMerged          = "merged"
	EventClosed          = "closed"
)

// Pending is a lifecycle prompt waiting for a session. The id goes into the
// text: Kandev treats an identical text to the same session as a replay, and a
// pull request closed, reopened and closed again must prompt twice.
type Pending struct {
	Event string `json:"event"`
	ID    string `json:"id"`
}

// Record is everything automation remembers about one linked pull request.
type Record struct {
	TaskID       string `json:"task_id"`
	RepositoryID string `json:"repository_id"`
	Number       int64  `json:"number"`
	// Seeded is set once the workspace defaults have been applied.
	Seeded bool `json:"seeded"`
	Switches

	// What the last observation saw, for the panel and the gates.
	Repository string `json:"repository,omitempty"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	// HeadSeenAt is when the current head was first observed.
	HeadSeenAt time.Time `json:"head_seen_at,omitempty"`
	Fork       bool      `json:"fork,omitempty"`
	Observed   time.Time `json:"observed,omitempty"`

	// Auto-fix.
	Rounds     int      `json:"rounds"`
	Exhausted  bool     `json:"exhausted,omitempty"`
	Checkpoint []string `json:"checkpoint,omitempty"`
	Attempt    *Attempt `json:"attempt,omitempty"`
	// After action_taken the agent is given until the head moves, or
	// progressWindow, before new feedback is sent.
	HoldHead  string    `json:"hold_head,omitempty"`
	HoldUntil time.Time `json:"hold_until,omitempty"`
	// After blocked, auto-merge waits an hour on the same head, as Kandev's
	// GitHub integration does: the agent said a change is needed.
	BlockedHead   string    `json:"blocked_head,omitempty"`
	BlockedUntil  time.Time `json:"blocked_until,omitempty"`
	LastOutcome   string    `json:"last_outcome,omitempty"`
	LastSummary   string    `json:"last_summary,omitempty"`
	LastOutcomeAt time.Time `json:"last_outcome_at,omitempty"`
	Ignored       int       `json:"ignored,omitempty"`

	// Auto-merge.
	MergeFailure          string `json:"merge_failure,omitempty"`
	MergeFailureSignature string `json:"merge_failure_signature,omitempty"`
	MergeRetry            bool   `json:"merge_retry,omitempty"`
	MergedByPlugin        bool   `json:"merged_by_plugin,omitempty"`

	// Lifecycle.
	LastState         PullState `json:"last_state,omitempty"`
	LastLifecycle     string    `json:"last_lifecycle,omitempty"`
	ReviewBaselineSet bool      `json:"review_baseline_set,omitempty"`
	ReviewRequested   bool      `json:"review_requested,omitempty"`
	Pending           []Pending `json:"pending,omitempty"`

	// The loop.
	NextDue time.Time `json:"next_due,omitempty"`
	// Status is one line for the panel: what automation is doing or waiting
	// for. Error is the last failure, fixed text only.
	Status    string    `json:"status,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Ref names the pull request a record is about.
func (r Record) Ref() PullRef {
	return PullRef{TaskID: r.TaskID, RepositoryID: r.RepositoryID, Number: r.Number}
}

// Terminal reports whether nothing will ever happen to the record again.
func (r Record) Terminal() bool {
	return r.LastState == StateMerged && len(r.Pending) == 0
}

// PullRef identifies a linked pull request: the task, the repository's
// immutable id, and the number.
type PullRef struct {
	TaskID       string `json:"task_id"`
	RepositoryID string `json:"repository_id"`
	Number       int64  `json:"number"`
}

// ApplySwitches sets the switches as a human chose them, with the side effects
// each change has: turning auto-fix on starts the round count again and clears
// a pause; turning it off drops the attempt and what the agent reported; turning the review-requested prompt on takes a new baseline so a
// request already open does not fire.
func (r *Record) ApplySwitches(next Switches) {
	if next.AutoFix && !r.AutoFix {
		r.Rounds = 0
		r.Exhausted = false
	}
	if !next.AutoFix {
		// What the agent reported for auto-fix goes with it: auto-merge would
		// otherwise keep waiting on a report the panel no longer shows.
		r.Attempt = nil
		r.HoldHead, r.HoldUntil = "", time.Time{}
		r.BlockedHead, r.BlockedUntil = "", time.Time{}
	}
	if next.OnReviewRequested && !r.OnReviewRequested {
		r.ReviewBaselineSet = false
	}
	if !next.AutoMerge {
		r.MergeRetry = false
	}
	if next.Any() && !r.Switches.Any() {
		// Nothing was observed while everything was off, so what the record
		// last saw is stale. Take a new baseline instead of prompting about a
		// change that happened while nobody asked to hear about it.
		r.LastState = ""
		r.LastLifecycle = ""
		r.ReviewBaselineSet = false
	}
	// A queued prompt whose switch is now off is not delivered.
	kept := r.Pending[:0]
	for _, pending := range r.Pending {
		if next.wants(pending.Event) {
			kept = append(kept, pending)
		}
	}
	r.Pending = kept
	if len(r.Pending) == 0 {
		r.Pending = nil
	}
	r.Switches = next
	r.Seeded = true
	r.NextDue = time.Time{}
}
