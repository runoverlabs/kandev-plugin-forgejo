package prautomation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func failingCI(obs *Observation) {
	obs.CI = CI{State: CIFailure, Failing: []Check{{Name: "ci/test", State: "failure", URL: "https://ci/1"}}}
}

func TestSeedingCopiesDefaultsExceptAutomationOnForks(t *testing.T) {
	h := newHarness(t)
	h.settings(Settings{Defaults: Switches{AutoFix: true, AutoMerge: true, OnMerged: true}})
	h.engine.Tick(context.Background())
	record := h.record()
	require.True(t, record.Seeded)
	require.Equal(t, Switches{AutoFix: true, AutoMerge: true, OnMerged: true}, record.Switches)

	fork := newHarness(t)
	fork.settings(Settings{Defaults: Switches{AutoFix: true, AutoMerge: true, OnMerged: true}})
	fork.setObs(func(obs *Observation) { obs.Fork = true })
	fork.engine.Tick(context.Background())
	record = fork.record()
	require.True(t, record.Fork)
	require.Equal(t, Switches{OnMerged: true}, record.Switches, "a default never turns on auto-fix or auto-merge for a fork")
}

func TestSeedingWithoutDefaultsMakesNoRequest(t *testing.T) {
	h := newHarness(t)
	h.engine.Tick(context.Background())
	record := h.record()
	require.True(t, record.Seeded)
	require.False(t, record.Switches.Any())
	require.Zero(t, h.observer.calls)
	h.engine.Tick(context.Background())
	require.Zero(t, h.observer.calls, "a record with every switch off is not observed")
}

func TestUnlinkedRecordIsDeleted(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.links.refs[testWorkspace] = nil
	h.engine.Tick(context.Background())
	_, found, err := h.engine.store.Get(context.Background(), testWorkspace, testRef)
	require.NoError(t, err)
	require.False(t, found)
}

func TestTickDoesNotOverwriteARecordThePanelCreatedMeanwhile(t *testing.T) {
	h := newHarness(t)
	h.settings(Settings{Defaults: Switches{AutoMerge: true}})
	// After the tick listed the records, a human turns auto-merge off for
	// this pull request, which creates its record.
	h.state.onGet = func(key string) {
		if key != settingsKey {
			return
		}
		h.state.onGet = nil
		_, err := h.engine.Upsert(context.Background(), testWorkspace, testRef, func(r *Record, created bool) error {
			require.True(t, created)
			r.ApplySwitches(Switches{OnMerged: true})
			return nil
		})
		require.NoError(t, err)
	}
	h.engine.Tick(context.Background())
	record := h.record()
	require.True(t, record.Seeded)
	require.Equal(t, Switches{OnMerged: true}, record.Switches, "the workspace default does not replace a human's choice")
}

func TestUnseededRecordBacksOffWhenItCannotBeObserved(t *testing.T) {
	h := newHarness(t)
	h.settings(Settings{Defaults: Switches{AutoFix: true}})
	h.observer.err = context.DeadlineExceeded
	h.engine.Tick(context.Background())
	require.Equal(t, 1, h.observer.calls)
	require.False(t, h.record().Seeded)

	h.advance(defaultTick)
	h.engine.Tick(context.Background())
	require.Equal(t, 1, h.observer.calls, "a failing record waits errorInterval like any other")

	h.advance(errorInterval)
	h.observer.err = nil
	h.engine.Tick(context.Background())
	require.Equal(t, 2, h.observer.calls)
	require.True(t, h.record().Seeded)
}

func TestIntegrationSwitchedOffDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true, AutoMerge: true})
	h.setObs(failingCI)
	h.enabled = false
	calls := h.observer.calls
	h.advance(time.Hour)
	h.engine.Tick(context.Background())
	require.Equal(t, calls, h.observer.calls)
	require.Empty(t, h.messenger.sent)
	require.Empty(t, h.merger.calls)
}

