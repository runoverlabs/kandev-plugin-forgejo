// Package watches turns Forgejo issues into Kandev tasks.
//
// It owns the provider-neutral half of the feature — the watch configuration,
// the dedup ledger, and the poll loop — and reaches the provider through the
// narrow IssueSource port that internal/forgejo implements. That split mirrors
// internal/sourcecontrol: this package never forms a Forgejo URL, and the
// adapter never decides what a watch means.
//
// # Why a plugin-owned poll loop
//
// Kandev's native issue watches (GitHub, GitLab, Jira, Linear, Sentry, Azure)
// are compiled into the host, one table and one poller per provider, with no
// extension point a plugin can register against. The plugin binary is a
// long-running process, so it runs the equivalent loop itself and creates
// tasks through the Host's api_write:tasks RPC. Everything stays in one repo
// and one release cycle.
package watches

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultPollInterval is the interval a watch polls at when the operator does
// not choose one. It matches the native providers' own default.
const DefaultPollInterval = 300 * time.Second

// MinPollInterval is the floor an operator can configure. A watch polls one
// REST call per repository per tick; anything faster is rate-limit pressure on
// the instance for no practical gain, because issues do not arrive that fast.
const MinPollInterval = 30 * time.Second

// MaxPollInterval is the ceiling. Past a day a watch is indistinguishable from
// a disabled one, and the operator is better served by turning it off.
const MaxPollInterval = 24 * time.Hour

// DefaultMaxInflightTasks bounds how many of a watch's tasks may be open at
// once. It is deliberately small: the kandev instances this plugin targets run
// with a low session ceiling, and a watch that imports a backlog of forty
// issues does not break anything but makes the board unusable.
const DefaultMaxInflightTasks = 5

// maxReposPerWatch bounds the fan-out of a single poll tick. Each repository
// costs at least one REST round trip per tick, so an unbounded list turns one
// misconfigured watch into a sustained load on the instance.
const maxReposPerWatch = 25

// DedupScope decides what "already seen" means for a watch.
type DedupScope string

const (
	// DedupScopeWatch records a created task against the watch that created
	// it. Two watches whose filters both match an issue each create a task.
	// This is what kandev's native providers do — their join tables are
	// UNIQUE(issue_watch_id, repo, number) — so it is the default here.
	DedupScopeWatch DedupScope = "watch"

	// DedupScopeWorkspace records a created task against the issue alone, so
	// one issue yields exactly one task in a workspace no matter how many
	// watches match it. Choose this when overlapping filters are expected and
	// duplicate cards are worse than a missed match.
	DedupScopeWorkspace DedupScope = "workspace"
)

// RepoRef names one repository on the connected instance.
type RepoRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// FullName renders the canonical "owner/name" form.
func (r RepoRef) FullName() string { return r.Owner + "/" + r.Name }

// ParseRepoRef reads an "owner/name" string. It rejects anything with a
// different shape rather than guessing, because a malformed entry would
// otherwise become a 404 on every poll with no obvious cause.
func ParseRepoRef(raw string) (RepoRef, error) {
	trimmed := strings.TrimSpace(raw)
	owner, name, found := strings.Cut(trimmed, "/")
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return RepoRef{}, fmt.Errorf("watches: repository %q must be in owner/name form", raw)
	}
	return RepoRef{Owner: owner, Name: name}, nil
}

// Watch is one issue watch: a query against the Forgejo instance plus the
// placement and launch settings for the tasks it creates.
//
// The field set is taken from kandev's own issue-watch tables. Fourteen
// columns are identical across all six native providers and are reproduced
// here unchanged; MaxInflightTasks, RepositoryID and BaseBranch are carried by
// every native provider except GitHub and are included because those gaps are
// accidents of history rather than design.
type Watch struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	// Name is the operator's label for this watch. Kandev's native watches
	// have no name column and are identified by their filters, which is
	// serviceable with one watch and confusing with four.
	Name string `json:"name"`

	// Placement: where a created task lands.
	WorkflowID     string `json:"workflow_id"`
	WorkflowStepID string `json:"workflow_step_id"`

	// Launch: how a created task runs, when it runs.
	AgentProfileID    string `json:"agent_profile_id"`
	ExecutorProfileID string `json:"executor_profile_id"`
	Prompt            string `json:"prompt"`
	// StartAgent asks the host to launch an agent as soon as the task is
	// created. It defaults to false: a workflow's own on_enter auto-start is
	// the operator's declared intent for the step, and a watch that launches
	// on its own authority fires agents the board was not configured to fire.
	StartAgent bool `json:"start_agent"`
	// RepositoryID and BaseBranch attach a kandev repository to the created
	// task so an agent has somewhere to work. Both are optional: a watch that
	// only files cards needs neither.
	RepositoryID string `json:"repository_id"`
	BaseBranch   string `json:"base_branch"`

	// Query: which issues match.
	Repos  []RepoRef `json:"repos"`
	Labels []string  `json:"labels"`
	// State is "open", "closed", or "all". Empty means "open".
	State string `json:"state"`
	// Query is free text matched against title and body.
	//
	// It is served by the host's own issue indexer, which ingests
	// asynchronously and can be disabled entirely. A newly filed issue may
	// therefore not match until the host has indexed it, and on a host with
	// indexing off this filter matches nothing at all. Labels and state are
	// evaluated directly against the database and have neither caveat, so
	// prefer them when the distinction matters.
	Query string `json:"query"`

	// Controls.
	Enabled             bool       `json:"enabled"`
	PollIntervalSeconds int        `json:"poll_interval_seconds"`
	MaxInflightTasks    int        `json:"max_inflight_tasks"`
	DedupScope          DedupScope `json:"dedup_scope"`

	// Observability. These mirror the native watch columns and are what the
	// panel shows when a watch stops working.
	LastPolledAt string `json:"last_polled_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	LastErrorAt  string `json:"last_error_at,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// PollInterval returns the configured interval, clamped into the supported
