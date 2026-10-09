package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/stretchr/testify/require"
)

// recorder captures the JSON body of the last request to a route and counts
// how often it was hit.
type recorder struct {
	body  map[string]any
	calls atomic.Int32
}

func record(api *apiServer, method, path string, status int) *recorder {
	rec := &recorder{}
	api.handleFunc(method, path, func(w http.ResponseWriter, r *http.Request) {
		rec.calls.Add(1)
		rec.body = nil
		_ = json.NewDecoder(r.Body).Decode(&rec.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if method == http.MethodPost && path == "/api/v1/repos/o/r/pulls/7/reviews" {
			_, _ = w.Write([]byte(`{"id":1,"state":"APPROVED"}`))
		}
	})
	return rec
}

func TestMergeRequiresTheHeadTheCallerSaw(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	merge := record(api, http.MethodPost, pullBase+"/merge", 200)
	ctx := context.Background()

	_, err := actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7})
	var invalidErr *InvalidError
	require.ErrorAs(t, err, &invalidErr, "a merge without head_sha must be refused")

	_, err = actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "deadbeef"})
	requireReason(t, err, ReasonHeadChanged)
	require.Zero(t, merge.calls.Load(), "a stale head must never reach the merge endpoint")

	_, err = actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "not hex!"})
	require.ErrorAs(t, err, &invalidErr)
}

func TestMergeSendsTheHeadAndPicksAnAllowedStyle(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	merge := record(api, http.MethodPost, pullBase+"/merge", 200)
	ctx := context.Background()

	result, err := actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234", DeleteBranch: true})
	require.NoError(t, err)
	require.Equal(t, sourcecontrol.MergeOutcomeMerged, result.Outcome)
	require.Equal(t, "squash", result.Style, "the repository default decides when no style is given")
	require.Equal(t, "squash", merge.body["Do"])
	require.Equal(t, "abc1234", merge.body["head_commit_id"], "the instance must also check the head")
	require.Equal(t, true, merge.body["delete_branch_after_merge"])

	_, err = actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234", Style: "rebase"})
	var invalidErr *InvalidError
	require.ErrorAs(t, err, &invalidErr, "a style the repository does not allow is refused")
	_, err = actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234", Style: "bogus"})
	require.ErrorAs(t, err, &invalidErr)

	result, err = actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234", Style: "merge", WhenChecksSucceed: true})
	require.NoError(t, err)
	require.Equal(t, sourcecontrol.MergeOutcomeScheduled, result.Outcome)
	require.Equal(t, true, merge.body["merge_when_checks_succeed"])
}

func TestMergeCancelAndFinishedPullRequests(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	cancel := record(api, http.MethodDelete, pullBase+"/merge", 204)
	ctx := context.Background()

	result, err := actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, CancelScheduled: true})
	require.NoError(t, err)
	require.Equal(t, sourcecontrol.MergeOutcomeCancelled, result.Outcome)
	require.EqualValues(t, 1, cancel.calls.Load())

	merge := record(api, http.MethodPost, pullBase+"/merge", 200)
	api.handle(http.MethodGet, pullBase, 200, map[string]any{
		"number": 7, "state": "closed", "merged": true, "head": map[string]any{"sha": "abc"},
	})
	_, err = actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234"})
	requireReason(t, err, ReasonAlreadyMerged)
	require.Zero(t, merge.calls.Load())
}

// Gitea 1.20 answers 405 "try again later" for a moment after a PR opens; the
// merge waits it out instead of reporting a failure.
func TestMergeRetriesWhileTheInstanceIsStillChecking(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	var calls atomic.Int32
	api.handleFunc(http.MethodPost, pullBase+"/merge", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"message":"Please try again later"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	_, err := actions.Merge(context.Background(), "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234"})
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}

func TestReviewValidatesAttributesAndPinsTheCommit(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	review := record(api, http.MethodPost, pullBase+"/reviews", 200)
	ctx := context.Background()
	var invalidErr *InvalidError

	_, err := actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{Number: 7, Event: "merge"})
	require.ErrorAs(t, err, &invalidErr)
	_, err = actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{Number: 7, Event: sourcecontrol.ReviewEventRequestChanges})
	require.ErrorAs(t, err, &invalidErr, "requesting changes with nothing to say is refused")
	_, err = actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{
		Number: 7, Event: sourcecontrol.ReviewEventComment, Comments: []sourcecontrol.InlineComment{{Path: "a.go", Body: "x"}},
	})
	require.ErrorAs(t, err, &invalidErr, "an inline comment needs a line")
	require.Zero(t, review.calls.Load())

	_, err = actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{Number: 7, Event: sourcecontrol.ReviewEventApprove, HeadSHA: "deadbeef"})
	requireReason(t, err, ReasonHeadChanged)

	result, err := actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{
		Number: 7, Event: sourcecontrol.ReviewEventRequestChanges, Body: "please fix", HeadSHA: "abc1234",
		Comments: []sourcecontrol.InlineComment{{Path: "a.go", Body: "nit", Line: 4}},
	})
	require.NoError(t, err)
	require.Equal(t, "APPROVED", result.State)
	require.Equal(t, "REQUEST_CHANGES", review.body["event"])
	require.Equal(t, "abc1234", review.body["commit_id"])
	require.Contains(t, review.body["body"], "please fix")
	require.Contains(t, review.body["body"], attribution, "the shared account must be disclosed")
	comments := review.body["comments"].([]any)
	require.Equal(t, float64(4), comments[0].(map[string]any)["new_position"])

	_, err = actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{Number: 7, Event: sourcecontrol.ReviewEventApprove})
	require.NoError(t, err)
	require.Equal(t, attribution, review.body["body"], "an approval with no words still says who posted it")
}

