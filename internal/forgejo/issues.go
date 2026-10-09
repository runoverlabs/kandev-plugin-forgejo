package forgejo

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Label is the subset of a Forgejo label this plugin reads. Name is the value
// the issue list endpoint filters on; the colour and description are display
// data the board does not use.
type Label struct {
	Name string `json:"name"`
}

// Issue mirrors the REST v1 issue object, restricted to fields Forgejo and
// Gitea both populate.
//
// The repository issue endpoint returns issues AND pull requests in one shape:
// `pull_request` is non-nil only for a pull request. A watch must never turn a
// pull request into a task — this plugin already has a first-class pull
// request surface — so IsPullRequest is checked on every result even though
// the request also asks for type=issues.
type Issue struct {
	ID          int64   `json:"id"`
	Number      int64   `json:"number"`
	Title       string  `json:"title"`
	Body        string  `json:"body"`
	State       string  `json:"state"`
	HTMLURL     string  `json:"html_url"`
	Labels      []Label `json:"labels"`
	User        User    `json:"user"`
	Assignees   []User  `json:"assignees"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	PullRequest *struct {
		Merged bool `json:"merged"`
	} `json:"pull_request"`
}

// IsPullRequest reports whether this result is really a pull request.
func (i Issue) IsPullRequest() bool { return i.PullRequest != nil }

// LabelNames returns the issue's label names in declaration order.
func (i Issue) LabelNames() []string {
	names := make([]string, 0, len(i.Labels))
	for _, label := range i.Labels {
		if trimmed := strings.TrimSpace(label.Name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

// IssueListOptions is the query one watch issues against one repository.
//
// Labels are AND-ed by the server: an issue must carry every name listed. That
// matches the native GitHub watch's semantics and is the behaviour an operator
// expects from "only issues labelled X and Y".
type IssueListOptions struct {
	// State is "open", "closed", or "all". Empty means "open".
	State string
	// Labels filters to issues carrying every one of these label names.
	Labels []string
	// Query is a free-text search over title and body. Empty means no filter.
	//
	// It maps to the `q` parameter, which the per-repository issue endpoint
	// serves from the host's issue indexer. Note this is NOT the same spelling
	// the endpoint accepts for everything else: `keyword` is silently ignored
	// here, so a wrong name filters nothing rather than erroring. Verified
	// against Gitea 1.27 in the live contract tests.
	Query string
	// Since bounds the result to issues updated at or after this instant. A
	// watch passes its last successful poll time so a busy repository does not
	// re-transfer its whole backlog every interval. Zero means unbounded.
	Since time.Time
	// Page is 1-based. Limit is capped server-side.
	Page  int
	Limit int
}

// maxIssuePageLimit is the per-page ceiling this plugin asks for. Forgejo and
// Gitea both clamp to their own configured maximum, so this is a request, not
// a guarantee, and the caller must still paginate.
const maxIssuePageLimit = 50

// ListIssues returns one page of issues for a repository, excluding pull
// requests.
//
// Filtering happens server-side wherever the shared REST surface supports it,
// because a watch polling on an interval should not pull a repository's whole
// issue list across the network to discard most of it locally.
func (c *Client) ListIssues(ctx context.Context, owner, name string, options IssueListOptions) ([]Issue, error) {
	issues, _, err := c.listIssuesPage(ctx, owner, name, options)
	return issues, err
}

// listIssuesPage is ListIssues that also reports how many rows the server
// returned before pull requests were filtered out. A paginating caller needs
// that raw count: the filtered length is not an end-of-results signal, because
// a server that ignores type=issues can fill a page with pull requests and make
// a full page look short.
func (c *Client) listIssuesPage(ctx context.Context, owner, name string, options IssueListOptions) (issues []Issue, raw int, err error) {
	page := options.Page
	if page < 1 {
		page = 1
	}
	limit := options.Limit
	if limit <= 0 || limit > maxIssuePageLimit {
		limit = maxIssuePageLimit
	}

	values := url.Values{}
	// type=issues asks the server to exclude pull requests. Older Gitea
	// releases ignore the parameter, which is why every result is still
	// checked with IsPullRequest below.
	values.Set("type", "issues")
	state := strings.TrimSpace(options.State)
	if state == "" {
		state = "open"
	}
	values.Set("state", state)
	values.Set("page", strconv.Itoa(page))
	values.Set("limit", strconv.Itoa(limit))
	if labels := joinLabels(options.Labels); labels != "" {
		values.Set("labels", labels)
	}
	if query := strings.TrimSpace(options.Query); query != "" {
		values.Set("q", query)
	}
	if !options.Since.IsZero() {
		values.Set("since", options.Since.UTC().Format(time.RFC3339))
	}

	var results []Issue
	path := "/repos/" + pathSegment(owner) + "/" + pathSegment(name) + "/issues"
	if err := c.get(ctx, path, values, &results); err != nil {
		return nil, 0, err
	}

	issues = make([]Issue, 0, len(results))
	for _, issue := range results {
		if issue.IsPullRequest() {
			continue
		}
		issues = append(issues, issue)
	}
	return issues, len(results), nil
}

// joinLabels renders the label filter the issue endpoint expects: a
// comma-separated list of names. Empty entries are dropped so a trailing comma
// in operator input cannot turn into a filter on the empty label.
func joinLabels(labels []string) string {
	cleaned := make([]string, 0, len(labels))
	for _, label := range labels {
		if trimmed := strings.TrimSpace(label); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return strings.Join(cleaned, ",")
}
