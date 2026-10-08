package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// Live contract tests. They are skipped unless a disposable instance is
// supplied, so the default `go test ./...` stays hermetic:
//
//	KANDEV_FORGEJO_URL=http://localhost:3000 \
//	KANDEV_FORGEJO_TOKEN=... \
//	KANDEV_FORGEJO_OWNER=kandev KANDEV_FORGEJO_REPO=demo \
//	go test ./internal/forgejo -run TestLive -v
//
// Point them at Forgejo and at Gitea in turn: this plugin targets the REST v1
// surface both hosts share, and these tests are what prove that claim.
func liveConfig(t *testing.T) (baseURL, token, owner, repo string) {
	t.Helper()
	baseURL = strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_URL"))
	token = strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_TOKEN"))
	if baseURL == "" || token == "" {
		t.Skip("set KANDEV_FORGEJO_URL and KANDEV_FORGEJO_TOKEN to run live contract tests")
	}
	owner = strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_OWNER"))
	repo = strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_REPO"))
	require.NotEmpty(t, owner, "KANDEV_FORGEJO_OWNER is required")
	require.NotEmpty(t, repo, "KANDEV_FORGEJO_REPO is required")
	return baseURL, token, owner, repo
}

func liveAdapters(t *testing.T) (*Client, *Repositories, *Associations, *Reviews, *References, *fakeHost) {
	t.Helper()
	baseURL, token, _, _ := liveConfig(t)
	host := newFakeHost(map[string]any{"base_url": baseURL, "api_token": token})
	connection := NewConnection(func() pluginsdk.Host { return host })
	client, err := connection.Client(context.Background())
	require.NoError(t, err)
	repositories := NewRepositories(connection, func() pluginsdk.Host { return host }, "forgejo")
	associations := NewAssociations(func() pluginsdk.Host { return host })
	reviews := NewReviews(connection, repositories, associations, "forgejo")
	return client, repositories, associations, reviews, NewReferences(connection), host
}

func TestLiveInstanceSpeaksRESTv1(t *testing.T) {
	client, _, _, _, _, _ := liveAdapters(t)
	ctx := context.Background()

	version, err := client.Version(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, version)
	t.Logf("instance version: %s", version)

	user, err := client.CurrentUser(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, user.Login)

	flavor := client.DetectFlavor(ctx, version)
	require.Contains(t, []Flavor{FlavorForgejo, FlavorGitea}, flavor)
	t.Logf("detected flavor: %s", flavor)
	// When the caller says which host this is, hold the probe to it.
	if expected := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_EXPECT_FLAVOR")); expected != "" {
		require.Equal(t, Flavor(expected), flavor)
	}
}

func TestLiveRepositoryDiscovery(t *testing.T) {
	client, repositories, _, _, _, _ := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx := context.Background()

	page, err := repositories.List(ctx, "workspace-1", "", sourcecontrol.RepositoryCursor{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, page.Repositories, "the token should see at least the test repository")

	var found *sourcecontrol.Repository
	for i := range page.Repositories {
		if page.Repositories[i].Name == repo && page.Repositories[i].OwnerOrProject == owner {
			found = &page.Repositories[i]
			break
		}
	}
	require.NotNil(t, found, "listing did not include %s/%s", owner, repo)
	require.Equal(t, client.Scope(), found.ConnectionScope)
	require.NotEmpty(t, found.CloneURL)
	require.NotEmpty(t, found.RepositoryID)

	// inspectURL is the ownership authority.
	inspected, err := repositories.Inspect(ctx, "workspace-1", client.Scope()+"/"+owner+"/"+repo)
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, found.RepositoryID, inspected.RepositoryID)

	foreign, err := repositories.Inspect(ctx, "workspace-1", "https://github.com/"+owner+"/"+repo)
	require.NoError(t, err)
	require.Nil(t, foreign, "a URL on another host must not be claimed")

	// Resolve by immutable id, then list its branches.
	resolved, err := repositories.Resolve(ctx, "workspace-1", sourcecontrol.RepositoryIdentity{
		ConnectionScope: found.ConnectionScope,
		RepositoryID:    found.RepositoryID,
	})
	require.NoError(t, err)
	require.Equal(t, repo, resolved.Name)

	branches, err := repositories.ListBranches(ctx, "workspace-1", resolved)
	require.NoError(t, err)
	require.NotEmpty(t, branches)
	var sawDefault bool
	for _, branch := range branches {
		if branch.IsDefault {
			sawDefault = true
		}
	}
	require.True(t, sawDefault, "the default branch must be flagged")
}