// range. A stored value outside the range is treated as the nearest legal one
// rather than as an error: a watch that cannot be scheduled is worse than a
// watch scheduled slightly differently from what a hand-edited record asked.
func (w Watch) PollInterval() time.Duration {
	interval := time.Duration(w.PollIntervalSeconds) * time.Second
	switch {
	case interval < MinPollInterval:
		return DefaultPollInterval
	case interval > MaxPollInterval:
		return MaxPollInterval
	default:
		return interval
	}
}

// DueAt reports when this watch should next poll, given its last successful
// poll. A watch that has never polled is due immediately.
func (w Watch) DueAt() time.Time {
	last, err := time.Parse(time.RFC3339, w.LastPolledAt)
	if err != nil {
		return time.Time{}
	}
	return last.Add(w.PollInterval())
}

// Due reports whether this watch is ready to poll at now.
func (w Watch) Due(now time.Time) bool { return !now.Before(w.DueAt()) }

// Normalize fills defaults and canonicalizes operator input. It is applied on
// every write so a record read back from state is always in a shape the poller
// can run without re-checking.
func (w *Watch) Normalize() {
	w.Name = strings.TrimSpace(w.Name)
	w.WorkflowID = strings.TrimSpace(w.WorkflowID)
	w.WorkflowStepID = strings.TrimSpace(w.WorkflowStepID)
	w.AgentProfileID = strings.TrimSpace(w.AgentProfileID)
	w.ExecutorProfileID = strings.TrimSpace(w.ExecutorProfileID)
	w.Prompt = strings.TrimSpace(w.Prompt)
	w.RepositoryID = strings.TrimSpace(w.RepositoryID)
	w.BaseBranch = strings.TrimSpace(w.BaseBranch)
	w.Query = strings.TrimSpace(w.Query)

	switch strings.ToLower(strings.TrimSpace(w.State)) {
	case "closed":
		w.State = "closed"
	case "all":
		w.State = "all"
	default:
		w.State = "open"
	}

	if w.DedupScope != DedupScopeWorkspace {
		w.DedupScope = DedupScopeWatch
	}
	if w.PollIntervalSeconds <= 0 {
		w.PollIntervalSeconds = int(DefaultPollInterval / time.Second)
	}
	if w.MaxInflightTasks <= 0 {
		w.MaxInflightTasks = DefaultMaxInflightTasks
	}

	labels := make([]string, 0, len(w.Labels))
	seen := map[string]struct{}{}
	for _, label := range w.Labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		labels = append(labels, trimmed)
	}
	w.Labels = labels

	repos := make([]RepoRef, 0, len(w.Repos))
	seenRepo := map[string]struct{}{}
	for _, repo := range w.Repos {
		ref := RepoRef{Owner: strings.TrimSpace(repo.Owner), Name: strings.TrimSpace(repo.Name)}
		if ref.Owner == "" || ref.Name == "" {
			continue
		}
		key := strings.ToLower(ref.FullName())
		if _, duplicate := seenRepo[key]; duplicate {
			continue
		}
		seenRepo[key] = struct{}{}
		repos = append(repos, ref)
	}
	w.Repos = repos
}

// Validate reports why a watch cannot be saved, or nil.
//
// Placement is required because a task with no workflow step has nowhere to
// land; everything else has a usable default. The launch profiles are NOT
// required: a watch that files cards for a human to pick up is a legitimate
// configuration, and the host rejects an unknown profile at create time
// anyway.
func (w Watch) Validate() error {
	switch {
	case strings.TrimSpace(w.WorkspaceID) == "":
		return errors.New("watches: a workspace is required")
	case w.Name == "":
		return errors.New("watches: a name is required")
	case w.WorkflowID == "":
		return errors.New("watches: a workflow is required")
	case w.WorkflowStepID == "":
		return errors.New("watches: a workflow step is required")
	case len(w.Repos) == 0:
		return errors.New("watches: at least one repository is required")
	case len(w.Repos) > maxReposPerWatch:
		return fmt.Errorf("watches: a watch covers at most %d repositories", maxReposPerWatch)
	}
	if w.StartAgent && w.AgentProfileID == "" {
		// Kandev's own launch treats an empty profile as "use the default",
		// but a watch that auto-starts is unattended by definition: making the
		// operator name the profile is the difference between a predictable
		// agent and whichever one the workspace defaults to today.
		return errors.New("watches: starting an agent automatically requires an agent profile")
	}
	return nil
}
