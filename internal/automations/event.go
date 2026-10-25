package automations

import (
	"path"
	"slices"
	"strings"
)

// Event is a verified delivery in provider-neutral form. Exactly one of Push,
// PullRequest and Run is set, matching Kind.
type Event struct {
	// Kind is the condition key this delivery can satisfy.
	Kind string
	// Provider is "forgejo" or "gitea", for display only.
	Provider string
	// Repository is the repository's `owner/name`.
	Repository string

	Push        *Push
	PullRequest *PullRequest
	Run         *Run
}

// Push is a push to one ref.
type Push struct {
	Ref         string
	Before      string
	After       string
	Pusher      string
	CommitCount int
	CompareURL  string
	HTMLURL     string
	HeadCommit  *Commit
}

// Commit is the pushed head commit.
type Commit struct {
	ID      string
	Message string
	URL     string
	Author  string
}

// Pull request actions after normalization. A provider action this plugin does
// not trigger on (labels changed, edited, assigned, reviewed, …) keeps its
// provider spelling and never matches.
const (
	ActionOpened          = "opened"
	ActionReopened        = "reopened"
	ActionSynchronized    = "synchronized"
	ActionClosed          = "closed"
	ActionMerged          = "merged"
	ActionReviewRequested = "review_requested"
	ActionCommented       = "commented"
)

// PullRequest is a pull request event or a new comment on a pull request.
type PullRequest struct {
	Action string
	Number int64
	// Detailed is false when the delivery names the pull request but carries
	// none of its fields (a comment on Forgejo 7). The branch, draft and fork
	// filters cannot be evaluated then, so it never matches.
	Detailed          bool
	Title             string
	Author            string
	Sender            string
	BaseBranch        string
	HeadBranch        string
	HeadSHA           string
	Draft             bool
	Fork              bool
	Labels            []string
	RequestedReviewer string
	HTMLURL           string
	Comment           *Comment
}

// Comment is a comment on a pull request.
type Comment struct {
	Author  string
	Body    string
	HTMLURL string
}

// Run states a ci_result condition can select.
const (
	StateSuccess   = "success"
	StateFailure   = "failure"
	StateCancelled = "cancelled"
)

// Run is a finished Actions run.
type Run struct {
	State     string
	Workflow  string
	RunNumber int64
	Title     string
	SHA       string
	// Branch is the pushed branch, or the head branch of a pull request run.
	// Empty for a tag, a schedule or a manual run.
	Branch      string
	Ref         string
	Event       string
	PullRequest int64
	TriggerUser string
	Fork        bool
	HTMLURL     string
}

// zeroSHA is how a push reports a ref that no longer exists.
func zeroSHA(sha string) bool {
	return sha != "" && strings.Trim(sha, "0") == ""
}

// Match reports whether event satisfies cfg. When it does, data is the
// normalized payload the host exposes to prompts as {{data.*}}.
func Match(cfg Config, event Event) (map[string]any, bool) {
	if event.Kind != cfg.Key || !strings.EqualFold(event.Repository, cfg.Repository) {
		return nil, false
	}
	switch cfg.Key {
	case ConditionPush:
		if event.Push != nil && matchPush(cfg, *event.Push) {
			return pushData(event), true
		}
	case ConditionPullRequest:
		if event.PullRequest != nil && matchPullRequest(cfg, *event.PullRequest) {
			return pullRequestData(event), true
		}
	case ConditionCIResult:
		if event.Run != nil && matchRun(cfg, *event.Run) {
			return runData(event), true
		}
	}
	return nil, false
}

func matchPush(cfg Config, push Push) bool {
	branch, isBranch := strings.CutPrefix(push.Ref, "refs/heads/")
	if !isBranch || branch == "" || zeroSHA(push.After) {
		return false
	}
	return matchGlobs(cfg.Branches, branch)
}

func matchPullRequest(cfg Config, pr PullRequest) bool {
	if !pr.Detailed || !actionSelected(cfg, pr.Action) {
		return false
	}
	if pr.Fork && !cfg.IncludeForks {
		return false
	}
	if pr.Draft && cfg.ExcludeDraft {
		return false
	}
	if !matchGlobs(cfg.Branches, pr.BaseBranch) {
		return false
	}
	if len(cfg.Authors) > 0 && !containsFold(cfg.Authors, pr.Author) {
		return false
	}
	if len(cfg.Labels) > 0 && !slices.ContainsFunc(pr.Labels, func(label string) bool { return containsFold(cfg.Labels, label) }) {
		return false
	}
	return true
}

func actionSelected(cfg Config, action string) bool {
	switch action {
	case ActionOpened:
		return cfg.OnOpened
	case ActionReopened:
		return cfg.OnReopened
	case ActionSynchronized:
		return cfg.OnSynchronized
	case ActionClosed:
		return cfg.OnClosed
	case ActionMerged:
		return cfg.OnMerged
	case ActionReviewRequested:
		return cfg.OnReviewRequested
	case ActionCommented:
		return cfg.OnCommented
	}
	return false
}

func matchRun(cfg Config, run Run) bool {
	switch run.State {
	case StateSuccess:
		if !cfg.OnSuccess {
			return false
		}
	case StateFailure:
		if !cfg.OnFailure {
			return false
		}
	case StateCancelled:
		if !cfg.OnCancelled {
			return false
		}
	default:
		return false
	}
	if run.Fork && !cfg.IncludeForks {
		return false
	}
	if len(cfg.Workflows) > 0 && !slices.Contains(cfg.Workflows, run.Workflow) {
		return false
	}
	return matchGlobs(cfg.Branches, run.Branch)
}

// matchGlobs: an empty list matches anything, including an empty value; a
// non-empty list never matches an empty value.
func matchGlobs(globs []string, value string) bool {
	if len(globs) == 0 {
		return true
	}
	if value == "" {
		return false
	}
	for _, glob := range globs {
		if ok, _ := path.Match(glob, value); ok {
			return true
		}
	}
	return false
}

func containsFold(list []string, value string) bool {
	return slices.ContainsFunc(list, func(item string) bool { return strings.EqualFold(item, value) })
}