func TestLiveReviewSnapshot(t *testing.T) {
	client, repositories, associations, reviews, _, host := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx := context.Background()

	inspected, err := repositories.Inspect(ctx, "workspace-1", client.Scope()+"/"+owner+"/"+repo)
	require.NoError(t, err)
	require.NotNil(t, inspected)

	host.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "workspace-1"}
	identity := sourcecontrol.ChangeRequestIdentity{
		ConnectionScope: inspected.ConnectionScope,
		RepositoryID:    inspected.RepositoryID,
		Number:          1,
	}
	require.NoError(t, associations.Link(ctx, "task-1", identity))

	snapshots, err := reviews.ForTask(ctx, "workspace-1", "task-1")
	require.NoError(t, err)
	require.Len(t, snapshots, 1, "pull request #1 must exist in the test repository")

	snapshot := snapshots[0]
	require.Contains(t, []string{"open", "draft", "merged", "closed"}, snapshot.State)
	require.NotEmpty(t, snapshot.Title)
	require.NotEmpty(t, snapshot.URL)
	require.NotNil(t, snapshot.TaskStatus)
	require.Equal(t, int64(1), snapshot.TaskStatus.Number)
	require.Contains(t, []string{"success", "failure", "pending", "neutral"}, snapshot.TaskStatus.PipelineState)
	t.Logf("state=%s pipeline=%s checks=%d review=%+v",
		snapshot.State, snapshot.TaskStatus.PipelineState, len(snapshot.TaskStatus.Checks), snapshot.TaskStatus.Review)

	// The workspace map must agree with the per-task snapshot on reviewKey.
	workspace, err := reviews.Associations(ctx, "workspace-1")
	require.NoError(t, err)
	require.Len(t, workspace, 1)
	require.Equal(t, snapshot.ReviewKey, workspace[0].ReviewKey)

	require.NoError(t, associations.Unlink(ctx, "task-1", identity))
	after, err := reviews.Associations(ctx, "workspace-1")
	require.NoError(t, err)
	require.Empty(t, after)
}

func TestLiveReferenceResolutionAndAuthorization(t *testing.T) {
	client, repositories, _, _, references, host := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx := context.Background()

	connection := NewConnection(func() pluginsdk.Host { return host })
	changeRequests := NewChangeRequests(connection, repositories)

	resolved, err := changeRequests.ResolveReference(ctx, "workspace-1", owner+"/"+repo+"#1")
	require.NoError(t, err)
	require.Equal(t, int64(1), resolved.Identity.Number)
	require.Equal(t, client.Scope(), resolved.Identity.ConnectionScope)

	// The same pull request addressed by URL must resolve identically.
	byURL, err := changeRequests.ResolveReference(ctx, "workspace-1",
		client.Scope()+"/"+owner+"/"+repo+"/pulls/1")
	require.NoError(t, err)
	require.Equal(t, resolved.Identity, byURL.Identity)

	candidates, err := references.Search(ctx, "workspace-1", "", 10)
	require.NoError(t, err)
	require.NotEmpty(t, candidates, "the composer picker should find the test pull request")

	allowed, err := references.Authorize(ctx, "workspace-1", "submission",
		map[string]any{"id": candidates[0].ProviderLocalID})
	require.NoError(t, err)
	require.True(t, allowed)

	// A reference to something that does not exist must fail closed.
	denied, err := references.Authorize(ctx, "workspace-1", "submission",
		map[string]any{"id": "999999:999999"})
	require.NoError(t, err)
	require.False(t, denied)
}

