package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The DTOs below carry only the fields this plugin actually reads, and only
// fields Forgejo and Gitea both populate. The README's "Verified against"
// table records which releases that has been checked on.

// User is the subset of a Forgejo user this plugin displays.
type User struct {
	Login string `json:"login"`
}

// Repo mirrors the REST v1 repository object. ID is the instance-immutable
// identifier and is the only value used as repository identity; full_name and
// clone_url are display/routing data that can change under a rename.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         User   `json:"owner"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
}

// OwnerLogin prefers the embedded owner object and falls back to parsing
// full_name, which some endpoints populate more reliably than others.
func (r Repo) OwnerLogin() string {
	if login := strings.TrimSpace(r.Owner.Login); login != "" {
		return login
	}
	owner, _, found := strings.Cut(strings.TrimSpace(r.FullName), "/")
	if found {
		return owner
	}
	return ""
}

// Branch mirrors the REST v1 branch object.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// PullRequestRef is one side of a pull request.
type PullRequestRef struct {
	Ref  string `json:"ref"`
	Sha  string `json:"sha"`
	Repo *Repo  `json:"repo"`
}

// PullRequest mirrors the REST v1 pull request object.
type PullRequest struct {
	ID             int64          `json:"id"`
	Number         int64          `json:"number"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	HTMLURL        string         `json:"html_url"`
	State          string         `json:"state"`
	Draft          bool           `json:"draft"`
	Merged         bool           `json:"merged"`
	Comments       int            `json:"comments"`
	ReviewComments int            `json:"review_comments"`
	UpdatedAt      string         `json:"updated_at"`
	Head           PullRequestRef `json:"head"`
	Base           PullRequestRef `json:"base"`
	User           User           `json:"user"`
}

// Review mirrors the REST v1 pull review object. Forgejo and Gitea both use
// the uppercase state vocabulary APPROVED / REQUEST_CHANGES / COMMENT / PENDING.
type Review struct {
	ID        int64  `json:"id"`
	State     string `json:"state"`
	Stale     bool   `json:"stale"`
	Dismissed bool   `json:"dismissed"`
	User      User   `json:"user"`
}

// CommitStatus is one entry of a combined status. The per-entry field is
// `status`, while the combined roll-up field is `state` — the two differ on
// both Forgejo and Gitea, so they are decoded separately.
type CommitStatus struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// CombinedStatus is the roll-up for one commit.
type CombinedStatus struct {
	State      string         `json:"state"`
	Sha        string         `json:"sha"`
	TotalCount int            `json:"total_count"`
	Statuses   []CommitStatus `json:"statuses"`
}

// SearchReposResponse is the envelope returned by /repos/search.
type SearchReposResponse struct {
	OK   bool   `json:"ok"`
	Data []Repo `json:"data"`
}

// IssueSearchResult is the subset of /repos/issues/search this plugin reads.
// The endpoint returns issues and pull requests in one shape; `pull_request`
// is non-nil only for pull requests.
type IssueSearchResult struct {
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	HTMLURL    string `json:"html_url"`
	Repository *struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Owner    string `json:"owner"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Merged bool `json:"merged"`
	} `json:"pull_request"`
}

// CurrentUser validates the configured token and returns the acting account.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var user User
	if err := c.get(ctx, "/user", nil, &user); err != nil {
		return User{}, err
	}
	return user, nil
}

// Version returns the instance version string. Forgejo reports a
// "<forgejo>+gitea-<compat>" value; Gitea reports its own version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var payload struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/version", nil, &payload); err != nil {
		return "", err
	}
	return payload.Version, nil
}

// Flavor names the software serving this instance, for display only. No
// behavior branches on it: the plugin targets the REST v1 surface both hosts
// share.
type Flavor string

const (
	// FlavorForgejo is a Forgejo instance.
	FlavorForgejo Flavor = "forgejo"
	// FlavorGitea is a Gitea instance.
	FlavorGitea Flavor = "gitea"
)

// DetectFlavor distinguishes Forgejo from Gitea by asking for Forgejo's own
// API namespace, which Gitea does not serve.
//
// The version string is not a reliable discriminator on its own: Forgejo
// reports "<version>+gitea-<compat>", but that suffix is a compatibility
// declaration Forgejo may stop publishing as it diverges. The namespace probe
// keeps working either way; the suffix is only a fallback for a deployment
// that proxies away the Forgejo namespace.
func (c *Client) DetectFlavor(ctx context.Context, version string) Flavor {
	var payload struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, apiForgejoV1, "/version", nil, nil, &payload); err == nil {
		return FlavorForgejo
	}
	if strings.Contains(strings.ToLower(version), "+gitea-") {
		return FlavorForgejo
	}
	return FlavorGitea
}

