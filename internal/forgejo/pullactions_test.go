package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const pullBase = "/api/v1/repos/o/r/pulls/7"

func newActionsClient(t *testing.T) (*Client, *apiServer) {
	t.Helper()
	api := newAPIServer(t)
	client, err := NewClient(api.url(), "super-secret-token", nil)
	require.NoError(t, err)
	return client, api
}

func TestMergePullRequestSendsExpectedBody(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	var got map[string]any
	api.handleFunc(http.MethodPost, pullBase+"/merge", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK) // empty body must be accepted
	})

	err := client.MergePullRequest(context.Background(), "o", "r", 7, MergeInput{
		Do: MergeStyleSquash, HeadCommitID: "abc", DeleteBranch: true,
	})
	require.NoError(t, err)
	require.Equal(t, "squash", got["Do"])
	require.Equal(t, "abc", got["head_commit_id"])
	require.Equal(t, true, got["delete_branch_after_merge"])
	require.NotContains(t, got, "force_merge")
}

func TestMergePullRequestRequiresStyle(t *testing.T) {
	t.Parallel()
	client, _ := newActionsClient(t)
	require.Error(t, client.MergePullRequest(context.Background(), "o", "r", 7, MergeInput{}))
}

func TestWriteStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   Reason
	}{
		{"not mergeable", 405, `{"message":""}`, ReasonNotMergeable},
		{"conflict", 409, `{"message":"Merge Conflict"}`, ReasonConflict},
		{"out of date", 409, `{"message":"Merge push out of date"}`, ReasonOutOfDate},
		{"head changed", 409, `{"message":"head commit does not match"}`, ReasonHeadChanged},
		{"protected", 403, `{"message":"Not all required status checks successful"}`, ReasonBlockedByProtection},
		{"approvals", 403, `{"message":"Does not have enough approvals yet"}`, ReasonBlockedByProtection},
		{"permission is required", 403, `{"message":"write permission is required"}`, ReasonForbidden},
		{"no permission", 403, `{"message":"user should have write permission"}`, ReasonForbidden},
		{"self review", 422, `{"message":"approve your own pull is not allowed"}`, ReasonSelfReview},
		{"already merged", 422, `{"message":"pull request has already merged"}`, ReasonAlreadyMerged},
		{"merge without permission", 405, `{"message":"User not allowed to merge PR"}`, ReasonForbidden},
		{"still computing", 405, `{"message":"Please try again later"}`, ReasonChecking},
		{"gitea already merged", 405, `{"message":"The PR is already merged"}`, ReasonAlreadyMerged},
		{"no-op update", 500, `{"message":"HeadBranch of PR 1 is up to date"}`, ReasonUpToDate},
		{"invalid", 422, `{"message":"unsupported merge style"}`, ReasonInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, api := newActionsClient(t)
			api.handleFunc(http.MethodPost, pullBase+"/merge", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := client.MergePullRequest(context.Background(), "o", "r", 7, MergeInput{Do: "merge"})
			var werr *WriteError
			require.ErrorAs(t, err, &werr)
			require.Equal(t, tc.status, werr.Status)
			require.Equal(t, tc.want, werr.Reason)
		})
	}
}

// A 401 is a bad token and stays one; a 403 on a write is not.
func TestWriteKeepsUnauthorizedAndNotFound(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handle(http.MethodPost, pullBase+"/merge", http.StatusUnauthorized, nil)
	require.ErrorIs(t, client.MergePullRequest(context.Background(), "o", "r", 7, MergeInput{Do: "merge"}), ErrUnauthorized)

	api.handle(http.MethodPost, pullBase+"/merge", http.StatusNotFound, nil)
	require.ErrorIs(t, client.MergePullRequest(context.Background(), "o", "r", 7, MergeInput{Do: "merge"}), ErrNotFound)
}

