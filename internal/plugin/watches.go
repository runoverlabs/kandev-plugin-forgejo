package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// ActionWatchesOptions returns the workflows, steps and profiles the
	// configuration form offers.
	ActionWatchesOptions = "watches.options"
)

// watchRequest is the shared body shape. Every field is optional at the
// transport level and checked per action, so one decode serves all of them.
type watchRequest struct {
	ID                  string             `json:"id"`
	Name                string             `json:"name"`
	WorkflowID          string             `json:"workflow_id"`
	WorkflowStepID      string             `json:"workflow_step_id"`
	AgentProfileID      string             `json:"agent_profile_id"`
	ExecutorProfileID   string             `json:"executor_profile_id"`
	Prompt              string             `json:"prompt"`
	StartAgent          bool               `json:"start_agent"`
	RepositoryID        string             `json:"repository_id"`
	BaseBranch          string             `json:"base_branch"`
	Repos               []string           `json:"repos"`
	Labels              []string           `json:"labels"`
	State               string             `json:"state"`
	Query               string             `json:"query"`
	Enabled             *bool              `json:"enabled"`
	PollIntervalSeconds int                `json:"poll_interval_seconds"`
	MaxInflightTasks    int                `json:"max_inflight_tasks"`
	DedupScope          watches.DedupScope `json:"dedup_scope"`
}

// handleWatchAction routes the watch actions.
func (r *Runtime) handleWatchAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return nil, errors.New("kandev-plugin-forgejo: watches require a verified workspace")
	}
	var body watchRequest
	if len(request.Body) > 0 {
		if err := json.Unmarshal(request.Body, &body); err != nil {
			return nil, fmt.Errorf("kandev-plugin-forgejo: decode watch body: %w", err)
		}
	}

	switch request.ActionKey {
	case ActionWatchesList:
		return r.listWatches(ctx, workspaceID)
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
	default:
		return nil, fmt.Errorf("kandev-plugin-forgejo: unknown watch action %q", request.ActionKey)
	}
}

func (r *Runtime) listWatches(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	found, err := r.watchStore.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]any{"watches": found})
}

func (r *Runtime) createWatch(ctx context.Context, workspaceID string, body watchRequest) (*pluginsdk.PluginActionResponse, error) {
	watch, err := watchFromRequest(workspaceID, watches.Watch{Enabled: true}, body)
	if err != nil {
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
		return nil, errors.New("kandev-plugin-forgejo: the Forgejo integration is turned off for this workspace")
	}
	if !watch.Enabled {
		return nil, errors.New("kandev-plugin-forgejo: this watch is paused")
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
	})
}

// requireWatch loads a watch, scoped to the verified workspace. Reading it
// through the workspace the caller was verified for is what keeps one
// workspace's watches — and the filters, repository ids and spawn prompts they
// carry — out of another's responses.
func (r *Runtime) requireWatch(ctx context.Context, workspaceID, id string) (watches.Watch, error) {
	if strings.TrimSpace(id) == "" {
		return watches.Watch{}, errors.New("kandev-plugin-forgejo: a watch id is required")
	}
	watch, err := r.watchStore.Get(ctx, workspaceID, id)
	if errors.Is(err, watches.ErrNotFound) {
		return watches.Watch{}, errors.New("kandev-plugin-forgejo: no such watch in this workspace")
	}
	return watch, err
}

// watchFromRequest folds a request body onto a base watch. Create passes a
// zero value with Enabled true; update passes the stored record, so a field
// the form omits keeps its stored value rather than reverting to a default.
func watchFromRequest(workspaceID string, base watches.Watch, body watchRequest) (watches.Watch, error) {
	watch := base
	watch.WorkspaceID = workspaceID

	assignString(&watch.Name, body.Name)
	assignString(&watch.WorkflowID, body.WorkflowID)
	assignString(&watch.WorkflowStepID, body.WorkflowStepID)
	assignString(&watch.State, body.State)

	// These four are clearable: an operator removing a prompt or detaching a
	// repository must be able to, so an empty string is taken as written.
	watch.AgentProfileID = body.AgentProfileID
	watch.ExecutorProfileID = body.ExecutorProfileID
	watch.Prompt = body.Prompt
	watch.RepositoryID = body.RepositoryID
	watch.BaseBranch = body.BaseBranch
	watch.Query = body.Query
	watch.Labels = body.Labels
	watch.StartAgent = body.StartAgent

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
			if strings.TrimSpace(raw) == "" {
				continue
			}
			ref, err := watches.ParseRepoRef(raw)
			if err != nil {
				return watches.Watch{}, err
			}
			repos = append(repos, ref)
		}
		watch.Repos = repos
	}
	return watch, nil
}

// assignString writes value onto target when value is non-empty, so an omitted
// form field leaves the stored value alone.
func assignString(target *string, value string) {
	if strings.TrimSpace(value) != "" {
		*target = value
	}
}