// SearchRepos returns one page of repositories the token can see. page is
// 1-based; Forgejo and Gitea both cap limit server-side.
func (c *Client) SearchRepos(ctx context.Context, query string, page, limit int) ([]Repo, error) {
	if page < 1 {
		page = 1
	}
	values := url.Values{}
	values.Set("page", strconv.Itoa(page))
	values.Set("limit", strconv.Itoa(limit))
	values.Set("sort", "updated")
	values.Set("order", "desc")
	if trimmed := strings.TrimSpace(query); trimmed != "" {
		values.Set("q", trimmed)
	}
	var response SearchReposResponse
	if err := c.get(ctx, "/repos/search", values, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

// Repo fetches one repository by owner and name.
func (c *Client) Repo(ctx context.Context, owner, name string) (Repo, error) {
	var repo Repo
	if err := c.get(ctx, "/repos/"+pathSegment(owner)+"/"+pathSegment(name), nil, &repo); err != nil {
		return Repo{}, err
	}
	return repo, nil
}

// RepoByID fetches one repository by its instance-immutable numeric id. This
// is the lookup used to re-resolve a stored identity, because it survives
// renames and transfers.
func (c *Client) RepoByID(ctx context.Context, id int64) (Repo, error) {
	var repo Repo
	if err := c.get(ctx, "/repositories/"+strconv.FormatInt(id, 10), nil, &repo); err != nil {
		return Repo{}, err
	}
	return repo, nil
}

// Branches returns one page of branches for a repository.
func (c *Client) Branches(ctx context.Context, owner, name string, page, limit int) ([]Branch, error) {
	if page < 1 {
		page = 1
	}
	values := url.Values{}
	values.Set("page", strconv.Itoa(page))
	values.Set("limit", strconv.Itoa(limit))
	var branches []Branch
	if err := c.get(ctx, "/repos/"+pathSegment(owner)+"/"+pathSegment(name)+"/branches", values, &branches); err != nil {
		return nil, err
	}
	return branches, nil
}

// CreatePullRequestInput is the request body for opening a pull request.
type CreatePullRequestInput struct {
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

// CreatePullRequest opens a pull request and returns the created object.
func (c *Client) CreatePullRequest(ctx context.Context, owner, name string, input CreatePullRequestInput) (PullRequest, error) {
	var created PullRequest
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/pulls"
	if err := c.post(ctx, path, input, &created); err != nil {
		return PullRequest{}, err
	}
	return created, nil
}

// PullRequest fetches one pull request by its per-repository number.
func (c *Client) PullRequest(ctx context.Context, owner, name string, number int64) (PullRequest, error) {
	var pull PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", pathSegment(owner), pathSegment(name), number)
	if err := c.get(ctx, path, nil, &pull); err != nil {
		return PullRequest{}, err
	}
	return pull, nil
}

// Reviews returns submitted reviews for a pull request.
func (c *Client) Reviews(ctx context.Context, owner, name string, number int64) ([]Review, error) {
	var reviews []Review
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", pathSegment(owner), pathSegment(name), number)
	if err := c.get(ctx, path, nil, &reviews); err != nil {
		return nil, err
	}
	return reviews, nil
}

// CombinedStatus returns the roll-up commit status for a ref. A repository
// with no CI configured returns an empty status list rather than an error.
func (c *Client) CombinedStatus(ctx context.Context, owner, name, ref string) (CombinedStatus, error) {
	var status CombinedStatus
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/commits/" + pathSegment(ref) + "/status"
	if err := c.get(ctx, path, nil, &status); err != nil {
		return CombinedStatus{}, err
	}
	return status, nil
}

// SearchPullRequests returns pull requests visible to the token across the
// instance, newest first. It backs the composer `#` reference source.
func (c *Client) SearchPullRequests(ctx context.Context, query string, limit int) ([]IssueSearchResult, error) {
	values := url.Values{}
	values.Set("type", "pulls")
	values.Set("limit", strconv.Itoa(limit))
	values.Set("state", "all")
	values.Set("sort", "recentupdate")
	if trimmed := strings.TrimSpace(query); trimmed != "" {
		values.Set("q", trimmed)
	}
	var results []IssueSearchResult
	if err := c.get(ctx, "/repos/issues/search", values, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// pathSegment escapes a single URL path segment. Owner and repository names
// are operator/provider data and must never be interpolated raw.
func pathSegment(value string) string {
	return url.PathEscape(strings.TrimSpace(value))
}

// ListPullRequests returns one page of pull requests, newest update first.
// state is "open", "closed", or "all".
func (c *Client) ListPullRequests(ctx context.Context, owner, name, state string, page, limit int) ([]PullRequest, error) {
	if page < 1 {
		page = 1
	}
	values := url.Values{}
	values.Set("state", state)
	values.Set("sort", "recentupdate")
	values.Set("page", strconv.Itoa(page))
	values.Set("limit", strconv.Itoa(limit))
	var pulls []PullRequest
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/pulls"
	if err := c.get(ctx, path, values, &pulls); err != nil {
		return nil, err
	}
	return pulls, nil
}

// EditPullRequestInput is the subset of the edit body this plugin sends. Every
// field is omitted when empty so an edit never clears a value it did not set.
type EditPullRequestInput struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	State string `json:"state,omitempty"`
}

// EditPullRequest applies a partial update and returns the updated object.
func (c *Client) EditPullRequest(ctx context.Context, owner, name string, number int64, input EditPullRequestInput) (PullRequest, error) {
	var updated PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", pathSegment(owner), pathSegment(name), number)
	if err := c.patch(ctx, path, input, &updated); err != nil {
		return PullRequest{}, err
	}
	return updated, nil
}
