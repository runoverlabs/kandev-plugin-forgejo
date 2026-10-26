package plugin

import (
	"context"
	"errors"
	"net/http"

	"github.com/kandev/kandev/pkg/pluginsdk"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"
)

// The operator-facing text and status for every failure the transport
// answers. Provider error bodies are never forwarded: on some deployments they
// echo the token. Only scm's error kinds are read here.

// safeMessage maps an adapter error onto an operator-facing message. Provider
// error bodies are never forwarded: on some deployments they echo the token.
func safeMessage(err error) string {
	var writeErr *scm.WriteError
	switch {
	case errors.As(err, &writeErr):
		return writeReasonMessage(writeErr.Reason)
	case errors.Is(err, scm.ErrUnauthorized):
		return "The instance rejected the access token. Check that it is valid and has repository scope."
	case errors.Is(err, scm.ErrNotFound):
		return "The instance did not expose the expected REST v1 API at this URL."
	case errors.Is(err, scm.ErrNotConfigured):
		return "Set the instance URL and access token in Settings > Plugins > Forgejo."
	default:
		return "Could not reach the configured instance."
	}
}

// writeReasonMessage maps a classified write failure onto a fixed message. A
// 403 on a write is a permission or protection problem, never a bad token.
func writeReasonMessage(reason scm.Reason) string {
	switch reason {
	case scm.ReasonNotMergeable:
		return "The pull request cannot be merged right now."
	case scm.ReasonChecking:
		return "The instance is still checking whether this can be merged. Try again in a moment."
	case scm.ReasonConflict:
		return "The pull request has merge conflicts."
	case scm.ReasonOutOfDate:
		return "The branch is out of date with its base. Update it first."
	case scm.ReasonHeadChanged:
		return "The pull request changed since it was last read. Refresh and try again."
	case scm.ReasonBlockedByProtection:
		return "Branch protection blocks this: required approvals or checks are missing."
	case scm.ReasonSelfReview:
		return "An account cannot review its own pull request."
	case scm.ReasonAlreadyMerged:
		return "The pull request is already merged."
	case scm.ReasonForbidden:
		return "The access token lacks permission for this action. It needs write access to the repository, and the write:repository scope (plus write:issue for comments and labels)."
	default:
		return "The instance rejected the request."
	}
}

// pullFailure turns an adapter error into an explicit domain status with a
// fixed message. The host masks a plain Go error as an internal error, which
// would hide a failure the user can act on.
func pullFailure(err error) (*pluginsdk.PluginActionResponse, error) {
	var writeErr *scm.WriteError
	var invalidErr *scm.InvalidError
	switch {
	case errors.Is(err, scm.ErrNotLinked):
		return kandev.Refuse(http.StatusNotFound, "That pull request is not linked to this task.")
	case errors.As(err, &invalidErr):
		return kandev.Refuse(http.StatusUnprocessableEntity, invalidErr.Message)
	case errors.Is(err, scm.ErrAmbiguous):
		return kandev.Refuse(http.StatusConflict, "This task links more than one pull request; say which one.")
	case errors.As(err, &writeErr):
		return kandev.Refuse(writeStatus(writeErr), writeReasonMessage(writeErr.Reason))
	case errors.Is(err, scm.ErrUnauthorized):
		return kandev.Refuse(http.StatusForbidden, safeMessage(err))
	case errors.Is(err, context.DeadlineExceeded):
		return kandev.Refuse(http.StatusGatewayTimeout, "The instance took too long to answer.")
	default:
		return kandev.Refuse(http.StatusBadGateway, safeMessage(err))
	}
}

// writeStatus keeps expected write failures in the 403/409/422 band the host
// passes through.
func writeStatus(err *scm.WriteError) int {
	switch err.Reason {
	case scm.ReasonForbidden, scm.ReasonBlockedByProtection:
		return http.StatusForbidden
	case scm.ReasonInvalid, scm.ReasonSelfReview:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusConflict
	}
}
