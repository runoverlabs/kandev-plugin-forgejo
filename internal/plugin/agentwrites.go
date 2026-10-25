package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// agentWriteWorkspace finds the workspace an agent call belongs to. The tool
// context normally carries it; when it does not, the task says.
func (h *agentTools) agentWriteWorkspace(ctx context.Context, request *pluginsdk.AgentToolRequest) (string, error) {
	if id := strings.TrimSpace(request.Context.WorkspaceID); id != "" {
		return id, nil
	}
	host := h.hosts()
	if host == nil || strings.TrimSpace(request.Context.TaskID) == "" {
		return "", scm.ErrNotLinked
	}
	task, err := host.Tasks().Get(ctx, request.Context.TaskID)
	if err != nil || task == nil {
		return "", scm.ErrNotLinked
	}
	return task.WorkspaceID, nil
}

// prWrite runs one of the writing ops against the task's linked pull request.
// The tool has no pull request number, on purpose: it acts on the task's only
// linked pull request and refuses when there are several, the same rule `ready`
// follows.
func (h *agentTools) prWrite(ctx context.Context, request *pluginsdk.AgentToolRequest, op string) (*pluginsdk.AgentToolResult, error) {
	workspaceID, err := h.agentWriteWorkspace(ctx, request)
	if err != nil {
		return agentWriteFailure(err), nil
	}
	result, number := h.prWriteOp(ctx, request, op, workspaceID)
	outcome := "ok"
	if result.IsError {
		outcome = "refused"
	}
	h.audit.Record(ctx, auditRecord{
		Action: "agent.pr." + op, Actor: "agent:" + request.Context.SessionID, Workspace: workspaceID,
		Task: request.Context.TaskID, Number: number, Outcome: outcome,
	})
	return result, nil
}

// prWriteOp runs the op and reports the pull request number it acted on, which
// is zero when it never got that far.
func (h *agentTools) prWriteOp(ctx context.Context, request *pluginsdk.AgentToolRequest, op, workspaceID string) (*pluginsdk.AgentToolResult, int64) {
	taskID := request.Context.TaskID
	args := request.Arguments

	switch op {
	case "merge":
		return h.prMerge(ctx, request, workspaceID)
	case "review":
		event := argString(args, "event")
		result, err := h.pullActions.Review(ctx, workspaceID, taskID, sourcecontrol.ReviewRequest{
			Event: event, Body: argString(args, "body"), HeadSHA: argString(args, "sha"),
		})
		if err != nil {
			return agentWriteFailure(err), 0
		}
		return agentWriteResult(fmt.Sprintf("review submitted on #%d: %s", result.Number, result.State), result.Number), result.Number
	case "request_review":
		result, err := h.pullActions.RequestReviewers(ctx, workspaceID, taskID, sourcecontrol.ReviewersRequest{
			Add: argStrings(args, "reviewers"),
		})
		if err != nil {
			return agentWriteFailure(err), 0
		}
		return agentWriteResult(fmt.Sprintf("review requested on #%d", result.Number), result.Number), result.Number
	case "update":
		result, err := h.pullActions.UpdateBranch(ctx, workspaceID, taskID, sourcecontrol.UpdateBranchRequest{
			Style: argString(args, "style"),
		})
		if err != nil {
			return agentWriteFailure(err), 0
		}
		return agentWriteResult(fmt.Sprintf("branch of #%d is up to date with its base", result.Number), result.Number), result.Number
	default: // comment
		result, err := h.pullActions.Comment(ctx, workspaceID, taskID, sourcecontrol.CommentRequest{
			Body: argString(args, "body"),
		})
		if err != nil {
			return agentWriteFailure(err), 0
		}
		return agentWriteResult(fmt.Sprintf("commented on #%d", result.Number), result.Number), result.Number
	}
}

// prOutcome records the agent's report on the auto-fix attempt in flight.
func (h *agentTools) prOutcome(ctx context.Context, request *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	outcome := strings.ToLower(argString(request.Arguments, "outcome"))
	if !prautomation.ValidOutcome(outcome) {
		return toolError("outcome must be action_taken, non_actionable, or blocked."), nil
	}
	workspaceID, err := h.agentWriteWorkspace(ctx, request)
	if err != nil {
		return toolError("This task is not in a workspace this plugin can see."), nil
	}
	record, err := h.automation.Outcome(ctx, workspaceID, request.Context.TaskID, request.Context.SessionID,
		argString(request.Arguments, "repo"), outcome, argString(request.Arguments, "body"))
	// Audited like the writing ops: blocked holds auto-merge back, and the
	// trail says which session asked for it.
	audited := outcome
	if err != nil {
		audited = "refused"
	}
	h.audit.Record(ctx, auditRecord{
		Action: "agent.pr.outcome", Actor: "agent:" + request.Context.SessionID, Workspace: workspaceID,
		Task: request.Context.TaskID, Number: record.Number, Outcome: audited,
	})
	switch {
	case errors.Is(err, prautomation.ErrNoAttempt), errors.Is(err, prautomation.ErrAmbiguousAttempt):
		return toolError(err.Error() + "."), nil
	case err != nil:
		return nil, err
	}
	return &pluginsdk.AgentToolResult{
		Text: fmt.Sprintf("Recorded %s for %s#%d.", outcome, record.Repository, record.Number),
		StructuredContent: map[string]any{
			"outcome": outcome, "repository": record.Repository, "number": record.Number,
		},
	}, nil
}

