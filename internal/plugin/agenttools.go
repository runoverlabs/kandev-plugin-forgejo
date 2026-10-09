package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Agent tool names. These are the manifest `agent_tools[].name` values; Kandev
// derives the exposed MCP name from them and the plugin id, so the prefix is
// already long and these stay short.
const (
	ToolCI = "ci"
	ToolPR = "pr"
)

// Bounds on the logs ci may inline. The host rejects any result above 1 MiB
// outright rather than truncating it, so the total budget sits far enough
// below that the surrounding JSON can never push a result over. The job count
// is capped too: each log is a separate request, and the whole invocation has
// 30 seconds.
const (
	ciLogTailBytes  = 128 << 10
	ciLogTotalBytes = 256 << 10
	ciLogMaxJobs    = 5
	ciLogMaxLines   = 2000
)

// prListPages and prListLimit bound the search for a pull request by head
// branch. Neither Gitea nor older Forgejo can filter by head server-side, so
// the newest pages are scanned and anything older is treated as absent.
const (
	prListPages = 3
	prListLimit = 50
)

var _ pluginsdk.AgentToolPlugin = (*Runtime)(nil)

// InvokeAgentTool serves the MCP tools this plugin exposes to task agents.
//
// Failures an agent can act on — no repository, nothing configured, a bad
// argument — come back as IsError results with one sentence of explanation,
// because an agent can read those and adapt. A Go error is reserved for faults
// the agent cannot do anything about. Text is never empty: the host rejects a
// result without it.
func (r *Runtime) InvokeAgentTool(ctx context.Context, request *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	if request == nil {
		return nil, errors.New("kandev-plugin-forgejo: agent tool request is required")
	}
	enabled, err := r.integrationEnabled(ctx, request.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return toolError("The Forgejo integration is turned off for this workspace."), nil
	}
	switch request.Name {
	case ToolCI:
		return r.toolCI(ctx, request)
	case ToolPR:
		return r.toolPR(ctx, request)
	default:
		return nil, fmt.Errorf("kandev-plugin-forgejo: unknown agent tool %q", request.Name)
	}
}

// toolCI reports the CI result for a ref, optionally with the tail of each
// failed job's log.
//
// The log fetch lives here rather than in a tool of its own because the
// question an agent actually has is "what failed and why", and two tools
// answered it in two round trips while costing a permanent slice of every
// agent's prompt. Logs stay opt-in through `logs`, so the cheap read stays
// cheap.
func (r *Runtime) toolCI(ctx context.Context, request *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	ref := argString(request.Arguments, "ref")
	if ref == "" {
		return toolError("ref is required: pass a branch name or commit SHA."), nil
	}
	lines := 0
	if requested, ok := argInt(request.Arguments, "logs"); ok && requested > 0 {
		// Compare before converting, so the bound is visible where the
		// int64 narrows.
		lines = ciLogMaxLines
		if requested < ciLogMaxLines {
			lines = int(requested)
		}
	}
	repository, client, failure := r.agentRepository(ctx, request)
	if failure != nil {
		return failure, nil
	}

	status, err := client.CIStatusFor(ctx, repository.OwnerOrProject, repository.Name, ref)
	if err != nil {
		return toolError(safeMessage(err)), nil
	}

	fullName := repository.OwnerOrProject + "/" + repository.Name
	var text strings.Builder
	fmt.Fprintf(&text, "%s %s@%s", status.State, fullName, ref)
	if status.Source == forgejo.CISourceNone {
		text.WriteString(" (no CI reported for this ref)")
	}
	for _, job := range status.Jobs {
		if job.Status == forgejo.CIStateSuccess {
			continue
		}
		fmt.Fprintf(&text, "\n  %s %s", job.Status, job.Name)
		// Only a job with an id has a log to fetch. Printing the id where
		// there is one is also what tells the agent which entries `logs` can
		// say anything more about.
		if job.ID > 0 {
			fmt.Fprintf(&text, " job=%d", job.ID)
		}
	}

	entries := jobsToAny(status.Jobs)
	if lines > 0 {
		appendFailedJobLogs(ctx, client, repository, status, lines, &text, entries)
	}

	return &pluginsdk.AgentToolResult{
		Text: text.String(),
		StructuredContent: map[string]any{
			"state":  status.State,
			"source": status.Source,
			"ref":    status.Ref,
			"sha":    status.SHA,
			"url":    status.URL,
			"repo":   fullName,
			"jobs":   entries,
		},
	}, nil
}