func TestWriteErrorsDoNotLeakBodyOrToken(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handleFunc(http.MethodPost, pullBase+"/reviews", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"token super-secret-token lacks scope"}`))
	})
	_, err := client.SubmitReview(context.Background(), "o", "r", 7, SubmitReviewInput{Event: ReviewApprove})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "super-secret-token")
	require.NotContains(t, err.Error(), "lacks scope")
}

func TestSubmitReviewAndReviewerRequests(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	var review, add, remove map[string]any
	api.handleFunc(http.MethodPost, pullBase+"/reviews", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&review))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":3,"state":"APPROVED"}`))
	})
	api.handleFunc(http.MethodPost, pullBase+"/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&add))
		w.WriteHeader(http.StatusCreated)
	})
	api.handleFunc(http.MethodDelete, pullBase+"/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&remove))
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := context.Background()

	got, err := client.SubmitReview(ctx, "o", "r", 7, SubmitReviewInput{
		Event: ReviewApprove, Body: "lgtm", CommitID: "abc",
		Comments: []InlineComment{{Path: "a.go", Body: "nit", NewPosition: 4}},
	})
	require.NoError(t, err)
	require.Equal(t, "APPROVED", got.State)
	require.Equal(t, "APPROVED", review["event"])
	require.Equal(t, "abc", review["commit_id"])
	require.Len(t, review["comments"], 1)

	require.NoError(t, client.RequestReviewers(ctx, "o", "r", 7, []string{"alice"}))
	require.Equal(t, []any{"alice"}, add["reviewers"])
	require.NoError(t, client.RemoveReviewRequest(ctx, "o", "r", 7, []string{"alice"}))
	require.Equal(t, []any{"alice"}, remove["reviewers"])
}

func TestCancelAutoMergeAndUpdateBranch(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handle(http.MethodDelete, pullBase+"/merge", http.StatusNoContent, nil)
	api.handle(http.MethodPost, pullBase+"/update", http.StatusOK, nil)
	ctx := context.Background()

	require.NoError(t, client.CancelAutoMerge(ctx, "o", "r", 7))
	require.NoError(t, client.UpdateBranch(ctx, "o", "r", 7, "rebase"))
	require.Equal(t, "style=rebase", api.requests[len(api.requests)-1].URL.RawQuery)
	require.Error(t, client.UpdateBranch(ctx, "o", "r", 7, "squash"))

	// Gitea answers 500 when there is nothing to update; that is success.
	api.handle(http.MethodPost, pullBase+"/update", http.StatusInternalServerError, map[string]any{"message": "HeadBranch of PR 7 is up to date"})
	require.NoError(t, client.UpdateBranch(ctx, "o", "r", 7, "merge"))
}

func TestPullFilesPagesAndReportsTruncation(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handleFunc(http.MethodGet, pullBase+"/files", func(w http.ResponseWriter, r *http.Request) {
		files := make([]ChangedFile, pullListPageSize)
		for i := range files {
			files[i] = ChangedFile{Filename: "f", Additions: 1}
		}
		require.NoError(t, json.NewEncoder(w).Encode(files))
	})

	files, truncated, err := client.PullFiles(context.Background(), "o", "r", 7, 70)
	require.NoError(t, err)
	require.Len(t, files, 70)
	require.True(t, truncated)

	// A server that never runs dry is still bounded by the page cap.
	files, truncated, err = client.PullFiles(context.Background(), "o", "r", 7, 100000)
	require.NoError(t, err)
	require.Len(t, files, pullListPageSize*maxPullListPages)
	require.True(t, truncated)
}

func TestPullCommitsShortPageIsComplete(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handle(http.MethodGet, pullBase+"/commits", http.StatusOK, []map[string]any{{"sha": "a"}, {"sha": "b"}})
	commits, truncated, err := client.PullCommits(context.Background(), "o", "r", 7, 100)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	require.False(t, truncated)
}

