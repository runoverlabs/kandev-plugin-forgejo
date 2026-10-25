package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/kandev"
	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/scm"

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
func (r *Runtime) newAutomationEngine(hosts kandev.Provider) *prautomation.Engine {
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
		return nil, kandev.ErrNoHost
	}
	return kandev.WorkspaceIDs(ctx, host)
}

// automationMessenger reaches task agents through the Host. It needs
// api_read: sessions and api_write: messages.
type automationMessenger struct {
	hosts kandev.Provider
}

func (m automationMessenger) Sessions(ctx context.Context, taskID string) ([]prautomation.Session, error) {
	host := m.hosts()
	if host == nil {
		return nil, kandev.ErrNoHost
	}
	// Every page: an attempt's session missing from the list reads as ended,
	// and its prompt would be sent again while the agent still runs it.
	var out []prautomation.Session
	page := pluginsdk.Page{Limit: sessionPageLimit}
	for range maxSessionPages {
		sessions, info, err := host.Sessions().List(ctx, pluginsdk.SessionFilter{TaskIDs: []string{taskID}}, page)
		if err != nil {
			return nil, hostMessagingError(err)
		}
		for _, session := range sessions {
			if session.TaskID != "" && session.TaskID != taskID {
				continue
			}
			out = append(out, prautomation.Session{
				ID: session.ID, State: session.State, StartedAt: session.StartedAt, ACPSessionID: session.ACPSessionID,
			})
		}
		if info == nil || !info.HasMore || info.NextCursor == "" || info.NextCursor == page.Cursor {
			return out, nil
		}
		page.Cursor = info.NextCursor
	}
	return nil, errTooManySessions
}

// The Host caps a page at 200. A task with more sessions than the pages
// below hold fails the read, which the engine treats as busy rather than
// guessing from part of the list.
const (
	sessionPageLimit = 200
	maxSessionPages  = 25
)

var errTooManySessions = errors.New("kandev-plugin-forgejo: too many sessions on the task to read")

func (m automationMessenger) Send(ctx context.Context, taskID, sessionID, text string) (string, error) {
	host := m.hosts()
	if host == nil {
		return "", kandev.ErrNoHost
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
//
// Aborted is not a refusal: the Host also returns it after the prompt was
// queued ("task manager changed before message dispatch"), so it is left
// ambiguous and the engine keeps its attempt.
func hostMessagingError(err error) error {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return fmt.Errorf("%w: %v", prautomation.ErrNotApproved, err)
	case codes.NotFound:
		return fmt.Errorf("%w: %v", prautomation.ErrNoSession, err)
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %v", prautomation.ErrTaskInactive, err)
	default:
		return err
	}
}

// automationMerger merges through the instance and audits the result: the
// shared Forgejo account hides who merged, so the trail names automation.
//
// An attempt the instance never answered, or refused for a transient reason
// (still checking, the head moved, already merged), is not audited: the loop
// retries those, an entry per retry would push the people's writes out of the
// bounded trail, and "failed" would misreport a merge someone else made.
type automationMerger struct {
	runtime *Runtime
}

func (m automationMerger) Merge(ctx context.Context, workspaceID string, ref prautomation.PullRef, headSHA string) error {
	err := m.runtime.automationSource.MergeLinked(ctx, ref, headSHA)
	var writeErr *scm.WriteError
	answered := err == nil || errors.As(err, &writeErr)
	if answered && (writeErr == nil || !transientMergeReason(writeErr.Reason)) {
		outcome := "merged"
		if err != nil {
			outcome = "failed"
		}
		m.runtime.audit(ctx, auditRecord{
			Action: "automation.merge", Actor: "auto-merge", Workspace: workspaceID,
			Task: ref.TaskID, Number: ref.Number, Outcome: outcome,
		})
	}
	if err == nil || writeErr == nil {
		return err
	}
	return &prautomation.MergeError{
		Transient: transientMergeReason(writeErr.Reason), Message: writeReasonMessage(writeErr.Reason),
	}
}

// transientMergeReason reports a refusal the next observation explains:
// mergeability is computed, the new head is evaluated afresh, the merge is
// seen.
func transientMergeReason(reason scm.Reason) bool {
	switch reason {
	case scm.ReasonChecking, scm.ReasonHeadChanged, scm.ReasonAlreadyMerged:
		return true
	}
	return false
}

// OnEvent receives the bus events the manifest subscribes to. It only marks
// records due, and off the delivery path: the host waits on it, and marking a
// record waits for any evaluation holding it.
func (r *Runtime) OnEvent(_ context.Context, event *pluginsdk.Event) error {
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
		r.automation.Hint(func(ctx context.Context) { r.automation.TurnCompleted(ctx, taskID, sessionID, startedAt) })
	case event.EventType == eventSessionStateChanged || strings.HasPrefix(event.EventType, eventSessionStateChanged+"."):
		r.automation.Hint(func(ctx context.Context) { r.automation.SessionChanged(ctx, taskID) })
	}
	return nil
}

