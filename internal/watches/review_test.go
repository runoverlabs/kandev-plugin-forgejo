package watches

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// reviewRig wires a poller over a host with two workflow steps: an inbox that
// does not start agents and a "run" step that does.
type reviewRig struct {
	host    *fakeHost
	reviews *fakeReviews
	poller  *Poller
	store   *Store
	watch   Watch
}

func newReviewRig(t *testing.T) *reviewRig {
	t.Helper()
	host := newFakeHost("ws-1")
	host.steps = map[string][]pluginsdk.WorkflowStep{"wf-1": {
		{ID: "step-inbox", WorkflowID: "wf-1"},
		{ID: "step-run", WorkflowID: "wf-1", OnEnterActionTypes: []string{autoStartAction}},
		{ID: "step-safe", WorkflowID: "wf-1", OnEnterActionTypes: []string{"notify"}},
	}}
	return rigOver(t, host, host)
}

func rigOver(t *testing.T, host *fakeHost, provided pluginsdk.Host) *reviewRig {
	t.Helper()
	reviews := newFakeReviews()
	store := NewStore(func() pluginsdk.Host { return provided })
	store.now = newTestStore(host).now
	poller := NewPoller(store, func() pluginsdk.Host { return provided }, newFakeIssues(), nil).WithReviews(reviews)
	poller.now = store.now
	saved, err := store.Put(context.Background(), sampleReviewWatch("ws-1"))
	require.NoError(t, err)
	return &reviewRig{host: host, reviews: reviews, poller: poller, store: store, watch: saved}
}

func (r *reviewRig) run(t *testing.T) Result {
	t.Helper()
	result, err := r.poller.RunWatch(context.Background(), r.watch)
	require.NoError(t, err)
	return result
}

func TestReviewWatchFilesTaskWithInterpolatedPromptAndCheckout(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.AgentProfileID = "agent-1"
	rig.watch.Prompt = "Look at {{pr.repo}}#{{pr.number}} ({{pr.title}}) by {{pr.author}}: {{pr.branch}} into {{pr.base_branch}} {{pr.link}} {{other}}"
	rig.reviews.add("acme/web", 7, openPR("Add export"))

	result := rig.run(t)
	require.Equal(t, 1, result.Created)
	require.Len(t, rig.host.created, 1)
	created := rig.host.created[0]
	require.Equal(t, "PR #7: Add export", created.Title)
	require.Equal(t, "Look at acme/web#7 (Add export) by alice: feature/x into main https://git.example/acme/web/pulls/1 {{other}}", created.Description)
	require.Equal(t, "step-inbox", *created.WorkflowStepID)
	require.Len(t, created.Repositories, 1)
	require.Equal(t, "feature/x", *created.Repositories[0].CheckoutBranch)
	require.Equal(t, int64(7), *created.Repositories[0].PullRequestNumber)
	require.Equal(t, "main", *created.Repositories[0].BaseBranch)
	require.Equal(t, "agent-1", *created.Launch.AgentProfileID)
	require.Equal(t, "acme/web", created.Metadata["forgejo_repo"])
	require.NotContains(t, created.Metadata, "forgejo_fork")
}

func TestReviewWatchUsesTheDefaultPromptWhenNoneIsSet(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.reviews.add("acme/web", 7, openPR("Add export"))
	rig.run(t)
	description := rig.host.created[0].Description
	require.Contains(t, description, "Review Pull Request #7: Add export")
	require.Contains(t, description, "git diff origin/main...HEAD")
	require.NotContains(t, description, "{{")
}

func TestReviewWatchDoesNotFileTheSamePullRequestTwice(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.reviews.add("acme/web", 7, openPR("Add export"))
	require.Equal(t, 1, rig.run(t).Created)
	second := rig.run(t)
	require.Zero(t, second.Created)
	require.Equal(t, 1, second.Duplicates)
	require.Len(t, rig.host.created, 1)
	// A pull request already in the ledger costs no detail request.
	require.Len(t, rig.reviews.detailed, 1)
}