// appendFailedJobLogs inlines the tail of each failed job's log, bounded by a
// shared byte budget and a job count so one pathological run cannot blow the
// result size or the invocation timeout.
//
// The log text goes only into text, never into the structured content: the
// host counts Text plus the encoded structured content against one 1 MiB
// ceiling, so carrying the same bytes twice would halve the budget for no
// gain. The structured entries record that a log was attached and whether it
// was cut.
func appendFailedJobLogs(ctx context.Context, client *forgejo.Client, repository sourcecontrol.Repository,
	status forgejo.CIStatus, lines int, text *strings.Builder, entries []any) {
	budget := ciLogTotalBytes
	fetched := 0
	for i, job := range status.Jobs {
		if job.Status != forgejo.CIStateFailure || job.ID <= 0 {
			continue
		}
		if fetched >= ciLogMaxJobs || budget <= 0 {
			fmt.Fprintf(text, "\n\n(more failed jobs have logs; ask again for one of them)")
			return
		}

		size := ciLogTailBytes
		if size > budget {
			size = budget
		}
		fmt.Fprintf(text, "\n\n--- %s (job %d) ---", job.Name, job.ID)
		log, truncated, err := client.JobLogTail(ctx, repository.OwnerOrProject, repository.Name, job.ID, size)
		if err != nil {
			// A release that lists jobs but serves no logs, an expired log and
			// an unknown id are indistinguishable here and mean the same thing
			// to the caller: there is nothing to read.
			text.WriteString("\n(no log available)")
			continue
		}
		tail, dropped := lastLines(log, lines)
		if strings.TrimSpace(tail) == "" {
			text.WriteString("\n(the job log is empty)")
			continue
		}
		if truncated || dropped {
			text.WriteString("\n(earlier output omitted)")
		}
		text.WriteString("\n" + tail)
		budget -= len(tail)
		fetched++
		if entry, ok := entries[i].(map[string]any); ok {
			entry["log_attached"] = true
			entry["log_truncated"] = truncated || dropped
		}
	}
}

// toolPR reads, opens, or readies the pull request for a task.
func (r *Runtime) toolPR(ctx context.Context, request *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	op := strings.ToLower(argString(request.Arguments, "op"))
	repository, client, failure := r.agentRepository(ctx, request)
	if failure != nil {
		return failure, nil
	}
	switch op {
	case "get":
		return r.prGet(ctx, request, client, repository)
	case "open":
		return r.prOpen(ctx, request, client, repository)
	case "ready":
		return r.prReady(ctx, request, client, repository)
	case "merge", "review", "request_review", "update", "comment":
		return r.prWrite(ctx, request, op)
	default:
		return toolError("op must be get, open, ready, merge, review, request_review, update, or comment."), nil
	}
}

// prGet returns the pull requests already linked to this task, falling back to
// a head-branch search when nothing is linked yet.
func (r *Runtime) prGet(ctx context.Context, request *pluginsdk.AgentToolRequest, client *forgejo.Client, repository sourcecontrol.Repository) (*pluginsdk.AgentToolResult, error) {
	pulls, err := r.taskPullRequests(ctx, request.Context.TaskID, request.Context.WorkspaceID, client, repository)
	if err != nil {
		return toolError(safeMessage(err)), nil
	}
	if len(pulls) == 0 {
		if head := argString(request.Arguments, "head"); head != "" {
			found, err := findPullRequestByHead(ctx, client, repository, head)
			if err != nil {
				return toolError(safeMessage(err)), nil
			}
			if found != nil {
				pulls = []forgejo.PullRequest{*found}
			}
		}
	}
	if len(pulls) == 0 {
		return &pluginsdk.AgentToolResult{
			Text:              "No pull request is linked to this task.",
			StructuredContent: map[string]any{"pulls": []any{}},
		}, nil
	}
	return pullResult(pulls, repository, "")
}

