package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/watches"

	"github.com/stretchr/testify/require"
)

func searchHit(number int, repo string) map[string]any {
	return map[string]any{
		"number": number, "title": fmt.Sprintf("PR %d", number), "state": "open",
		"html_url":     fmt.Sprintf("https://git.example/%s/pulls/%d", repo, number),
		"updated_at":   "2026-09-19T10:00:00Z",
		"user":         map[string]any{"login": "alice"},
		"repository":   map[string]any{"full_name": repo},
		"pull_request": map[string]any{"merged": false},
	}
}

func newReviewSource(t *testing.T, api *apiServer) *ReviewSource {
	t.Helper()
	connection, _ := newTestConnection(t, api, "token-1")
	return NewReviewSource(connection)
}

func TestSearchReviewRequestsAsksTheInstanceForMyRequests(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	var query map[string][]string
	api.handleFunc(http.MethodGet, "/api/v1/repos/issues/search", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		hit := searchHit(4, "acme/web")
		hit["labels"] = []any{map[string]any{"name": "Needs-Review"}, map[string]any{"name": "ui"}}
		_ = json.NewEncoder(w).Encode([]any{hit})
	})
	found, err := newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{
		Labels: []string{"needs-review", "ui"}, Query: "export",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"pulls"}, query["type"])
	require.Equal(t, []string{"open"}, query["state"])
	require.Equal(t, []string{"true"}, query["review_requested"])
	require.Equal(t, []string{"needs-review,ui"}, query["labels"])
	require.Equal(t, []string{"export"}, query["q"])
	require.Equal(t, []string{"1"}, query["page"])
	require.Len(t, found, 1)
	require.Equal(t, watches.PullRequest{
		Repo: watches.RepoRef{Owner: "acme", Name: "web"}, Number: 4, Title: "PR 4",
		URL: "https://git.example/acme/web/pulls/4", Author: "alice", UpdatedAt: "2026-09-19T10:00:00Z",
	}, found[0])
}

func TestSearchReviewRequestsFiltersByRepositoryCaseInsensitively(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/issues/search", http.StatusOK, []any{
		searchHit(1, "Acme/Web"), searchHit(2, "acme/other"), searchHit(3, "elsewhere/web"),
	})
	found, err := newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{
		Repos: []watches.RepoRef{{Owner: "acme", Name: "web"}},
	})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.EqualValues(t, 1, found[0].Number)
}

func TestSearchReviewRequestsWithNoRepositoryFilterReturnsEverything(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/issues/search", http.StatusOK, []any{
		searchHit(1, "acme/web"), searchHit(2, "acme/other"),
	})
	found, err := newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{})
	require.NoError(t, err)
	require.Len(t, found, 2)
}

// The walk ends on a short raw page. A page that is full of pull requests the
// repository filter then discards must not end it early.
func TestSearchReviewRequestsContinuesOnTheRawPageLength(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	var pages []string
	api.handleFunc(http.MethodGet, "/api/v1/repos/issues/search", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		var hits []any
		switch page {
		case "1", "2":
			for i := 0; i < maxIssuePageLimit; i++ {
				hits = append(hits, searchHit(i+1, "elsewhere/noise"))
			}
		case "3":
			hits = append(hits, searchHit(900, "acme/web"))
		}
		_ = json.NewEncoder(w).Encode(hits)
	})
	found, err := newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{
		Repos: []watches.RepoRef{{Owner: "acme", Name: "web"}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"1", "2", "3"}, pages)
	require.Len(t, found, 1)
	require.EqualValues(t, 900, found[0].Number)
}

func TestSearchReviewRequestsIsBoundedByThePageCap(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	calls := 0
	api.handleFunc(http.MethodGet, "/api/v1/repos/issues/search", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		var hits []any
		for i := 0; i < maxIssuePageLimit; i++ {
			hits = append(hits, searchHit(i+1, "acme/web"))
		}
		_ = json.NewEncoder(w).Encode(hits)
	})
	_, err := newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{})
	require.NoError(t, err)
	require.Equal(t, maxReviewPages, calls)
}

