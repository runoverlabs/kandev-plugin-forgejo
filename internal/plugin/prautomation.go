package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/prautomation"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pull request automation actions (docs/pr-automation.md). The task-scoped ones
// act on a pull request the verified task links; the number in the body only
// picks among those, as for every other pull request action.
const (
	ActionAutomationGet         = "automation.get"
	ActionAutomationSet         = "automation.set"
	ActionAutomationRetry       = "automation.retry"
	ActionAutomationSettings    = "automation.settings"
	ActionAutomationSettingsSet = "automation.settings_set"
)

// Event subjects the manifest subscribes to. Both are hints that let the loop
// react sooner; it does not depend on them.
const (
	eventTurnCompleted       = "turn.completed"
	eventSessionStateChanged = "task_session.state_changed"
)

// newAutomationEngine wires the engine to the Host and the instance.
func (r *Runtime) newAutomationEngine(hosts forgejo.HostProvider) *prautomation.Engine {
	return prautomation.NewEngine(prautomation.Config{
		Store: prautomation.NewStore(func() prautomation.StateHost {
			host := hosts()
			if host == nil {
				return nil
			}
			return host
		}),
		Links:     r.automationSource,
		Observer:  r.automationSource,
		Merger:    automationMerger{runtime: r},
		Messenger: automationMessenger{hosts: hosts},
		// Fails closed: prompting an agent or merging in a workspace whose
		// operator turned the integration off is worse than waiting a tick.
		Gate:       r.integrationEnabledStrict,
		Workspaces: r.workspaceIDs,
		Describe:   safeMessage,
	})
}

