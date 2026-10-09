package watches

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRepoRef(t *testing.T) {
	t.Parallel()
	ref, err := ParseRepoRef("  acme/app  ")
	require.NoError(t, err)
	require.Equal(t, RepoRef{Owner: "acme", Name: "app"}, ref)
	require.Equal(t, "acme/app", ref.FullName())

	// A malformed entry is rejected rather than guessed at: it would otherwise
	// become a 404 on every poll with no obvious cause.
	for _, raw := range []string{"", "acme", "/app", "acme/", "acme/app/extra", "   "} {
		_, err := ParseRepoRef(raw)
		require.Errorf(t, err, "expected %q to be rejected", raw)
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	t.Parallel()
	watch := Watch{WorkspaceID: "ws-1"}
	watch.Normalize()

	require.Equal(t, "open", watch.State)
	require.Equal(t, DedupScopeWatch, watch.DedupScope, "native parity is the default")
	require.Equal(t, int(DefaultPollInterval/time.Second), watch.PollIntervalSeconds)
	require.Equal(t, DefaultMaxInflightTasks, watch.MaxInflightTasks)
}

func TestNormalizeCanonicalizesInput(t *testing.T) {
	t.Parallel()
	watch := Watch{
		Name:   "  Bugs  ",
		State:  "  CLOSED ",
		Labels: []string{" bug ", "bug", "", "regression"},
		Repos: []RepoRef{
			{Owner: " acme ", Name: " app "},
			{Owner: "ACME", Name: "APP"},
			{Owner: "", Name: "orphan"},
			{Owner: "acme", Name: "other"},
		},
	}
	watch.Normalize()

	require.Equal(t, "Bugs", watch.Name)
	require.Equal(t, "closed", watch.State)
	require.Equal(t, []string{"bug", "regression"}, watch.Labels, "duplicates and blanks are dropped")
	require.Len(t, watch.Repos, 2, "case-insensitive duplicates and incomplete refs are dropped")
	require.Equal(t, "acme/other", watch.Repos[1].FullName())
	require.Equal(t, "acme/app", watch.Repos[0].FullName())
}

func TestNormalizeRejectsAnUnknownState(t *testing.T) {
	t.Parallel()
	watch := Watch{State: "banana"}
	watch.Normalize()
	require.Equal(t, "open", watch.State, "an unrecognised state falls back rather than reaching the API")
}

func TestPollIntervalIsClamped(t *testing.T) {
	t.Parallel()
	require.Equal(t, DefaultPollInterval, Watch{PollIntervalSeconds: 1}.PollInterval(),
		"below the floor falls back to the default rather than hammering the instance")
	require.Equal(t, MaxPollInterval, Watch{PollIntervalSeconds: 60 * 60 * 24 * 30}.PollInterval())
	require.Equal(t, 600*time.Second, Watch{PollIntervalSeconds: 600}.PollInterval())
}

func TestDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	// A watch that has never polled is due immediately.
	require.True(t, Watch{PollIntervalSeconds: 600}.Due(now))
	// One inside its interval is not.
	require.False(t, Watch{PollIntervalSeconds: 600, LastPolledAt: "2026-09-19T11:55:00Z"}.Due(now))
	// One past it is.
	require.True(t, Watch{PollIntervalSeconds: 600, LastPolledAt: "2026-09-19T11:45:00Z"}.Due(now))
	// An unparseable timestamp is treated as never polled rather than never
	// polling again.
	require.True(t, Watch{PollIntervalSeconds: 600, LastPolledAt: "not a time"}.Due(now))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	base := func() Watch {
		watch := Watch{
			WorkspaceID:    "ws-1",
			Name:           "Bugs",
			WorkflowID:     "wf-1",
			WorkflowStepID: "step-1",
			Repos:          []RepoRef{{Owner: "acme", Name: "app"}},
		}
		watch.Normalize()
		return watch
	}
	require.NoError(t, base().Validate())

	// Launch profiles are optional: a watch that only files cards for a human
	// to pick up is a legitimate configuration.
	watch := base()
	watch.AgentProfileID, watch.ExecutorProfileID, watch.Prompt = "", "", ""
	require.NoError(t, watch.Validate())

	// Unless it auto-starts, in which case the operator must name the profile.
	watch = base()
	watch.StartAgent = true
	require.Error(t, watch.Validate())
	watch.AgentProfileID = "agent-1"
	require.NoError(t, watch.Validate())

	// The repository fan-out of one poll tick is bounded.
	watch = base()
	for i := 0; i < maxReposPerWatch+1; i++ {
		watch.Repos = append(watch.Repos, RepoRef{Owner: "acme", Name: string(rune('a' + i))})
	}
	require.Error(t, watch.Validate())
}

func TestNormalizeReviewWatch(t *testing.T) {
	t.Parallel()
	watch := Watch{Kind: KindReview, PollIntervalSeconds: 45, DedupScope: DedupScopeWorkspace, State: "all"}
	watch.Normalize()
	require.Equal(t, ReviewScopeUserAndTeams, watch.ReviewScope, "the search's own scope is the default")
	require.Equal(t, CleanupNever, watch.CleanupPolicy, "cleanup is opt-in")
	require.Equal(t, 60, watch.PollIntervalSeconds, "the review floor is higher than the issue floor")
	require.Equal(t, DedupScopeWatch, watch.DedupScope)
	require.Equal(t, "open", watch.State)

	watch = Watch{Kind: KindReview, ReviewScope: "nonsense", CleanupPolicy: "auto"}
	watch.Normalize()
	require.Equal(t, ReviewScopeUserAndTeams, watch.ReviewScope)
	require.Equal(t, CleanupNever, watch.CleanupPolicy, "an unknown policy never cleans up")
	require.Equal(t, int(DefaultPollInterval.Seconds()), watch.PollIntervalSeconds)

	watch = Watch{Kind: KindReview, ReviewScope: ReviewScopeUser, CleanupPolicy: CleanupWhenClosed, PollIntervalSeconds: 600}
	watch.Normalize()
	require.Equal(t, ReviewScopeUser, watch.ReviewScope)
	require.Equal(t, CleanupWhenClosed, watch.CleanupPolicy)
	require.Equal(t, 600, watch.PollIntervalSeconds)
}

func TestNormalizeIssueWatchDropsReviewFields(t *testing.T) {
	t.Parallel()
	watch := Watch{Kind: "bogus", ReviewScope: ReviewScopeUser, IncludeDrafts: true, CleanupPolicy: CleanupWhenClosed,
		ForkWorkflowStepID: "s", LastCleanupNote: "n", PollIntervalSeconds: 30}
	watch.Normalize()
	require.Equal(t, Kind(""), watch.Kind)
	require.Empty(t, watch.ReviewScope)
	require.False(t, watch.IncludeDrafts)
	require.Empty(t, watch.CleanupPolicy)
	require.Empty(t, watch.ForkWorkflowStepID)
	require.Empty(t, watch.LastCleanupNote)
	require.Equal(t, 30, watch.PollIntervalSeconds, "the issue floor is unchanged")
}

func TestReviewWatchNeedsNoRepositories(t *testing.T) {
	t.Parallel()
	watch := sampleReviewWatch("ws-1")
	watch.Normalize()
	require.NoError(t, watch.Validate(), "no repositories means every repository the token can see")
	issueWatch := sampleWatch("ws-1")
	issueWatch.Normalize()
	require.Error(t, issueWatch.Validate())
}
