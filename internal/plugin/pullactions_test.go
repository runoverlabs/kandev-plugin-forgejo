package plugin

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"kandev-plugin-forgejo/internal/forgejo"
)

func TestPullFailureUsesExplicitFixedStatuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err    error
		status int
	}{
		{forgejo.ErrNotLinked, http.StatusNotFound},
		{forgejo.ErrAmbiguous, http.StatusConflict},
		{forgejo.ErrUnauthorized, http.StatusForbidden},
		{&forgejo.WriteError{Status: 403, Reason: forgejo.ReasonBlockedByProtection}, http.StatusForbidden},
		{&forgejo.WriteError{Status: 409, Reason: forgejo.ReasonHeadChanged}, http.StatusConflict},
		{&forgejo.WriteError{Status: 422, Reason: forgejo.ReasonSelfReview}, http.StatusUnprocessableEntity},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{errors.New("token=super-secret"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		response, err := pullFailure(tc.err)
		require.NoError(t, err, "an expected failure must be a status, not a Go error the host masks")
		require.Equal(t, tc.status, response.Status)
		require.NotContains(t, string(response.Body), "super-secret")
	}
}

// Every write key is routed through the same gate: a disabled workspace is
// refused, a bad body is a 422, and a task with no linked pull request is a 404
// that never reaches an instance (the host here has no instance at all).
func TestWriteActionsAreGatedAndRefuseUnlinkedPullRequests(t *testing.T) {
	t.Parallel()
	keys := []string{
		sourcecontrol.ActionChangeRequestsMerge, sourcecontrol.ActionChangeRequestsReview,
		sourcecontrol.ActionChangeRequestsRequestReviewers, sourcecontrol.ActionChangeRequestsUpdateBranch,
		sourcecontrol.ActionChangeRequestsComment,
	}
	runtime := NewRuntime()
	runtime.SetHost(newConfigHost(map[string]any{"base_url": "http://127.0.0.1:1", "api_token": "t"}))
	ctx := context.Background()
	verified := pluginsdk.VerifiedActionContext{ActorID: "u1", WorkspaceID: "ws-1", TaskID: "task-1"}

	for _, key := range keys {
		bad, err := runtime.HandleAction(ctx, &pluginsdk.PluginActionRequest{ActionKey: key, Context: verified, Body: []byte(`{"number":`)})
		require.NoError(t, err, key)
		require.Equal(t, http.StatusUnprocessableEntity, bad.Status, key)

		unlinked, err := runtime.HandleAction(ctx, &pluginsdk.PluginActionRequest{
			ActionKey: key, Context: verified, Body: []byte(`{"number":5,"head_sha":"abc1234","event":"approve","body":"x","add":["bob"]}`),
		})
		require.NoError(t, err, key)
		require.Equal(t, http.StatusNotFound, unlinked.Status, key)
	}

	setEnabled(t, runtime, "ws-1", false)
	for _, key := range keys {
		off, err := runtime.HandleAction(ctx, &pluginsdk.PluginActionRequest{ActionKey: key, Context: verified, Body: []byte(`{}`)})
		require.NoError(t, err, key)
		require.Equal(t, http.StatusConflict, off.Status, "a disabled workspace must refuse %s", key)
	}
}