func (r *Runtime) workspaceIDs(ctx context.Context) ([]string, error) {
	host := r.Host()
	if host == nil {
		return nil, prautomation.ErrNoHost
	}
	workspaces, _, err := host.Workspaces().List(ctx, pluginsdk.Page{Limit: 200})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		if id := strings.TrimSpace(workspace.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// automationMessenger reaches task agents through the Host. It needs
// api_read: sessions and api_write: messages.
type automationMessenger struct {
	hosts forgejo.HostProvider
}

func (m automationMessenger) Sessions(ctx context.Context, taskID string) ([]prautomation.Session, error) {
	host := m.hosts()
	if host == nil {
		return nil, prautomation.ErrNoHost
	}
	sessions, _, err := host.Sessions().List(ctx, pluginsdk.SessionFilter{TaskIDs: []string{taskID}}, pluginsdk.Page{Limit: 50})
	if err != nil {
		return nil, hostMessagingError(err)
	}
	out := make([]prautomation.Session, 0, len(sessions))
	for _, session := range sessions {
		if session.TaskID != "" && session.TaskID != taskID {
			continue
		}
		out = append(out, prautomation.Session{ID: session.ID, State: session.State, StartedAt: session.StartedAt})
	}
	return out, nil
}

func (m automationMessenger) Send(ctx context.Context, taskID, sessionID, text string) (string, error) {
	host := m.hosts()
	if host == nil {
		return "", prautomation.ErrNoHost
	}
	dispatch, err := host.Messages().Send(ctx, taskID, sessionID, text)
	if err != nil {
		return "", hostMessagingError(err)
	}
	if dispatch == nil {
		return "", errors.New("kandev-plugin-forgejo: empty message dispatch")
	}
	return dispatch.Status, nil
}

// hostMessagingError maps the Host's refusals onto the engine's vocabulary.
// PermissionDenied is what every gated call returns while the capability set
// awaits re-approval after an upgrade.
func hostMessagingError(err error) error {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return fmt.Errorf("%w: %v", prautomation.ErrNotApproved, err)
	case codes.NotFound:
		return fmt.Errorf("%w: %v", prautomation.ErrNoSession, err)
	case codes.FailedPrecondition, codes.Aborted:
		return fmt.Errorf("%w: %v", prautomation.ErrTaskInactive, err)
	default:
		return err
	}
}

// automationMerger merges through the instance and audits the result: the
// shared Forgejo account hides who merged, so the trail names automation.
type automationMerger struct {
	runtime *Runtime
}

func (m automationMerger) Merge(ctx context.Context, workspaceID string, ref prautomation.PullRef, headSHA string) error {
	err := m.runtime.automationSource.MergeLinked(ctx, ref, headSHA)
	outcome := "merged"
	if err != nil {
		outcome = "failed"
	}
	m.runtime.audit(ctx, auditRecord{
		Action: "automation.merge", Actor: "auto-merge", Workspace: workspaceID,
		Task: ref.TaskID, Number: ref.Number, Outcome: outcome,
	})
	var writeErr *forgejo.WriteError
	if err == nil || !errors.As(err, &writeErr) {
		return err
	}
	switch writeErr.Reason {
	case forgejo.ReasonChecking, forgejo.ReasonHeadChanged, forgejo.ReasonAlreadyMerged:
		// The next observation explains these: mergeability is computed, the
		// new head is evaluated afresh, the merge is seen.
		return &prautomation.MergeError{Transient: true, Message: writeReasonMessage(writeErr.Reason)}
	default:
		return &prautomation.MergeError{Message: writeReasonMessage(writeErr.Reason)}
	}
}

// OnEvent receives the bus events the manifest subscribes to. It only marks
// records due; the host waits on it, so it does no provider work.
func (r *Runtime) OnEvent(ctx context.Context, event *pluginsdk.Event) error {
	if event == nil || r.automation == nil {
		return nil
	}
	taskID, _ := event.Payload["task_id"].(string)
	sessionID, _ := event.Payload["session_id"].(string)
	if strings.TrimSpace(taskID) == "" {
		return nil
	}
	switch {
	case event.EventType == eventTurnCompleted || strings.HasPrefix(event.EventType, eventTurnCompleted+"."):
		var startedAt time.Time
		if raw, ok := event.Payload["started_at"].(string); ok {
			startedAt, _ = time.Parse(time.RFC3339Nano, raw)
		}
		r.automation.TurnCompleted(ctx, taskID, sessionID, startedAt)
	case event.EventType == eventSessionStateChanged || strings.HasPrefix(event.EventType, eventSessionStateChanged+"."):
		r.automation.SessionChanged(ctx, taskID)
	}
	return nil
}

// handleAutomationAction routes the automation actions.
func (r *Runtime) handleAutomationAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return domainResponse(422, "This action needs a verified workspace.")
	}
	switch request.ActionKey {
	case ActionAutomationSettings:
		return r.automationSettings(ctx, workspaceID)
	case ActionAutomationSettingsSet:
		return r.automationSettingsSet(ctx, request, workspaceID)
	}

	var input struct {
		Number            int64  `json:"number"`
		AutoFix           *bool  `json:"auto_fix"`
		AutoMerge         *bool  `json:"auto_merge"`
		OnReviewRequested *bool  `json:"on_review_requested"`
		OnMerged          *bool  `json:"on_merged"`
		OnClosed          *bool  `json:"on_closed"`
		What              string `json:"what"`
	}
	if bad := decodeActionBody(request, &input); bad != nil {
		return bad, nil
	}
	ref, err := r.automationRef(ctx, workspaceID, request.Context.TaskID, input.Number)
	if err != nil {
		return pullFailure(err)
	}

	switch request.ActionKey {
	case ActionAutomationGet:
		record, found, err := r.automation.Store().Get(ctx, workspaceID, ref)
		if err != nil {
			return nil, err
		}
		if !found {
			record = prautomation.Record{TaskID: ref.TaskID, RepositoryID: ref.RepositoryID, Number: ref.Number}
		}
		return r.automationView(ctx, workspaceID, record)

	case ActionAutomationSet:
		var changes []string
		record, err := r.automation.Update(ctx, workspaceID, ref, func(record *prautomation.Record) error {
			next := record.Switches
			apply := func(name string, value *bool, field *bool) {
				if value != nil && *value != *field {
					*field = *value
					changes = append(changes, fmt.Sprintf("%s=%t", name, *value))
				}
			}
			apply("auto_fix", input.AutoFix, &next.AutoFix)
			apply("auto_merge", input.AutoMerge, &next.AutoMerge)
			apply("on_review_requested", input.OnReviewRequested, &next.OnReviewRequested)
			apply("on_merged", input.OnMerged, &next.OnMerged)
			apply("on_closed", input.OnClosed, &next.OnClosed)
			record.ApplySwitches(next)
			return nil
		})
		if errors.Is(err, prautomation.ErrNotFound) {
			// The loop has not seen this link yet. A human's choice does not
			// wait for it: the record starts from what they set.
			record = prautomation.Record{TaskID: ref.TaskID, RepositoryID: ref.RepositoryID, Number: ref.Number}
			var next prautomation.Switches
			changes = []string{"created"}
			setIf := func(name string, value *bool, field *bool) {
				if value != nil {
					*field = *value
					changes = append(changes, fmt.Sprintf("%s=%t", name, *value))
				}
			}
			setIf("auto_fix", input.AutoFix, &next.AutoFix)
			setIf("auto_merge", input.AutoMerge, &next.AutoMerge)
			setIf("on_review_requested", input.OnReviewRequested, &next.OnReviewRequested)
			setIf("on_merged", input.OnMerged, &next.OnMerged)
			setIf("on_closed", input.OnClosed, &next.OnClosed)
			record.ApplySwitches(next)
			err = r.automation.Store().Put(ctx, workspaceID, record)
		}
		if err != nil {
			return nil, err
		}
		r.audit(ctx, auditRecord{
			Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: workspaceID,
			Task: ref.TaskID, Number: ref.Number, Outcome: strings.Join(changes, ","),
		})
		return r.automationView(ctx, workspaceID, record)

	case ActionAutomationRetry:
		if input.What != "merge" && input.What != "rounds" {
			return domainResponse(422, "what must be merge or rounds.")
		}
		record, err := r.automation.Update(ctx, workspaceID, ref, func(record *prautomation.Record) error {
			if input.What == "merge" {
				record.MergeRetry = true
			} else {
				record.Rounds, record.Exhausted = 0, false
			}
			record.NextDue = time.Time{}
			return nil
		})
		if errors.Is(err, prautomation.ErrNotFound) {
			return domainResponse(409, "Automation has not looked at this pull request yet.")
		}
		if err != nil {
			return nil, err
		}
		r.audit(ctx, auditRecord{
			Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: workspaceID,
			Task: ref.TaskID, Number: ref.Number, Outcome: "retry=" + input.What,
		})
		return r.automationView(ctx, workspaceID, record)
	}
	return domainResponse(404, "Unknown automation action.")
}

