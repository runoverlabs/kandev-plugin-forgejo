package watches

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// newTestPoller wires a poller over a fake host at a fixed clock.
func newTestPoller(t *testing.T, host *fakeHost, issues IssueSource, gate WorkspaceGate) (*Poller, *Store) {
	t.Helper()
	store := newTestStore(host)
	poller := NewPoller(store, func() pluginsdk.Host { return host }, issues, gate)
	poller.now = store.now
	return poller, store
}

func issue(number int64, title string) Issue {
	return Issue{
		Number: number,
		Title:  title,
		Body:   "Something is broken.",
		State:  "open",
		URL:    "https://git.example/acme/app/issues/" + string(rune('0'+number)),
		Labels: []string{"bug"},
		Author: "reporter",
	}
}

func TestRunWatchCreatesOneTaskPerIssue(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash on save"), issue(2, "Slow import"))
	poller, store := newTestPoller(t, host, issues, nil)

	watch := sampleWatch("ws-1", "acme/app")
	watch.AgentProfileID = "agent-1"
	watch.ExecutorProfileID = "exec-1"
	watch.Prompt = "Fix it."
	watch.RepositoryID = "repo-1"
	watch.BaseBranch = "main"
	saved, err := store.Put(context.Background(), watch)
	require.NoError(t, err)

	result, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 2, result.Matched)
	require.Equal(t, 2, result.Created)
	require.Zero(t, result.Duplicates)
	require.Empty(t, result.Errors)
	require.Len(t, host.created, 2)

	created := host.created[0]
	require.Equal(t, "ws-1", created.WorkspaceID)
	require.Equal(t, "wf-1", created.WorkflowID)
	require.NotNil(t, created.WorkflowStepID)
	require.Equal(t, "step-inbox", *created.WorkflowStepID)
	require.Equal(t, "#1 Crash on save", created.Title)
	require.Contains(t, created.Description, "acme/app#1")
	require.Contains(t, created.Description, "opened by reporter")
	require.Contains(t, created.Description, "Something is broken.")

	// The launch settings are what make the created task run the way the
	// operator configured the watch.
	require.NotNil(t, created.Launch)
	require.Equal(t, "agent-1", *created.Launch.AgentProfileID)
	require.Equal(t, "exec-1", *created.Launch.ExecutorProfileID)
	require.Equal(t, "Fix it.", *created.Launch.Prompt)

	require.Len(t, created.Repositories, 1)
	require.Equal(t, "repo-1", created.Repositories[0].RepositoryID)
	require.Equal(t, "main", *created.Repositories[0].BaseBranch)

	require.Equal(t, "acme/app", created.Metadata["forgejo_repo"])
	require.Equal(t, saved.ID, created.Metadata["forgejo_watch_id"])
}

// The dedup ledger is the whole point of the feature: a watch that re-creates
// a card on every poll is worse than no watch at all.
func TestRunWatchIsIdempotentAcrossRuns(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash on save"))
	poller, store := newTestPoller(t, host, issues, nil)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)

	first, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 1, first.Created)

	// Re-read: the run advanced the poll clock and the ledger.
	saved, err = store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err)

	second, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Zero(t, second.Created, "the same issue must not become a second task")
	require.Equal(t, 1, second.Duplicates)
	require.Len(t, host.created, 1)
}

// Two watches matching the same issue: under the default (native-parity) watch
// scope each creates its own task; under workspace scope the issue yields one
// task in total.
func TestRunWatchDedupScope(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		scope     DedupScope
		wantTasks int
	}{
		{"watch scope creates one task per watch", DedupScopeWatch, 2},
		{"workspace scope creates one task per issue", DedupScopeWorkspace, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			host := newFakeHost("ws-1")
			issues := newFakeIssues()
			issues.set("acme/app", issue(1, "Crash on save"))
			poller, store := newTestPoller(t, host, issues, nil)

			first := sampleWatch("ws-1", "acme/app")
			first.ID, first.Name, first.DedupScope = "watch-a", "A", testCase.scope
			second := sampleWatch("ws-1", "acme/app")
			second.ID, second.Name, second.DedupScope = "watch-b", "B", testCase.scope

			savedFirst, err := store.Put(context.Background(), first)
			require.NoError(t, err)
			savedSecond, err := store.Put(context.Background(), second)
			require.NoError(t, err)

			_, err = poller.RunWatch(context.Background(), savedFirst)
			require.NoError(t, err)
			_, err = poller.RunWatch(context.Background(), savedSecond)
			require.NoError(t, err)

			require.Len(t, host.created, testCase.wantTasks)
		})
	}
}

