package plugin

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

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