func TestPullDiffKeepsTheStart(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handleFunc(http.MethodGet, pullBase+".diff", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	})
	ctx := context.Background()

	diff, truncated, err := client.PullDiff(ctx, "o", "r", 7, 4)
	require.NoError(t, err)
	require.Equal(t, "0123", diff)
	require.True(t, truncated)

	diff, truncated, err = client.PullDiff(ctx, "o", "r", 7, 10)
	require.NoError(t, err)
	require.Equal(t, "0123456789", diff)
	require.False(t, truncated)
}

func TestIssueSideWrites(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	var labels, assignees, comment map[string]any
	decode := func(into *map[string]any, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(into))
			w.WriteHeader(status)
		}
	}
	api.handleFunc(http.MethodPut, "/api/v1/repos/o/r/issues/7/labels", decode(&labels, 200))
	api.handleFunc(http.MethodPatch, "/api/v1/repos/o/r/issues/7", decode(&assignees, 201))
	api.handleFunc(http.MethodPost, "/api/v1/repos/o/r/issues/7/comments", decode(&comment, 201))
	ctx := context.Background()

	require.NoError(t, client.SetLabels(ctx, "o", "r", 7, nil))
	require.Equal(t, []any{}, labels["labels"], "clearing must send [], not omit the field")
	require.NoError(t, client.SetAssignees(ctx, "o", "r", 7, nil))
	require.Equal(t, []any{}, assignees["assignees"])
	require.NoError(t, client.CreateComment(ctx, "o", "r", 7, "hi"))
	require.Equal(t, "hi", comment["body"])
}

func TestRepoMergeStylesOrderAndAbsence(t *testing.T) {
	t.Parallel()
	require.Empty(t, Repo{}.MergeStyles(), "an unreporting instance means let the backend choose")
	repo := Repo{AllowMergeCommits: true, AllowSquashMerge: true, AllowRebase: true, AllowFastForwardOnlyMerge: true}
	require.Equal(t, []string{"squash", "merge", "rebase", "fast-forward-only"}, repo.MergeStyles())
}

func TestBranchDetailDecodesProtection(t *testing.T) {
	t.Parallel()
	client, api := newActionsClient(t)
	api.handle(http.MethodGet, "/api/v1/repos/o/r/branches/main", http.StatusOK, map[string]any{
		"name": "main", "protected": true, "required_approvals": 2,
		"enable_status_check": true, "status_check_contexts": []string{"ci"}, "user_can_merge": true,
	})
	api.handle(http.MethodGet, "/api/v1/repos/o/r/branch_protections/main", http.StatusForbidden, nil)
	ctx := context.Background()

	branch, err := client.BranchDetail(ctx, "o", "r", "main")
	require.NoError(t, err)
	require.True(t, branch.Protected)
	require.EqualValues(t, 2, branch.RequiredApprovals)
	require.Equal(t, []string{"ci"}, branch.StatusCheckContext)

	_, err = client.BranchProtectionRule(ctx, "o", "r", "main")
	require.ErrorIs(t, err, ErrUnauthorized, "a non-admin read is unavailable, not a failure")
}

// An absent field must decode to nil (unknown), not to zero or false.
func TestPullRequestUnknownFieldsStayNil(t *testing.T) {
	t.Parallel()
	var old, current PullRequest
	require.NoError(t, json.Unmarshal([]byte(`{"number":1}`), &old))
	require.Nil(t, old.Additions)
	require.Nil(t, old.Deletions)
	require.Nil(t, old.ChangedFiles)
	require.Nil(t, old.Mergeable)

	require.NoError(t, json.Unmarshal([]byte(`{"number":1,"additions":0,"deletions":3,"changed_files":2,"mergeable":false}`), &current))
	require.NotNil(t, current.Additions)
	require.Equal(t, 0, *current.Additions, "a reported zero is zero, not unknown")
	require.Equal(t, 3, *current.Deletions)
	require.Equal(t, 2, *current.ChangedFiles)
	require.NotNil(t, current.Mergeable)
	require.False(t, *current.Mergeable)
}