// prOpen opens a pull request from head.
//
// It is idempotent by construction rather than by retry: an existing open pull
// request for the same head is returned as-is. Kandev never retries an agent
// tool call, precisely because it cannot know whether the side effect already
// happened — so this tool makes "already happened" the same answer as "done".
func (r *Runtime) prOpen(ctx context.Context, request *pluginsdk.AgentToolRequest, client *forgejo.Client, repository sourcecontrol.Repository) (*pluginsdk.AgentToolResult, error) {
	head := argString(request.Arguments, "head")
	if head == "" {
		return toolError("head is required for op=open: pass the branch holding the changes."), nil
	}
	title := argString(request.Arguments, "title")
	if title == "" {
		return toolError("title is required for op=open."), nil
	}

	if existing, err := findPullRequestByHead(ctx, client, repository, head); err != nil {
		return toolError(safeMessage(err)), nil
	} else if existing != nil {
		r.linkPullRequest(ctx, request.Context.TaskID, repository, existing.Number)
		return pullResult([]forgejo.PullRequest{*existing}, repository, "already open")
	}

	created, err := r.changeRequests.Create(ctx, repository, head, sourcecontrol.CreateChangeRequestInput{
		Title:       title,
		Description: argString(request.Arguments, "body"),
		Destination: argString(request.Arguments, "base"),
		Draft:       argBool(request.Arguments, "draft"),
	})
	if err != nil {
		// The create may still have landed — a duplicate-head rejection means
		// exactly that. Look before reporting a failure the agent would retry.
		if existing, lookupErr := findPullRequestByHead(ctx, client, repository, head); lookupErr == nil && existing != nil {
			r.linkPullRequest(ctx, request.Context.TaskID, repository, existing.Number)
			return pullResult([]forgejo.PullRequest{*existing}, repository, "already open")
		}
		return toolError(safeMessage(err)), nil
	}
	r.linkPullRequest(ctx, request.Context.TaskID, repository, created.Identity.Number)

	pull, err := client.PullRequest(ctx, repository.OwnerOrProject, repository.Name, created.Identity.Number)
	if err != nil {
		// The pull request exists; only the read-back failed.
		return &pluginsdk.AgentToolResult{
			Text: fmt.Sprintf("opened #%d %s", created.Identity.Number, created.URL),
			StructuredContent: map[string]any{
				"pulls": []any{map[string]any{"number": created.Identity.Number, "url": created.URL, "title": created.Title}},
			},
		}, nil
	}
	return pullResult([]forgejo.PullRequest{pull}, repository, "opened")
}

// prReady clears the work-in-progress title marker. Forgejo and Gitea have no
// draft flag in REST v1, so a draft is a "WIP:" title prefix and readying one
// is a title edit. Readying an already-ready pull request is a no-op success.
func (r *Runtime) prReady(ctx context.Context, request *pluginsdk.AgentToolRequest, client *forgejo.Client, repository sourcecontrol.Repository) (*pluginsdk.AgentToolResult, error) {
	pulls, err := r.taskPullRequests(ctx, request.Context.TaskID, request.Context.WorkspaceID, client, repository)
	if err != nil {
		return toolError(safeMessage(err)), nil
	}
	if len(pulls) == 0 {
		if head := argString(request.Arguments, "head"); head != "" {
			found, err := findPullRequestByHead(ctx, client, repository, head)
			if err != nil {
				return toolError(safeMessage(err)), nil
			}
			if found != nil {
				pulls = []forgejo.PullRequest{*found}
			}
		}
	}
	if len(pulls) == 0 {
		return toolError("No pull request is linked to this task. Open one first, or pass head."), nil
	}
	if len(pulls) > 1 {
		return toolError("This task has more than one pull request; ready them from the Forgejo UI."), nil
	}

	pull := pulls[0]
	stripped, changed := forgejo.StripWorkInProgressPrefix(pull.Title)
	if !changed {
		return pullResult([]forgejo.PullRequest{pull}, repository, "already ready")
	}
	updated, err := client.EditPullRequest(ctx, repository.OwnerOrProject, repository.Name, pull.Number,
		forgejo.EditPullRequestInput{Title: stripped})
	if err != nil {
		return toolError(safeMessage(err)), nil
	}
	return pullResult([]forgejo.PullRequest{updated}, repository, "ready")
}

