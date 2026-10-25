package prautomation

import (
	"context"
	"errors"
)

// Observer reads a linked pull request from the instance.
type Observer interface {
	Observe(ctx context.Context, workspaceID string, ref PullRef) (Observation, error)
}

// Merger merges a pull request, pinned to the head the gates were checked on:
// the instance refuses if the head moved, so commits nobody evaluated are
// never merged.
type Merger interface {
	Merge(ctx context.Context, workspaceID string, ref PullRef, headSHA string) error
}

// MergeError is a merge the instance refused, with a fixed, operator-facing
// message. Transient refusals (mergeability still computing, the head moved
// under us) are retried on the next evaluation instead of being recorded.
type MergeError struct {
	Transient bool
	Message   string
}

func (e *MergeError) Error() string { return "prautomation: merge refused: " + e.Message }

// Session is a task session as the Host lists it.
type Session struct {
	ID        string
	State     string
	StartedAt string
}

// Messenger reaches a task's agent through Kandev.
type Messenger interface {
	Sessions(ctx context.Context, taskID string) ([]Session, error)
	// Send delivers text to a session and returns the Host's dispatch status
	// ("queued", "sent" or "started").
	Send(ctx context.Context, taskID, sessionID, text string) (string, error)
}

// Errors a Messenger maps the Host's refusals onto.
var (
	// ErrNoSession: the task has no session that can take a prompt.
	ErrNoSession = errors.New("prautomation: no session to prompt")
	// ErrNotApproved: the operator has not approved the capabilities sending
	// needs (after an upgrade, every gated call is refused until they do).
	ErrNotApproved = errors.New("prautomation: capability approval needed")
	// ErrTaskInactive: the task is archived or managed by another owner.
	ErrTaskInactive = errors.New("prautomation: task no longer takes prompts")
)

// Links lists the pull requests linked to tasks in a workspace.
type Links interface {
	Linked(ctx context.Context, workspaceID string) ([]PullRef, error)
}
