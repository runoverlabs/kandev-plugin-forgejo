package prautomation

import (
	"context"
	"errors"
	"time"
)

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
