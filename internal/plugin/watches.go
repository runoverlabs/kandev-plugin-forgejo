package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"kandev-plugin-forgejo/internal/watches"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// The watch actions. Kandev verifies the caller's workspace before the handler
// runs and passes it in VerifiedActionContext; the workspace is never read
// from the request body.
//
// Pause and resume are deliberately absent: they are watches.update with a
// different `enabled`, and a separate action would be a second way to write
// the same field.
const (
	// ActionWatchesList returns every watch in the workspace.
	ActionWatchesList = "watches.list"
	// ActionWatchesCreate creates one watch.
	ActionWatchesCreate = "watches.create"
	// ActionWatchesUpdate replaces one watch's configuration.
	ActionWatchesUpdate = "watches.update"
	// ActionWatchesDelete removes one watch and its dedup ledger.
	ActionWatchesDelete = "watches.delete"
	// ActionWatchesRun polls one watch immediately, ignoring its interval.
	ActionWatchesRun = "watches.run"
	// ActionWatchesReset forgets which issues a watch already handled, so the
	// next run re-creates their tasks.
	ActionWatchesReset = "watches.reset"
	// ActionWatchesCleanup retires the tasks of one review watch's finished
	// pull requests now, instead of at the next poll.
	ActionWatchesCleanup = "watches.cleanup"
	// ActionWatchesOptions returns the workflows, steps and profiles the
	// configuration form offers.
	ActionWatchesOptions = "watches.options"
)

// watchRequest is the shared body shape. Every field is optional at the
// transport level and checked per action, so one decode serves all of them.
//
// The clearable fields are pointers, and so are Labels and Repos by being nil
// when absent: an operator must be able to clear a prompt or detach a
// repository by sending an empty value, and a partial update such as the
// pause toggle's `{id, enabled}` must leave everything else alone. A plain
// string cannot tell those two apart, which is how pausing a watch once erased
// its prompt, profiles, repository, query and labels.
type watchRequest struct {
	ID                  string                `json:"id"`
	Name                string                `json:"name"`
	WorkflowID          string                `json:"workflow_id"`
	WorkflowStepID      string                `json:"workflow_step_id"`
	AgentProfileID      *string               `json:"agent_profile_id"`
	ExecutorProfileID   *string               `json:"executor_profile_id"`
	Prompt              *string               `json:"prompt"`
	StartAgent          *bool                 `json:"start_agent"`
	RepositoryID        *string               `json:"repository_id"`
	BaseBranch          *string               `json:"base_branch"`
	Repos               []repoInput           `json:"repos"`
	Labels              []string              `json:"labels"`
	State               string                `json:"state"`
	Query               *string               `json:"query"`
	Kind                watches.Kind          `json:"kind"`
	ReviewScope         watches.ReviewScope   `json:"review_scope"`
	IncludeDrafts       *bool                 `json:"include_drafts"`
	CleanupPolicy       watches.CleanupPolicy `json:"cleanup_policy"`
	ForkWorkflowStepID  *string               `json:"fork_workflow_step_id"`
	Enabled             *bool                 `json:"enabled"`
	PollIntervalSeconds int                   `json:"poll_interval_seconds"`
	MaxInflightTasks    int                   `json:"max_inflight_tasks"`
	DedupScope          watches.DedupScope    `json:"dedup_scope"`
}

// watchRefusal is a failure the operator can act on: a bad setting, a missing
// watch, a paused one. It is answered with its own HTTP status and its text,
// because the host replaces any Go error with "plugin action unavailable", which
// would leave the operator guessing which field to change.
type watchRefusal struct {
	status  int
	message string
}

func (e watchRefusal) Error() string { return "kandev-plugin-forgejo: " + e.message }

func refuse(status int, message string) error { return watchRefusal{status: status, message: message} }

// repoInput is one repository in a request: "owner/name", or the
// {"owner","name"} object watches.list returns, so a watch read back can be
// written back unchanged.
type repoInput string

func (r *repoInput) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*r = repoInput(text)
		return nil
	}
	var object struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	*r = repoInput(object.Owner + "/" + object.Name)
	return nil
}