// automationRef resolves the pull request a task-scoped call may act on, from
// the task's own links on the configured instance.
func (r *Runtime) automationRef(ctx context.Context, workspaceID, taskID string, number int64) (prautomation.PullRef, error) {
	if strings.TrimSpace(taskID) == "" {
		return prautomation.PullRef{}, forgejo.ErrNotLinked
	}
	client, err := r.connection.Client(ctx)
	if err != nil {
		return prautomation.PullRef{}, err
	}
	identities, err := r.associations.ListForTask(ctx, workspaceID, taskID)
	if err != nil {
		return prautomation.PullRef{}, err
	}
	var matches []prautomation.PullRef
	for _, identity := range identities {
		if identity.ConnectionScope != client.Scope() || (number != 0 && identity.Number != number) {
			continue
		}
		matches = append(matches, prautomation.PullRef{TaskID: taskID, RepositoryID: identity.RepositoryID, Number: identity.Number})
	}
	switch len(matches) {
	case 0:
		return prautomation.PullRef{}, forgejo.ErrNotLinked
	case 1:
		return matches[0], nil
	default:
		return prautomation.PullRef{}, forgejo.ErrAmbiguous
	}
}

// automationView is the panel's model of one record.
func (r *Runtime) automationView(ctx context.Context, workspaceID string, record prautomation.Record) (*pluginsdk.PluginActionResponse, error) {
	settings, err := r.automation.Store().Settings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	enabled, _ := r.integrationEnabled(ctx, workspaceID)
	view := map[string]any{
		"number":              record.Number,
		"seeded":              record.Seeded,
		"auto_fix":            record.AutoFix,
		"auto_merge":          record.AutoMerge,
		"on_review_requested": record.OnReviewRequested,
		"on_merged":           record.OnMerged,
		"on_closed":           record.OnClosed,
		"fork":                record.Fork,
		"rounds":              record.Rounds,
		"max_rounds":          prautomation.MaxRounds,
		"exhausted":           record.Exhausted,
		"last_outcome":        record.LastOutcome,
		"last_summary":        record.LastSummary,
		"merge_failure":       record.MergeFailure,
		"merge_retry":         record.MergeRetry,
		"ignored":             record.Ignored,
		"status":              record.Status,
		"error":               record.Error,
		"trust":               string(settings.Trust),
		"integration_enabled": enabled,
		"attempt":             nil,
	}
	if !record.LastOutcomeAt.IsZero() {
		view["last_outcome_at"] = record.LastOutcomeAt.Format(time.RFC3339)
	}
	if !record.Observed.IsZero() {
		view["observed_at"] = record.Observed.Format(time.RFC3339)
	}
	if attempt := record.Attempt; attempt != nil {
		view["attempt"] = map[string]any{"round": attempt.Round, "state": attempt.State, "dispatch": attempt.Dispatch}
	}
	return jsonResponse(view)
}

