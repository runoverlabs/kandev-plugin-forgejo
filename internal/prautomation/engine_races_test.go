package prautomation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The agent can report as soon as the prompt reaches it, while the evaluation
// that sent it still holds the record: the attempt must already be stored.
func TestAttemptIsStoredBeforeThePromptIsSent(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	var seen *Attempt
	h.messenger.onSend = func() {
		records, err := h.engine.store.List(context.Background(), testWorkspace)
		require.NoError(t, err)
		require.Len(t, records, 1)
		seen = records[0].Attempt
	}
	record := h.evaluate()
	require.NotNil(t, seen, "the outcome tool finds the attempt while the prompt is delivered")
	require.Equal(t, "s-1", seen.SessionID)
	require.Equal(t, 1, record.Rounds)
	require.Equal(t, "sent", record.Attempt.Dispatch)
}

func TestNoPromptIsSentWhenTheAttemptCannotBeStored(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.state.setErr = errors.New("host unavailable")
	h.evaluate()
	require.Empty(t, h.messenger.sent, "a prompt with no stored attempt would be sent again on top of itself")

	h.state.setErr = nil
	record := h.evaluate()
	require.Len(t, h.messenger.sent, 1)
	require.Equal(t, 1, record.Rounds)
}

func TestRefusedDeliveryUndoesTheStoredAttempt(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.messenger.sendErr = fmt.Errorf("%w: gone", ErrTaskInactive)
	record := h.evaluate()
	require.Nil(t, record.Attempt)
	require.Zero(t, record.Rounds)
}

func TestAmbiguousDeliveryKeepsTheStoredAttempt(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.messenger.sendErr = errors.New("deadline exceeded")
	record := h.evaluate()
	require.NotNil(t, record.Attempt, "the Host may have queued the prompt before the call failed")
	require.Equal(t, 1, record.Rounds)

	// The next evaluation must not stack a second round on it.
	h.messenger.sendErr = nil
	h.evaluate()
	require.Empty(t, h.messenger.sent)
}

func TestForkPromptsAnAgentThatHasRun(t *testing.T) {
	h := newHarness(t)
	h.setObs(func(obs *Observation) {
		obs.Fork = true
		failingCI(obs)
	})
	h.switches(Switches{AutoFix: true})
	h.messenger.sessions[testRef.TaskID] = []Session{{ID: "s-new", State: sessionWaitingForInput}}
	record := h.evaluate()
	require.Empty(t, h.messenger.sent, "waiting for input with no agent run is a session nobody started")
	require.Contains(t, record.Status, "has not been started")

	h.messenger.sessions[testRef.TaskID] = []Session{{ID: "s-ran", State: sessionWaitingForInput, ACPSessionID: "acp-1"}}
	h.evaluate()
	require.Len(t, h.messenger.sent, 1, "an agent that finished its turn waits for input too")
	require.Equal(t, "s-ran", h.messenger.sent[0].sessionID)
}

func TestTickLeavesARecordForALinkMadeMeanwhile(t *testing.T) {
	h := newHarness(t)
	// While the tick reads the links, the pull request is linked and a human
	// turns auto-merge on for it.
	h.links.afterLinked = func() {
		h.links.afterLinked = nil
		h.links.refs[testWorkspace] = []PullRef{testRef}
		_, err := h.engine.Upsert(context.Background(), testWorkspace, testRef, func(r *Record, _ bool) error {
			r.ApplySwitches(Switches{AutoMerge: true})
			return nil
		})
		require.NoError(t, err)
	}
	h.links.refs[testWorkspace] = nil
	h.engine.Tick(context.Background())
	require.True(t, h.record().AutoMerge)
}

func TestTickKeepsATaskSwitchedOnMeanwhileTracked(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{})
	h.engine.Tick(context.Background())
	// The record was listed switched off; a human turns auto-fix on while the
	// tick is running.
	h.links.afterLinked = func() {
		h.links.afterLinked = nil
		_, err := h.engine.Update(context.Background(), testWorkspace, testRef, func(r *Record) error {
			r.ApplySwitches(Switches{AutoFix: true})
			return nil
		})
		require.NoError(t, err)
	}
	h.engine.Tick(context.Background())
	h.engine.mu.Lock()
	_, ok := h.engine.tracked[testRef.TaskID]
	h.engine.mu.Unlock()
	require.True(t, ok, "the task's first-round events would be dropped until the next tick")
}

func TestTurningOneLifecycleSwitchOffDropsOnlyItsQueuedPrompt(t *testing.T) {
	record := Record{
		Switches: Switches{OnMerged: true, OnClosed: true, OnReviewRequested: true},
		Pending:  []Pending{{Event: EventReviewRequested, ID: "a"}, {Event: EventMerged, ID: "b"}},
	}
	record.ApplySwitches(Switches{OnClosed: true, OnReviewRequested: true})
	require.Equal(t, []Pending{{Event: EventReviewRequested, ID: "a"}}, record.Pending)
	record.ApplySwitches(Switches{OnClosed: true})
	require.Nil(t, record.Pending)
}