func TestAutoFixWaitsForCIThenSendsOneAttempt(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(func(obs *Observation) { obs.CI = CI{State: CIPending} })
	record := h.evaluate()
	require.Empty(t, h.messenger.sent)
	require.Contains(t, record.Status, "waiting for CI")

	h.setObs(failingCI)
	record = h.evaluate()
	require.Len(t, h.messenger.sent, 1)
	require.Equal(t, 1, record.Rounds)
	require.NotNil(t, record.Attempt)
	require.Equal(t, "s-1", record.Attempt.SessionID)
	text := h.messenger.sent[0].text
	require.Contains(t, text, "attempt "+record.Attempt.ID)
	require.Contains(t, text, "ci/test (failure)")
	require.Contains(t, text, `op "outcome"`)

	// In flight: the session is busy, nothing more is sent.
	h.messenger.sessions[testRef.TaskID][0].State = sessionRunning
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 9, Author: "alice", Body: "also rename this"}}
	})
	h.evaluate()
	require.Len(t, h.messenger.sent, 1, "one attempt in flight at a time")
}

func TestOutcomeAdvancesCheckpointSoTheSameFeedbackIsNotResent(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.evaluate()
	require.Len(t, h.messenger.sent, 1)

	record, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "s-1", "", OutcomeNonActionable, "flaky")
	require.NoError(t, err)
	require.Nil(t, record.Attempt)
	require.Equal(t, "flaky", record.LastSummary)

	h.advance(time.Minute)
	h.evaluate()
	require.Len(t, h.messenger.sent, 1, "the same failing check on the same head is not new")

	// A new head failing the same check is new feedback.
	h.setObs(func(obs *Observation) { obs.HeadSHA = "bbbbbbbbbbbb" })
	h.evaluate()
	require.Len(t, h.messenger.sent, 2)
}

func TestActionTakenHoldsUntilTheHeadMovesOrTheWindowPasses(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.evaluate()
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "s-1", "", OutcomeActionTaken, "pushed a fix")
	require.NoError(t, err)

	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 3, Author: "alice", Body: "and the docs"}}
	})
	record := h.evaluate()
	require.Len(t, h.messenger.sent, 1)
	require.Contains(t, record.Status, "waiting for the agent's change")

	h.setObs(func(obs *Observation) { obs.HeadSHA = "cccccccccccc" })
	h.evaluate()
	require.Len(t, h.messenger.sent, 2, "a new head ends the hold")
}

func TestActionTakenHoldExpires(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.evaluate()
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeActionTaken, "")
	require.NoError(t, err)
	h.setObs(func(obs *Observation) { obs.Comments = []Comment{{ID: 3, Author: "alice", Body: "x"}} })
	h.evaluate()
	require.Len(t, h.messenger.sent, 1)
	h.advance(progressWindow + time.Second)
	h.evaluate()
	require.Len(t, h.messenger.sent, 2)
}

func TestTurnEndingWithoutOutcomeResendsItsFeedback(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(func(obs *Observation) {
		failingCI(obs)
		obs.Comments = []Comment{{ID: 1, Author: "alice", Body: "old"}}
	})
	h.evaluate()
	first := h.record().Attempt
	require.NotNil(t, first)
	h.engine.Tick(context.Background()) // builds the task index

	// A turn that started before the prompt was sent is not its turn.
	h.engine.TurnCompleted(context.Background(), testRef.TaskID, "s-1", first.SentAt.Add(-time.Second))
	require.Equal(t, AttemptDispatched, h.record().Attempt.State)

	h.engine.TurnCompleted(context.Background(), testRef.TaskID, "s-1", first.SentAt)
	require.Equal(t, AttemptRetryable, h.record().Attempt.State)

	record := h.evaluate()
	require.Len(t, h.messenger.sent, 1, "a resend waits out the cool-down")
	require.Contains(t, record.Status, "retried shortly")

	h.advance(retryCooldown)
	record = h.evaluate()
	require.Len(t, h.messenger.sent, 2)
	require.Equal(t, 2, record.Rounds, "a resend spends a round")
	require.NotEqual(t, first.ID, record.Attempt.ID)
	retry := h.messenger.sent[1].text
	require.Contains(t, retry, "ended without reporting an outcome")
	require.Contains(t, retry, "ci/test")
	require.Contains(t, retry, "old")
}

func TestRetryLeavesOutWhatAnEarlierOutcomeHandled(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 1, Author: "alice", Body: "handled already"}}
	})
	h.evaluate()
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeActionTaken, "")
	require.NoError(t, err)

	h.setObs(func(obs *Observation) {
		obs.HeadSHA = "bbbbbbbbbbbb"
		obs.Comments = append(obs.Comments, Comment{ID: 2, Author: "alice", Body: "one more thing"})
	})
	h.evaluate()
	require.Len(t, h.messenger.sent, 2)
	h.advance(idleGrace + retryCooldown)
	h.evaluate()
	require.Len(t, h.messenger.sent, 3)
	retry := h.messenger.sent[2].text
	require.Contains(t, retry, "ended without reporting an outcome")
	require.Contains(t, retry, "one more thing")
	require.NotContains(t, retry, "handled already")
}

