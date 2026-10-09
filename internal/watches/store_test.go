package watches

import (
	"context"
	"strings"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestStoreRoundTripsAWatch(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	store := newTestStore(host)

	watch := sampleWatch("ws-1", "acme/app")
	watch.Prompt = "Investigate."
	saved, err := store.Put(context.Background(), watch)
	require.NoError(t, err)
	require.NotEmpty(t, saved.CreatedAt)
	require.NotEmpty(t, saved.UpdatedAt)

	loaded, err := store.Get(context.Background(), "ws-1", watch.ID)
	require.NoError(t, err)
	require.Equal(t, saved, loaded)

	all, err := store.List(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestStoreGetReportsMissing(t *testing.T) {
	t.Parallel()
	store := newTestStore(newFakeHost("ws-1"))
	_, err := store.Get(context.Background(), "ws-1", "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

// A watch is only ever read through the workspace it belongs to. This is the
// property that keeps one workspace's filters, repository ids and prompts out
// of another's responses (#3681).
func TestStoreIsScopedToItsWorkspace(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1", "ws-2")
	store := newTestStore(host)

	_, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)

	other, err := store.List(context.Background(), "ws-2")
	require.NoError(t, err)
	require.Empty(t, other)

	_, err = store.Get(context.Background(), "ws-2", "w1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStoreRejectsAnInvalidWatch(t *testing.T) {
	t.Parallel()
	store := newTestStore(newFakeHost("ws-1"))

	for name, mutate := range map[string]func(*Watch){
		"no name":       func(w *Watch) { w.Name = "" },
		"no workflow":   func(w *Watch) { w.WorkflowID = "" },
		"no step":       func(w *Watch) { w.WorkflowStepID = "" },
		"no repository": func(w *Watch) { w.Repos = nil },
		"auto-start without an agent profile": func(w *Watch) {
			w.StartAgent, w.AgentProfileID = true, ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			watch := sampleWatch("ws-1", "acme/app")
			mutate(&watch)
			_, err := store.Put(context.Background(), watch)
			require.Error(t, err)
		})
	}
}

// Deleting a watch must take its ledger with it: a stale record would silently
// suppress a later watch reusing the same id.
func TestStoreDeleteRemovesTheLedger(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	store := newTestStore(host)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	repo := RepoRef{Owner: "acme", Name: "app"}
	require.NoError(t, store.Record(context.Background(), saved, repo, 1, "u1", "task-1"))
	require.NoError(t, store.Record(context.Background(), saved, repo, 2, "u2", "task-2"))

	records, err := store.Records(context.Background(), saved)
	require.NoError(t, err)
	require.Len(t, records, 2)

	require.NoError(t, store.Delete(context.Background(), "ws-1", saved.ID))

	for _, key := range host.stateKeys("ws-1") {
		require.False(t, strings.HasPrefix(key, watchTaskKeyPrefix), "ledger entry %q outlived its watch", key)
		require.False(t, strings.HasPrefix(key, watchKeyPrefix), "watch entry %q was not removed", key)
	}
}

// Forget clears the ledger but keeps the watch, so the next run re-creates the
// tasks. It backs the panel's reset control.
func TestStoreForgetClearsTheLedgerOnly(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	store := newTestStore(host)

	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	repo := RepoRef{Owner: "acme", Name: "app"}
	require.NoError(t, store.Record(context.Background(), saved, repo, 1, "u1", "task-1"))

	forgotten, err := store.Forget(context.Background(), saved)
	require.NoError(t, err)
	require.Equal(t, 1, forgotten)

	_, seen, err := store.Seen(context.Background(), saved, repo, 1)
	require.NoError(t, err)
	require.False(t, seen)

	_, err = store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err, "the watch itself survives a reset")
}

func TestStoreSeenReportsTheRecordedTask(t *testing.T) {
	t.Parallel()
	store := newTestStore(newFakeHost("ws-1"))
	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	repo := RepoRef{Owner: "acme", Name: "app"}

	_, seen, err := store.Seen(context.Background(), saved, repo, 42)
	require.NoError(t, err)
	require.False(t, seen)

	require.NoError(t, store.Record(context.Background(), saved, repo, 42, "https://git.example/i/42", "task-42"))

	record, seen, err := store.Seen(context.Background(), saved, repo, 42)
	require.NoError(t, err)
	require.True(t, seen)
	require.Equal(t, "task-42", record.TaskID)
	require.Equal(t, "acme/app", record.Repo)
	require.EqualValues(t, 42, record.Number)
}

// State keys are restricted to [a-zA-Z0-9][a-zA-Z0-9._-]{0,127}. A repository
// name alone can exceed that, which is why the key carries a digest.
func TestStateKeysStayWithinTheHostVocabulary(t *testing.T) {
	t.Parallel()
	watch := sampleWatch("ws-1", "acme/app")
	watch.ID = strings.Repeat("a", 32)
	long := RepoRef{Owner: strings.Repeat("o", 100), Name: strings.Repeat("n", 100)}

	for _, key := range []string{
		watchKey(watch.ID),
		dedupKey(watch, long, 999999),
		dedupKey(Watch{ID: watch.ID, DedupScope: DedupScopeWorkspace}, long, 999999),
	} {
		require.LessOrEqual(t, len(key), 128, "key %q is too long", key)
		require.Regexp(t, `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, key)
	}
}

// The two dedup scopes must not collide: an entry written under one must not
// satisfy a lookup under the other.
func TestDedupKeysDifferByScope(t *testing.T) {
	t.Parallel()
	repo := RepoRef{Owner: "acme", Name: "app"}
	perWatch := dedupKey(Watch{ID: "w1", DedupScope: DedupScopeWatch}, repo, 1)
	perWorkspace := dedupKey(Watch{ID: "w1", DedupScope: DedupScopeWorkspace}, repo, 1)
	require.NotEqual(t, perWatch, perWorkspace)

	// Different repositories with the same issue number stay distinct.
	require.NotEqual(t,
		dedupKey(Watch{ID: "w1"}, RepoRef{Owner: "acme", Name: "app"}, 1),
		dedupKey(Watch{ID: "w1"}, RepoRef{Owner: "acme", Name: "other"}, 1))
}

// Watch entries share the workspace state scope with the plugin's existing
// association and connection-cache keys, so the prefixes must not overlap.
func TestWatchKeysDoNotCollideWithExistingState(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"integration_enabled", "connection_status", "task:abc:repo:1"} {
		require.False(t, strings.HasPrefix(existing, watchKeyPrefix))
		require.False(t, strings.HasPrefix(existing, watchTaskKeyPrefix))
		require.False(t, strings.HasPrefix(existing, issueTaskKeyPrefix))
	}
}

func TestStoreMarksPollAndError(t *testing.T) {
	t.Parallel()
	store := newTestStore(newFakeHost("ws-1"))
	saved, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)

	require.NoError(t, store.MarkError(context.Background(), saved, "instance unreachable"))
	stored, err := store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err)
	require.Equal(t, "instance unreachable", stored.LastError)
	require.NotEmpty(t, stored.LastErrorAt)

	require.NoError(t, store.MarkPolled(context.Background(), stored))
	stored, err = store.Get(context.Background(), "ws-1", saved.ID)
	require.NoError(t, err)
	require.Empty(t, stored.LastError, "a successful poll clears the previous error")
	require.Empty(t, stored.LastErrorAt)
	require.NotEmpty(t, stored.LastPolledAt)
}

// A corrupt entry must not take down the whole workspace listing.
func TestStoreSkipsUnreadableEntries(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	store := newTestStore(host)
	_, err := store.Put(context.Background(), sampleWatch("ws-1", "acme/app"))
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-1", watchKeyPrefix+"broken",
		map[string]any{"not": "a watch"}))

	all, err := store.List(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestStoreRequiresAHost(t *testing.T) {
	t.Parallel()
	store := NewStore(func() pluginsdk.Host { return nil })
	_, err := store.List(context.Background(), "ws-1")
	require.ErrorIs(t, err, ErrNoHost)
}

func TestReviewWatchesLiveUnderTheirOwnPrefix(t *testing.T) {
	t.Parallel()
	host := newFakeHost("ws-1")
	store := newTestStore(host)
	review, err := store.Put(context.Background(), sampleReviewWatch("ws-1"))
	require.NoError(t, err)
	issueWatch := sampleWatch("ws-1", "acme/app")
	issueWatch.ID = "w-issue"
	_, err = store.Put(context.Background(), issueWatch)
	require.NoError(t, err)

	// A 0.3.x binary lists "watch." only. It must see the issue watch and
	// nothing of the review watch, whose filters mean nothing to it.
	var legacy []string
	for _, key := range host.stateKeys("ws-1") {
		if strings.HasPrefix(key, watchKeyPrefix) {
			legacy = append(legacy, key)
		}
	}
	require.Equal(t, []string{"watch.w-issue"}, legacy)
	require.Contains(t, host.stateKeys("ws-1"), "rwatch."+review.ID)

	all, err := store.List(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, all, 2)

	got, err := store.Get(context.Background(), "ws-1", review.ID)
	require.NoError(t, err)
	require.Equal(t, KindReview, got.Kind)

	require.NoError(t, store.Delete(context.Background(), "ws-1", review.ID))
	_, err = store.Get(context.Background(), "ws-1", review.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Get(context.Background(), "ws-1", "w-issue")
	require.NoError(t, err)
}

func TestIssueWatchRecordsAreByteIdenticalToZeroPointThree(t *testing.T) {
	t.Parallel()
	encoded, err := encodeWatch(func() Watch {
		w := sampleWatch("ws-1", "acme/app")
		w.Normalize()
		return w
	}())
	require.NoError(t, err)
	for _, added := range []string{"kind", "review_scope", "include_drafts", "cleanup_policy", "fork_workflow_step_id", "last_cleanup_note"} {
		require.NotContains(t, encoded, added, "an issue watch's stored shape must not change")
	}
}

func TestReviewKeyPrefixStaysInTheHostVocabulary(t *testing.T) {
	t.Parallel()
	for _, key := range []string{reviewWatchKey("0123abcd"), reviewWatchKey("a b/c")} {
		require.Regexp(t, `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`, key)
	}
	for _, existing := range []string{"integration_enabled", "connection_status", "task:abc:repo:1", "agent_merge_enabled", "audit:00000000000000000001"} {
		require.False(t, strings.HasPrefix(existing, reviewWatchKeyPrefix))
	}
}