func TestReviewersAndCommentAndUpdate(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	add := record(api, http.MethodPost, pullBase+"/requested_reviewers", 201)
	remove := record(api, http.MethodDelete, pullBase+"/requested_reviewers", 204)
	comment := record(api, http.MethodPost, "/api/v1/repos/o/r/issues/7/comments", 201)
	update := record(api, http.MethodPost, pullBase+"/update", 200)
	ctx := context.Background()
	var invalidErr *InvalidError

	_, err := actions.RequestReviewers(ctx, "ws-1", "task-1", sourcecontrol.ReviewersRequest{Number: 7, Add: []string{"bob; drop"}})
	require.ErrorAs(t, err, &invalidErr)
	_, err = actions.RequestReviewers(ctx, "ws-1", "task-1", sourcecontrol.ReviewersRequest{Number: 7})
	require.ErrorAs(t, err, &invalidErr)
	_, err = actions.RequestReviewers(ctx, "ws-1", "task-1", sourcecontrol.ReviewersRequest{Number: 7, Add: []string{"bob"}, Remove: []string{"carol"}})
	require.NoError(t, err)
	require.Equal(t, []any{"bob"}, add.body["reviewers"])
	require.Equal(t, []any{"carol"}, remove.body["reviewers"])

	_, err = actions.Comment(ctx, "ws-1", "task-1", sourcecontrol.CommentRequest{Number: 7, Body: "  "})
	require.ErrorAs(t, err, &invalidErr)
	_, err = actions.Comment(ctx, "ws-1", "task-1", sourcecontrol.CommentRequest{Number: 7, Body: "hello"})
	require.NoError(t, err)
	require.Contains(t, comment.body["body"], "hello")
	require.Contains(t, comment.body["body"], attribution)

	_, err = actions.UpdateBranch(ctx, "ws-1", "task-1", sourcecontrol.UpdateBranchRequest{Number: 7, Style: "squash"})
	require.ErrorAs(t, err, &invalidErr)
	_, err = actions.UpdateBranch(ctx, "ws-1", "task-1", sourcecontrol.UpdateBranchRequest{Number: 7})
	require.NoError(t, err)
	require.EqualValues(t, 1, update.calls.Load())
}

// Every write resolves through the task's links, and a refused call never
// reaches the instance, whatever number or task it names.
func TestEveryWriteRefusesAnUnlinkedPullRequest(t *testing.T) {
	t.Parallel()
	actions, api, _ := detailsFixture(t)
	ctx := context.Background()
	before := len(api.requests)

	calls := map[string]func() error{
		"merge": func() error {
			_, err := actions.Merge(ctx, "ws-1", "task-1", sourcecontrol.MergeRequest{Number: 99, HeadSHA: "abc1234"})
			return err
		},
		"review": func() error {
			_, err := actions.Review(ctx, "ws-1", "task-1", sourcecontrol.ReviewRequest{Number: 99, Event: sourcecontrol.ReviewEventApprove})
			return err
		},
		"reviewers": func() error {
			_, err := actions.RequestReviewers(ctx, "ws-1", "task-1", sourcecontrol.ReviewersRequest{Number: 99, Add: []string{"bob"}})
			return err
		},
		"update": func() error {
			_, err := actions.UpdateBranch(ctx, "ws-1", "task-1", sourcecontrol.UpdateBranchRequest{Number: 99})
			return err
		},
		"comment": func() error {
			_, err := actions.Comment(ctx, "ws-1", "task-1", sourcecontrol.CommentRequest{Number: 99, Body: "x"})
			return err
		},
		"other task": func() error {
			_, err := actions.Merge(ctx, "ws-1", "other-task", sourcecontrol.MergeRequest{Number: 7, HeadSHA: "abc1234"})
			return err
		},
	}
	for name, call := range calls {
		require.ErrorIs(t, call(), ErrNotLinked, name)
	}
	require.Len(t, api.requests, before)
}
