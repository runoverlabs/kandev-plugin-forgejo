package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/scm"
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
		return kandev.Refuse(http.StatusConflict, "The Forgejo integration is turned off for this workspace.")
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
			return kandev.Refuse(http.StatusUnprocessableEntity, "The request body is not valid.")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, detailsBudget)
	defer cancel()
	details, err := r.pullActions.Details(ctx, request.Context.WorkspaceID, request.Context.TaskID, body.Number)
	if err != nil {
		return pullFailure(err)
	}
	return kandev.JSON(details)
}

// writeBudget bounds a write. A merge may wait out a "still computing" answer.
const writeBudget = 12 * time.Second

// finish turns a port result or error into the reply, auditing either way.
func finish[T any](ctx context.Context, r *Runtime, request *pluginsdk.PluginActionRequest, number int64, result T, err error) (*pluginsdk.PluginActionResponse, error) {
	outcome := "ok"
	if err != nil {
		outcome = "refused"
	}
	r.audit(ctx, auditRecord{
		Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: request.Context.WorkspaceID,
		Task: request.Context.TaskID, Number: number, Outcome: outcome,
	})
	if err != nil {
		return pullFailure(err)
	}
	return kandev.JSON(result)
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
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.Merge(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.MergeRequest{
		Number: body.Number, HeadSHA: body.HeadSHA, Style: body.Style, DeleteBranch: body.DeleteBranch,
		WhenChecksSucceed: body.WhenChecksSucceed, CancelScheduled: body.CancelScheduled,
	})
	return finish(ctx, r, request, body.Number, result, err)
}

func (r *Runtime) changeRequestReview(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number   int64                         `json:"number"`
		Event    string                        `json:"event"`
		Body     string                        `json:"body"`
		HeadSHA  string                        `json:"head_sha"`
		Comments []sourcecontrol.InlineComment `json:"comments"`
	}
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.Review(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.ReviewRequest{
		Number: body.Number, Event: body.Event, Body: body.Body, HeadSHA: body.HeadSHA, Comments: body.Comments,
	})
	return finish(ctx, r, request, body.Number, result, err)
}

func (r *Runtime) changeRequestReviewers(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64    `json:"number"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.RequestReviewers(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.ReviewersRequest{
		Number: body.Number, Add: body.Add, Remove: body.Remove,
	})
	return finish(ctx, r, request, body.Number, result, err)
}

func (r *Runtime) changeRequestUpdateBranch(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64  `json:"number"`
		Style  string `json:"style"`
	}
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.UpdateBranch(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.UpdateBranchRequest{
		Number: body.Number, Style: body.Style,
	})
	return finish(ctx, r, request, body.Number, result, err)
}

func (r *Runtime) changeRequestComment(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64  `json:"number"`
		Body   string `json:"body"`
	}
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := r.pullActions.Comment(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.CommentRequest{
		Number: body.Number, Body: body.Body,
	})
	return finish(ctx, r, request, body.Number, result, err)
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
