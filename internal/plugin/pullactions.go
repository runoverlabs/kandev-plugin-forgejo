package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// detailsBudget keeps the composite read inside the host's 15-second action
// timeout, with room to write the reply.
const detailsBudget = 12 * time.Second

// pullRequestActions serve the change_requests.* actions this plugin owns
// beyond the recipe's: the review panel's details and its writes.
type pullRequestActions struct {
	pullActions linkedPullRequests
	switches    workspaceSwitches
	audit       *auditTrail
}

// handlePullAction routes the pull request actions this plugin owns beyond the
// recipe's. The workspace enable switch is checked here for the same reason the
// default dispatch branch does: a disabled integration contributes nothing.
func (h *pullRequestActions) handlePullAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	enabled, err := h.switches.Enabled(ctx, request.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return kandev.Refuse(http.StatusConflict, "The Forgejo integration is turned off for this workspace.")
	}
	switch request.ActionKey {
	case sourcecontrol.ActionChangeRequestsDetails:
		return h.changeRequestDetails(ctx, request)
	case sourcecontrol.ActionChangeRequestsMerge:
		return h.changeRequestMerge(ctx, request)
	case sourcecontrol.ActionChangeRequestsReview:
		return h.changeRequestReview(ctx, request)
	case sourcecontrol.ActionChangeRequestsRequestReviewers:
		return h.changeRequestReviewers(ctx, request)
	case sourcecontrol.ActionChangeRequestsUpdateBranch:
		return h.changeRequestUpdateBranch(ctx, request)
	case sourcecontrol.ActionChangeRequestsComment:
		return h.changeRequestComment(ctx, request)
	default:
		return nil, errors.New("kandev-plugin-forgejo: unsupported pull request action")
	}
}

// changeRequestDetails returns the full panel model for a pull request linked
// to the verified task. The body may name which one when the task links
// several; a number the task does not own is refused, whatever it points at.
func (h *pullRequestActions) changeRequestDetails(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
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
	details, err := h.pullActions.Details(ctx, request.Context.WorkspaceID, request.Context.TaskID, body.Number)
	if err != nil {
		return pullFailure(err)
	}
	return kandev.JSON(details)
}

// writeBudget bounds a write. A merge may wait out a "still computing" answer.
const writeBudget = 12 * time.Second

// finish turns a port result or error into the reply, auditing either way.
func finish[T any](ctx context.Context, h *pullRequestActions, request *pluginsdk.PluginActionRequest, number int64, result T, err error) (*pluginsdk.PluginActionResponse, error) {
	outcome := "ok"
	if err != nil {
		outcome = "refused"
	}
	h.audit.Record(ctx, auditRecord{
		Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: request.Context.WorkspaceID,
		Task: request.Context.TaskID, Number: number, Outcome: outcome,
	})
	if err != nil {
		return pullFailure(err)
	}
	return kandev.JSON(result)
}

func (h *pullRequestActions) changeRequestMerge(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
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
	result, err := h.pullActions.Merge(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.MergeRequest{
		Number: body.Number, HeadSHA: body.HeadSHA, Style: body.Style, DeleteBranch: body.DeleteBranch,
		WhenChecksSucceed: body.WhenChecksSucceed, CancelScheduled: body.CancelScheduled,
	})
	return finish(ctx, h, request, body.Number, result, err)
}

func (h *pullRequestActions) changeRequestReview(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
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
	result, err := h.pullActions.Review(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.ReviewRequest{
		Number: body.Number, Event: body.Event, Body: body.Body, HeadSHA: body.HeadSHA, Comments: body.Comments,
	})
	return finish(ctx, h, request, body.Number, result, err)
}

func (h *pullRequestActions) changeRequestReviewers(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
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
	result, err := h.pullActions.RequestReviewers(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.ReviewersRequest{
		Number: body.Number, Add: body.Add, Remove: body.Remove,
	})
	return finish(ctx, h, request, body.Number, result, err)
}

func (h *pullRequestActions) changeRequestUpdateBranch(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64  `json:"number"`
		Style  string `json:"style"`
	}
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := h.pullActions.UpdateBranch(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.UpdateBranchRequest{
		Number: body.Number, Style: body.Style,
	})
	return finish(ctx, h, request, body.Number, result, err)
}

func (h *pullRequestActions) changeRequestComment(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		Number int64  `json:"number"`
		Body   string `json:"body"`
	}
	if bad := kandev.DecodeBody(request, &body); bad != nil {
		return bad, nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result, err := h.pullActions.Comment(ctx, request.Context.WorkspaceID, request.Context.TaskID, sourcecontrol.CommentRequest{
		Number: body.Number, Body: body.Body,
	})
	return finish(ctx, h, request, body.Number, result, err)
}