func (r *Runtime) automationSettings(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	settings, err := r.automation.Store().Settings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(settingsView(settings))
}

func settingsView(settings prautomation.Settings) map[string]any {
	return map[string]any{
		"defaults": map[string]any{
			"auto_fix":            settings.Defaults.AutoFix,
			"auto_merge":          settings.Defaults.AutoMerge,
			"on_review_requested": settings.Defaults.OnReviewRequested,
			"on_merged":           settings.Defaults.OnMerged,
			"on_closed":           settings.Defaults.OnClosed,
		},
		"trust":            string(settings.Trust),
		"template":         settings.Template,
		"default_template": prautomation.DefaultTemplate,
		"max_rounds":       prautomation.MaxRounds,
	}
}

func (r *Runtime) automationSettingsSet(ctx context.Context, request *pluginsdk.PluginActionRequest, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	var input struct {
		Defaults *prautomation.Switches `json:"defaults"`
		Trust    *string                `json:"trust"`
		Template *string                `json:"template"`
	}
	if bad := decodeActionBody(request, &input); bad != nil {
		return bad, nil
	}
	settings, err := r.automation.Store().Settings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if input.Defaults != nil {
		settings.Defaults = *input.Defaults
	}
	if input.Trust != nil {
		settings.Trust = prautomation.Trust(strings.TrimSpace(*input.Trust))
	}
	if input.Template != nil {
		settings.Template = *input.Template
	}
	if err := settings.Validate(); err != nil {
		return domainResponse(422, strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
	}
	if err := r.automation.Store().PutSettings(ctx, workspaceID, settings); err != nil {
		return nil, err
	}
	r.audit(ctx, auditRecord{
		Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: workspaceID,
		Outcome: "trust=" + string(settings.Trust),
	})
	return jsonResponse(settingsView(settings.Normalize()))
}

// prOutcome records the agent's report on the auto-fix attempt in flight.
func (r *Runtime) prOutcome(ctx context.Context, request *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	outcome := strings.ToLower(argString(request.Arguments, "outcome"))
	if !prautomation.ValidOutcome(outcome) {
		return toolError("outcome must be action_taken, non_actionable, or blocked."), nil
	}
	workspaceID, err := r.agentWriteWorkspace(ctx, request)
	if err != nil {
		return toolError("This task is not in a workspace this plugin can see."), nil
	}
	record, err := r.automation.Outcome(ctx, workspaceID, request.Context.TaskID, request.Context.SessionID,
		argString(request.Arguments, "repo"), outcome, argString(request.Arguments, "body"))
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