// taskPullRequests resolves the pull requests this plugin has associated with
// a task, in the repository the tool is acting on.
func (r *Runtime) taskPullRequests(ctx context.Context, taskID, workspaceID string, client *forgejo.Client, repository sourcecontrol.Repository) ([]forgejo.PullRequest, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	if strings.TrimSpace(workspaceID) == "" {
		host := r.Host()
		if host == nil {
			return nil, forgejo.ErrNotConfigured
		}
		task, err := host.Tasks().Get(ctx, taskID)
		if err != nil || task == nil {
			return nil, err
		}
		workspaceID = task.WorkspaceID
	}
	identities, err := r.associations.ListForTask(ctx, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	pulls := make([]forgejo.PullRequest, 0, len(identities))
	for _, identity := range identities {
		if identity.ConnectionScope != client.Scope() || identity.RepositoryID != repository.RepositoryID {
			continue
		}
		pull, err := client.PullRequest(ctx, repository.OwnerOrProject, repository.Name, identity.Number)
		if err != nil {
			if errors.Is(err, forgejo.ErrNotFound) {
				continue
			}
			return nil, err
		}
		pulls = append(pulls, pull)
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	return pulls, nil
}

// linkPullRequest records the task association so Kandev's own review sidebar
// sees a pull request an agent opened, exactly as if it had been opened from
// the UI. A failure here does not fail the tool: the pull request exists.
func (r *Runtime) linkPullRequest(ctx context.Context, taskID string, repository sourcecontrol.Repository, number int64) {
	if strings.TrimSpace(taskID) == "" || number <= 0 {
		return
	}
	_ = r.associations.Link(ctx, taskID, sourcecontrol.ChangeRequestIdentity{
		ConnectionScope: repository.ConnectionScope,
		RepositoryID:    repository.RepositoryID,
		Number:          number,
	})
}

// findPullRequestByHead looks for an open pull request whose head is branch.
// Only current Forgejo can filter by head server-side, so the newest pages are
// scanned and compared locally.
func findPullRequestByHead(ctx context.Context, client *forgejo.Client, repository sourcecontrol.Repository, branch string) (*forgejo.PullRequest, error) {
	wanted := strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	if wanted == "" {
		return nil, nil
	}
	for page := 1; page <= prListPages; page++ {
		pulls, err := client.ListPullRequests(ctx, repository.OwnerOrProject, repository.Name, "open", page, prListLimit)
		if err != nil {
			if errors.Is(err, forgejo.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		for i := range pulls {
			if strings.EqualFold(strings.TrimSpace(pulls[i].Head.Ref), wanted) {
				return &pulls[i], nil
			}
		}
		if len(pulls) < prListLimit {
			break
		}
	}
	return nil, nil
}

// agentRepository resolves the Forgejo repository an agent tool acts on, and
// the client for its instance. It returns a ready IsError result rather than
// an error whenever the agent could fix the situation itself.
func (r *Runtime) agentRepository(ctx context.Context, request *pluginsdk.AgentToolRequest) (sourcecontrol.Repository, *forgejo.Client, *pluginsdk.AgentToolResult) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.Repository{}, nil, toolError(safeMessage(err))
	}
	repository, err := r.resolveTaskRepository(ctx, request.Context.TaskID, request.Context.WorkspaceID,
		argString(request.Arguments, "repo"))
	if err != nil {
		return sourcecontrol.Repository{}, nil, toolError(err.Error())
	}
	return repository, client, nil
}

// resolveTaskRepository finds the task's Forgejo repository. A task with
// several is not guessed at: the caller names one with `repo`.
func (r *Runtime) resolveTaskRepository(ctx context.Context, taskID, workspaceID, hint string) (sourcecontrol.Repository, error) {
	if strings.TrimSpace(taskID) == "" {
		return sourcecontrol.Repository{}, errors.New("This tool runs on a task session and none was supplied.")
	}
	host := r.Host()
	if host == nil {
		return sourcecontrol.Repository{}, errors.New("The Forgejo plugin is not connected to Kandev right now.")
	}
	task, err := host.Tasks().Get(ctx, taskID)
	if err != nil {
		return sourcecontrol.Repository{}, errors.New("Could not read this task from Kandev.")
	}
	if task == nil || len(task.Repositories) == 0 {
		return sourcecontrol.Repository{}, errors.New("This task has no repository attached.")
	}
	if strings.TrimSpace(workspaceID) == "" {
		workspaceID = task.WorkspaceID
	}

	candidates := make([]sourcecontrol.Repository, 0, len(task.Repositories))
	for _, attached := range task.Repositories {
		// A task may mix providers. ResolveAttached rejects anything that is
		// not this connection's, which is the filter rather than an error.
		resolved, err := r.repositories.ResolveAttached(ctx, pluginsdk.VerifiedActionContext{
			WorkspaceID:  workspaceID,
			TaskID:       taskID,
			RepositoryID: attached.RepositoryID,
		})
		if err != nil {
			continue
		}
		candidates = append(candidates, resolved)
	}
	if len(candidates) == 0 {
		return sourcecontrol.Repository{}, errors.New("This task has no Forgejo repository attached.")
	}

	if trimmed := strings.TrimSpace(hint); trimmed != "" {
		for _, candidate := range candidates {
			full := candidate.OwnerOrProject + "/" + candidate.Name
			if strings.EqualFold(full, trimmed) || strings.EqualFold(candidate.Name, trimmed) {
				return candidate, nil
			}
		}
		return sourcecontrol.Repository{}, fmt.Errorf("No Forgejo repository named %q on this task. Attached: %s.",
			trimmed, strings.Join(repositoryNames(candidates), ", "))
	}
	if len(candidates) > 1 {
		return sourcecontrol.Repository{}, fmt.Errorf("This task has several Forgejo repositories; pass repo. Attached: %s.",
			strings.Join(repositoryNames(candidates), ", "))
	}
	return candidates[0], nil
}

func repositoryNames(repositories []sourcecontrol.Repository) []string {
	names := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		names = append(names, repository.OwnerOrProject+"/"+repository.Name)
	}
	sort.Strings(names)
	return names
}

// pullResult renders pull requests into the one-line-per-entry text an agent
// reads plus the structured form it can act on.
func pullResult(pulls []forgejo.PullRequest, repository sourcecontrol.Repository, note string) (*pluginsdk.AgentToolResult, error) {
	entries := make([]any, 0, len(pulls))
	var text strings.Builder
	for i, pull := range pulls {
		if i > 0 {
			text.WriteString("\n")
		}
		state := pullState(pull)
		fmt.Fprintf(&text, "#%d %s %s", pull.Number, state, pull.HTMLURL)
		// The head commit is what merge and review pin themselves to.
		if sha := strings.TrimSpace(pull.Head.Sha); sha != "" {
			fmt.Fprintf(&text, " sha=%s", sha)
		}
		if i == 0 && note != "" {
			fmt.Fprintf(&text, " (%s)", note)
		}
		fmt.Fprintf(&text, "\n  %s", strings.TrimSpace(pull.Title))
		entries = append(entries, map[string]any{
			"number": pull.Number,
			"state":  state,
			"url":    pull.HTMLURL,
			"title":  strings.TrimSpace(pull.Title),
			"head":   pull.Head.Ref,
			"base":   pull.Base.Ref,
			"draft":  forgejo.IsWorkInProgressTitle(pull.Title),
			"sha":    pull.Head.Sha,
		})
	}
	content := map[string]any{"pulls": entries, "repo": repository.OwnerOrProject + "/" + repository.Name}
	if note != "" {
		content["result"] = note
	}
	return &pluginsdk.AgentToolResult{Text: text.String(), StructuredContent: content}, nil
}

// pullState reports merged separately from closed, which the REST state field
// does not.
func pullState(pull forgejo.PullRequest) string {
	if pull.Merged {
		return "merged"
	}
	if state := strings.TrimSpace(pull.State); state != "" {
		return state
	}
	return "unknown"
}

func jobsToAny(jobs []forgejo.CIJob) []any {
	entries := make([]any, 0, len(jobs))
	for _, job := range jobs {
		entry := map[string]any{"name": job.Name, "status": job.Status}
		if job.ID > 0 {
			entry["id"] = job.ID
		}
		if job.URL != "" {
			entry["url"] = job.URL
		}
		entries = append(entries, entry)
	}
	return entries
}

// lastLines keeps the final count lines of text and reports whether anything
// was dropped.
func lastLines(text string, count int) (string, bool) {
	if count <= 0 || text == "" {
		return text, false
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= count {
		return strings.Join(lines, "\n"), false
	}
	return strings.Join(lines[len(lines)-count:], "\n"), true
}

// toolError builds the IsError result an agent can read and act on.
func toolError(message string) *pluginsdk.AgentToolResult {
	if strings.TrimSpace(message) == "" {
		message = "The Forgejo plugin could not complete this call."
	}
	return &pluginsdk.AgentToolResult{Text: message, IsError: true}
}

// argString reads a trimmed string argument. The host has already validated
// arguments against the manifest input schema, so a wrong type here means a
// host that skipped validation, not a caller to argue with.
func argString(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return strings.TrimSpace(value)
}

func argBool(arguments map[string]any, key string) bool {
	value, _ := arguments[key].(bool)
	return value
}

// argInt reads an integer argument. JSON numbers arrive as float64 over the
// wire, and a plain string is accepted because some agents quote numbers.
func argInt(arguments map[string]any, key string) (int64, bool) {
	switch typed := arguments[key].(type) {
	case float64:
		converted := int64(typed)
		return converted, float64(converted) == typed
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
