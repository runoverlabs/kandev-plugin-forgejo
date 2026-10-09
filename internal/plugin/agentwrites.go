package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// agentMergeStateKey stores the per-workspace switch that lets agents merge.
// Absent means off: an agent that can merge to the default branch is the one
// capability here whose default has to be "no". Review, comment and the other
// writes stay available, because they can be undone or ignored.
const agentMergeStateKey = "agent_merge_enabled"

// agentMergeEnabled reports whether agents may merge in a workspace. Unlike
// integrationEnabled it fails closed: if the state cannot be read, merging does
// not happen.
func (r *Runtime) agentMergeEnabled(ctx context.Context, workspaceID string) bool {
	host := r.Host()
	if host == nil || strings.TrimSpace(workspaceID) == "" {
		return false
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, agentMergeStateKey)
	if err != nil || !found || value == nil {
		return false
	}
	enabled, _ := value["enabled"].(bool)
	return enabled
}

// setAgentMerge records the operator's choice. It is a workspace-scoped action,
// so the workspace is the verified one and never comes from the body.
func (r *Runtime) setAgentMerge(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return nil, errors.New("kandev-plugin-forgejo: this setting requires a verified workspace")
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if bad := decodeActionBody(request, &input); bad != nil || input.Enabled == nil {
		return domainResponse(422, "enabled is required.")
	}
	host := r.Host()
	if host == nil {
		return nil, errors.New("kandev-plugin-forgejo: host is unavailable")
	}
	if err := host.SetState(ctx, "workspace", workspaceID, agentMergeStateKey, map[string]any{"enabled": *input.Enabled}); err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: store agent merge setting: %w", err)
	}
	audit(request, 0, fmt.Sprintf("agent_merge=%t", *input.Enabled))
	return jsonResponse(map[string]any{"agent_merge": *input.Enabled})
}

// agentWriteWorkspace finds the workspace an agent call belongs to. The tool
// context normally carries it; when it does not, the task says.
func (r *Runtime) agentWriteWorkspace(ctx context.Context, request *pluginsdk.AgentToolRequest) (string, error) {
	if id := strings.TrimSpace(request.Context.WorkspaceID); id != "" {
		return id, nil
	}
	host := r.Host()
	if host == nil || strings.TrimSpace(request.Context.TaskID) == "" {
		return "", forgejo.ErrNotLinked
	}
	task, err := host.Tasks().Get(ctx, request.Context.TaskID)
	if err != nil || task == nil {
		return "", forgejo.ErrNotLinked
	}
	return task.WorkspaceID, nil
}

// prWrite runs one of the writing ops against the task's linked pull request.
// The tool has no pull request number, on purpose: it acts on the task's only
// linked pull request and refuses when there are several, the same rule `ready`
// follows.
func (r *Runtime) prWrite(ctx context.Context, request *pluginsdk.AgentToolRequest, op string) (*pluginsdk.AgentToolResult, error) {
	workspaceID, err := r.agentWriteWorkspace(ctx, request)
	if err != nil {
		return agentWriteFailure(err), nil
	}
	taskID := request.Context.TaskID
	args := request.Arguments

	switch op {
	case "merge":
		return r.prMerge(ctx, request, workspaceID)
	case "review":
		event := argString(args, "event")
		result, err := r.pullActions.Review(ctx, workspaceID, taskID, sourcecontrol.ReviewRequest{
			Event: event, Body: argString(args, "body"), HeadSHA: argString(args, "sha"),
		})
		if err != nil {
			return agentWriteFailure(err), nil
		}
		return agentWriteResult(fmt.Sprintf("review submitted on #%d: %s", result.Number, result.State), result.Number), nil
	case "request_review":
		result, err := r.pullActions.RequestReviewers(ctx, workspaceID, taskID, sourcecontrol.ReviewersRequest{
			Add: argStrings(args, "reviewers"),
		})
		if err != nil {
			return agentWriteFailure(err), nil
		}
		return agentWriteResult(fmt.Sprintf("review requested on #%d", result.Number), result.Number), nil
	case "update":
		result, err := r.pullActions.UpdateBranch(ctx, workspaceID, taskID, sourcecontrol.UpdateBranchRequest{
			Style: argString(args, "style"),
		})
		if err != nil {
			return agentWriteFailure(err), nil
		}
		return agentWriteResult(fmt.Sprintf("branch of #%d is up to date with its base", result.Number), result.Number), nil
	default: // comment
		result, err := r.pullActions.Comment(ctx, workspaceID, taskID, sourcecontrol.CommentRequest{
			Body: argString(args, "body"),
		})
		if err != nil {
			return agentWriteFailure(err), nil
		}
		return agentWriteResult(fmt.Sprintf("commented on #%d", result.Number), result.Number), nil
	}
}

// prMerge merges only when the operator has switched agent merging on and the
// pull request is clean. The checks rule is enforced here regardless of what
// the repository's own protection requires, so an agent cannot merge red or
// still-running CI in a repository that does not itself insist on it.
func (r *Runtime) prMerge(ctx context.Context, request *pluginsdk.AgentToolRequest, workspaceID string) (*pluginsdk.AgentToolResult, error) {
	if !r.agentMergeEnabled(ctx, workspaceID) {
		return toolError("Agents may not merge in this workspace. An operator can allow it in Settings > Plugins > Forgejo; until then merge from the Forgejo UI."), nil
	}
	sha := argString(request.Arguments, "sha")
	if sha == "" {
		return toolError("sha is required for merge: pass the head commit from op=get."), nil
	}
	taskID := request.Context.TaskID
	details, err := r.pullActions.Details(ctx, workspaceID, taskID, 0)
	if err != nil {
		return agentWriteFailure(err), nil
	}
	if reasons := agentMergeBlockers(details); len(reasons) > 0 {
		audit(&pluginsdk.PluginActionRequest{ActionKey: "agent.pr.merge", Context: pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID, TaskID: taskID}}, details.Number, "blocked")
		return toolError(fmt.Sprintf("Not merged: %s.", strings.Join(reasons, "; "))), nil
	}
	result, err := r.pullActions.Merge(ctx, workspaceID, taskID, sourcecontrol.MergeRequest{
		HeadSHA: sha, Style: argString(request.Arguments, "style"), DeleteBranch: argBool(request.Arguments, "delete_branch"),
	})
	audit(&pluginsdk.PluginActionRequest{ActionKey: "agent.pr.merge", Context: pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID, TaskID: taskID}}, details.Number, outcomeOf(err))
	if err != nil {
		return agentWriteFailure(err), nil
	}
	return agentWriteResult(fmt.Sprintf("merged #%d (%s)", result.Number, result.Style), result.Number), nil
}

func outcomeOf(err error) string {
	if err != nil {
		return "refused"
	}
	return "ok"
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
	var invalidErr *forgejo.InvalidError
	var writeErr *forgejo.WriteError
	switch {
	case errors.Is(err, forgejo.ErrNotLinked):
		return toolError("No pull request is linked to this task. Open one first.")
	case errors.Is(err, forgejo.ErrAmbiguous):
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
