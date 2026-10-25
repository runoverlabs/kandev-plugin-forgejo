package forgejo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kandev-plugin-forgejo/internal/prautomation"
)

// Team is a team as the pull request and /user/teams report it.
type Team struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Organization *struct {
		UserName string `json:"username"`
	} `json:"organization"`
}

// ReviewThreadComment is an inline comment of a submitted review.
type ReviewThreadComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	Path string `json:"path"`
	// Position is the line on the new side, OriginalPosition on the old; the
	// other is 0. With the path they place the comment's conversation.
	Position         int64 `json:"position"`
	OriginalPosition int64 `json:"original_position"`
	User             User  `json:"user"`
	// Resolver is set on the comment a conversation was resolved from. There
	// is no API to resolve one, only the web UI, which resolves from the
	// conversation's first comment and reads its state from there alone.
	Resolver *User `json:"resolver"`
}

// ReviewComments lists the inline comments of one review.
func (c *Client) ReviewComments(ctx context.Context, owner, name string, number, reviewID int64) ([]ReviewThreadComment, error) {
	var comments []ReviewThreadComment
	path := pullPath(owner, name, number, "/reviews/"+strconv.FormatInt(reviewID, 10)+"/comments")
	if err := c.get(ctx, path, nil, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// UserTeams lists the teams the token's account belongs to.
func (c *Client) UserTeams(ctx context.Context) ([]Team, error) {
	teams, _, err := pageAll[Team](ctx, c, "/user/teams", maxUserTeams)
	return teams, err
}

// errStatusesCut: the head has more checks than one observation reads, or they
// changed while they were read. A check that was not read may be failing.
var errStatusesCut = errors.New("forgejo: not every commit status could be read")

// Bounds on one observation.
const (
	maxUserTeams          = 200
	maxObservedComments   = 300
	maxReviewCommentFetch = 20
	operatorCacheLifetime = 10 * time.Minute
)

// AutomationSource reads linked pull requests for pull request automation. It
// implements the neutral package's Observer and Links ports, and exposes the
// merge the plugin wraps into its Merger.
type AutomationSource struct {
	connection   *Connection
	associations *Associations
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
func NewAutomationSource(connection *Connection, associations *Associations) *AutomationSource {
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
func (s *AutomationSource) repository(ctx context.Context, ref prautomation.PullRef) (*Client, Repo, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return nil, Repo{}, err
	}
	id, err := parseRepositoryID(ref.RepositoryID)
	if err != nil {
		return nil, Repo{}, err
	}
	repo, err := client.RepoByID(ctx, id)
	if err != nil {
		return nil, Repo{}, err
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
	obs := prautomation.Observation{
		Number:     pull.Number,
		State:      automationState(pull),
		Draft:      pull.Draft || hasWIPPrefix(pull.Title),
		Fork:       isForkPull(pull),
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
		obs.URL = PullRequestURL(client.Scope(), owner, name, pull.Number)
	}
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
	var statuses []CommitStatus
	if sha := strings.TrimSpace(pull.Head.Sha); sha != "" {
		combined, err := client.CombinedStatus(ctx, owner, name, sha)
		if err != nil {
			return prautomation.Observation{}, err
		}
		if len(combined.Statuses) < combined.TotalCount {
			return prautomation.Observation{}, errStatusesCut
		}
		obs.CI = automationCI(combined)
		statuses = combined.Statuses
	}

	if base := strings.TrimSpace(pull.Base.Ref); base != "" {
		branch, err := client.BranchDetail(ctx, owner, name, base)
		if err != nil {
			return prautomation.Observation{}, err
		}
		required := int(branch.RequiredApprovals)
		obs.RequiredApprovals = &required
		if branch.EnableStatusCheck {
			obs.CI.Missing = missingRequiredChecks(branch.StatusCheckContext, statuses)
		}
	}

	reviews, truncated, err := client.NewestReviews(ctx, owner, name, ref.Number)
	if err != nil {
		return prautomation.Observation{}, err
	}
	obs.ReviewsIncomplete = truncated
	for _, review := range reviews {
		if state := automationReviewState(review.State); state != "" {
			obs.Reviews = append(obs.Reviews, prautomation.Review{
				ID:        review.ID,
				Author:    review.User.Login,
				Bot:       isBotUser(review.User),
				State:     state,
				Official:  review.Official,
				Dismissed: review.Dismissed,
				Body:      review.Body,
			})
		}
	}
	complete, err := observeReviewComments(ctx, client, owner, name, ref.Number, reviews, obs.Reviews)
	if err != nil {
		return prautomation.Observation{}, err
	}
	if !complete {
		obs.ReviewsIncomplete = true
	}

	comments, _, err := client.IssueComments(ctx, owner, name, ref.Number, maxObservedComments)
	if err != nil {
		return prautomation.Observation{}, err
	}
	for _, comment := range comments {
		obs.Comments = append(obs.Comments, prautomation.Comment{
			ID:     comment.ID,
			Author: comment.User.Login,
			Bot:    isBotUser(comment.User),
			Body:   comment.Body,
		})
	}
	return obs, nil
}

// observeReviewComments fills in the inline comments of the observed reviews,
// which are in the order of the raw ones they came from, and reports whether
// it read them all. They are fetched per review, newest first: the bound keeps
// a long review history from costing a request per review, and the newest
// reviews are the ones still open. Reviews past the bound are reported, so
// auto-merge does not take an unread comment for a resolved one. A dismissed
// review's comments are read too: a conversation it started carries the
// resolver for replies filed under other, live reviews.
func observeReviewComments(ctx context.Context, client *Client, owner, name string, number int64, raw []Review, observed []prautomation.Review) (bool, error) {
	byID := make(map[int64]int, len(observed))
	for i, review := range observed {
		byID[review.ID] = i
	}
	type placed struct {
		review, comment int
		id              int64
		thread          string
		resolved        bool
	}
	var all []placed
	fetched, complete := 0, true
	for i := len(raw) - 1; i >= 0; i-- {
		review := raw[i]
		index, ok := byID[review.ID]
		if !ok || review.CommentsCount == 0 {
			continue
		}
		if fetched == maxReviewCommentFetch {
			complete = false
			break
		}
		fetched++
		comments, err := client.ReviewComments(ctx, owner, name, number, review.ID)
		if errors.Is(err, ErrNotFound) {
			// Deleted since the list was read: it has nothing left to say.
			continue
		}
		if err != nil {
			return false, err
		}
		for _, comment := range comments {
			all = append(all, placed{
				review: index, comment: len(observed[index].Comments), id: comment.ID,
				thread:   fmt.Sprintf("%s\x00%d\x00%d", comment.Path, comment.Position, comment.OriginalPosition),
				resolved: comment.Resolver != nil && comment.Resolver.ID != 0,
			})
			observed[index].Comments = append(observed[index].Comments, prautomation.InlineComment{
				ID:     comment.ID,
				Author: comment.User.Login,
				Bot:    isBotUser(comment.User),
				Path:   comment.Path,
				Body:   comment.Body,
			})
		}
	}
	// A conversation is resolved when its first comment is, and so is every
	// reply in it. It is grouped by place, not by review, as the web UI does:
	// Gitea files a reply under the replier's pending review when there is one.
	first := map[string]placed{}
	for _, comment := range all {
		if current, ok := first[comment.thread]; !ok || comment.id < current.id {
			first[comment.thread] = comment
		}
	}
	for _, comment := range all {
		observed[comment.review].Comments[comment.comment].Resolved = first[comment.thread].resolved
	}
	return complete, nil
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
	return isForkPull(pull), nil
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
		style = MergeStyleMerge
	}
	return client.MergePullRequest(ctx, repo.OwnerLogin(), repo.Name, ref.Number, MergeInput{
		Do:           style,
		HeadCommitID: headSHA,
		DeleteBranch: repo.DefaultDeleteBranch,
	})
}

func (s *AutomationSource) operatorIdentity(ctx context.Context, client *Client) (string, map[string]bool, error) {
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
func operatorRequested(pull PullRequest, owner, login string, teams map[string]bool) bool {
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

func automationState(pull PullRequest) prautomation.PullState {
	switch {
	case pull.Merged:
		return prautomation.StateMerged
	case strings.EqualFold(strings.TrimSpace(pull.State), "closed"):
		return prautomation.StateClosed
	default:
		return prautomation.StateOpen
	}
}

// isForkPull: the head lives in another repository, or in one that is gone (a
// deleted fork), which is how the webhook decoding decides it too.
func isForkPull(pull PullRequest) bool {
	if pull.Head.Repo == nil && pull.Head.RepoID == 0 {
		return true
	}
	headID := pull.Head.RepoID
	if headID == 0 && pull.Head.Repo != nil {
		headID = pull.Head.Repo.ID
	}
	baseID := pull.Base.RepoID
	if baseID == 0 && pull.Base.Repo != nil {
		baseID = pull.Base.Repo.ID
	}
	return headID != baseID
}

// automationCI maps a combined status. A cancelled or skipped check is not a
// failure: nothing in it asks for a change.
func automationCI(combined CombinedStatus) prautomation.CI {
	ci := prautomation.CI{State: prautomation.CINone}
	if len(combined.Statuses) == 0 {
		return ci
	}
	pending := false
	for _, status := range combined.Statuses {
		switch strings.ToLower(strings.TrimSpace(status.Status)) {
		case "failure", "error":
			name := strings.TrimSpace(status.Context)
			if name == "" {
				name = "check"
			}
			ci.Failing = append(ci.Failing, prautomation.Check{
				Name:  name,
				State: strings.ToLower(strings.TrimSpace(status.Status)),
				URL:   strings.TrimSpace(status.TargetURL),
			})
		case "pending", "running":
			pending = true
		}
	}
	switch {
	case pending:
		ci.State = prautomation.CIPending
	case len(ci.Failing) > 0:
		ci.State = prautomation.CIFailure
	default:
		ci.State = prautomation.CISuccess
	}
	return ci
}

// missingRequiredChecks lists the required context patterns no status on the
// head matches. The instance holds such a pattern pending and refuses the
// merge, so a check that has not reported yet is not a green build even when
// every check that has is.
func missingRequiredChecks(patterns []string, statuses []CommitStatus) []string {
	var missing []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		matcher := contextGlob(pattern)
		found := false
		for _, status := range statuses {
			if matcher != nil && matcher.MatchString(status.Context) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, pattern)
		}
	}
	return missing
}

// contextGlob compiles a required context pattern as the instance does
// (gobwas/glob without separators): "*" matches any run of characters, "/"
// included, "?" one character, "[...]" a class ("[!...]" negated) and
// "{a,b}" either alternative. A pattern it cannot compile matches nothing, so
// auto-merge waits rather than guessing.
func contextGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			end := i + 1
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end == len(runes) {
				return nil
			}
			class := string(runes[i+1 : end])
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = end
		case '{':
			depth++
			b.WriteString("(?:")
		case '}':
			if depth == 0 {
				return nil
			}
			depth--
			b.WriteString(")")
		case ',':
			if depth > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '\\':
			if i+1 < len(runes) {
				i++
				b.WriteString(regexp.QuoteMeta(string(runes[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if depth != 0 {
		return nil
	}
	b.WriteString("$")
	compiled, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return compiled
}

func automationReviewState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED":
		return prautomation.ReviewApproved
	case "REQUEST_CHANGES", "CHANGES_REQUESTED":
		return prautomation.ReviewChangesRequested
	case "COMMENT", "COMMENTED":
		return prautomation.ReviewCommented
	default:
		// PENDING (an unsubmitted draft review) and REQUEST_REVIEW are not
		// reviews anyone wrote.
		return ""
	}
}

// isBotUser recognises the accounts the instance itself posts as: the Actions
// user and the ghost user have negative ids on both hosts. Neither host marks
// other bots; a "[bot]" login suffix is the common convention.
func isBotUser(user User) bool {
	login := strings.ToLower(strings.TrimSpace(user.Login))
	return user.ID < 0 || strings.HasSuffix(login, "[bot]") ||
		login == "forgejo-actions" || login == "gitea-actions"
}

// settleMergeable returns the pull request's mergeable flag, or nil (unknown)
// when it says false but the pull request changed too recently to believe it.
// A true answer is always passed through: a stale true is caught by the head SHA
// precondition on merge, whereas a stale false would wrongly block.
func settleMergeable(pull PullRequest, now time.Time) *bool {
	if pull.Mergeable == nil || *pull.Mergeable {
		return pull.Mergeable
	}
	updated := parseTimestampMillis(pull.UpdatedAt)
	if updated > 0 && now.Sub(time.UnixMilli(updated)) < mergeableSettle {
		return nil
	}
	return pull.Mergeable
}
