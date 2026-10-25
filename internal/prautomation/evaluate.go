package prautomation

import (
	"context"
	"strings"
	"time"
)

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
