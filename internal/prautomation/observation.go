package prautomation

// PullState is a pull request's state as automation sees it. Merged is its
// own state, not a kind of closed: a merged pull request never reopens.
type PullState string

const (
	StateOpen   PullState = "open"
	StateClosed PullState = "closed"
	StateMerged PullState = "merged"
)

// CIState is the roll-up of every check on the head commit.
type CIState string

const (
	CISuccess CIState = "success"
	CIFailure CIState = "failure"
	// CIPending means something is still queued or running. Auto-fix and
	// auto-merge both wait for it to settle.
	CIPending CIState = "pending"
	// CINone means no check reported anything for the head.
	CINone CIState = "none"
)

// Check is one failing check.
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// CI is the head commit's checks.
type CI struct {
	State   CIState `json:"state"`
	Failing []Check `json:"failing,omitempty"`
}

// Review states, normalized by the adapter.
const (
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes_requested"
	ReviewCommented        = "commented"
)

// Review is one submitted review with its inline comments.
type Review struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Bot    bool   `json:"bot,omitempty"`
	State  string `json:"state"`
	// Official is the instance's own flag: the reviewer's approval counts
	// towards the branch's required approvals.
	Official  bool            `json:"official,omitempty"`
	Dismissed bool            `json:"dismissed,omitempty"`
	Body      string          `json:"body,omitempty"`
	Comments  []InlineComment `json:"comments,omitempty"`
}

// InlineComment is a review comment on a line of the diff.
type InlineComment struct {
	ID       int64  `json:"id"`
	Author   string `json:"author"`
	Bot      bool   `json:"bot,omitempty"`
	Path     string `json:"path,omitempty"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved,omitempty"`
}

// Comment is one conversation comment.
type Comment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Bot    bool   `json:"bot,omitempty"`
	Body   string `json:"body"`
}

// Observation is everything one evaluation needs about a pull request, read in
// one pass by the provider adapter.
type Observation struct {
	Number     int64
	State      PullState
	Draft      bool
	Fork       bool
	Title      string
	URL        string
	Repository string // owner/name, for display and prompts
	BaseBranch string
	HeadBranch string
	HeadSHA    string
	// Mergeable is nil while the instance has not settled it; a recent "no"
	// is reported as unknown by the adapter, as the review panel does.
	Mergeable *bool
	CI        CI
	Reviews   []Review
	Comments  []Comment
	// RequiredApprovals is nil when the branch's protection is not visible.
	RequiredApprovals *int
	// Operator is the account the plugin's token belongs to. Every comment and
	// review the agent posts through the plugin is authored by it.
	Operator string
	// OperatorRequested is true when the operator's review is requested,
	// directly or through one of its teams.
	OperatorRequested bool
}
