package forgejo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// errStatusesCut: the head has more checks than one observation reads, or they
// changed while they were read. A check that was not read may be failing.
var errStatusesCut = errors.New("forgejo: not every commit status could be read")

// Bounds on one observation.
const (
	maxObservedComments   = 300
	maxReviewCommentFetch = 20
	operatorCacheLifetime = 10 * time.Minute
)

// AutomationSource reads linked pull requests for pull request automation. It
// implements the neutral package's Observer and Links ports, and exposes the
// merge the plugin wraps into its Merger.
type AutomationSource struct {
	connection   *Connection
	associations *sourcecontrol.Associations
	now          func() time.Time

	mu       sync.Mutex
	operator operatorCache
}

// operatorCache holds the token's login and teams for a while: they are read on
// every observation and change rarely.
type operatorCache struct {
	scope   string
	login   string
	teams   map[string]bool // "org/team", lower case
	fetched time.Time
}

var (
	_ prautomation.Observer = (*AutomationSource)(nil)
	_ prautomation.Links    = (*AutomationSource)(nil)
)

// NewAutomationSource returns the automation adapter.
func NewAutomationSource(connection *Connection, associations *sourcecontrol.Associations) *AutomationSource {
	return &AutomationSource{connection: connection, associations: associations, now: time.Now}
}

// Linked lists the pull requests linked to tasks in a workspace on the
// configured instance. Links to another instance (the URL was changed) are
// left out, as everywhere else.
func (s *AutomationSource) Linked(ctx context.Context, workspaceID string) ([]prautomation.PullRef, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return nil, err
	}
	identities, taskIDs, err := s.associations.ListForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	refs := make([]prautomation.PullRef, 0, len(identities))
	for i, identity := range identities {
		if identity.ConnectionScope != client.Scope() {
			continue
		}
		refs = append(refs, prautomation.PullRef{TaskID: taskIDs[i], RepositoryID: identity.RepositoryID, Number: identity.Number})
	}
	return refs, nil
}

// repository resolves a reference's repository by its immutable id.
func (s *AutomationSource) repository(ctx context.Context, ref prautomation.PullRef) (*rest.Client, rest.Repo, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return nil, rest.Repo{}, err
	}
	id, err := parseRepositoryID(ref.RepositoryID)
	if err != nil {
		return nil, rest.Repo{}, err
	}
	repo, err := client.RepoByID(ctx, id)
	if err != nil {
		return nil, rest.Repo{}, err
	}
	return client, repo, nil
}

// Observe reads what automation decides on, in one pass: the pull request,
// and of the rest only the parts reads asks for.
func (s *AutomationSource) Observe(ctx context.Context, _ string, ref prautomation.PullRef, reads prautomation.Reads) (prautomation.Observation, error) {
	client, repo, err := s.repository(ctx, ref)
	if err != nil {
		return prautomation.Observation{}, err
	}
	owner, name := repo.OwnerLogin(), repo.Name
	pull, err := client.PullRequest(ctx, owner, name, ref.Number)
	if err != nil {
		return prautomation.Observation{}, err
	}
	obs := s.observePull(client.Scope(), owner, name, pull)
	if obs.State != prautomation.StateOpen {
		// A closed or merged pull request only feeds the lifecycle prompts,
		// which need nothing more.
		return obs, nil
	}

	if reads.ReviewRequest || reads.Feedback {
		// The feedback needs the operator too: their own words are not feedback.
		login, teams, err := s.operatorIdentity(ctx, client)
		if err != nil {
			return prautomation.Observation{}, err
		}
		obs.Operator = login
		obs.OperatorRequested = operatorRequested(pull, owner, login, teams)
	}
	if !reads.Feedback {
		return obs, nil
	}

	// Every read below feeds a merge gate, so none fails soft: a token that
	// cannot see the checks or the reviews fails the observation instead of
	// reading as no CI or no objections.
	if err := observeChecks(ctx, client, owner, name, pull, &obs); err != nil {
		return prautomation.Observation{}, err
	}
	if err := observeReviews(ctx, client, owner, name, ref.Number, &obs); err != nil {
		return prautomation.Observation{}, err
	}
	comments, _, err := client.IssueComments(ctx, owner, name, ref.Number, maxObservedComments)
	if err != nil {
		return prautomation.Observation{}, err
	}
	obs.Comments = automationComments(comments)
	return obs, nil
}

// observePull maps the pull request itself, before any further read.
func (s *AutomationSource) observePull(scope, owner, name string, pull rest.PullRequest) prautomation.Observation {
	obs := prautomation.Observation{
		Number:     pull.Number,
		State:      automationState(pull),
		Draft:      pull.Draft || hasWIPPrefix(pull.Title),
		Fork:       isFork(pull),
		Title:      strings.TrimSpace(pull.Title),
		URL:        strings.TrimSpace(pull.HTMLURL),
		Repository: owner + "/" + name,
		BaseBranch: pull.Base.Ref,
		HeadBranch: pull.Head.Ref,
		HeadSHA:    pull.Head.Sha,
		Mergeable:  settleMergeable(pull, s.now()),
		CI:         prautomation.CI{State: prautomation.CINone},
	}
	if obs.URL == "" {
		obs.URL = PullRequestURL(scope, owner, name, pull.Number)
	}
	return obs
}