// prMerge merges only when the operator has switched agent merging on and the
// pull request is clean. The checks rule is enforced here regardless of what
// the repository's own protection requires, so an agent cannot merge red or
// still-running CI in a repository that does not itself insist on it.
func (h *agentTools) prMerge(ctx context.Context, request *pluginsdk.AgentToolRequest, workspaceID string) (*pluginsdk.AgentToolResult, int64) {
	if !h.switches.AgentMerge(ctx, workspaceID) {
		return toolError("Agents may not merge in this workspace. An operator can allow it in Settings > Plugins > Forgejo; until then merge from the Forgejo UI."), 0
	}
	sha := argString(request.Arguments, "sha")
	if sha == "" {
		return toolError("sha is required for merge: pass the head commit from op=get."), 0
	}
	taskID := request.Context.TaskID
	details, err := h.pullActions.Details(ctx, workspaceID, taskID, 0)
	if err != nil {
		return agentWriteFailure(err), 0
	}
	if reasons := agentMergeBlockers(details); len(reasons) > 0 {
		return toolError(fmt.Sprintf("Not merged: %s.", strings.Join(reasons, "; "))), details.Number
	}
	result, err := h.pullActions.Merge(ctx, workspaceID, taskID, sourcecontrol.MergeRequest{
		HeadSHA: sha, Style: argString(request.Arguments, "style"), DeleteBranch: argBool(request.Arguments, "delete_branch"),
	})
	if err != nil {
		return agentWriteFailure(err), details.Number
	}
	return agentWriteResult(fmt.Sprintf("merged #%d (%s)", result.Number, result.Style), result.Number), result.Number
}

// agentMergeBlockers lists, in plain words, why an agent may not merge. A merge
// needs an open pull request, no known blocker, and CI that is neither failing
// nor still running; a pull request with no CI at all is allowed.
func agentMergeBlockers(details sourcecontrol.ChangeRequestDetails) []string {
	words := map[string]string{
		sourcecontrol.BlockerNotOpen:           "the pull request is not open",
		sourcecontrol.BlockerConflicts:         "it has merge conflicts",
		sourcecontrol.BlockerChecksFailing:     "required checks are failing",
		sourcecontrol.BlockerChecksPending:     "required checks are still running",
		sourcecontrol.BlockerApprovalsRequired: "it needs more approvals",
		sourcecontrol.BlockerChangesRequested:  "changes were requested",
	}
	var reasons []string
	for _, blocker := range details.Merge.Blockers {
		reasons = append(reasons, words[blocker])
	}
	switch details.PipelineState {
	case "failure":
		if !contains(details.Merge.Blockers, sourcecontrol.BlockerChecksFailing) {
			reasons = append(reasons, "checks are failing")
		}
	case "pending":
		if !contains(details.Merge.Blockers, sourcecontrol.BlockerChecksPending) {
			reasons = append(reasons, "checks are still running")
		}
	}
	return reasons
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func agentWriteResult(text string, number int64) *pluginsdk.AgentToolResult {
	return &pluginsdk.AgentToolResult{Text: text, StructuredContent: map[string]any{"number": number, "result": text}}
}

// agentWriteFailure turns an adapter error into a sentence an agent can act on.
func agentWriteFailure(err error) *pluginsdk.AgentToolResult {
	var invalidErr *scm.InvalidError
	var writeErr *scm.WriteError
	switch {
	case errors.Is(err, scm.ErrNotLinked):
		return toolError("No pull request is linked to this task. Open one first.")
	case errors.Is(err, scm.ErrAmbiguous):
		return toolError("This task has more than one pull request; do this from the Forgejo UI.")
	case errors.As(err, &invalidErr):
		return toolError(invalidErr.Message)
	case errors.As(err, &writeErr):
		return toolError(writeReasonMessage(writeErr.Reason))
	default:
		return toolError(safeMessage(err))
	}
}

// argStrings reads a list of strings argument.
func argStrings(arguments map[string]any, key string) []string {
	raw, _ := arguments[key].([]any)
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			values = append(values, strings.TrimSpace(text))
		}
	}
	return values
}