// handleWatchAction routes the watch actions, turning an operator-actionable
// failure into a response with its own status.
func (r *Runtime) handleWatchAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	response, err := r.routeWatchAction(ctx, request)
	var refusal watchRefusal
	var invalid watches.ValidationError
	switch {
	case err == nil:
		return response, nil
	case errors.As(err, &refusal):
		return domainResponse(refusal.status, refusal.Error())
	case errors.As(err, &invalid):
		return domainResponse(http.StatusUnprocessableEntity, invalid.Error())
	case errors.Is(err, watches.ErrNotFound):
		return domainResponse(http.StatusNotFound, "kandev-plugin-forgejo: no such watch in this workspace")
	case errors.Is(err, watches.ErrCleanupOff):
		return domainResponse(http.StatusConflict, "kandev-plugin-forgejo: cleanup is off for this watch")
	default:
		return nil, err
	}
}

func (r *Runtime) routeWatchAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return nil, errors.New("kandev-plugin-forgejo: watches require a verified workspace")
	}
	var body watchRequest
	if len(request.Body) > 0 {
		if err := json.Unmarshal(request.Body, &body); err != nil {
			// Say which field, not the whole decoder error: it can echo input.
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) && typeErr.Field != "" {
				return domainResponse(http.StatusUnprocessableEntity,
					"kandev-plugin-forgejo: the field "+typeErr.Field+" has the wrong type")
			}
			return domainResponse(http.StatusUnprocessableEntity, "kandev-plugin-forgejo: the request body is not valid JSON")
		}
	}

	switch request.ActionKey {
	case ActionWatchesList:
		return r.listWatches(ctx, workspaceID, body.Kind)
	case ActionWatchesOptions:
		return r.watchOptions(ctx, workspaceID)
	case ActionWatchesCreate:
		return r.createWatch(ctx, workspaceID, body)
	case ActionWatchesUpdate:
		return r.updateWatch(ctx, workspaceID, body)
	case ActionWatchesDelete:
		return r.deleteWatch(ctx, workspaceID, body)
	case ActionWatchesRun:
		return r.runWatch(ctx, workspaceID, body)
	case ActionWatchesReset:
		return r.resetWatch(ctx, workspaceID, body)
	case ActionWatchesCleanup:
		return r.cleanupWatch(ctx, workspaceID, body)
	default:
		return nil, fmt.Errorf("kandev-plugin-forgejo: unknown watch action %q", request.ActionKey)
	}
}

func (r *Runtime) listWatches(ctx context.Context, workspaceID string, kind watches.Kind) (*pluginsdk.PluginActionResponse, error) {
	all, err := r.watchStore.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// The kind filter is optional so a caller that predates review watches, and
	// asks for none, still gets every watch.
	found := make([]watches.Watch, 0, len(all))
	for _, watch := range all {
		if kind == "" || watch.IsReview() == (kind == watches.KindReview) {
			found = append(found, watch)
		}
	}
	return jsonResponse(map[string]any{"watches": found})
}

func (r *Runtime) createWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	watch, err := watchFromRequest(workspaceID, watches.Watch{Enabled: true}, body)
	if err != nil {
		return nil, err
	}
	if err := r.checkForkStep(ctx, watch); err != nil {
		return nil, err
	}
	id, err := watches.NewID()
	if err != nil {
		return nil, err
	}
	watch.ID = id
	saved, err := r.watchStore.Put(ctx, watch)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"watch": saved})
}

func (r *Runtime) updateWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	existing, err := r.requireWatch(ctx, workspaceID, body.ID)
	if err != nil {
		return nil, err
	}
	watch, err := watchFromRequest(workspaceID, existing, body)
	if err != nil {
		return nil, err
	}
	if err := r.checkForkStep(ctx, watch); err != nil {
		return nil, err
	}
	saved, err := r.watchStore.Put(ctx, watch)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"watch": saved})
}

func (r *Runtime) deleteWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	if _, err := r.requireWatch(ctx, workspaceID, body.ID); err != nil {
		return nil, err
	}
	if err := r.watchStore.Delete(ctx, workspaceID, body.ID); err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"deleted": body.ID})
}

func (r *Runtime) runWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	watch, err := r.requireWatch(ctx, workspaceID, body.ID)
	if err != nil {
		return nil, err
	}
	// A manual run ignores the interval but not the enable switches: running a
	// disabled watch by hand would contradict both toggles at once.
	enabled, err := r.integrationEnabled(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, refuse(http.StatusConflict, "the Forgejo integration is turned off for this workspace")
	}
	if !watch.Enabled {
		return nil, refuse(http.StatusConflict, "this watch is paused")
	}
	result, err := r.watchPoller.RunWatch(ctx, watch)
	if err != nil {
		return nil, err
	}
	// The watch is re-read so the response carries the poll timestamps and
	// error the run just wrote.
	refreshed, err := r.watchStore.Get(ctx, workspaceID, watch.ID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"result": result, "watch": refreshed})
}