// observeChecks reads the head's statuses and the base branch's protection:
// the required approvals, and the required checks that have not reported.
func observeChecks(ctx context.Context, client *rest.Client, owner, name string, pull rest.PullRequest, obs *prautomation.Observation) error {
	var statuses []rest.CommitStatus
	if sha := strings.TrimSpace(pull.Head.Sha); sha != "" {
		combined, err := client.CombinedStatus(ctx, owner, name, sha)
		if err != nil {
			return err
		}
		if len(combined.Statuses) < combined.TotalCount {
			return errStatusesCut
		}
		obs.CI = automationCI(combined)
		statuses = combined.Statuses
	}

	base := strings.TrimSpace(pull.Base.Ref)
	if base == "" {
		return nil
	}
	branch, err := client.BranchDetail(ctx, owner, name, base)
	if err != nil {
		return err
	}
	required := int(branch.RequiredApprovals)
	obs.RequiredApprovals = &required
	if branch.EnableStatusCheck {
		obs.CI.Missing = missingRequiredChecks(branch.StatusCheckContext, statuses)
	}
	return nil
}

// observeReviews reads the newest reviews and their inline comments.
func observeReviews(ctx context.Context, client *rest.Client, owner, name string, number int64, obs *prautomation.Observation) error {
	reviews, truncated, err := client.NewestReviews(ctx, owner, name, number)
	if err != nil {
		return err
	}
	obs.ReviewsIncomplete = truncated
	obs.Reviews = automationReviews(reviews)
	complete, err := observeReviewComments(ctx, client, owner, name, number, reviews, obs.Reviews)
	if err != nil {
		return err
	}
	if !complete {
		obs.ReviewsIncomplete = true
	}
	return nil
}

// IsFork reports whether a linked pull request comes from a fork. The panel
// needs it before the loop has observed the pull request.
func (s *AutomationSource) IsFork(ctx context.Context, ref prautomation.PullRef) (bool, error) {
	client, repo, err := s.repository(ctx, ref)
	if err != nil {
		return false, err
	}
	pull, err := client.PullRequest(ctx, repo.OwnerLogin(), repo.Name, ref.Number)
	if err != nil {
		return false, err
	}
	return isFork(pull), nil
}

// MergeLinked merges a linked pull request with the repository's default style
// and branch deletion, pinned to headSHA: the instance answers 409 if the head
// moved since the gates were checked.
//
// Not merge_when_checks_succeed: probed on Forgejo 7 to 17, a scheduled merge
// never fires when the checks were already green or the repository has no CI,
// and on every host it merges a head pushed after scheduling, which the head
// pin does not prevent.
func (s *AutomationSource) MergeLinked(ctx context.Context, ref prautomation.PullRef, headSHA string) error {
	client, repo, err := s.repository(ctx, ref)
	if err != nil {
		return err
	}
	style := strings.TrimSpace(repo.DefaultMergeStyle)
	if style == "" {
		style = rest.MergeStyleMerge
	}
	return client.MergePullRequest(ctx, repo.OwnerLogin(), repo.Name, ref.Number, rest.MergeInput{
		Do:           style,
		HeadCommitID: headSHA,
		DeleteBranch: repo.DefaultDeleteBranch,
	})
}

func (s *AutomationSource) operatorIdentity(ctx context.Context, client *rest.Client) (string, map[string]bool, error) {
	s.mu.Lock()
	cached := s.operator
	s.mu.Unlock()
	if cached.scope == client.Scope() && s.now().Sub(cached.fetched) < operatorCacheLifetime {
		return cached.login, cached.teams, nil
	}
	me, err := client.CurrentUser(ctx)
	if err != nil {
		return "", nil, err
	}
	teams := map[string]bool{}
	// The plugin's documented scopes can list teams (checked on Forgejo 17 and
	// Gitea 1.27). If the list fails anyway, team requests go unrecognised and
	// direct ones still count.
	if list, err := client.UserTeams(ctx); err == nil {
		for _, team := range list {
			if team.Organization != nil {
				teams[strings.ToLower(team.Organization.UserName+"/"+team.Name)] = true
			}
		}
	}
	s.mu.Lock()
	s.operator = operatorCache{scope: client.Scope(), login: me.Login, teams: teams, fetched: s.now()}
	s.mu.Unlock()
	return me.Login, teams, nil
}

// operatorRequested reports a review request for the operator, directly or
// through a team of the repository's owner it belongs to.
func operatorRequested(pull rest.PullRequest, owner, login string, teams map[string]bool) bool {
	for _, user := range pull.Requested {
		if login != "" && strings.EqualFold(user.Login, login) {
			return true
		}
	}
	for _, team := range pull.RequestedTeams {
		if teams[strings.ToLower(owner+"/"+team.Name)] {
			return true
		}
	}
	return false
}

func automationState(pull rest.PullRequest) prautomation.PullState {
	switch {
	case pull.Merged:
		return prautomation.StateMerged
	case strings.EqualFold(strings.TrimSpace(pull.State), "closed"):
		return prautomation.StateClosed
	default:
		return prautomation.StateOpen
	}
}

// settleMergeable returns the pull request's mergeable flag, or nil (unknown)
// when it says false but the pull request changed too recently to believe it.
// A true answer is always passed through: a stale true is caught by the head SHA
// precondition on merge, whereas a stale false would wrongly block.
func settleMergeable(pull rest.PullRequest, now time.Time) *bool {
	if pull.Mergeable == nil || *pull.Mergeable {
		return pull.Mergeable
	}
	updated := parseTimestampMillis(pull.UpdatedAt)
	if updated > 0 && now.Sub(time.UnixMilli(updated)) < mergeableSettle {
		return nil
	}
	return pull.Mergeable
}
