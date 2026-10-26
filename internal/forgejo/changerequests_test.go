package forgejo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestParsePullRequestReference(t *testing.T) {
	t.Parallel()
	const scope = "https://forge.example.com"
	for _, testCase := range []struct {
		name       string
		input      string
		wantOwner  string
		wantRepo   string
		wantNumber int64
		wantOK     bool
	}{
		{name: "canonical owner/repo#n", input: "acme/widgets#42", wantOwner: "acme", wantRepo: "widgets", wantNumber: 42, wantOK: true},
		{name: "web url", input: "https://forge.example.com/acme/widgets/pulls/42", wantOwner: "acme", wantRepo: "widgets", wantNumber: 42, wantOK: true},
		{name: "url with trailing segment", input: "https://forge.example.com/acme/widgets/pulls/42/files", wantOwner: "acme", wantRepo: "widgets", wantNumber: 42, wantOK: true},
		{name: "bare number is ambiguous", input: "#42", wantOK: false},
		{name: "issue url is not a pull request", input: "https://forge.example.com/acme/widgets/issues/42", wantOK: false},
		{name: "other host rejected", input: "https://github.com/acme/widgets/pulls/42", wantOK: false},
		{name: "zero number rejected", input: "acme/widgets#0", wantOK: false},
		{name: "empty rejected", input: "   ", wantOK: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, number, ok := ParsePullRequestReference(scope, testCase.input)
			require.Equal(t, testCase.wantOK, ok)
			if testCase.wantOK {
				require.Equal(t, testCase.wantOwner, owner)
				require.Equal(t, testCase.wantRepo, repo)
				require.Equal(t, testCase.wantNumber, number)
			}
		})
	}
}

func TestResolveReferenceVerifiesAgainstInstance(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusOK, map[string]any{
		"number": 42, "title": "Add widgets", "html_url": "https://forge.example.com/acme/widgets/pulls/42",
	})
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets", http.StatusOK, repoPayload(7, "acme", "widgets"))
	connection, _ := newTestConnection(t, api, "token")
	repositories := NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo")
	changeRequests := NewChangeRequests(connection, repositories)

	resolved, err := changeRequests.ResolveReference(context.Background(), "workspace-1", "acme/widgets#42")
	require.NoError(t, err)
	require.Equal(t, int64(42), resolved.Identity.Number)
	require.Equal(t, "7", resolved.Identity.RepositoryID, "identity uses the immutable repository id, not the path")
	require.Equal(t, api.URL(), resolved.Identity.ConnectionScope)
}

func TestResolveReferenceRejectsUnknownPullRequest(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodGet, "/api/v1/repos/acme/widgets/pulls/42", http.StatusNotFound, nil)
	connection, _ := newTestConnection(t, api, "token")
	changeRequests := NewChangeRequests(connection, NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo"))

	_, err := changeRequests.ResolveReference(context.Background(), "workspace-1", "acme/widgets#42")
	require.ErrorContains(t, err, "was not found")

	_, err = changeRequests.ResolveReference(context.Background(), "workspace-1", "nonsense")
	require.ErrorContains(t, err, "must be a pull request URL")
}

func TestCreateUsesVerifiedHeadAndDefaultsDestination(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	var captured map[string]any
	api.HandleFunc(http.MethodPost, "/api/v1/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 11, "title": captured["title"], "html_url": "https://forge.example.com/acme/widgets/pulls/11",
		})
	})
	connection, _ := newTestConnection(t, api, "token")
	changeRequests := NewChangeRequests(connection, NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo"))

	repository := sourcecontrol.Repository{
		OwnerOrProject: "acme", Name: "widgets", RepositoryID: "7", DefaultBranch: "main",
	}
	created, err := changeRequests.Create(context.Background(), repository, "kandev/feature", sourcecontrol.CreateChangeRequestInput{
		Title: "Add widgets", Description: "body",
	})
	require.NoError(t, err)
	require.Equal(t, "kandev/feature", captured["head"], "head must come from the verified context, never the browser body")
	require.Equal(t, "main", captured["base"], "an empty destination falls back to the repository default branch")
	require.Equal(t, int64(11), created.Identity.Number)
	require.Equal(t, "7", created.Identity.RepositoryID)
}