func TestReviewWatchPassesItsFilterToTheSource(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.Repos = []RepoRef{{Owner: "acme", Name: "web"}}
	rig.watch.Labels = []string{"needs-review"}
	rig.watch.Query = "export"
	rig.run(t)
	require.Equal(t, ReviewQuery{Repos: rig.watch.Repos, Labels: []string{"needs-review"}, Query: "export"}, rig.reviews.query)
}

func TestReviewWatchSkipsDraftsUnlessAsked(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	draft := openPR("WIP: later")
	draft.Draft = true
	rig.reviews.add("acme/web", 8, draft)

	result := rig.run(t)
	require.Zero(t, result.Created)
	require.Equal(t, 1, result.Drafts)

	rig.watch.IncludeDrafts = true
	require.Equal(t, 1, rig.run(t).Created)
}

func TestReviewWatchUserScopeNeedsADirectRequest(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.ReviewScope = ReviewScopeUser
	viaTeam := openPR("Team request")
	viaTeam.RequestedFromMe = false
	rig.reviews.add("acme/web", 8, viaTeam)
	rig.reviews.add("acme/web", 9, openPR("Direct request"))

	result := rig.run(t)
	require.Equal(t, 1, result.Created)
	require.Equal(t, "PR #9: Direct request", rig.host.created[0].Title)
}

func TestReviewWatchSkipsAPullRequestThatIsNoLongerOpen(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	closed := openPR("Closed meanwhile")
	closed.Open = false
	rig.reviews.add("acme/web", 8, closed)
	require.Zero(t, rig.run(t).Created)
}

func TestReviewWatchBudgetStopsBeforeFetchingDetail(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.MaxInflightTasks = 1
	rig.reviews.add("acme/web", 1, openPR("One"))
	rig.reviews.add("acme/web", 2, openPR("Two"))
	result := rig.run(t)
	require.Equal(t, 1, result.Created)
	require.Equal(t, 1, result.Throttled)
	require.Equal(t, []string{"acme/web#1"}, rig.reviews.detailed)
}

func TestReviewWatchBranchThatIsNotSafeGetsNoCheckout(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"--upload-pack=x", "a..b", "with space", "x.lock", "", "feat/\nx"} {
		t.Run(branch, func(t *testing.T) {
			t.Parallel()
			rig := newReviewRig(t)
			detail := openPR("Odd branch")
			detail.HeadBranch = branch
			rig.reviews.add("acme/web", 1, detail)
			require.Equal(t, 1, rig.run(t).Created)
			attached := rig.host.created[0].Repositories[0]
			require.Nil(t, attached.CheckoutBranch)
			require.Nil(t, attached.PullRequestNumber)
		})
	}
}

func forkPR(title string) PullDetail {
	detail := openPR(title)
	detail.Fork = true
	return detail
}

func TestForkPullRequestNeverChecksOutOrStartsAnAgent(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.StartAgent = true
	rig.watch.AgentProfileID = "agent-1"
	rig.reviews.add("acme/web", 3, forkPR("From a fork"))

	require.Equal(t, 1, rig.run(t).Created)
	created := rig.host.created[0]
	require.False(t, created.StartAgent)
	require.Equal(t, "step-inbox", *created.WorkflowStepID)
	require.Nil(t, created.Repositories[0].CheckoutBranch, "a fork's head is content its author controls")
	require.Nil(t, created.Repositories[0].PullRequestNumber)
	require.Equal(t, true, created.Metadata["forgejo_fork"])
}

func TestForkPullRequestUsesTheForkStep(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.WorkflowStepID = "step-run"
	rig.watch.ForkWorkflowStepID = "step-safe"
	rig.reviews.add("acme/web", 3, forkPR("From a fork"))
	rig.reviews.add("acme/web", 4, openPR("Same repo"))

	result := rig.run(t)
	require.Equal(t, 2, result.Created)
	require.Equal(t, "step-safe", *rig.host.created[0].WorkflowStepID)
	require.Equal(t, "step-run", *rig.host.created[1].WorkflowStepID)
}