// handleAutomationAction routes the automation actions.
func (r *Runtime) handleAutomationAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	workspaceID := strings.TrimSpace(request.Context.WorkspaceID)
	if workspaceID == "" {
		return kandev.Refuse(422, "This action needs a verified workspace.")
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
		// ConfirmFork is the panel's record that a human confirmed the fork
		// warning.
		ConfirmFork bool `json:"confirm_fork"`
	}
	if bad := kandev.DecodeBody(request, &input); bad != nil {
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
		if record.Observed.IsZero() {
			// Not observed yet, so the record cannot say whether this is a fork,
			// and the panel decides on its warning from it. Shown, not stored:
			// the loop's observation is what the record keeps.
			if fork, err := r.automationSource.IsFork(ctx, ref); err == nil {
				record.Fork = fork
			}
		}
		return r.automationView(ctx, workspaceID, record)

	case ActionAutomationSet:
		// Auto-fix and auto-merge run an agent on a fork author's code and
		// comments. That takes a human's confirmation, checked here and not
		// only in the panel, which may not know yet that it is a fork.
		var fork *bool
		isFork := func() (bool, error) {
			if fork == nil {
				value, err := r.automationFork(ctx, workspaceID, ref)
				if err != nil {
					return false, err
				}
				fork = &value
			}
			return *fork, nil
		}
		enabling := (input.AutoFix != nil && *input.AutoFix) || (input.AutoMerge != nil && *input.AutoMerge)
		if enabling && !input.ConfirmFork {
			fork, err := isFork()
			if err != nil {
				return pullFailure(err)
			}
			if fork {
				return kandev.Refuse(http.StatusConflict, "This pull request comes from a fork. Confirm the warning in the panel to turn this on.")
			}
		}
		// The defaults an unseeded record takes, as the loop would seed it: a
		// fork never takes auto-fix or auto-merge from them.
		settings, err := r.automation.Store().Settings(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
		defaults := settings.Defaults
		if defaults.AutoFix || defaults.AutoMerge {
			fork, err := isFork()
			if err != nil {
				return pullFailure(err)
			}
			if fork {
				defaults.AutoFix, defaults.AutoMerge = false, false
			}
		}
		var changes []string
		// Upsert: when the loop has not seen this link yet, a human's choice
		// does not wait for it. An unseeded record starts from the defaults,
		// with what they set on top.
		record, err := r.automation.Upsert(ctx, workspaceID, ref, func(record *prautomation.Record, created bool) error {
			if created {
				changes = append(changes, "created")
			}
			next, fresh := record.Switches, !record.Seeded
			if fresh {
				next = defaults
			}
			apply := func(name string, value *bool, field *bool) {
				// A new record logs every switch it was given, off ones too:
				// "created" alone would not say a human refused one.
				if value != nil && (fresh || *value != *field) {
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
			return kandev.Refuse(422, "what must be merge or rounds.")
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
			return kandev.Refuse(409, "Automation has not looked at this pull request yet.")
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
	return kandev.Refuse(404, "Unknown automation action.")
}

// automationFork reports whether a linked pull request comes from a fork: as
// the loop last observed it, or read now when it has not observed it yet.
func (r *Runtime) automationFork(ctx context.Context, workspaceID string, ref prautomation.PullRef) (bool, error) {
	record, found, err := r.automation.Store().Get(ctx, workspaceID, ref)
	if err != nil {
		return false, err
	}
	if found && !record.Observed.IsZero() {
		return record.Fork, nil
	}
	return r.automationSource.IsFork(ctx, ref)
}

// automationRef resolves the pull request a task-scoped call may act on, from
// the task's own links on the configured instance.
func (r *Runtime) automationRef(ctx context.Context, workspaceID, taskID string, number int64) (prautomation.PullRef, error) {
	if strings.TrimSpace(taskID) == "" {
		return prautomation.PullRef{}, scm.ErrNotLinked
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
		return prautomation.PullRef{}, scm.ErrNotLinked
	case 1:
		return matches[0], nil
	default:
		return prautomation.PullRef{}, scm.ErrAmbiguous
	}
}

// automationView is the panel's model of one record.
func (r *Runtime) automationView(ctx context.Context, workspaceID string, record prautomation.Record) (*pluginsdk.PluginActionResponse, error) {
	settings, err := r.automation.Store().Settings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	enabled, _ := r.integrationEnabled(ctx, workspaceID)
	// An unseeded record shows the switches it is about to take, which a
	// change from the panel keeps: showing them off would let one toggle turn
	// on defaults nobody saw. A fork never takes auto-fix or auto-merge from
	// them.
	switches := record.Switches
	if !record.Seeded {
		switches = settings.Defaults
		if record.Fork {
			switches.AutoFix, switches.AutoMerge = false, false
		}
	}
	view := map[string]any{
		"number":              record.Number,
		"seeded":              record.Seeded,
		"auto_fix":            switches.AutoFix,
		"auto_merge":          switches.AutoMerge,
		"on_review_requested": switches.OnReviewRequested,
		"on_merged":           switches.OnMerged,
		"on_closed":           switches.OnClosed,
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
	return kandev.JSON(view)
}

func (r *Runtime) automationSettings(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	settings, err := r.automation.Store().Settings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return kandev.JSON(settingsView(settings))
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
	if bad := kandev.DecodeBody(request, &input); bad != nil {
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
		return kandev.Refuse(422, strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
	}
	if err := r.automation.Store().PutSettings(ctx, workspaceID, settings); err != nil {
		return nil, err
	}
	// Every field the request set, the template only as custom or default:
	// the trail never holds a body.
	var changes []string
	if defaults := input.Defaults; defaults != nil {
		changes = append(changes,
			fmt.Sprintf("default.auto_fix=%t", defaults.AutoFix),
			fmt.Sprintf("default.auto_merge=%t", defaults.AutoMerge),
			fmt.Sprintf("default.on_review_requested=%t", defaults.OnReviewRequested),
			fmt.Sprintf("default.on_merged=%t", defaults.OnMerged),
			fmt.Sprintf("default.on_closed=%t", defaults.OnClosed))
	}
	if input.Trust != nil {
		changes = append(changes, "trust="+string(settings.Trust))
	}
	if input.Template != nil {
		template := "custom"
		if strings.TrimSpace(settings.Template) == "" {
			template = "default"
		}
		changes = append(changes, "template="+template)
	}
	r.audit(ctx, auditRecord{
		Action: request.ActionKey, Actor: request.Context.ActorID, Workspace: workspaceID,
		Outcome: strings.Join(changes, ","),
	})
	return kandev.JSON(settingsView(settings.Normalize()))
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
	// Audited like the writing ops: blocked holds auto-merge back, and the
	// trail says which session asked for it.
	audited := outcome
	if err != nil {
		audited = "refused"
	}
	r.audit(ctx, auditRecord{
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