func TestLiveCreatePullRequest(t *testing.T) {
	baseBranch := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_HEAD_BRANCH"))
	if baseBranch == "" {
		t.Skip("set KANDEV_FORGEJO_HEAD_BRANCH to exercise pull-request creation")
	}
	client, repositories, _, _, _, host := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx := context.Background()

	inspected, err := repositories.Inspect(ctx, "workspace-1", client.Scope()+"/"+owner+"/"+repo)
	require.NoError(t, err)
	require.NotNil(t, inspected)

	// Both hosts reject a second open pull request for the same head/base
	// pair with 409, so branch off a fresh head to keep this test re-runnable
	// against a long-lived instance.
	branch := fmt.Sprintf("%s-%d", baseBranch, time.Now().UnixNano())
	require.NoError(t, createBranch(ctx, client, owner, repo, branch, baseBranch))

	connection := NewConnection(func() pluginsdk.Host { return host })
	created, err := NewChangeRequests(connection, repositories).Create(ctx, *inspected, branch,
		sourcecontrol.CreateChangeRequestInput{
			Title:       "Kandev plugin live test",
			Description: "Opened by the kandev-plugin-forgejo live contract test.",
			Draft:       true,
		})
	require.NoError(t, err)
	require.Positive(t, created.Identity.Number)
	require.NotEmpty(t, created.URL)
	t.Logf("created %s", created.URL)

	// A draft request is marked with the portable WIP title prefix.
	pull, err := client.PullRequest(ctx, owner, repo, created.Identity.Number)
	require.NoError(t, err)
	require.Equal(t, "draft", pullRequestState(pull))
}

// createBranch is test support: the plugin never creates branches at runtime,
// Kandev's executor does.
func createBranch(ctx context.Context, client *Client, owner, repo, newBranch, fromBranch string) error {
	body := map[string]string{"new_branch_name": newBranch, "old_branch_name": fromBranch}
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(repo) + "/branches"
	return client.do(ctx, http.MethodPost, apiV1, path, nil, body, nil)
}

// TestLiveCIStatusResolves proves the fallback chain against a real instance.
// The interesting assertion is not the state — a disposable repo usually has no
// CI at all — but that whichever surfaces this release serves are reached
// without error, and that a ref with nothing on it reports "none" rather than
// failing.
func TestLiveCIStatusResolves(t *testing.T) {
	client, _, _, _, _, _ := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	branches, err := client.Branches(ctx, owner, repo, 1, 1)
	require.NoError(t, err)
	require.NotEmpty(t, branches, "the live repository needs at least one branch")
	ref := branches[0].Name

	status, err := client.CIStatusFor(ctx, owner, repo, ref)
	require.NoError(t, err)
	require.Equal(t, ref, status.Ref)
	require.Contains(t, []string{CISourceRuns, CISourceTasks, CISourceStatus, CISourceNone}, status.Source)
	require.Contains(t, []string{CIStateSuccess, CIStateFailure, CIStateRunning, CIStatePending, CIStateNone}, status.State)
	if status.Source == CISourceNone {
		require.Equal(t, CIStateNone, status.State)
		require.Empty(t, status.Jobs)
	}
	t.Logf("live CI: source=%s state=%s jobs=%d", status.Source, status.State, len(status.Jobs))

	// Resolving by commit id must take the other query path without error.
	byCommit, err := client.CIStatusFor(ctx, owner, repo, branches[0].Commit.ID)
	require.NoError(t, err)
	require.Contains(t, []string{CISourceRuns, CISourceTasks, CISourceStatus, CISourceNone}, byCommit.Source)
}

// TestLiveActionsSurfaceMatrix records which Actions endpoints this release
// serves. It asserts nothing about availability — the point is that each probe
// either answers or reports a clean "absent", never an unhandled error — and it
// prints the matrix the fallback chain is designed around.
func TestLiveActionsSurfaceMatrix(t *testing.T) {
	client, _, _, _, _, _ := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	version, err := client.Version(ctx)
	require.NoError(t, err)

	runs, runsErr := client.ActionRuns(ctx, owner, repo, "", 5)
	if runsErr != nil {
		require.ErrorIs(t, runsErr, ErrNotFound, "an unsupported endpoint must map to ErrNotFound")
	}
	tasks, tasksErr := client.ActionTasks(ctx, owner, repo, 1, 5)
	if tasksErr != nil {
		require.ErrorIs(t, tasksErr, ErrNotFound)
	}
	_, _, logsErr := client.JobLogTail(ctx, owner, repo, 1, 1024)
	if logsErr != nil {
		require.ErrorIs(t, logsErr, ErrNotFound)
	}

	t.Logf("version=%s runs=%v(%d) tasks=%v(%d) job_logs=%v",
		version, runsErr == nil, len(runs), tasksErr == nil, len(tasks), logsErr == nil)
}