// REST v1 has no draft flag on create; the portable marker is a WIP title.
func TestCreateMarksDraftWithWorkInProgressPrefix(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	var captured map[string]any
	api.HandleFunc(http.MethodPost, "/api/v1/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 12, "html_url": "https://x/pulls/12"})
	})
	connection, _ := newTestConnection(t, api, "token")
	changeRequests := NewChangeRequests(connection, NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo"))
	repository := sourcecontrol.Repository{OwnerOrProject: "acme", Name: "widgets", RepositoryID: "7", DefaultBranch: "main"}

	_, err := changeRequests.Create(context.Background(), repository, "head", sourcecontrol.CreateChangeRequestInput{
		Title: "Add widgets", Draft: true,
	})
	require.NoError(t, err)
	require.Equal(t, "WIP: Add widgets", captured["title"])

	_, err = changeRequests.Create(context.Background(), repository, "head", sourcecontrol.CreateChangeRequestInput{
		Title: "WIP: already marked", Draft: true,
	})
	require.NoError(t, err)
	require.Equal(t, "WIP: already marked", captured["title"], "an existing marker must not be doubled")
}

func TestCreateValidatesRequiredInput(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	connection, _ := newTestConnection(t, api, "token")
	changeRequests := NewChangeRequests(connection, NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo"))

	_, err := changeRequests.Create(context.Background(), sourcecontrol.Repository{OwnerOrProject: "a", Name: "b", DefaultBranch: "main"}, "  ",
		sourcecontrol.CreateChangeRequestInput{Title: "t"})
	require.ErrorContains(t, err, "verified head branch is required")

	_, err = changeRequests.Create(context.Background(), sourcecontrol.Repository{OwnerOrProject: "a", Name: "b"}, "head",
		sourcecontrol.CreateChangeRequestInput{Title: "t"})
	require.ErrorContains(t, err, "no destination branch")

	_, err = changeRequests.Create(context.Background(), sourcecontrol.Repository{OwnerOrProject: "a", Name: "b", DefaultBranch: "main"}, "head",
		sourcecontrol.CreateChangeRequestInput{Title: "  "})
	require.ErrorContains(t, err, "title is required")
}

// A created remote PR must never be reported as a failure just because the
// instance omitted html_url — that would invite a duplicate retry.
func TestCreateFallsBackToCanonicalURL(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.Handle(http.MethodPost, "/api/v1/repos/acme/widgets/pulls", http.StatusCreated, map[string]any{"number": 13})
	connection, _ := newTestConnection(t, api, "token")
	changeRequests := NewChangeRequests(connection, NewRepositories(connection, func() pluginsdk.Host { return nil }, "forgejo"))

	created, err := changeRequests.Create(context.Background(),
		sourcecontrol.Repository{OwnerOrProject: "acme", Name: "widgets", RepositoryID: "7", DefaultBranch: "main"},
		"head", sourcecontrol.CreateChangeRequestInput{Title: "t"})
	require.NoError(t, err)
	require.Equal(t, api.URL()+"/acme/widgets/pulls/13", created.URL)
}

func TestStripWorkInProgressPrefix(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		title   string
		want    string
		changed bool
	}{
		{"WIP: add thing", "add thing", true},
		{"wip: add thing", "add thing", true},
		{"[WIP] add thing", "add thing", true},
		{"Draft: add thing", "add thing", true},
		{"add thing", "add thing", false},
		{"  WIP: add thing  ", "add thing", true},
	} {
		stripped, changed := StripWorkInProgressPrefix(testCase.title)
		require.Equalf(t, testCase.want, stripped, "title %q", testCase.title)
		require.Equalf(t, testCase.changed, changed, "title %q", testCase.title)
	}
}
