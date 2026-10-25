package automations

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func parse(t *testing.T, key string, settings map[string]any) Config {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	cfg, err := Parse(key, raw)
	require.NoError(t, err)
	return cfg
}

func TestParseValidates(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		key string
		raw string
	}{
		"unknown condition":     {"issues", `{"repository":"o/r"}`},
		"not an object":         {ConditionPush, `[]`},
		"null":                  {ConditionPush, `null`},
		"missing repository":    {ConditionPush, `{}`},
		"repository no owner":   {ConditionPush, `{"repository":"repo"}`},
		"repository too deep":   {ConditionPush, `{"repository":"o/r/x"}`},
		"repository whitespace": {ConditionPush, `{"repository":"o/r x"}`},
		"unknown field":         {ConditionPush, `{"repository":"o/r","branch":"main"}`},
		"field of other cond":   {ConditionPush, `{"repository":"o/r","on_opened":true}`},
		"wrong type":            {ConditionPullRequest, `{"repository":"o/r","on_opened":"yes"}`},
		"list of non-strings":   {ConditionPush, `{"repository":"o/r","branches":[1]}`},
		"bad glob":              {ConditionPush, `{"repository":"o/r","branches":["release/["]}`},
		"bad base glob":         {ConditionPullRequest, `{"repository":"o/r","base_branches":["["]}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.key, []byte(tc.raw))
			require.True(t, errors.Is(err, ErrInvalidConfig), "%v", err)
		})
	}
}

func TestParseReadsAbsentBooleansAsFalse(t *testing.T) {
	t.Parallel()
	cfg := parse(t, ConditionPullRequest, map[string]any{"repository": " /Owner/Repo/ "})
	require.Equal(t, "Owner/Repo", cfg.Repository)
	require.False(t, cfg.OnOpened, "the editor shows an absent switch as off; the plugin agrees")
	require.False(t, cfg.ExcludeDraft)
	require.False(t, cfg.IncludeForks)

	cfg = parse(t, ConditionPush, map[string]any{"repository": "o/r", "branches": []any{" main ", "", "release/*"}})
	require.Equal(t, []string{"main", "release/*"}, cfg.Branches)
}

func pushEvent(ref, after string) Event {
	return Event{Kind: ConditionPush, Provider: "forgejo", Repository: "Owner/Repo",
		Push: &Push{Ref: ref, Before: "a", After: after, HeadCommit: &Commit{ID: after}}}
}

func TestMatchPush(t *testing.T) {
	t.Parallel()
	all := parse(t, ConditionPush, map[string]any{"repository": "owner/repo"})
	release := parse(t, ConditionPush, map[string]any{"repository": "owner/repo", "branches": []any{"main", "release/*"}})

	data, ok := Match(all, pushEvent("refs/heads/feature/x", "b"))
	require.True(t, ok, "empty branches match every branch; repository compares case-insensitively")
	require.Equal(t, "feature/x", data["branch"])
	require.Equal(t, "forgejo", data["provider"])

	_, ok = Match(all, pushEvent("refs/tags/v1", "b"))
	require.False(t, ok, "tag pushes do not fire")
	_, ok = Match(all, pushEvent("refs/heads/gone", "0000000000000000000000000000000000000000"))
	require.False(t, ok, "branch deletions do not fire")

	_, ok = Match(release, pushEvent("refs/heads/release/1.2", "b"))
	require.True(t, ok)
	_, ok = Match(release, pushEvent("refs/heads/release/1/2", "b"))
	require.False(t, ok, "path.Match: * does not cross a slash")
	_, ok = Match(release, pushEvent("refs/heads/feature", "b"))
	require.False(t, ok)

	other := pushEvent("refs/heads/main", "b")
	other.Repository = "owner/other"
	_, ok = Match(all, other)
	require.False(t, ok, "another repository never matches")

	wrongKind := pushEvent("refs/heads/main", "b")
	wrongKind.Kind = ConditionCIResult
	_, ok = Match(all, wrongKind)
	require.False(t, ok)
}

func prEvent(action string, mutate func(*PullRequest)) Event {
	pr := &PullRequest{Action: action, Number: 1, Detailed: true, Title: "t", Author: "alice",
		BaseBranch: "main", HeadBranch: "x", HeadSHA: "abc", Labels: []string{"Bug"}}
	if mutate != nil {
		mutate(pr)
	}
	return Event{Kind: ConditionPullRequest, Repository: "owner/repo", PullRequest: pr}
}

func TestMatchPullRequest(t *testing.T) {
	t.Parallel()
	every := parse(t, ConditionPullRequest, map[string]any{
		"repository": "owner/repo", "on_opened": true, "on_reopened": true, "on_synchronized": true,
		"on_closed": true, "on_merged": true, "on_review_requested": true, "on_commented": true,
	})
	for _, action := range []string{ActionOpened, ActionReopened, ActionSynchronized, ActionClosed,
		ActionMerged, ActionReviewRequested, ActionCommented} {
		data, ok := Match(every, prEvent(action, nil))
		require.True(t, ok, action)
		require.Equal(t, action, data["action"])
	}
	for _, action := range []string{"label_updated", "edited", "assigned", "comment_edited", ""} {
		_, ok := Match(every, prEvent(action, nil))
		require.False(t, ok, action)
	}

	mergedOnly := parse(t, ConditionPullRequest, map[string]any{"repository": "owner/repo", "on_merged": true})
	_, ok := Match(mergedOnly, prEvent(ActionMerged, nil))
	require.True(t, ok)
	_, ok = Match(mergedOnly, prEvent(ActionClosed, nil))
	require.False(t, ok, "closed without a merge is its own switch")

	_, ok = Match(every, prEvent(ActionOpened, func(pr *PullRequest) { pr.Detailed = false }))
	require.False(t, ok, "a pull request without detail cannot be filtered, so it never matches")

	_, ok = Match(every, prEvent(ActionOpened, func(pr *PullRequest) { pr.Fork = true }))
	require.False(t, ok, "forks are out unless included")
	withForks := parse(t, ConditionPullRequest, map[string]any{"repository": "owner/repo", "on_opened": true, "include_forks": true})
	_, ok = Match(withForks, prEvent(ActionOpened, func(pr *PullRequest) { pr.Fork = true }))
	require.True(t, ok)

	_, ok = Match(every, prEvent(ActionOpened, func(pr *PullRequest) { pr.Draft = true }))
	require.True(t, ok, "drafts are in unless excluded")
	noDrafts := parse(t, ConditionPullRequest, map[string]any{"repository": "owner/repo", "on_opened": true, "exclude_draft": true})
	_, ok = Match(noDrafts, prEvent(ActionOpened, func(pr *PullRequest) { pr.Draft = true }))
	require.False(t, ok)

	filtered := parse(t, ConditionPullRequest, map[string]any{
		"repository": "owner/repo", "on_opened": true,
		"base_branches": []any{"release/*"}, "authors": []any{"ALICE"}, "labels": []any{"bug", "urgent"},
	})
	_, ok = Match(filtered, prEvent(ActionOpened, func(pr *PullRequest) { pr.BaseBranch = "release/2" }))
	require.True(t, ok, "base glob, author and any-of labels, case-insensitive")
	_, ok = Match(filtered, prEvent(ActionOpened, nil))
	require.False(t, ok, "base branch main is not release/*")
	_, ok = Match(filtered, prEvent(ActionOpened, func(pr *PullRequest) { pr.BaseBranch = "release/2"; pr.Author = "bob" }))
	require.False(t, ok)
	_, ok = Match(filtered, prEvent(ActionOpened, func(pr *PullRequest) { pr.BaseBranch = "release/2"; pr.Labels = nil }))
	require.False(t, ok)

	data, ok := Match(every, prEvent(ActionCommented, func(pr *PullRequest) {
		pr.Comment = &Comment{Author: "bob", Body: "hi", HTMLURL: "u"}
		pr.Labels = nil
	}))
	require.True(t, ok)
	require.Equal(t, map[string]any{"author": "bob", "body": "hi", "html_url": "u"}, data["comment"])
	require.Equal(t, []string{}, data["labels"], "labels is always a list")
}

func runEvent(state string, mutate func(*Run)) Event {
	run := &Run{State: state, Workflow: "ci.yml", RunNumber: 3, Branch: "main", Event: "push", Ref: "main"}
	if mutate != nil {
		mutate(run)
	}
	return Event{Kind: ConditionCIResult, Repository: "owner/repo", Run: run}
}

func TestMatchRun(t *testing.T) {
	t.Parallel()
	failures := parse(t, ConditionCIResult, map[string]any{"repository": "owner/repo", "on_failure": true})
	_, ok := Match(failures, runEvent(StateFailure, nil))
	require.True(t, ok)
	_, ok = Match(failures, runEvent(StateSuccess, nil))
	require.False(t, ok)
	_, ok = Match(failures, runEvent(StateCancelled, nil))
	require.False(t, ok, "a cancelled run is not a failure, whatever event carried it")
	_, ok = Match(failures, runEvent("skipped", nil))
	require.False(t, ok)

	every := parse(t, ConditionCIResult, map[string]any{"repository": "owner/repo",
		"on_success": true, "on_failure": true, "on_cancelled": true})
	for _, state := range []string{StateSuccess, StateFailure, StateCancelled} {
		data, ok := Match(every, runEvent(state, nil))
		require.True(t, ok, state)
		require.Equal(t, state, data["state"])
		require.NotContains(t, data, "pull_request")
	}

	_, ok = Match(failures, runEvent(StateFailure, func(run *Run) { run.Fork = true }))
	require.False(t, ok, "fork pull request runs are out unless included")
	withForks := parse(t, ConditionCIResult, map[string]any{"repository": "owner/repo", "on_failure": true, "include_forks": true})
	_, ok = Match(withForks, runEvent(StateFailure, func(run *Run) { run.Fork = true }))
	require.True(t, ok)

	scoped := parse(t, ConditionCIResult, map[string]any{"repository": "owner/repo", "on_failure": true,
		"branches": []any{"feature/*"}, "workflows": []any{"ci.yml"}})
	data, ok := Match(scoped, runEvent(StateFailure, func(run *Run) {
		run.Branch, run.Event, run.PullRequest, run.Ref = "feature/x", "pull_request", 7, "#7"
	}))
	require.True(t, ok, "a pull request run matches on its head branch")
	require.Equal(t, int64(7), data["pull_request"])
	_, ok = Match(scoped, runEvent(StateFailure, nil))
	require.False(t, ok)
	_, ok = Match(scoped, runEvent(StateFailure, func(run *Run) { run.Branch = "" }))
	require.False(t, ok, "a tag or schedule run has no branch and fails a branch filter")
	_, ok = Match(failures, runEvent(StateFailure, func(run *Run) { run.Branch = "" }))
	require.True(t, ok, "but passes when there is none")
	anyBranch := parse(t, ConditionCIResult, map[string]any{"repository": "owner/repo", "on_failure": true, "branches": []any{"*"}})
	_, ok = Match(anyBranch, runEvent(StateFailure, func(run *Run) { run.Branch = "" }))
	require.False(t, ok, `"*" means any branch, not "no branch" (path.Match("*", "") is true)`)
	_, ok = Match(scoped, runEvent(StateFailure, func(run *Run) { run.Branch = "feature/x"; run.Workflow = "lint.yml" }))
	require.False(t, ok)
}

func TestDefaultsParse(t *testing.T) {
	t.Parallel()
	for _, key := range Conditions {
		settings := map[string]any{"repository": "o/r"}
		for name, value := range Defaults[key] {
			settings[name] = value
		}
		cfg := parse(t, key, settings)
		if key == ConditionPullRequest {
			require.True(t, cfg.OnOpened)
			require.True(t, cfg.ExcludeDraft)
		}
		if key == ConditionCIResult {
			require.True(t, cfg.OnFailure)
		}
	}
}