func TestReviewErrorsNeverCarryTheResponseBody(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status  int
		backoff bool
		message string
	}{
		"unauthorized": {http.StatusUnauthorized, true, "rejected the access token"},
		"forbidden":    {http.StatusForbidden, true, "rejected the access token"},
		"rate limited": {http.StatusTooManyRequests, true, "rate limited"},
		"not found":    {http.StatusNotFound, false, "not visible"},
		"server error": {http.StatusInternalServerError, false, "could not read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newAPIServer(t)
			api.handleFunc(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"token-1 is invalid"}`))
			})
			_, err := newReviewSource(t, api).PullState(context.Background(), watches.RepoRef{Owner: "acme", Name: "web"}, 7)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "token-1")
			require.Contains(t, err.Error(), tc.message)
			require.Equal(t, tc.backoff, errors.Is(err, watches.ErrBackoff))
		})
	}
}

func pullBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"number": 7, "title": "Add export", "state": "open", "merged": false,
		"html_url":            "https://git.example/acme/web/pulls/7",
		"user":                map[string]any{"login": "alice"},
		"head":                map[string]any{"ref": "feature/x", "repo": map[string]any{"full_name": "acme/web"}},
		"base":                map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/web"}},
		"requested_reviewers": []any{map[string]any{"login": "Bob"}},
	}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

func TestPullDetailMapsBranchesAndAuthor(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(nil))
	detail, err := newReviewSource(t, api).PullDetail(context.Background(), watches.RepoRef{Owner: "acme", Name: "web"}, 7, watches.ReviewScopeUserAndTeams)
	require.NoError(t, err)
	require.Equal(t, watches.PullDetail{
		Title: "Add export", URL: "https://git.example/acme/web/pulls/7", Author: "alice",
		HeadBranch: "feature/x", BaseBranch: "main", Open: true,
	}, detail)
}

func TestPullDetailFlagsDraftsByFieldAndByTitle(t *testing.T) {
	t.Parallel()
	for name, overrides := range map[string]map[string]any{
		"draft field":   {"draft": true},
		"WIP colon":     {"title": "WIP: export"},
		"lowercase wip": {"title": "wip: export"},
		"bracketed wip": {"title": "[WIP] export"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newAPIServer(t)
			api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(overrides))
			detail, err := newReviewSource(t, api).PullDetail(context.Background(), watches.RepoRef{Owner: "acme", Name: "web"}, 7, watches.ReviewScopeUserAndTeams)
			require.NoError(t, err)
			require.True(t, detail.Draft)
		})
	}
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(map[string]any{"title": "Swipe the WIP out"}))
	detail, err := newReviewSource(t, api).PullDetail(context.Background(), watches.RepoRef{Owner: "acme", Name: "web"}, 7, watches.ReviewScopeUserAndTeams)
	require.NoError(t, err)
	require.False(t, detail.Draft, "only a prefix marks a draft")
}

func TestPullDetailForkDetectionFailsClosed(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		head any
		fork bool
	}{
		"same repo":            {map[string]any{"ref": "x", "repo": map[string]any{"full_name": "acme/web"}}, false},
		"same repo other case": {map[string]any{"ref": "x", "repo": map[string]any{"full_name": "ACME/Web"}}, false},
		"other repo":           {map[string]any{"ref": "x", "repo": map[string]any{"full_name": "mallory/web"}}, true},
		"deleted fork":         {map[string]any{"ref": "x", "repo": nil}, true},
		"no repo key":          {map[string]any{"ref": "x"}, true},
		"empty name":           {map[string]any{"ref": "x", "repo": map[string]any{"full_name": ""}}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newAPIServer(t)
			api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(map[string]any{"head": tc.head}))
			detail, err := newReviewSource(t, api).PullDetail(context.Background(), watches.RepoRef{Owner: "acme", Name: "web"}, 7, watches.ReviewScopeUserAndTeams)
			require.NoError(t, err)
			require.Equal(t, tc.fork, detail.Fork)
		})
	}
}

func TestPullDetailChecksTheDirectRequestOnlyForUserScope(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(nil))
	userCalls := 0
	api.handleFunc(http.MethodGet, "/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		userCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "bob"})
	})
	source := newReviewSource(t, api)
	repo := watches.RepoRef{Owner: "acme", Name: "web"}

	detail, err := source.PullDetail(context.Background(), repo, 7, watches.ReviewScopeUserAndTeams)
	require.NoError(t, err)
	require.False(t, detail.RequestedFromMe)
	require.Zero(t, userCalls, "the teams scope needs no extra request")

	detail, err = source.PullDetail(context.Background(), repo, 7, watches.ReviewScopeUser)
	require.NoError(t, err)
	require.True(t, detail.RequestedFromMe, "the login is compared case-insensitively")
	require.Equal(t, 1, userCalls)

	api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(map[string]any{"requested_reviewers": []any{}}))
	detail, err = source.PullDetail(context.Background(), repo, 7, watches.ReviewScopeUser)
	require.NoError(t, err)
	require.False(t, detail.RequestedFromMe, "a team-only request is not a direct one")
}

func TestPullStateReadsMergedClosedAndOpen(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      watches.PullState
	}{
		"open":   {nil, watches.PullOpen},
		"merged": {map[string]any{"state": "closed", "merged": true}, watches.PullMerged},
		"closed": {map[string]any{"state": "closed"}, watches.PullClosed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newAPIServer(t)
			api.handle(http.MethodGet, "/api/v1/repos/acme/web/pulls/7", http.StatusOK, pullBody(tc.overrides))
			state, err := newReviewSource(t, api).PullState(context.Background(), watches.RepoRef{Owner: "acme", Name: "web"}, 7)
			require.NoError(t, err)
			require.Equal(t, tc.want, state)
		})
	}
}

// The instance ignores a label name it does not know rather than matching
// nothing, so a typo would otherwise match every pull request.
func TestSearchReviewRequestsChecksLabelsItself(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	labelled := searchHit(1, "acme/web")
	labelled["labels"] = []any{map[string]any{"name": "ui"}}
	api.handle(http.MethodGet, "/api/v1/repos/issues/search", http.StatusOK, []any{labelled, searchHit(2, "acme/web")})
	found, err := newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{Labels: []string{"ui"}})
	require.NoError(t, err)
	require.Len(t, found, 1)
	found, err = newReviewSource(t, api).SearchReviewRequests(context.Background(), watches.ReviewQuery{Labels: []string{"nolabel-xyz"}})
	require.NoError(t, err)
	require.Empty(t, found)
}