func (r *Runtime) resetWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	watch, err := r.requireWatch(ctx, workspaceID, body.ID)
	if err != nil {
		return nil, err
	}
	forgotten, err := r.watchStore.Forget(ctx, watch)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"forgotten": forgotten, "watch_id": watch.ID})
}

func (r *Runtime) cleanupWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	watch, err := r.requireWatch(ctx, workspaceID, body.ID)
	if err != nil {
		return nil, err
	}
	enabled, err := r.integrationEnabled(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, refuse(http.StatusConflict, "the Forgejo integration is turned off for this workspace")
	}
	result, err := r.watchPoller.Cleanup(ctx, watch)
	if err != nil {
		return nil, err
	}
	refreshed, err := r.watchStore.Get(ctx, workspaceID, watch.ID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"result": result, "watch": refreshed})
}

// checkForkStep refuses a fork step that would start an agent on entry. It is
// the save-time half of the fork protection; the poller checks again on every
// run, because a step can be edited after the watch is saved.
func (r *Runtime) checkForkStep(ctx context.Context, watch watches.Watch) error {
	if !watch.IsReview() || strings.TrimSpace(watch.ForkWorkflowStepID) == "" {
		return nil
	}
	host := r.Host()
	if host == nil {
		return errors.New("kandev-plugin-forgejo: host is unavailable")
	}
	steps, err := host.Workflows().ListSteps(ctx, watch.WorkflowID)
	if err != nil {
		return fmt.Errorf("kandev-plugin-forgejo: list workflow steps: %w", err)
	}
	for _, step := range steps {
		if step.ID != watch.ForkWorkflowStepID {
			continue
		}
		if startsAgent(step) {
			return refuse(http.StatusUnprocessableEntity, "the fork step starts an agent when a task enters it; choose a step that does not")
		}
		return nil
	}
	return refuse(http.StatusUnprocessableEntity, "the fork step is not in the watch's workflow")
}

// startsAgent reports whether entering a step launches an agent.
func startsAgent(step pluginsdk.WorkflowStep) bool {
	return watches.StartsAgent(step.OnEnterActionTypes)
}

// watchOptions serves the configuration form's pickers. Without these the
// operator has no way to name a workflow step or a profile, and a watch has
// nowhere to put the task it creates.
func (r *Runtime) watchOptions(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	host := r.Host()
	if host == nil {
		return nil, errors.New("kandev-plugin-forgejo: host is unavailable")
	}
	page := pluginsdk.Page{Limit: 200}

	workflows, _, err := host.Workflows().List(ctx, workspaceID, page)
	if err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: list workflows: %w", err)
	}
	rendered := make([]map[string]any, 0, len(workflows))
	for _, workflow := range workflows {
		steps, err := host.Workflows().ListSteps(ctx, workflow.ID)
		if err != nil {
			return nil, fmt.Errorf("kandev-plugin-forgejo: list workflow steps: %w", err)
		}
		renderedSteps := make([]map[string]any, 0, len(steps))
		for _, step := range steps {
			renderedSteps = append(renderedSteps, map[string]any{
				"id": step.ID, "name": step.Name, "position": step.Position,
				"is_start_step": step.IsStartStep,
				// The fork picker offers only steps that do not start an agent.
				"auto_starts_agent": startsAgent(step),
			})
		}
		rendered = append(rendered, map[string]any{
			"id": workflow.ID, "name": workflow.Name, "steps": renderedSteps,
		})
	}

	profiles, _, err := host.AgentProfiles().List(ctx, page)
	if err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: list agent profiles: %w", err)
	}
	renderedProfiles := make([]map[string]any, 0, len(profiles))
	for _, profile := range profiles {
		renderedProfiles = append(renderedProfiles, map[string]any{
			"id": profile.ID, "name": profile.DisplayName, "model": profile.Model,
		})
	}

	// Executor profiles are an optional Host extension; a host without them
	// still serves everything else, so their absence is an empty list rather
	// than an error.
	renderedExecutors := make([]map[string]any, 0)
	if reader, ok := pluginsdk.ExecutorProfiles(host); ok {
		executors, _, err := reader.List(ctx, page)
		if err == nil {
			for _, executor := range executors {
				renderedExecutors = append(renderedExecutors, map[string]any{
					"id": executor.ID, "name": executor.DisplayName, "type": executor.ExecutorType,
				})
			}
		}
	}

	return jsonResponse(map[string]any{
		"workflows":            rendered,
		"agent_profiles":       renderedProfiles,
		"executor_profiles":    renderedExecutors,
		"default_interval":     int(watches.DefaultPollInterval.Seconds()),
		"min_interval":         int(watches.MinPollInterval.Seconds()),
		"default_max_inflight": watches.DefaultMaxInflightTasks,
		// Review watches.
		"min_review_interval":   int(watches.MinReviewPollInterval.Seconds()),
		"default_review_prompt": watches.DefaultReviewPrompt,
		"archive_granted":       watches.ArchiveGranted(ctx, host, workspaceID),
	})
}