func TestTurnCompletedLeavesRecordsWithNothingInFlightAlone(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{OnMerged: true})
	h.engine.Tick(context.Background()) // tracks the task, which events need
	before := h.record()
	h.advance(time.Minute)
	h.engine.TurnCompleted(context.Background(), testRef.TaskID, "s-1", h.now)
	require.Equal(t, before, h.record(), "no write for a record that waits on nothing from the agent")
}

func TestLockWaitEndsWithTheContext(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	unlock, err := h.engine.locks.lock(context.Background(), recordKey(testRef))
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = h.engine.Update(ctx, testWorkspace, testRef, func(*Record) error { return nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestHintRunsOnlyWhileTheEngineRuns(t *testing.T) {
	h := newHarness(t)
	ran := make(chan struct{}, 1)
	h.engine.Hint(func(context.Context) { ran <- struct{}{} })
	h.engine.Start()
	h.engine.Hint(func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the hint never ran")
	}

	// Stop cancels a hint still waiting and waits for it.
	blocked := make(chan error, 1)
	unlock, err := h.engine.locks.lock(context.Background(), "k")
	require.NoError(t, err)
	h.engine.Hint(func(ctx context.Context) {
		_, err := h.engine.locks.lock(ctx, "k")
		blocked <- err
	})
	h.engine.Stop()
	unlock()
	require.ErrorIs(t, <-blocked, context.Canceled)
	require.Empty(t, ran, "a hint while stopped is dropped")
}

func TestAutoMergeWaitsForRequiredChecks(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true})
	h.setObs(func(obs *Observation) { obs.CI.Missing = []string{"ci/deploy"} })
	record := h.evaluate()
	require.Empty(t, h.merger.calls)
	require.Contains(t, record.Status, "required checks to report: ci/deploy")

	h.setObs(func(obs *Observation) { obs.CI.Missing = nil })
	h.evaluate()
	require.Len(t, h.merger.calls, 1)
}

// Kandev queues a prompt behind a running turn, first in first out. A message
// already queued ahead of it runs first, and its turn starts after the prompt
// was sent: it ends while the prompt is still waiting, so the attempt reads
// as ended without an outcome. The resend waits until the session is idle.
func TestRetryWaitsWhileThePromptMayStillBeQueued(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	session := &h.messenger.sessions[testRef.TaskID][0]
	session.State = sessionRunning
	first := h.evaluate().Attempt
	require.NotNil(t, first)
	h.engine.Tick(context.Background()) // builds the task index

	h.engine.TurnCompleted(context.Background(), testRef.TaskID, "s-1", first.SentAt.Add(time.Second))
	require.Equal(t, AttemptRetryable, h.record().Attempt.State)

	h.advance(retryCooldown)
	record := h.evaluate()
	require.Len(t, h.messenger.sent, 1, "the session is running: the prompt may be what it runs")
	require.Contains(t, record.Status, "once the agent's turn ends")

	session.State = sessionWaitingForInput
	record = h.evaluate()
	require.Len(t, h.messenger.sent, 2)
	require.Equal(t, 2, record.Rounds)
}

// A turn that starts while the instance is being read, before the prompt is
// sent, is not the prompt's turn.
func TestAttemptIsStampedWhenItIsSent(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	started := h.now.Add(time.Second)
	h.observer.onObserve = func() { h.advance(3 * time.Second) }
	first := h.evaluate().Attempt
	require.NotNil(t, first)
	require.True(t, first.SentAt.After(started))
	h.observer.onObserve = nil
	h.engine.Tick(context.Background())

	h.engine.TurnCompleted(context.Background(), testRef.TaskID, "s-1", started)
	require.Equal(t, AttemptDispatched, h.record().Attempt.State)
}

// A record switched on from the panel is evaluated before the next tick
// rebuilds the task index; its turn's event still counts. A hint reads the
// task's records, not the workspace's whole state.
func TestHintReachesARecordSwitchedOnSinceTheLastTick(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true}) // no tick after the switch
	h.setObs(failingCI)
	first := h.evaluate().Attempt
	require.NotNil(t, first)

	h.state.mu.Lock()
	lists := h.state.lists
	h.state.mu.Unlock()
	h.engine.TurnCompleted(context.Background(), testRef.TaskID, "s-1", first.SentAt)
	require.Equal(t, AttemptRetryable, h.record().Attempt.State)
	require.Equal(t, lists, h.state.lists)
}