// The inflight budget is what stops a first run against a backlog from burying
// the board.
func TestRunWatchHonoursInflightBudget(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "One"), issue(2, "Two"), issue(3, "Three"), issue(4, "Four"))
	poller, store := newTestPoller(t, host, issues, nil)

	watch := sampleWatch("ws-1", "acme/app")
	watch.MaxInflightTasks = 2
	saved, err := store.Put(context.Background(), watch)
	require.NoError(t, err)

	result, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 2, result.Created)
	require.Equal(t, 2, result.Throttled)
	require.Len(t, host.created, 2)

	// Finishing one task frees exactly one slot: the budget throttles, it does
	// not cap the watch for life.
	host.completeTask("task-1")
	saved, err = store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err)

	next, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 1, next.Created)
	require.Equal(t, 2, next.Duplicates)
	require.Len(t, host.created, 3)
}

// One unreachable repository must not stop the others, and the watch must say
// what failed.
func TestRunWatchIsolatesRepositoryFailures(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash on save"))
	issues.fail("acme/broken", errBoom)
	poller, store := newTestPoller(t, host, issues, nil)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app", "acme/broken"))
	require.NoError(t, err)

	result, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 1, result.Created, "the reachable repository still produced its task")
	require.Len(t, result.Errors, 1)
	require.Contains(t, result.Errors[0], "acme/broken")

	stored, err := store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err)
	require.Contains(t, stored.LastError, "acme/broken")
	require.NotEmpty(t, stored.LastErrorAt)
}

// A watch whose every repository failed is marked failed, and does not report
// a clean poll.
func TestRunWatchMarksTotalFailure(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.fail("acme/app", errBoom)
	poller, store := newTestPoller(t, host, issues, nil)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)

	result, err := poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)
	require.Zero(t, result.Created)
	require.Len(t, result.Errors, 1)

	stored, err := store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.LastError)
	require.NotEmpty(t, stored.LastPolledAt, "a failed poll still advances the clock so it backs off")
}

// The incremental bound is rewound by one interval, because clock skew between
// this process and the instance is real and a missed issue never reappears.
func TestRunWatchRewindsTheIncrementalBound(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app")
	poller, store := newTestPoller(t, host, issues, nil)

	watch := sampleWatch("ws-1", "acme/app")
	watch.PollIntervalSeconds = 600
	watch.LastPolledAt = "2026-09-19T11:00:00Z"
	saved, err := store.Put(context.Background(), watch)
	require.NoError(t, err)

	_, err = poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)

	require.Equal(t, time.Date(2026, 9, 19, 10, 50, 0, 0, time.UTC), issues.lastQuery().Since)
}

// The first run of a watch has no bound at all, so a backlog is visible and
// the inflight budget — not the time filter — is what meters it.
func TestRunWatchFirstRunIsUnbounded(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app")
	poller, store := newTestPoller(t, host, issues, nil)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	_, err = poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)

	require.True(t, issues.lastQuery().Since.IsZero())
}

// A watch with no launch settings leaves kandev's own defaults in charge.
func TestRunWatchOmitsLaunchWhenUnset(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash on save"))
	poller, store := newTestPoller(t, host, issues, nil)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	_, err = poller.RunWatch(context.Background(), saved)
	require.NoError(t, err)

	require.Nil(t, host.created[0].Launch)
	require.Empty(t, host.created[0].Repositories)
	require.False(t, host.created[0].StartAgent, "a watch must not launch an agent it was not told to launch")
}

