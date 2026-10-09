package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
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
	case sourcecontrol.ActionChangeRequestsMerge:
		return r.changeRequestMerge(ctx, request)
	case sourcecontrol.ActionChangeRequestsReview:
		return r.changeRequestReview(ctx, request)
	case sourcecontrol.ActionChangeRequestsRequestReviewers:
		return r.changeRequestReviewers(ctx, request)
	case sourcecontrol.ActionChangeRequestsUpdateBranch:
		return r.changeRequestUpdateBranch(ctx, request)
	case sourcecontrol.ActionChangeRequestsComment:
		return r.changeRequestComment(ctx, request)
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

// writeBudget bounds a write. A merge may wait out a "still computing" answer.
const writeBudget = 12 * time.Second

// decodeActionBody reads an action body, answering 422 itself when it is not valid.
func decodeActionBody(request *pluginsdk.PluginActionRequest, into any) *pluginsdk.PluginActionResponse {
	if len(request.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(request.Body, into); err != nil {
		response, _ := domainResponse(http.StatusUnprocessableEntity, "The request body is not valid.")
		return response
	}
	return nil
}

// audit records who did what, by action and number only: never a body, a title
// or a token. Every write acts as the operator's shared Forgejo account, so
// this line is the only place the Kandev user is tied to it.
func audit(request *pluginsdk.PluginActionRequest, number int64, outcome string) {
	log.Printf("kandev-plugin-forgejo: action=%s actor=%s workspace=%s task=%s number=%d outcome=%s",
		request.ActionKey, request.Context.ActorID, request.Context.WorkspaceID, request.Context.TaskID, number, outcome)
}

// finish turns a port result or error into the reply, auditing either way.
func finish[T any](request *pluginsdk.PluginActionRequest, number int64, result T, err error) (*pluginsdk.PluginActionResponse, error) {
	if err != nil {
		audit(request, number, "refused")
		return pullFailure(err)
	}
	audit(request, number, "ok")
	return jsonResponse(result)
}

func (r *Runtime) changeRequestMerge(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number            int64  `json:"number"`
		HeadSHA           string `json:"head_sha"`
		Style             string `json:"style"`
		DeleteBranch      bool   `json:"delete_branch"`
		WhenChecksSucceed bool   `json:"when_checks_succeed"`
		CancelScheduled   bool   `json:"cancel_scheduled"`
	}
	if bad := decodeActionBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.Merge(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.MergeRequest{
		Number: body.Number, HeadSHA: body.HeadSHA, Style: body.Style, DeleteBranch: body.DeleteBranch,
		WhenChecksSucceed: body.WhenChecksSucceed, CancelScheduled: body.CancelScheduled,
	})
	return finish(request, body.Number, result, err)
}

func (r *Runtime) changeRequestReview(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number   int64                         `json:"number"`
		Event    string                        `json:"event"`
		Body     string                        `json:"body"`
		HeadSHA  string                        `json:"head_sha"`
		Comments []sourcecontrol.InlineComment `json:"comments"`
	}
	if bad := decodeActionBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.Review(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.ReviewRequest{
		Number: body.Number, Event: body.Event, Body: body.Body, HeadSHA: body.HeadSHA, Comments: body.Comments,
	})
	return finish(request, body.Number, result, err)
}

func (r *Runtime) changeRequestReviewers(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64    `json:"number"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if bad := decodeActionBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.RequestReviewers(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.ReviewersRequest{
		Number: body.Number, Add: body.Add, Remove: body.Remove,
	})
	return finish(request, body.Number, result, err)
}

func (r *Runtime) changeRequestUpdateBranch(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64  `json:"number"`
		Style  string `json:"style"`
	}
	if bad := decodeActionBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.UpdateBranch(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.UpdateBranchRequest{
		Number: body.Number, Style: body.Style,
	})
	return finish(request, body.Number, result, err)
}

func (r *Runtime) changeRequestComment(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64  `json:"number"`
		Body   string `json:"body"`
	}
	if bad := decodeActionBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.Comment(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.CommentRequest{
		Number: body.Number, Body: body.Body,
	})
	return finish(request, body.Number, result, err)
}

// pullFailure turns an adapter error into an explicit domain status with a
// fixed message. The host masks a plain Go error as an internal error, which
// would hide a failure the user can act on.
func pullFailure(err error) (*pluginsdk.PluginActionResponse, error) {
	var writeErr *forgejo.WriteError
	var invalidErr *forgejo.InvalidError
	switch {
	case errors.Is(err, forgejo.ErrNotLinked):
		return domainResponse(http.StatusNotFound, "That pull request is not linked to this task.")
	case errors.As(err, &invalidErr):
		return domainResponse(http.StatusUnprocessableEntity, invalidErr.Message)
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
