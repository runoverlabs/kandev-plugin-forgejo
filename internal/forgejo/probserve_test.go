package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/scm"

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

var (
	observedRef = prautomation.PullRef{TaskID: "task-1", RepositoryID: "5", Number: 7}
	allReads    = prautomation.Reads{Feedback: true, ReviewRequest: true}
)

func TestObserveReadsEverythingAutomationDecidesOn(t *testing.T) {
	source, _ := automationSourceFixture(t, nil)
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
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
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.False(t, obs.OperatorRequested)
}

func TestObserveForkAndClosedStates(t *testing.T) {
	source, _ := automationSourceFixture(t, map[string]any{
		"head": map[string]any{"ref": "feat", "sha": "abc1234", "repo_id": 9},
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.True(t, obs.Fork)

	gone, _ := automationSourceFixture(t, map[string]any{
		"head": map[string]any{"ref": "feat", "sha": "abc1234"},
	})
	obs, err = gone.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.True(t, obs.Fork, "a deleted fork has no head repository")

	merged, mergedAPI := automationSourceFixture(t, map[string]any{"state": "closed", "merged": true})
	obs, err = merged.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Equal(t, prautomation.StateMerged, obs.State)
	require.Zero(t, mergedAPI.count(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews"),
		"a finished pull request only feeds lifecycle prompts")
}

func TestObserveUnsettledConflictIsUnknown(t *testing.T) {
	source, _ := automationSourceFixture(t, map[string]any{
		"mergeable": false, "updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Nil(t, obs.Mergeable)
}

func TestObserveFailsWhenAMergeGateCannotBeRead(t *testing.T) {
	for _, path := range []string{
		"/api/v1/repos/o/r/commits/abc1234/status",
		"/api/v1/repos/o/r/branches/main",
		"/api/v1/repos/o/r/pulls/7/reviews",
		"/api/v1/repos/o/r/pulls/7/reviews/1/comments",
		"/api/v1/repos/o/r/issues/7/comments",
	} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			source, api := automationSourceFixture(t, nil)
			api.handle(http.MethodGet, path, status, map[string]any{"message": "no"})
			_, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
			require.ErrorIs(t, err, scm.ErrUnauthorized, "%s answering %d must not read as no CI or no objections", path, status)
		}
	}
	source, api := automationSourceFixture(t, nil)
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", http.StatusNotFound, nil)
	_, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.ErrorIs(t, err, scm.ErrNotFound)
}

func TestObserveSkipsTheCommentsOfAReviewDeletedMeanwhile(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews/1/comments", http.StatusNotFound, nil)
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Empty(t, obs.Reviews[0].Comments)
}

func TestObserveFetchesTheCommentsOfTheNewestReviews(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	const total = maxReviewCommentFetch + 5
	var reviews []map[string]any
	for id := 1; id <= total; id++ {
		reviews = append(reviews, map[string]any{
			"id": id, "state": "COMMENT", "comments_count": 1, "user": map[string]any{"id": 4, "login": "bob"},
		})
		api.handle(http.MethodGet, fmt.Sprintf("/api/v1/repos/o/r/pulls/7/reviews/%d/comments", id), 200, []map[string]any{
			{"id": 100 + id, "body": "look", "path": "a.go", "user": map[string]any{"id": 4, "login": "bob"}},
		})
	}
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", 200, reviews)
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Len(t, obs.Reviews, total)
	for i, review := range obs.Reviews {
		require.Equal(t, int64(i+1), review.ID, "reviews keep their order")
		if i < total-maxReviewCommentFetch {
			require.Empty(t, review.Comments, "review %d is among the oldest", review.ID)
		} else {
			require.Len(t, review.Comments, 1, "review %d is among the newest", review.ID)
		}
	}
	require.True(t, obs.ReviewsIncomplete, "comments past the bound were not read")
}

func TestObserveReportsReviewsPastTheBound(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	api.handleFunc(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", strconv.Itoa(maxPullReviews+1))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var reviews []map[string]any
		for i := 1; i <= pullListPageSize; i++ {
			id := (page-1)*pullListPageSize + i
			if id > maxPullReviews+1 {
				break
			}
			reviews = append(reviews, map[string]any{"id": id, "state": "APPROVED", "user": map[string]any{"id": 4, "login": "bob"}})
		}
		_ = json.NewEncoder(w).Encode(reviews)
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Len(t, obs.Reviews, maxPullReviews)
	require.True(t, obs.ReviewsIncomplete, "the oldest review was not read")

	complete, _ := automationSourceFixture(t, nil)
	obs, err = complete.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.False(t, obs.ReviewsIncomplete)
}

// A conversation is resolved from its first comment, which is the only one
// that carries the resolver (Forgejo 16, Gitea 1.27). Replies in it are resolved
// with it, wherever they are filed.
func TestObserveResolvesAConversationFromItsFirstComment(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", 200, []map[string]any{
		{"id": 1, "state": "COMMENT", "official": true, "comments_count": 4, "user": map[string]any{"id": 4, "login": "bob"}},
		{"id": 2, "state": "COMMENT", "comments_count": 1, "user": map[string]any{"id": 6, "login": "eve"}},
	})
	resolved := map[string]any{"id": 3, "login": "kandev"}
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews/1/comments", 200, []map[string]any{
		{"id": 11, "body": "off by one", "path": "a.go", "position": 4, "user": map[string]any{"id": 4, "login": "bob"}, "resolver": resolved},
		{"id": 13, "body": "fixed", "path": "a.go", "position": 4, "user": map[string]any{"id": 3, "login": "kandev"}},
		{"id": 14, "body": "this one", "path": "a.go", "position": 9, "user": map[string]any{"id": 4, "login": "bob"}},
		{"id": 16, "body": "done", "path": "a.go", "position": 9, "user": map[string]any{"id": 3, "login": "kandev"}, "resolver": resolved},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews/2/comments", 200, []map[string]any{
		// Gitea files a reply under the replier's own pending review.
		{"id": 15, "body": "agreed", "path": "a.go", "position": 4, "user": map[string]any{"id": 6, "login": "eve"}},
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	resolution := map[int64]bool{}
	for _, review := range obs.Reviews {
		for _, comment := range review.Comments {
			resolution[comment.ID] = comment.Resolved
		}
	}
	require.Equal(t, map[int64]bool{11: true, 13: true, 15: true, 14: false, 16: false}, resolution,
		"resolved from a reply, the web UI still shows the conversation open")
}

// Gitea 1.27 files a reply under the replier's pending review, so a
// conversation can start in a review dismissed later and go on in a live one.
// The dismissed review still decides whether the conversation is resolved.
func TestObserveResolvesAConversationStartedInADismissedReview(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews", 200, []map[string]any{
		{"id": 1, "state": "REQUEST_CHANGES", "dismissed": true, "comments_count": 1, "user": map[string]any{"id": 4, "login": "bob"}},
		{"id": 2, "state": "COMMENT", "comments_count": 1, "user": map[string]any{"id": 6, "login": "eve"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews/1/comments", 200, []map[string]any{
		{"id": 11, "body": "off by one", "path": "a.go", "position": 4, "user": map[string]any{"id": 4, "login": "bob"},
			"resolver": map[string]any{"id": 3, "login": "kandev"}},
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews/2/comments", 200, []map[string]any{
		{"id": 15, "body": "agreed", "path": "a.go", "position": 4, "user": map[string]any{"id": 6, "login": "eve"}},
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Len(t, obs.Reviews, 2)
	require.True(t, obs.Reviews[0].Dismissed)
	require.Len(t, obs.Reviews[1].Comments, 1)
	require.True(t, obs.Reviews[1].Comments[0].Resolved, "the reply is in a conversation resolved from its first comment")
}

func TestObserveReadsOnlyWhatIsAskedFor(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, prautomation.Reads{})
	require.NoError(t, err)
	require.Equal(t, prautomation.StateOpen, obs.State)
	for _, path := range []string{
		"/api/v1/user",
		"/api/v1/repos/o/r/commits/abc1234/status",
		"/api/v1/repos/o/r/branches/main",
		"/api/v1/repos/o/r/pulls/7/reviews",
		"/api/v1/repos/o/r/issues/7/comments",
	} {
		require.Zero(t, api.count(http.MethodGet, path), "%s is read by nothing switched on", path)
	}

	obs, err = source.Observe(context.Background(), "ws-1", observedRef, prautomation.Reads{ReviewRequest: true})
	require.NoError(t, err)
	require.True(t, obs.OperatorRequested)
	require.Zero(t, api.count(http.MethodGet, "/api/v1/repos/o/r/commits/abc1234/status"))
	require.Zero(t, api.count(http.MethodGet, "/api/v1/repos/o/r/pulls/7/reviews"))
}

// The combined status pages, and its total_count and state describe the page
// alone (Forgejo 16, Gitea 1.27): a failing check on a later page must count.
func TestObserveReadsEveryPageOfTheCombinedStatus(t *testing.T) {
	contexts := 70
	serve := func(api *apiServer, total int) {
		api.handleFunc(http.MethodGet, "/api/v1/repos/o/r/commits/abc1234/status", func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			limit = min(limit, statusPageSize)
			var statuses []map[string]any
			for i := (page - 1) * limit; i < min(page*limit, contexts); i++ {
				state := "success"
				if i == contexts-1 {
					state = "failure"
				}
				statuses = append(statuses, map[string]any{"id": i + 1, "status": state, "context": fmt.Sprintf("ci/%02d", i)})
			}
			if total >= 0 {
				w.Header().Set("X-Total-Count", strconv.Itoa(total))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": len(statuses), "statuses": statuses})
		})
	}
	source, api := automationSourceFixture(t, nil)
	serve(api, contexts)
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Equal(t, prautomation.CIFailure, obs.CI.State)
	require.Equal(t, "ci/69", obs.CI.Failing[0].Name)
	require.Equal(t, 2, api.count(http.MethodGet, "/api/v1/repos/o/r/commits/abc1234/status"))

	// Fewer contexts than counted (one moved between pages): fail closed.
	short, shortAPI := automationSourceFixture(t, nil)
	serve(shortAPI, contexts+1)
	_, err = short.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.ErrorIs(t, err, errStatusesCut)

	// Uncounted (a proxy dropped X-Total-Count): a short last page ends the
	// list, and reading up to the bound does not.
	uncounted, uncountedAPI := automationSourceFixture(t, nil)
	serve(uncountedAPI, -1)
	obs, err = uncounted.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Equal(t, prautomation.CIFailure, obs.CI.State)
	contexts = statusPageSize*maxStatusPages + 1
	capped, cappedAPI := automationSourceFixture(t, nil)
	serve(cappedAPI, -1)
	_, err = capped.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.ErrorIs(t, err, errStatusesCut, "the failing check past the bound was never read")
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

func TestObserveReportsRequiredChecksThatHaveNotReported(t *testing.T) {
	source, api := automationSourceFixture(t, nil)
	api.handle(http.MethodGet, "/api/v1/repos/o/r/branches/main", 200, map[string]any{
		"name": "main", "enable_status_check": true,
		"status_check_contexts": []string{"ci/lint", "ci/te?t", "deploy / *", "ci/deploy"},
	})
	obs, err := source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Equal(t, []string{"deploy / *", "ci/deploy"}, obs.CI.Missing)

	api.handle(http.MethodGet, "/api/v1/repos/o/r/branches/main", 200, map[string]any{
		"name": "main", "status_check_contexts": []string{"ci/deploy"},
	})
	obs, err = source.Observe(context.Background(), "ws-1", observedRef, allReads)
	require.NoError(t, err)
	require.Empty(t, obs.CI.Missing, "contexts are required only while status checks are on")
}

func TestContextGlobMatchesAsTheInstanceDoes(t *testing.T) {
	for _, tc := range []struct {
		pattern, context string
		match            bool
	}{
		{"ci/test", "ci/test", true},
		{"ci/test", "ci/test2", false},
		{"*", "CI / build (push)", true},
		{"CI / *", "CI / build (push)", true},
		{"ci/te?t", "ci/test", true},
		{"ci/[a-c]x", "ci/bx", true},
		{"ci/[!a-c]x", "ci/bx", false},
		{"{lint,test}", "test", true},
		{"{lint,test}", "build", false},
		{"a.b", "axb", false},
		{"ci/(x)+", "ci/(x)+", true},
	} {
		matcher := contextGlob(tc.pattern)
		require.NotNil(t, matcher, tc.pattern)
		require.Equal(t, tc.match, matcher.MatchString(tc.context), "%s ~ %s", tc.pattern, tc.context)
	}
	for _, broken := range []string{"{a,b", "a}", "[ab"} {
		require.Nil(t, contextGlob(broken), broken)
	}
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