func TestForkPullRequestIsSkippedWhenNoStepIsSafe(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Watch){
		"main step starts agents, no fork step": func(w *Watch) { w.WorkflowStepID = "step-run" },
		"fork step starts agents":               func(w *Watch) { w.ForkWorkflowStepID = "step-run" },
		"fork step is not in the workflow":      func(w *Watch) { w.ForkWorkflowStepID = "step-gone" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newReviewRig(t)
			mutate(&rig.watch)
			rig.reviews.add("acme/web", 3, forkPR("From a fork"))
			result := rig.run(t)
			require.Zero(t, result.Created)
			require.Equal(t, 1, result.SkippedForks)
			require.Empty(t, rig.host.created)
		})
	}
}

func TestForkPullRequestIsSkippedWhenStepsCannotBeRead(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.host.stepsErr = errBoom
	rig.reviews.add("acme/web", 3, forkPR("From a fork"))
	rig.reviews.add("acme/web", 4, openPR("Same repo"))
	result := rig.run(t)
	require.Equal(t, 1, result.SkippedForks, "unknown placement fails closed")
	require.Equal(t, 1, result.Created, "a same-repository pull request does not need the step list")
}

func TestReviewWatchSearchFailureIsReported(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.reviews.searchErr = errors.New("the review search is not visible to this token")
	result, err := rig.poller.RunWatch(context.Background(), rig.watch)
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	stored, err := rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.NoError(t, err)
	require.Contains(t, stored.LastError, "review search")
}

func TestReviewWatchWithoutASourceReportsInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	store := newTestStore(host)
	poller := NewPoller(store, func() pluginsdk.Host { return host }, newFakeIssues(), nil)
	saved, err := store.Put(context.Background(), sampleReviewWatch("ws-1"))
	require.NoError(t, err)
	result, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.NotEmpty(t, result.Errors)
}

func TestReviewWatchDoesNotDisturbIssueWatches(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash"))
	rig.poller.issues = issues
	rig.reviews.add("acme/web", 7, openPR("Add export"))
	issueWatch := sampleWatch("ws-1", "acme/app")
	issueWatch.ID = "w-issue"
	saved, err := rig.store.Put(context.Background(), issueWatch)
	require.NoError(t, err)
	result, err := rig.poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 1, result.Created)
	require.Empty(t, rig.reviews.detailed, "an issue watch never touches the review source")
	require.Equal(t, "#1 Crash", rig.host.created[0].Title)
}

// --- cleanup ---------------------------------------------------------------

// filedPR files one pull request through a normal run and returns its task id.
func filedPR(t *testing.T, rig *reviewRig, number int64) string {
	t.Helper()
	rig.reviews.add("acme/web", number, openPR(fmt.Sprintf("PR %d", number)))
	require.NoError(t, rigErr(rig.poller.RunWatch(context.Background(), rig.watch)))
	// Filing runs a cleanup pass too; start each test's accounting afresh.
	rig.reviews.checked = nil
	return rig.host.tasks[len(rig.host.tasks)-1].ID
}

func rigErr(_ Result, err error) error { return err }

func (r *reviewRig) finish(number int64, state PullState) {
	r.reviews.states[prKey(RepoRef{Owner: "acme", Name: "web"}, number)] = state
}

func TestCleanupPolicyNeverMakesNoRequests(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	filedPR(t, rig, 7)
	rig.finish(7, PullMerged)
	rig.run(t)
	require.Empty(t, rig.reviews.checked)
	require.Empty(t, rig.host.updated)
}

