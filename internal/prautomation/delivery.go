package prautomation

import (
	"errors"
	"sort"

	"kandev-plugin-forgejo/internal/kandev"
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
		errors.Is(err, ErrTaskInactive) || errors.Is(err, kandev.ErrNoHost)
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