// requireWatch loads a watch, scoped to the verified workspace. Reading it
// through the workspace the caller was verified for is what keeps one
// workspace's watches — and the filters, repository ids and spawn prompts they
// carry — out of another's responses.
func (r *Runtime) requireWatch(ctx context.Context, workspaceID, id string) (watches.Watch, error) {
	if strings.TrimSpace(id) == "" {
		return watches.Watch{}, refuse(http.StatusUnprocessableEntity, "a watch id is required")
	}
	return r.watchStore.Get(ctx, workspaceID, id)
}

// watchFromRequest folds a request body onto a base watch. Create passes a
// zero value with Enabled true; update passes the stored record, so a field
// the form omits keeps its stored value rather than reverting to a default.
func watchFromRequest(workspaceID string, base watches.Watch, body watchRequest) (watches.Watch, error) {
	watch := base
	watch.WorkspaceID = workspaceID

	// A watch's kind is fixed when it is created. Changing it would reinterpret
	// the filters and the ledger it already has.
	if base.ID == "" {
		watch.Kind = body.Kind
	} else if body.Kind != "" && body.Kind != watches.Kind(effectiveKind(base)) {
		return watches.Watch{}, refuse(http.StatusUnprocessableEntity, "a watch's kind cannot be changed")
	}
	if body.ReviewScope != "" {
		watch.ReviewScope = body.ReviewScope
	}
	if body.CleanupPolicy != "" {
		watch.CleanupPolicy = body.CleanupPolicy
	}
	assignPtr(&watch.IncludeDrafts, body.IncludeDrafts)
	assignPtr(&watch.ForkWorkflowStepID, body.ForkWorkflowStepID)

	assignString(&watch.Name, body.Name)
	assignString(&watch.WorkflowID, body.WorkflowID)
	assignString(&watch.WorkflowStepID, body.WorkflowStepID)
	assignString(&watch.State, body.State)

	// These are clearable, so an empty value is taken as written, but only when
	// the body carries the field at all. An absent field keeps the stored value.
	assignPtr(&watch.AgentProfileID, body.AgentProfileID)
	assignPtr(&watch.ExecutorProfileID, body.ExecutorProfileID)
	assignPtr(&watch.Prompt, body.Prompt)
	assignPtr(&watch.RepositoryID, body.RepositoryID)
	assignPtr(&watch.BaseBranch, body.BaseBranch)
	assignPtr(&watch.Query, body.Query)
	assignPtr(&watch.StartAgent, body.StartAgent)
	if body.Labels != nil {
		watch.Labels = body.Labels
	}

	if body.Enabled != nil {
		watch.Enabled = *body.Enabled
	}
	if body.PollIntervalSeconds > 0 {
		watch.PollIntervalSeconds = body.PollIntervalSeconds
	}
	if body.MaxInflightTasks > 0 {
		watch.MaxInflightTasks = body.MaxInflightTasks
	}
	if body.DedupScope != "" {
		watch.DedupScope = body.DedupScope
	}

	if body.Repos != nil {
		repos := make([]watches.RepoRef, 0, len(body.Repos))
		for _, raw := range body.Repos {
			if strings.TrimSpace(string(raw)) == "" {
				continue
			}
			ref, err := watches.ParseRepoRef(string(raw))
			if err != nil {
				return watches.Watch{}, err
			}
			repos = append(repos, ref)
		}
		watch.Repos = repos
	}
	return watch, nil
}

func effectiveKind(watch watches.Watch) string {
	if watch.IsReview() {
		return string(watches.KindReview)
	}
	return string(watches.KindIssue)
}

// assignPtr writes *value onto target when the body carried the field, empty
// or not. A nil pointer means the field was absent, and the stored value stays.
func assignPtr[T any](target *T, value *T) {
	if value != nil {
		*target = *value
	}
}

// assignString writes value onto target when value is non-empty, so an omitted
// form field leaves the stored value alone.
func assignString(target *string, value string) {
	if strings.TrimSpace(value) != "" {
		*target = value
	}
}