func TestCleanupCompletesTheTaskOfAMergedPullRequestWithoutTheGrant(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	taskID := filedPR(t, rig, 7)
	rig.finish(7, PullMerged)

	result := rig.run(t)
	require.Equal(t, 1, result.Completed)
	require.Equal(t, archiveUnsupportedNote, result.CleanupNote, "a host without the v2 contract cannot archive")
	require.Len(t, rig.host.updated, 1)
	require.Equal(t, taskID, rig.host.updated[0].ID)
	require.Equal(t, "COMPLETED", *rig.host.updated[0].State)
	stored, err := rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.NoError(t, err)
	require.Equal(t, archiveUnsupportedNote, stored.LastCleanupNote)

	// The task is done now: the next pass makes no provider request for it, and
	// the ledger still stops the pull request being filed again.
	rig.reviews.checked = nil
	rig.run(t)
	require.Empty(t, rig.reviews.checked)
	require.Len(t, rig.host.created, 1)
}

func TestCleanupLeavesAnOpenPullRequestAlone(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	filedPR(t, rig, 7)
	rig.run(t)
	require.Equal(t, []string{"acme/web#7"}, rig.reviews.checked)
	require.Empty(t, rig.host.updated)
}

func archivingRig(t *testing.T, granted bool, outcomes ...pluginsdk.CommandStatus) (*reviewRig, *archivingHost) {
	t.Helper()
	base := newFakeHost("ws-1")
	host := &archivingHost{fakeHost: base, granted: granted, outcomes: outcomes}
	rig := rigOver(t, base, host)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	return rig, host
}

func TestCleanupArchivesWhenTheOperatorGrantedIt(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, true)
	taskID := filedPR(t, rig, 7)
	rig.host.tasks[0].ResourceVersion = "2026-09-19T11:00:00Z"
	rig.finish(7, PullClosed)

	result := rig.run(t)
	require.Equal(t, 1, result.Archived)
	require.Zero(t, result.Completed)
	require.Empty(t, result.CleanupNote)
	require.Len(t, host.archived, 1)
	require.Equal(t, taskID, host.archived[0].TaskID)
	require.Equal(t, "2026-09-19T11:00:00Z", host.archived[0].ExpectedResourceVersion)
	require.Equal(t, uint64(3), host.archived[0].ApprovalRevision)
	require.Empty(t, host.updated, "an archived task is not also completed")
	// Archived tasks cost nothing on the next pass.
	rig.reviews.checked = nil
	rig.run(t)
	require.Empty(t, rig.reviews.checked)
}

func TestCleanupFallsBackToCompletingWhenArchiveIsDenied(t *testing.T) {
	t.Parallel()
	for _, status := range []pluginsdk.CommandStatus{pluginsdk.CommandDenied, pluginsdk.CommandUnsupported} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			rig, _ := archivingRig(t, true, status)
			filedPR(t, rig, 7)
			rig.finish(7, PullMerged)
			result := rig.run(t)
			require.Equal(t, 1, result.Completed)
			require.NotEmpty(t, result.CleanupNote)
		})
	}
}

func TestCleanupRetriesArchiveOnceOnAVersionConflict(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, true, pluginsdk.CommandConflict, pluginsdk.CommandApplied)
	filedPR(t, rig, 7)
	rig.host.tasks[0].ResourceVersion = "2026-09-19T11:30:00Z"
	rig.finish(7, PullMerged)
	require.Equal(t, 1, rig.run(t).Archived)
	require.Len(t, host.archived, 2)
	require.Equal(t, "2026-09-19T11:30:00Z+1", host.archived[1].ExpectedResourceVersion)
	require.NotEqual(t, host.archived[0].IdempotencyKey, host.archived[1].IdempotencyKey, "a retry at a new version is a new command")
}

func TestCleanupGivesUpAfterTwoConflictsAndCompletes(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, true, pluginsdk.CommandConflict, pluginsdk.CommandConflict)
	filedPR(t, rig, 7)
	rig.finish(7, PullMerged)
	result := rig.run(t)
	require.Len(t, host.archived, 2)
	require.Equal(t, 1, result.Completed)
}