func TestRetryOfOnlyBotFeedbackIsDropped(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{
			{ID: 1, Author: "alice", Body: "rename it"},
			{ID: 2, Author: "ci[bot]", Bot: true, Body: "coverage report"},
		}
	})
	h.evaluate()
	h.setObs(func(obs *Observation) { obs.Comments = obs.Comments[1:] })
	h.advance(idleGrace + retryCooldown)
	record := h.evaluate()
	require.Len(t, h.messenger.sent, 1, "a bot's comment alone does not wake the agent, on a retry either")
	require.Nil(t, record.Attempt)
}

func TestIdleSessionWithoutOutcomeBecomesRetryableAfterGrace(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.evaluate()
	h.advance(idleGrace / 2)
	h.evaluate()
	require.Len(t, h.messenger.sent, 1, "idle but within the grace period")
	h.advance(idleGrace)
	h.evaluate()
	require.Len(t, h.messenger.sent, 2)
}

func TestRoundsAreCappedAndResetByTogglingAutoFix(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	for i := 0; i < MaxRounds+2; i++ {
		head := strings.Repeat(string(rune('a'+i)), 12)
		h.setObs(func(obs *Observation) {
			failingCI(obs)
			obs.HeadSHA = head
		})
		h.evaluate()
		if h.record().Attempt != nil {
			_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeNonActionable, "")
			require.NoError(t, err)
		}
	}
	record := h.record()
	require.Len(t, h.messenger.sent, MaxRounds)
	require.True(t, record.Exhausted)
	require.Contains(t, record.Status, "paused after 10 rounds")

	_, err := h.engine.Update(context.Background(), testWorkspace, testRef, func(r *Record) error {
		r.ApplySwitches(Switches{})
		r.ApplySwitches(Switches{AutoFix: true})
		return nil
	})
	require.NoError(t, err)
	h.setObs(func(obs *Observation) { obs.HeadSHA = "zzzzzzzzzzzz" })
	record = h.evaluate()
	require.Len(t, h.messenger.sent, MaxRounds+1)
	require.Equal(t, 1, record.Rounds)
}

func TestExhaustedRetryDoesNotHoldAutoMergeBack(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true, AutoMerge: true})
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 1, Author: "alice", Body: "nit"}}
	})
	_, err := h.engine.Update(context.Background(), testWorkspace, testRef, func(r *Record) error {
		r.Rounds = MaxRounds
		r.Attempt = &Attempt{ID: "last", Round: MaxRounds, State: AttemptRetryable, SessionID: "s-1", SentAt: h.now.Add(-time.Hour)}
		return nil
	})
	require.NoError(t, err)
	record := h.evaluate()
	require.True(t, record.Exhausted)
	require.Nil(t, record.Attempt)
	require.Len(t, h.merger.calls, 1)
}

func TestSendFailuresSpendNoRound(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.setObs(failingCI)
	h.messenger.sendErr = ErrNotApproved
	record := h.evaluate()
	require.Zero(t, record.Rounds)
	require.Nil(t, record.Attempt)
	require.Contains(t, record.Error, "re-approve")

	h.messenger.sendErr = nil
	h.messenger.sessions[testRef.TaskID] = nil
	record = h.evaluate()
	require.Zero(t, record.Rounds)
	require.Contains(t, record.Status, "Waiting for a session")

	h.messenger.sessions[testRef.TaskID] = []Session{{ID: "s-2", State: sessionIdle}}
	record = h.evaluate()
	require.Equal(t, 1, record.Rounds)
	require.Empty(t, record.Error)
}

func TestForkNeverWakesANeverStartedAgent(t *testing.T) {
	h := newHarness(t)
	h.setObs(func(obs *Observation) {
		obs.Fork = true
		failingCI(obs)
	})
	h.switches(Switches{AutoFix: true})
	h.messenger.sessions[testRef.TaskID] = []Session{{ID: "s-new", State: sessionCreated}}
	record := h.evaluate()
	require.Empty(t, h.messenger.sent)
	require.Contains(t, record.Status, "has not been started")

	// The same task off a fork would be started, as on GitHub.
	same := newHarness(t)
	same.setObs(failingCI)
	same.switches(Switches{AutoFix: true})
	same.messenger.sessions[testRef.TaskID] = []Session{{ID: "s-new", State: sessionCreated}}
	same.evaluate()
	require.Len(t, same.messenger.sent, 1)
}

