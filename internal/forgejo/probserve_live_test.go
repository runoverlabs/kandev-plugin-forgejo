package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// liveAutomationSource is the automation adapter on the admin token, which is
// the operator account here.
func liveAutomationSource(t *testing.T) (*AutomationSource, *Client) {
	t.Helper()
	admin, _, associations, _, _, host := liveAdapters(t)
	return NewAutomationSource(NewConnection(func() pluginsdk.Host { return host }), associations), admin
}

func liveRepoRef(t *testing.T, client *Client, owner, repo string, number int64) prautomation.PullRef {
	t.Helper()
	found, err := client.Repo(context.Background(), owner, repo)
	require.NoError(t, err)
	return prautomation.PullRef{TaskID: "task-1", RepositoryID: strconv.FormatInt(found.ID, 10), Number: number}
}

// retryChecking repeats call while the instance answers "still computing
// mergeability", which it does for a moment after every push.
func retryChecking(t *testing.T, call func() error) error {
	t.Helper()
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = call()
		var writeErr *scm.WriteError
		if !errors.As(err, &writeErr) || writeErr.Reason != scm.ReasonChecking {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// TestLiveAutomationObservation pins what the observation reads on a real
// instance: the review's official flag (the trust signal every token can see),
// inline comments with their resolver field, a failing commit status, and a
// review requested of the operator on someone else's pull request.
func TestLiveAutomationObservation(t *testing.T) {
	reviewer := liveUserClient(t, "KANDEV_FORGEJO_REVIEWER_TOKEN")
	readonly := liveUserClient(t, "KANDEV_FORGEJO_READONLY_TOKEN")
	source, admin := liveAutomationSource(t)
	_, _, owner, repo := liveConfig(t)
	base := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_HEAD_BRANCH"))
	if base == "" {
		t.Skip("set KANDEV_FORGEJO_HEAD_BRANCH")
	}
	ctx := context.Background()
	me, err := admin.CurrentUser(ctx)
	require.NoError(t, err)

	// The reviewer authors this one, so the operator can be asked to review.
	head := fmt.Sprintf("automation-%d", time.Now().UnixNano())
	require.NoError(t, createBranch(ctx, reviewer, owner, repo, head, base))
	file := map[string]string{"content": "b25lCnR3bwp0aHJlZQo=", "message": "live automation", "branch": head}
	require.NoError(t, reviewer.do(ctx, http.MethodPost, apiV1,
		"/repos/"+pathSegment(owner)+"/"+pathSegment(repo)+"/contents/"+head+".txt", nil, file, nil))
	pull, err := reviewer.CreatePullRequest(ctx, owner, repo, CreatePullRequestInput{Head: head, Base: base, Title: "live automation"})
	require.NoError(t, err)
	require.NoError(t, reviewer.RequestReviewers(ctx, owner, repo, pull.Number, []string{me.Login}))

	_, err = admin.SubmitReview(ctx, owner, repo, pull.Number, SubmitReviewInput{
		Event: ReviewRequestChanges, Body: "please fix", CommitID: pull.Head.Sha,
		Comments: []InlineComment{{Path: head + ".txt", Body: "line two", NewPosition: 2}},
	})
	require.NoError(t, err)
	_, err = readonly.SubmitReview(ctx, owner, repo, pull.Number, SubmitReviewInput{Event: ReviewComment, Body: "a reader's view"})
	require.NoError(t, err)
	require.NoError(t, readonly.CreateComment(ctx, owner, repo, pull.Number, "drive-by"))
	status := map[string]string{"state": "failure", "context": "live/ci", "target_url": "https://ci.example/1"}
	require.NoError(t, admin.do(ctx, http.MethodPost, apiV1,
		"/repos/"+pathSegment(owner)+"/"+pathSegment(repo)+"/statuses/"+pull.Head.Sha, nil, status, nil))

	obs, err := source.Observe(ctx, "ws-1", liveRepoRef(t, admin, owner, repo, pull.Number), allReads)
	require.NoError(t, err)
	require.Equal(t, prautomation.StateOpen, obs.State)
	require.False(t, obs.Fork)
	require.Equal(t, me.Login, obs.Operator)
	require.True(t, obs.OperatorRequested)
	require.Equal(t, prautomation.CIFailure, obs.CI.State)
	require.Equal(t, "live/ci", obs.CI.Failing[0].Name)

	byAuthor := map[string]prautomation.Review{}
	for _, review := range obs.Reviews {
		byAuthor[review.Author] = review
	}
	operatorReview := byAuthor[me.Login]
	require.Equal(t, prautomation.ReviewChangesRequested, operatorReview.State)
	require.True(t, operatorReview.Official, "a writer's review counts towards approvals")
	require.Len(t, operatorReview.Comments, 1)
	require.Equal(t, "line two", operatorReview.Comments[0].Body)
	require.False(t, operatorReview.Comments[0].Resolved)
	readerReview := byAuthor["readonly"]
	require.Equal(t, prautomation.ReviewCommented, readerReview.State)
	require.False(t, readerReview.Official, "a reader's review does not count")

	require.NotEmpty(t, obs.Comments)
	require.Equal(t, "drive-by", strings.TrimSpace(strings.Split(obs.Comments[len(obs.Comments)-1].Body, "\n")[0]))
}

// TestLiveAutomationMergePinsTheHead: a merge pinned to a head that has since
// moved is refused, and the current head merges. This is the guarantee the
// instance's scheduled merge does not give (docs/development.md).
func TestLiveAutomationMergePinsTheHead(t *testing.T) {
	source, admin := liveAutomationSource(t)
	ctx := context.Background()
	pull, owner, repo := livePull(t, admin)
	ref := liveRepoRef(t, admin, owner, repo, pull.Number)
	stale := pull.Head.Sha

	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(repo) + "/contents/" + pull.Head.Ref + ".txt"
	var existing struct {
		Sha string `json:"sha"`
	}
	require.NoError(t, admin.get(ctx, path, map[string][]string{"ref": {pull.Head.Ref}}, &existing))
	update := map[string]string{"content": "c2Vjb25k", "message": "second", "branch": pull.Head.Ref, "sha": existing.Sha}
	require.NoError(t, admin.do(ctx, http.MethodPut, apiV1, path, nil, update, nil))

	err := retryChecking(t, func() error { return source.MergeLinked(ctx, ref, stale) })
	requireReason(t, err, scm.ReasonHeadChanged)

	moved, err := admin.PullRequest(ctx, owner, repo, pull.Number)
	require.NoError(t, err)
	require.False(t, moved.Merged)
	require.NotEqual(t, stale, moved.Head.Sha)

	require.NoError(t, retryChecking(t, func() error { return source.MergeLinked(ctx, ref, moved.Head.Sha) }))
	merged, err := admin.PullRequest(ctx, owner, repo, pull.Number)
	require.NoError(t, err)
	require.True(t, merged.Merged)
	require.Equal(t, prautomation.StateMerged, automationState(merged))
}
