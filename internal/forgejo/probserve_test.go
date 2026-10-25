package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/prautomation"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// automationSourceFixture serves pull request 7 of repository o/r (id 5),
// linked to task-1, with a little of everything the observation reads.
func automationSourceFixture(t *testing.T, pull map[string]any) (*AutomationSource, *apiServer) {
	t.Helper()
	api := newAPIServer(t)
	connection, host := newTestConnection(t, api, "token")
	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "ws-1"}
	associations := NewAssociations(func() pluginsdk.Host { return host })
	require.NoError(t, associations.Link(context.Background(), "task-1", identity(api.url(), "5", 7)))
	require.NoError(t, associations.Link(context.Background(), "task-1", identity("https://elsewhere.example", "5", 8)))

	api.handle(http.MethodGet, "/api/v1/repositories/5", 200, map[string]any{
		"id": 5, "name": "r", "full_name": "o/r", "owner": map[string]any{"login": "o"},
		"default_merge_style": "squash", "default_delete_branch_after_merge": true,
	})
	base := map[string]any{
		"number": 7, "title": "Add x", "state": "open", "mergeable": true,
		"html_url":                  "https://forge/o/r/pulls/7",
		"user":                      map[string]any{"login": "alice"},
		"head":                      map[string]any{"ref": "feat", "sha": "abc1234", "repo_id": 5},
		"base":                      map[string]any{"ref": "main", "repo_id": 5},
		"requested_reviewers":       []any{nil, map[string]any{"login": "carol"}},
		"requested_reviewers_teams": []map[string]any{{"name": "core"}},
	}
	for key, value := range pull {
		base[key] = value
	}
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7", 200, base)
	api.handle(http.MethodGet, "/api/v1/user", 200, map[string]any{"id": 3, "login": "kandev"})
	api.handle(http.MethodGet, "/api/v1/user/teams", 200, []map[string]any{
		{"name": "core", "organization": map[string]any{"username": "o"}},
		{"name": "core", "organization": map[string]any{"username": "other-org"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/commits/abc1234/status", 200, map[string]any{
		"state": "failure", "statuses": []map[string]any{
			{"id": 1, "status": "failure", "context": "ci/test", "target_url": "https://ci/1"},
			{"id": 2, "status": "success", "context": "ci/lint"},
			{"id": 3, "status": "cancelled", "context": "ci/slow"},
		},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/branches/main", 200, map[string]any{"name": "main", "required_approvals": 2})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", 200, []map[string]any{
		{"id": 1, "state": "REQUEST_CHANGES", "official": true, "comments_count": 2, "body": "no",
			"user": map[string]any{"id": 4, "login": "bob"}},
		{"id": 2, "state": "PENDING", "user": map[string]any{"id": 5, "login": "dave"}},
		{"id": 3, "state": "APPROVED", "official": false, "user": map[string]any{"id": 6, "login": "eve"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews/1/comments", 200, []map[string]any{
		{"id": 11, "body": "off by one", "path": "a.go", "user": map[string]any{"id": 4, "login": "bob"}},
		{"id": 12, "body": "fine now", "path": "b.go", "user": map[string]any{"id": 4, "login": "bob"},
			"resolver": map[string]any{"id": 3, "login": "kandev"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/issues/7/comments", 200, []map[string]any{
		{"id": 21, "body": "hi", "user": map[string]any{"id": 6, "login": "eve"}},
		{"id": 22, "body": "run passed", "user": map[string]any{"id": -2, "login": "forgejo-actions"}},
		{"id": 23, "body": "from a deleted account", "user": map[string]any{"id": -1, "login": "Ghost"}},
	})
	return NewAutomationSource(connection, associations), api
}

var observedRef = prautomation.PullRef{TaskID: "task-1", RepositoryID: "5", Number: 7}

func TestObserveReadsEverythingAutomationDecidesOn(t *testing.T) {
	source, _ := automationSourceFixture(t, nil)
	obs, err := source.Observe(context.Background(), "ws-1", observedRef)
	require.NoError(t, err)

	require.Equal(t, prautomation.StateOpen, obs.State)
	require.False(t, obs.Fork)
	require.Equal(t, "o/r", obs.Repository)
	require.Equal(t, "kandev", obs.Operator)
	require.True(t, obs.OperatorRequested, "requested through a team of the repository's owner")
	require.Equal(t, prautomation.CIFailure, obs.CI.State)
	require.Equal(t, []prautomation.Check{{Name: "ci/test", State: "failure", URL: "https://ci/1"}}, obs.CI.Failing,
		"a cancelled check asks for no change")
	require.Equal(t, 2, *obs.RequiredApprovals)

	require.Len(t, obs.Reviews, 2, "a pending review is nobody's feedback yet")
	require.Equal(t, prautomation.ReviewChangesRequested, obs.Reviews[0].State)
	require.True(t, obs.Reviews[0].Official)
	require.Equal(t, []prautomation.InlineComment{
		{ID: 11, Author: "bob", Path: "a.go", Body: "off by one"},
		{ID: 12, Author: "bob", Path: "b.go", Body: "fine now", Resolved: true},
	}, obs.Reviews[0].Comments)
	require.False(t, obs.Reviews[1].Official)

	require.Len(t, obs.Comments, 3)
	require.False(t, obs.Comments[0].Bot)
	require.True(t, obs.Comments[1].Bot, "the Actions user")
	require.True(t, obs.Comments[2].Bot, "the instance's own users have negative ids")
}

func TestObserveTeamRequestOfAnotherOwnerDoesNotCount(t *testing.T) {
	source, _ := automationSourceFixture(t, map[string]any{
		"requested_reviewers_teams": []map[string]any{{"name": "docs"}},
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef)
	require.NoError(t, err)
	require.False(t, obs.OperatorRequested)
}

func TestObserveForkAndClosedStates(t *testing.T) {
	source, _ := automationSourceFixture(t, map[string]any{
		"head": map[string]any{"ref": "feat", "sha": "abc1234", "repo_id": 9},
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef)
	require.NoError(t, err)
	require.True(t, obs.Fork)

	gone, _ := automationSourceFixture(t, map[string]any{
		"head": map[string]any{"ref": "feat", "sha": "abc1234"},
	})
	obs, err = gone.Observe(context.Background(), "ws-1", observedRef)
	require.NoError(t, err)
	require.True(t, obs.Fork, "a deleted fork has no head repository")

	merged, mergedAPI := automationSourceFixture(t, map[string]any{"state": "closed", "merged": true})
	obs, err = merged.Observe(context.Background(), "ws-1", observedRef)
	require.NoError(t, err)
	require.Equal(t, prautomation.StateMerged, obs.State)
	require.Zero(t, mergedAPI.count(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews"),
		"a finished pull request only feeds lifecycle prompts")
}

func TestObserveUnsettledConflictIsUnknown(t *testing.T) {
	source, _ := automationSourceFixture(t, map[string]any{
		"mergeable": false, "updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef)
	require.NoError(t, err)
	require.Nil(t, obs.Mergeable)
}

func TestLinkedListsOnlyThisInstance(t *testing.T) {
	source, _ := automationSourceFixture(t, nil)
	refs, err := source.Linked(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, []prautomation.PullRef{observedRef}, refs)
}

func TestMergeLinkedUsesRepositoryDefaultsAndPinsTheHead(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	var body map[string]any
	api.handleFunc(http.MethodPost, "/api/v1/repos/o/r/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, source.MergeLinked(context.Background(), observedRef, "abc1234"))
	require.Equal(t, "squash", body["Do"])
	require.Equal(t, "abc1234", body["head_commit_id"])
	require.Equal(t, true, body["delete_branch_after_merge"])
	require.NotContains(t, body, "merge_when_checks_succeed")
}

func TestAutomationCIStates(t *testing.T) {
	require.Equal(t, prautomation.CINone, automationCI(CombinedStatus{}).State)
	require.Equal(t, prautomation.CIPending, automationCI(CombinedStatus{Statuses: []CommitStatus{
		{Status: "failure", Context: "a"}, {Status: "pending", Context: "b"},
	}}).State, "a failure while something still runs is not settled")
	require.Equal(t, prautomation.CISuccess, automationCI(CombinedStatus{Statuses: []CommitStatus{
		{Status: "success"}, {Status: "warning"}, {Status: "skipped"},
	}}).State)
}

// count reports how many requests reached method and path.
func (a *apiServer) count(method, path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, r := range a.requests {
		if r.Method == method && r.URL.Path == path {
			n++
		}
	}
	return n
}