func TestPickSessionPrefersTheAttemptsSessionThenTheNewest(t *testing.T) {
	sessions := []Session{
		{ID: "old", State: sessionIdle, StartedAt: "2026-01-01T00:00:00Z"},
		{ID: "new", State: sessionIdle, StartedAt: "2026-02-01T00:00:00Z"},
		{ID: "dead", State: "FAILED", StartedAt: "2026-03-01T00:00:00Z"},
	}
	id, _ := pickSession(sessions, "old", false)
	require.Equal(t, "old", id)
	id, _ = pickSession(sessions, "dead", false)
	require.Equal(t, "new", id)
	id, why := pickSession([]Session{{ID: "x", State: "COMPLETED"}}, "", false)
	require.Empty(t, id)
	require.NotEmpty(t, why)
}

func TestOutcomeBinding(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeBlocked, "")
	require.ErrorIs(t, err, ErrNoAttempt)

	h.setObs(failingCI)
	h.evaluate()
	_, err = h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "another-session", "", OutcomeBlocked, "")
	require.ErrorIs(t, err, ErrNoAttempt, "only the session the attempt went to may report it")
	_, err = h.engine.Outcome(context.Background(), testWorkspace, "other-task", "", "", OutcomeBlocked, "")
	require.ErrorIs(t, err, ErrNoAttempt)

	record, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "s-1", "o/r", OutcomeBlocked, strings.Repeat("x", 5000))
	require.NoError(t, err)
	require.Len(t, record.LastSummary, maxSummaryBytes)
	require.Equal(t, record.HeadSHA, record.BlockedHead)
}

func TestOutcomeAmbiguousAcrossPullRequests(t *testing.T) {
	h := newHarness(t)
	other := PullRef{TaskID: testRef.TaskID, RepositoryID: "43", Number: 8}
	h.links.refs[testWorkspace] = []PullRef{testRef, other}
	second := openObservation()
	second.Repository = "o/other"
	failingCI(&second)
	h.observer.obs[other] = second
	h.setObs(failingCI)
	h.engine.Tick(context.Background())
	for _, ref := range []PullRef{testRef, other} {
		_, err := h.engine.Update(context.Background(), testWorkspace, ref, func(r *Record) error {
			r.ApplySwitches(Switches{AutoFix: true})
			return nil
		})
		require.NoError(t, err)
		h.engine.Evaluate(context.Background(), testWorkspace, ref)
	}
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeBlocked, "")
	require.ErrorIs(t, err, ErrAmbiguousAttempt)
	record, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "O/OTHER", OutcomeBlocked, "")
	require.NoError(t, err)
	require.Equal(t, other.Number, record.Number)
}

func TestAutoMergeGates(t *testing.T) {
	two := 2
	cases := []struct {
		name   string
		change func(*Observation, *Record)
		want   string
	}{
		{"draft", func(o *Observation, _ *Record) { o.Draft = true }, "draft"},
		{"ci pending", func(o *Observation, _ *Record) { o.CI.State = CIPending }, "waiting for CI"},
		{"ci failing", func(o *Observation, _ *Record) { failingCI(o) }, "CI is failing"},
		{"mergeability unknown", func(o *Observation, _ *Record) { o.Mergeable = nil }, "mergeability"},
		{"conflicts", func(o *Observation, _ *Record) { no := false; o.Mergeable = &no }, "conflicts"},
		{"changes requested", func(o *Observation, _ *Record) {
			o.Reviews = []Review{{ID: 1, Author: "bob", State: ReviewChangesRequested, Official: true}}
		}, "changes are requested"},
		{"approvals", func(o *Observation, _ *Record) {
			o.RequiredApprovals = &two
			o.Reviews = []Review{{ID: 1, Author: "bob", State: ReviewApproved, Official: true},
				{ID: 2, Author: "eve", State: ReviewApproved, Official: false}}
		}, "1 of 2 required approvals"},
		{"unresolved", func(o *Observation, _ *Record) {
			o.Reviews = []Review{{ID: 1, Author: "bob", State: ReviewCommented,
				Comments: []InlineComment{{ID: 5, Author: "bob", Body: "hm"}}}}
		}, "unresolved"},
		{"blocked", func(o *Observation, r *Record) {
			r.BlockedHead, r.BlockedUntil = o.HeadSHA, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		}, "blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.switches(Switches{AutoMerge: true})
			_, err := h.engine.Update(context.Background(), testWorkspace, testRef, func(r *Record) error {
				obs := openObservation()
				tc.change(&obs, r)
				h.observer.obs[testRef] = obs
				return nil
			})
			require.NoError(t, err)
			record := h.evaluate()
			require.Empty(t, h.merger.calls)
			require.Contains(t, record.Status, tc.want)
		})
	}
}