func TestCleanupNoteClearsOnceArchivingWorks(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, false)
	filedPR(t, rig, 7)
	filedPR(t, rig, 8)
	rig.finish(7, PullMerged)
	rig.run(t)
	stored, _ := rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.Equal(t, ArchiveGrantNote, stored.LastCleanupNote)

	host.granted = true
	rig.watch = stored
	rig.finish(8, PullMerged)
	require.Equal(t, 1, rig.run(t).Archived)
	stored, _ = rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.Empty(t, stored.LastCleanupNote)
}

func TestCleanupStopsTheBatchWhenTheInstanceRefuses(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	filedPR(t, rig, 1)
	filedPR(t, rig, 2)
	filedPR(t, rig, 3)
	rig.finish(1, PullMerged)
	rig.finish(2, PullMerged)
	rig.finish(3, PullMerged)
	for _, key := range []string{"acme/web#1", "acme/web#2", "acme/web#3"} {
		rig.reviews.errs[key] = fmt.Errorf("%w: rate limited", ErrBackoff)
	}
	result, err := rig.poller.RunWatch(context.Background(), rig.watch)
	require.NoError(t, err)
	require.Len(t, rig.reviews.checked, 1, "the first refusal ends the batch")
	require.NotEmpty(t, result.Errors)
	require.Empty(t, rig.host.updated)
	records, err := rig.store.Records(context.Background(), rig.watch)
	require.NoError(t, err)
	require.Len(t, records, 3, "state is intact")
}

func TestCleanupOneFailureDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	filedPR(t, rig, 1)
	filedPR(t, rig, 2)
	rig.finish(1, PullMerged)
	rig.finish(2, PullMerged)
	rig.reviews.errs["acme/web#1"] = errors.New("could not read acme/web#1")
	result := rig.run(t)
	require.Equal(t, 1, result.Completed)
	require.Len(t, result.Errors, 1)
}

func TestCleanupDropsTheRecordOfAMissingTaskOnlyOnceThePullRequestIsFinished(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	filedPR(t, rig, 7)
	rig.host.tasks = nil // the operator deleted the card

	rig.run(t)
	records, _ := rig.store.Records(context.Background(), rig.watch)
	require.Len(t, records, 1, "the pull request is still open: the record stops it being filed again")
	require.Len(t, rig.host.created, 1)

	rig.finish(7, PullMerged)
	rig.run(t)
	records, _ = rig.store.Records(context.Background(), rig.watch)
	require.Empty(t, records)
}

func TestCleanupChecksAtMostABoundedNumberOfPullRequestsPerPass(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	rig.watch.MaxInflightTasks = maxCleanupChecks + 10
	for i := int64(1); i <= maxCleanupChecks+5; i++ {
		rig.reviews.add("acme/web", i, openPR("PR"))
	}
	rig.run(t)
	rig.reviews.checked = nil
	rig.run(t)
	require.Len(t, rig.reviews.checked, maxCleanupChecks)
}

func TestManualCleanupRefusesAWatchWithoutAPolicy(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	_, err := rig.poller.Cleanup(context.Background(), rig.watch)
	require.Error(t, err)
	issueWatch := sampleWatch("ws-1", "acme/app")
	_, err = rig.poller.Cleanup(context.Background(), issueWatch)
	require.Error(t, err)
}

func TestManualCleanupDoesNotAdvanceThePollClock(t *testing.T) {
	t.Parallel()
	rig := newReviewRig(t)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	filedPR(t, rig, 7)
	rig.finish(7, PullMerged)
	before, _ := rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	result, err := rig.poller.Cleanup(context.Background(), before)
	require.NoError(t, err)
	require.Equal(t, 1, result.Completed)
	after, _ := rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.Equal(t, before.LastPolledAt, after.LastPolledAt)
}
