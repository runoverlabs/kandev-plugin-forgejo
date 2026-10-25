package prautomation

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Outcome records the agent's report for the attempt in flight on a task.
// repository narrows the choice when several linked pull requests have one;
// sessionID, when known, must be the session the attempt went to.
func (e *Engine) Outcome(ctx context.Context, workspaceID, taskID, sessionID, repository, outcome, summary string) (Record, error) {
	records, err := e.store.List(ctx, workspaceID)
	if err != nil {
		return Record{}, err
	}
	ref, err := attemptAwaitingOutcome(records, taskID, repository)
	if err != nil {
		return Record{}, err
	}

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
	if sessionID != "" && record.Attempt.SessionID != "" && sessionID != record.Attempt.SessionID {
		return Record{}, ErrNoAttempt
	}
	applyOutcome(&record, outcome, summary, e.now().UTC())
	e.save(ctx, workspaceID, &record)
	return record, nil
}

// attemptAwaitingOutcome picks the one record of the task with an attempt in
// flight, narrowed to repository when it is given.
func attemptAwaitingOutcome(records []Record, taskID, repository string) (PullRef, error) {
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
		return PullRef{}, ErrNoAttempt
	case len(matches) > 1:
		return PullRef{}, ErrAmbiguousAttempt
	}
	return matches[0].Ref(), nil
}

// applyOutcome closes the record's attempt with the agent's report and makes
// the record due at once.
func applyOutcome(record *Record, outcome, summary string, now time.Time) {
	attempt := *record.Attempt
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