// TestLivePullRequestEditRoundTrips covers the "mark ready" path: the title
// edit REST v1 offers in place of a draft flag neither host has.
func TestLivePullRequestEditRoundTrips(t *testing.T) {
	client, _, _, _, _, _ := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	base := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_HEAD_BRANCH"))
	if base == "" {
		t.Skip("set KANDEV_FORGEJO_HEAD_BRANCH to exercise pull-request editing")
	}
	// A fresh head per run: both hosts reject a second open pull request for
	// the same head/base pair.
	head := fmt.Sprintf("%s-edit-%d", base, time.Now().UnixNano())
	require.NoError(t, createBranch(ctx, client, owner, repo, head, base))

	created, err := client.CreatePullRequest(ctx, owner, repo, CreatePullRequestInput{
		Head: head, Base: base, Title: "WIP: kandev plugin live edit check",
	})
	require.NoError(t, err)
	require.True(t, IsWorkInProgressTitle(created.Title))

	stripped, changed := StripWorkInProgressPrefix(created.Title)
	require.True(t, changed)
	updated, err := client.EditPullRequest(ctx, owner, repo, created.Number, EditPullRequestInput{Title: stripped})
	require.NoError(t, err)
	require.Equal(t, stripped, strings.TrimSpace(updated.Title))
	require.False(t, IsWorkInProgressTitle(updated.Title))

	// The listing used to make open idempotent must find it by head branch.
	pulls, err := client.ListPullRequests(ctx, owner, repo, "open", 1, 50)
	require.NoError(t, err)
	found := false
	for _, pull := range pulls {
		if pull.Number == created.Number {
			require.Equal(t, head, pull.Head.Ref)
			found = true
		}
	}
	require.True(t, found, "a freshly opened pull request must be listed with its head ref")

	_, err = client.EditPullRequest(ctx, owner, repo, created.Number, EditPullRequestInput{State: "closed"})
	require.NoError(t, err)
}

// The issue endpoint is the one surface issue watches depend on, and it is the
// one most likely to differ between hosts and across the supported range:
// `type=issues` and `since` are both parameters older Gitea releases may
// ignore. This test creates an issue and a pull request in the live repository
// and asserts the filter behaves, rather than trusting the documentation.
func TestLiveIssueListingExcludesPullRequests(t *testing.T) {
	client, _, _, _, _, _ := liveAdapters(t)
	_, _, owner, repo := liveConfig(t)
	ctx := context.Background()

	// A label no seeded issue carries, so the filter assertion below is not
	// satisfied by pre-existing data.
	marker := fmt.Sprintf("watch-%d", time.Now().UnixNano())
	var created struct {
		Number int64 `json:"number"`
	}
	require.NoError(t, client.post(ctx,
		"/repos/"+pathSegment(owner)+"/"+pathSegment(repo)+"/issues",
		map[string]any{"title": marker, "body": "created by the live contract test"},
		&created))
	require.Positive(t, created.Number)

	issues, err := client.ListIssues(ctx, owner, repo, IssueListOptions{State: "all"})
	require.NoError(t, err)
	require.NotEmpty(t, issues)

	// Every result must be an issue. The seeded repository carries a pull
	// request, so a host ignoring type=issues is caught here.
	var found bool
	for _, issue := range issues {
		require.Falsef(t, issue.IsPullRequest(), "issue %d is a pull request", issue.Number)
		if issue.Number == created.Number {
			found = true
			require.Equal(t, marker, issue.Title)
		}
	}
	require.True(t, found, "the created issue did not come back from the listing")

	// A free-text search narrows to the issue just created — eventually. The
	// `q` filter is served by the host's issue indexer, which ingests
	// asynchronously, so a search issued immediately after a create legitimately
	// returns nothing. Retry briefly rather than racing it.
	//
	// A host that never indexes is a deployment choice, not a broken contract:
	// the watch still works, its optional free-text filter just matches nothing.
	// That is worth logging, not failing.
	var matched []Issue
	for attempt := 0; attempt < 15; attempt++ {
		matched, err = client.ListIssues(ctx, owner, repo, IssueListOptions{State: "all", Query: marker})
		require.NoError(t, err)
		if len(matched) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if len(matched) == 0 {
		t.Logf("this host's issue indexer did not pick up %q within 15s; the free-text filter will match nothing here", marker)
	} else {
		require.Equal(t, created.Number, matched[0].Number)
	}

	// `since` in the future must exclude it. This is the parameter the poller
	// relies on to keep a steady-state tick cheap; a host that ignores it
	// still works, but the plugin should know which behaviour it got.
	future, err := client.ListIssues(ctx, owner, repo, IssueListOptions{
		State: "all",
		Since: time.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)
	if len(future) != 0 {
		t.Logf("this host ignores `since` (%d issues returned); the poller still dedups correctly", len(future))
	}
}
