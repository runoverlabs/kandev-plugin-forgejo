package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/watches"

	"github.com/stretchr/testify/require"
)

func TestListIssuesBuildsTheServerSideFilter(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	var query map[string][]string
	api.handleFunc(http.MethodGet, "/api/v1/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{})
	})
	connection, _ := newTestConnection(t, api, "token-1")
	client, err := connection.Client(context.Background())
	require.NoError(t, err)

	since := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	_, err = client.ListIssues(context.Background(), "acme", "app", IssueListOptions{
		State:  "all",
		Labels: []string{"bug", " regression "},
		Query:  "crash",
		Since:  since,
		Page:   2,
		Limit:  10,
	})
	require.NoError(t, err)

	// Filtering happens on the server: a watch polling on an interval must not
	// pull a whole issue list across the network to discard most of it.
	require.Equal(t, []string{"issues"}, query["type"])
	require.Equal(t, []string{"all"}, query["state"])
	require.Equal(t, []string{"bug,regression"}, query["labels"])
	require.Equal(t, []string{"crash"}, query["q"])
	require.Equal(t, []string{"2026-09-19T10:00:00Z"}, query["since"])
	require.Equal(t, []string{"2"}, query["page"])
	require.Equal(t, []string{"10"}, query["limit"])
}

func TestListIssuesOmitsEmptyFilters(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	var query map[string][]string
	api.handleFunc(http.MethodGet, "/api/v1/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		_ = json.NewEncoder(w).Encode([]Issue{})
	})
	connection, _ := newTestConnection(t, api, "token-1")
	client, err := connection.Client(context.Background())
	require.NoError(t, err)

	_, err = client.ListIssues(context.Background(), "acme", "app", IssueListOptions{Labels: []string{" ", ""}})
	require.NoError(t, err)

	require.NotContains(t, query, "labels", "a blank label must not become a filter on the empty label")
	require.NotContains(t, query, "q")
	require.NotContains(t, query, "since")
	require.Equal(t, []string{"open"}, query["state"], "state defaults to open")
}

// The repository issue endpoint returns issues and pull requests in one shape.
// A watch must never turn a pull request into a task — this plugin already has
// a first-class pull request surface.
func TestListIssuesExcludesPullRequests(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/acme/app/issues", http.StatusOK, []map[string]any{
		{"number": 1, "title": "A real issue"},
		{"number": 2, "title": "A pull request", "pull_request": map[string]any{"merged": false}},
		{"number": 3, "title": "Another issue"},
	})
	connection, _ := newTestConnection(t, api, "token-1")
	client, err := connection.Client(context.Background())
	require.NoError(t, err)

	issues, err := client.ListIssues(context.Background(), "acme", "app", IssueListOptions{})
	require.NoError(t, err)
	require.Len(t, issues, 2)
	require.EqualValues(t, 1, issues[0].Number)
	require.EqualValues(t, 3, issues[1].Number)
}

func TestIssueLabelNames(t *testing.T) {
	t.Parallel()
	issue := Issue{Labels: []Label{{Name: "bug"}, {Name: "  "}, {Name: " regression "}}}
	require.Equal(t, []string{"bug", "regression"}, issue.LabelNames())
}

func TestIssueSourceMapsOntoTheNeutralPort(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.handle(http.MethodGet, "/api/v1/repos/acme/app/issues", http.StatusOK, []map[string]any{
		{
			"number": 7, "title": "Crash", "body": "It crashes.", "state": "open",
			"html_url": "https://git.example/acme/app/issues/7",
			"labels":   []map[string]any{{"name": "bug"}},
			"user":     map[string]any{"login": "reporter"},
		},
	})
	connection, _ := newTestConnection(t, api, "token-1")
	source := NewIssueSource(connection)

	issues, err := source.ListIssues(context.Background(),
		watches.RepoRef{Owner: "acme", Name: "app"}, watches.IssueQuery{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	require.Equal(t, watches.Issue{
		Number: 7,
		Title:  "Crash",
		Body:   "It crashes.",
		State:  "open",
		URL:    "https://git.example/acme/app/issues/7",
		Labels: []string{"bug"},
		Author: "reporter",
	}, issues[0])
}

// Provider error bodies are never forwarded: on some deployments they echo the
// presented token.
func TestIssueSourceRedactsProviderErrors(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		status int
		want   string
	}{
		{"not found", http.StatusNotFound, "not visible to this token"},
		{"unauthorized", http.StatusUnauthorized, "rejected the access token"},
		{"server error", http.StatusInternalServerError, "could not read issues"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			api := newAPIServer(t)
			api.handle(http.MethodGet, "/api/v1/repos/acme/app/issues", testCase.status,
				map[string]any{"message": "token abcd1234 is invalid"})
			connection, _ := newTestConnection(t, api, "token-1")
			source := NewIssueSource(connection)

			_, err := source.ListIssues(context.Background(),
				watches.RepoRef{Owner: "acme", Name: "app"}, watches.IssueQuery{})
			require.Error(t, err)
			require.Contains(t, err.Error(), testCase.want)
			require.Contains(t, err.Error(), "acme/app")
			require.NotContains(t, err.Error(), "abcd1234", "a provider body must never reach the operator")
		})
	}
}

// A full page means there may be more; a short page ends the walk.
func TestIssueSourceFollowsPagination(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	pages := 0
	api.handleFunc(http.MethodGet, "/api/v1/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			full := make([]map[string]any, maxIssuePageLimit)
			for i := range full {
				full[i] = map[string]any{"number": i + 1, "title": "Issue"}
			}
			_ = json.NewEncoder(w).Encode(full)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 999, "title": "Last"}})
	})
	connection, _ := newTestConnection(t, api, "token-1")
	source := NewIssueSource(connection)

	issues, err := source.ListIssues(context.Background(),
		watches.RepoRef{Owner: "acme", Name: "app"}, watches.IssueQuery{})
	require.NoError(t, err)
	require.Equal(t, 2, pages)
	require.Len(t, issues, maxIssuePageLimit+1)
}
