package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// detailsBudget keeps the composite read inside the host's 15-second action
// timeout, with room to write the reply.
const detailsBudget = 12 * time.Second

// handlePullAction routes the pull request actions this plugin owns beyond the
// recipe's. The workspace enable switch is checked here for the same reason the
// default dispatch branch does: a disabled integration contributes nothing.
func (r *Runtime) handlePullAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	enabled, err := r.integrationEnabled(ctx, request.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return domainResponse(http.StatusConflict, "The Forgejo integration is turned off for this workspace.")
	}
	switch request.ActionKey {
	case sourcecontrol.ActionChangeRequestsDetails:
		return r.changeRequestDetails(ctx, request)
	default:
		return nil, errors.New("kandev-plugin-forgejo: unsupported pull request action")
	}
}

// changeRequestDetails returns the full panel model for a pull request linked
// to the verified task. The body may name which one when the task links
// several; a number the task does not own is refused, whatever it points at.
func (r *Runtime) changeRequestDetails(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64 `json:"number"`
	}
	if len(request.Body) > 0 {
		if err := json.Unmarshal(request.Body, &body); err != nil || body.Number < 0 {
			return domainResponse(http.StatusUnprocessableEntity, "The request body is not valid.")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, detailsBudget)
	defer cancel()
	details, err := r.pullActions.Details(ctx, request.Context.WorkspaceID, request.Context.TaskID, body.Number)
	if err != nil {
		return pullFailure(err)
	}
	return jsonResponse(details)
}

// pullFailure turns an adapter error into an explicit domain status with a
// fixed message. The host masks a plain Go error as an internal error, which
// would hide a failure the user can act on.
func pullFailure(err error) (*pluginsdk.PluginActionResponse, error) {
	var writeErr *forgejo.WriteError
	switch {
	case errors.Is(err, forgejo.ErrNotLinked):
		return domainResponse(http.StatusNotFound, "That pull request is not linked to this task.")
	case errors.Is(err, forgejo.ErrAmbiguous):
		return domainResponse(http.StatusConflict, "This task links more than one pull request; say which one.")
	case errors.As(err, &writeErr):
		return domainResponse(writeStatus(writeErr), writeReasonMessage(writeErr.Reason))
	case errors.Is(err, forgejo.ErrUnauthorized):
		return domainResponse(http.StatusForbidden, safeMessage(err))
	case errors.Is(err, context.DeadlineExceeded):
		return domainResponse(http.StatusGatewayTimeout, "The instance took too long to answer.")
	default:
		return domainResponse(http.StatusBadGateway, safeMessage(err))
	}
}

// writeStatus keeps expected write failures in the 403/409/422 band the host
// passes through.
func writeStatus(err *forgejo.WriteError) int {
	switch err.Reason {
	case forgejo.ReasonForbidden, forgejo.ReasonBlockedByProtection:
		return http.StatusForbidden
	case forgejo.ReasonInvalid, forgejo.ReasonSelfReview:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusConflict
	}
}

// domainResponse is an expected failure: a fixed message under an explicit
// status, never provider text.
func domainResponse(status int, message string) (*pluginsdk.PluginActionResponse, error) {
	response, err := jsonResponse(map[string]string{"error": message})
	if err != nil {
		return nil, err
	}
	response.Status = status
	return response, nil
}