func TestAutoMergeLatestReviewWinsAndMergesPinnedToHead(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true})
	one := 1
	h.setObs(func(obs *Observation) {
		obs.RequiredApprovals = &one
		obs.Reviews = []Review{
			{ID: 1, Author: "bob", State: ReviewChangesRequested, Official: true},
			{ID: 2, Author: "bob", State: ReviewApproved, Official: true},
		}
	})
	record := h.evaluate()
	require.Equal(t, []mergeCall{{ref: testRef, head: "aaaaaaaaaaaa"}}, h.merger.calls)
	require.True(t, record.MergedByPlugin)
}

func TestAutoMergeFailureIsNotRetriedUntilSomethingChanges(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true})
	h.merger.err = &MergeError{Message: "Branch protection prevents this merge."}
	record := h.evaluate()
	require.Len(t, h.merger.calls, 1)
	require.Equal(t, "Branch protection prevents this merge.", record.MergeFailure)

	h.evaluate()
	require.Len(t, h.merger.calls, 1, "same state, no automatic retry")

	_, err := h.engine.Update(context.Background(), testWorkspace, testRef, func(r *Record) error {
		r.MergeRetry = true
		return nil
	})
	require.NoError(t, err)
	h.evaluate()
	require.Len(t, h.merger.calls, 2, "the panel's retry")
	h.evaluate()
	require.Len(t, h.merger.calls, 2, "a retry is one attempt")

	h.setObs(func(obs *Observation) { obs.HeadSHA = "dddddddddddd" })
	h.evaluate()
	require.Len(t, h.merger.calls, 3, "a changed pull request may try again")
}

func TestAutoMergeTransientRefusalIsRetried(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true})
	h.merger.err = &MergeError{Transient: true, Message: "later"}
	record := h.evaluate()
	require.Empty(t, record.MergeFailure)
	h.evaluate()
	require.Len(t, h.merger.calls, 2)
}

func TestAutoMergeWaitsForAutoFix(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true, AutoFix: true})
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 1, Author: "alice", Body: "please rename"}}
	})
	record := h.evaluate()
	require.Len(t, h.messenger.sent, 1)
	require.Empty(t, h.merger.calls)
	require.Contains(t, record.Status, "auto-fix round is in progress")
}

func TestAutoMergeWaitsForTheChangeTheAgentReported(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true, AutoFix: true})
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 1, Author: "alice", Body: "please rename"}}
	})
	h.evaluate()
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeActionTaken, "")
	require.NoError(t, err)

	record := h.evaluate()
	require.Empty(t, h.merger.calls, "the head the agent was asked to change is not merged")
	require.Contains(t, record.Status, "waiting for the agent's change")

	h.setObs(func(obs *Observation) { obs.HeadSHA = "dddddddddddd" })
	h.evaluate()
	require.Equal(t, []mergeCall{{ref: testRef, head: "dddddddddddd"}}, h.merger.calls)
}

func TestAutoMergeHoldForTheAgentsChangeExpires(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true, AutoFix: true})
	h.setObs(func(obs *Observation) {
		obs.Comments = []Comment{{ID: 1, Author: "alice", Body: "is this right?"}}
	})
	h.evaluate()
	_, err := h.engine.Outcome(context.Background(), testWorkspace, testRef.TaskID, "", "", OutcomeActionTaken, "answered")
	require.NoError(t, err)
	h.evaluate()
	require.Empty(t, h.merger.calls)
	h.advance(progressWindow + time.Second)
	h.evaluate()
	require.Len(t, h.merger.calls, 1, "a change that needed no push merges once the window passes")
}