// Tick is the scheduling layer: it must skip watches that are paused, not yet
// due, or in a workspace where the integration is off.
func TestTickSkipsPausedNotDueAndDisabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("paused watch is not polled", func(t *testing.T) {
		t.Parallel()
		host := newFakeHost("ws-1")
		issues := newFakeIssues()
		issues.set("acme/app", issue(1, "Crash on save"))
		poller, store := newTestPoller(t, host, issues, nil)

		watch := sampleWatch("ws-1", "acme/app")
		watch.Enabled = false
		_, err := store.Put(ctx, watch)
		require.NoError(t, err)

		poller.Tick(ctx)
		require.Empty(t, host.created)
	})

	t.Run("watch inside its interval is not polled", func(t *testing.T) {
		t.Parallel()
		host := newFakeHost("ws-1")
		issues := newFakeIssues()
		issues.set("acme/app", issue(1, "Crash on save"))
		poller, store := newTestPoller(t, host, issues, nil)

		watch := sampleWatch("ws-1", "acme/app")
		watch.PollIntervalSeconds = 600
		// The store's clock is 12:00:00, so a poll at 11:55 is not yet due.
		watch.LastPolledAt = "2026-09-19T11:55:00Z"
		_, err := store.Put(ctx, watch)
		require.NoError(t, err)

		poller.Tick(ctx)
		require.Empty(t, host.created)
	})

	t.Run("disabled integration stops polling", func(t *testing.T) {
		t.Parallel()
		host := newFakeHost("ws-1")
		issues := newFakeIssues()
		issues.set("acme/app", issue(1, "Crash on save"))
		gate := func(context.Context, string) (bool, error) { return false, nil }
		poller, store := newTestPoller(t, host, issues, gate)

		_, err := store.Put(ctx, sampleWatch("ws-1", "acme/app"))
		require.NoError(t, err)

		poller.Tick(ctx)
		require.Empty(t, host.created, "the workspace toggle must stop tasks appearing, not just hide them")
	})

	t.Run("a due watch in an enabled workspace is polled", func(t *testing.T) {
		t.Parallel()
		host := newFakeHost("ws-1")
		issues := newFakeIssues()
		issues.set("acme/app", issue(1, "Crash on save"))
		gate := func(context.Context, string) (bool, error) { return true, nil }
		poller, store := newTestPoller(t, host, issues, gate)

		_, err := store.Put(ctx, sampleWatch("ws-1", "acme/app"))
		require.NoError(t, err)

		poller.Tick(ctx)
		require.Len(t, host.created, 1)
	})
}

// Tick covers every workspace the host reports, not just the first.
func TestTickCoversEveryWorkspace(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1", "ws-2")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash on save"))
	poller, store := newTestPoller(t, host, issues, nil)

	for _, workspaceID := range []string{"ws-1", "ws-2"} {
		_, err := store.Put(context.Background(), sampleWatch(workspaceID, "acme/app"))
		require.NoError(t, err)
	}

	poller.Tick(context.Background())
	require.Len(t, host.created, 2)
}

// A budget that cannot be read is treated as exhausted: creating tasks with an
// unknown inflight count is exactly what the budget exists to prevent.
func TestRunWatchRefusesWhenTheBudgetIsUnreadable(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	issues := newFakeIssues()
	issues.set("acme/app", issue(1, "Crash on save"))
	poller, store := newTestPoller(t, host, issues, nil)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	// Seed one record so the inflight scan actually runs, then break the list.
	require.NoError(t, store.Record(context.Background(), saved, RepoRef{Owner: "acme", Name: "app"}, 9, "u", "task-9"))
	host.listErr = errBoom

	_, err = poller.RunWatch(context.Background(), saved)
	require.Error(t, err)
	require.Empty(t, host.created)
}

func TestTaskTitleAndDescriptionAreBounded(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", maxTitleRunes*2)
	title := taskTitle(Issue{Number: 7, Title: long})
	require.LessOrEqual(t, len([]rune(title)), maxTitleRunes)
	require.True(t, strings.HasPrefix(title, "#7 "))

	body := taskDescription(RepoRef{Owner: "acme", Name: "app"}, Issue{
		Number: 7, URL: "https://git.example/i/7", Body: strings.Repeat("b", maxBodyRunes*2),
	})
	require.Less(t, len([]rune(body)), maxBodyRunes+400)
	require.Contains(t, body, "acme/app#7")

	// An issue with no body still produces a usable card.
	empty := taskDescription(RepoRef{Owner: "acme", Name: "app"}, Issue{Number: 8, URL: "https://git.example/i/8"})
	require.Contains(t, empty, "no description")
}

func TestPollerStartStopIsIdempotent(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	poller, _ := newTestPoller(t, host, newFakeIssues(), nil)

	poller.Start()
	poller.Start() // a second injection must not leave two loops racing
	poller.Stop()
	poller.Stop() // stopping twice is not an error
}