func TestLifecyclePrompts(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{OnMerged: true, OnClosed: true, OnReviewRequested: true})

	h.evaluate() // baseline
	require.Empty(t, h.messenger.sent)

	h.setObs(func(obs *Observation) { obs.OperatorRequested = true })
	h.evaluate()
	require.Len(t, h.messenger.sent, 1)
	require.Contains(t, h.messenger.sent[0].text, "Your review was requested on o/r#7")
	h.evaluate()
	require.Len(t, h.messenger.sent, 1, "still requested is not a new request")

	h.setObs(func(obs *Observation) { obs.State = StateClosed })
	h.evaluate()
	h.setObs(func(obs *Observation) { obs.State = StateOpen })
	h.evaluate()
	h.setObs(func(obs *Observation) { obs.State = StateClosed })
	h.evaluate()
	require.Len(t, h.messenger.sent, 3)
	require.Contains(t, h.messenger.sent[1].text, "closed without merging")
	require.NotEqual(t, h.messenger.sent[1].text, h.messenger.sent[2].text, "a repeat must not read as a replay")

	h.setObs(func(obs *Observation) { obs.State = StateOpen })
	h.evaluate()
	h.setObs(func(obs *Observation) { obs.State = StateMerged })
	record := h.evaluate()
	require.Len(t, h.messenger.sent, 4)
	require.Contains(t, h.messenger.sent[3].text, "was merged")
	require.True(t, record.Terminal())

	// Merged never reverts.
	h.setObs(func(obs *Observation) { obs.State = StateClosed })
	record = h.evaluate()
	require.Len(t, h.messenger.sent, 4)
	require.Equal(t, StateMerged, record.LastState)
}

func TestLifecycleBaselineDoesNotFireForAnAlreadyMergedPullRequest(t *testing.T) {
	h := newHarness(t)
	h.setObs(func(obs *Observation) { obs.State = StateMerged })
	h.switches(Switches{OnMerged: true})
	record := h.evaluate()
	require.Empty(t, h.messenger.sent)
	require.True(t, record.Terminal())
}

func TestLifecyclePromptWaitsForASession(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{OnMerged: true})
	h.evaluate()
	h.messenger.sessions[testRef.TaskID] = nil
	h.setObs(func(obs *Observation) { obs.State = StateMerged })
	record := h.evaluate()
	require.Empty(t, h.messenger.sent)
	require.Len(t, record.Pending, 1)
	require.False(t, record.Terminal(), "a merged record with an undelivered prompt is still due")

	h.messenger.sessions[testRef.TaskID] = []Session{{ID: "s-9", State: sessionIdle}}
	h.advance(pendingInterval)
	h.engine.Tick(context.Background())
	record = h.record()
	require.Len(t, h.messenger.sent, 1)
	require.Empty(t, record.Pending)
	require.True(t, record.Terminal())
}

func TestObserveErrorIsDescribedAndBacksOff(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoFix: true})
	h.observer.err = context.DeadlineExceeded
	record := h.evaluate()
	require.Equal(t, "described", record.Error)
	require.Equal(t, h.now.Add(errorInterval), record.NextDue)
}

func TestTickBudget(t *testing.T) {
	h := newHarness(t)
	var refs []PullRef
	for i := 0; i < budgetPerWorkspace+5; i++ {
		ref := PullRef{TaskID: "t", RepositoryID: "1", Number: int64(i + 1)}
		refs = append(refs, ref)
		h.observer.obs[ref] = openObservation()
	}
	h.links.refs[testWorkspace] = refs
	h.settings(Settings{Defaults: Switches{OnMerged: true}})
	h.engine.Tick(context.Background())
	require.Equal(t, budgetPerWorkspace, h.observer.calls)
}

func TestAutoMergeWaitsBeforeTakingNoChecksForNoCI(t *testing.T) {
	h := newHarness(t)
	h.switches(Switches{AutoMerge: true})
	h.setObs(func(obs *Observation) { obs.CI = CI{State: CINone} })
	record := h.evaluate()
	require.Empty(t, h.merger.calls)
	require.Contains(t, record.Status, "waiting for CI to report")

	h.advance(noCIGrace / 2)
	h.setObs(func(obs *Observation) { obs.HeadSHA = "eeeeeeeeeeee" })
	h.evaluate()
	h.advance(noCIGrace / 2)
	h.evaluate()
	require.Empty(t, h.merger.calls, "a new head starts the wait again")

	h.advance(noCIGrace)
	h.evaluate()
	require.Equal(t, []mergeCall{{ref: testRef, head: "eeeeeeeeeeee"}}, h.merger.calls)
}
