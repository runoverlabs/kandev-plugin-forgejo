package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// ChangeRequests implements the recipe's ChangeRequestService port. Forgejo
// calls these pull requests; the recipe's provider-neutral vocabulary calls
// them change requests.
type ChangeRequests struct {
	connection   *Connection
	repositories *Repositories
}

var _ sourcecontrol.ChangeRequestService = (*ChangeRequests)(nil)

// NewChangeRequests returns the pull-request adapter.
func NewChangeRequests(connection *Connection, repositories *Repositories) *ChangeRequests {
	return &ChangeRequests{connection: connection, repositories: repositories}
}

// ownerRepoNumberPattern matches the canonical "owner/repo#123" reference.
var ownerRepoNumberPattern = regexp.MustCompile(`^([^/\s]+)/([^#\s]+)#(\d+)$`)

// ResolveReference turns an operator-pasted reference into a verified identity.
// Resolution is server-side and authenticated: a reference is only accepted
// once the instance confirms the pull request exists and is visible.
func (c *ChangeRequests) ResolveReference(ctx context.Context, _ string, reference string) (sourcecontrol.ChangeRequest, error) {
	client, err := c.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.ChangeRequest{}, err
	}
	owner, name, number, ok := ParsePullRequestReference(client.Scope(), reference)
	if !ok {
		return sourcecontrol.ChangeRequest{}, errors.New("forgejo: reference must be a pull request URL or owner/repo#number on the configured instance")
	}
	pull, err := client.PullRequest(ctx, owner, name, number)
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) {
			return sourcecontrol.ChangeRequest{}, errors.New("forgejo: pull request was not found on the configured instance")
		}
		return sourcecontrol.ChangeRequest{}, err
	}
	repo, err := client.Repo(ctx, owner, name)
	if err != nil {
		return sourcecontrol.ChangeRequest{}, err
	}
	if repo.ID <= 0 {
		return sourcecontrol.ChangeRequest{}, errors.New("forgejo: resolved repository has no identity")
	}
	return sourcecontrol.ChangeRequest{
		Identity: sourcecontrol.ChangeRequestIdentity{
			ConnectionScope: client.Scope(),
			RepositoryID:    strconv.FormatInt(repo.ID, 10),
			Number:          pull.Number,
		},
		Title: strings.TrimSpace(pull.Title),
		URL:   strings.TrimSpace(pull.HTMLURL),
	}, nil
}

// Create opens a pull request from the verified head branch. The caller has
// already proven the repository and head branch through
// VerifiedActionContext; only title, body, destination and draft come from the
// browser.
func (c *ChangeRequests) Create(ctx context.Context, repository sourcecontrol.Repository, headBranch string, input sourcecontrol.CreateChangeRequestInput) (sourcecontrol.ChangeRequest, error) {
	client, err := c.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.ChangeRequest{}, err
	}
	head := strings.TrimSpace(headBranch)
	if head == "" {
		return sourcecontrol.ChangeRequest{}, errors.New("forgejo: verified head branch is required")
	}
	base := strings.TrimSpace(input.Destination)
	if base == "" {
		base = strings.TrimSpace(repository.DefaultBranch)
	}
	if base == "" {
		return sourcecontrol.ChangeRequest{}, errors.New("forgejo: no destination branch was supplied and the repository has no default branch")
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return sourcecontrol.ChangeRequest{}, errors.New("forgejo: pull request title is required")
	}

	body := input.Description
	// Neither Forgejo nor Gitea expose a draft flag on pull-request creation
	// in REST v1. The portable convention both UIs recognize is a "WIP:"
	// title prefix, so honor the caller's draft request that way rather than
	// silently dropping it.
	if input.Draft && !IsWorkInProgressTitle(title) {
		title = "WIP: " + title
	}

	created, err := client.CreatePullRequest(ctx, repository.OwnerOrProject, repository.Name, CreatePullRequestInput{
		Head:  head,
		Base:  base,
		Title: title,
		Body:  body,
	})
	if err != nil {
		return sourcecontrol.ChangeRequest{}, err
	}

	identity := sourcecontrol.ChangeRequestIdentity{
		ConnectionScope: client.Scope(),
		RepositoryID:    repository.RepositoryID,
		Number:          created.Number,
	}
	htmlURL := strings.TrimSpace(created.HTMLURL)
	if htmlURL == "" {
		// Fall back to the canonical web path so the recipe still has a URL
		// to return; a created remote PR must never look like a failure.
		htmlURL = fmt.Sprintf("%s/%s/%s/pulls/%d", client.Scope(),
			url.PathEscape(repository.OwnerOrProject), url.PathEscape(repository.Name), created.Number)
	}
	return sourcecontrol.ChangeRequest{
		Identity: identity,
		Title:    strings.TrimSpace(created.Title),
		URL:      htmlURL,
	}, nil
}

// workInProgressPrefixes are the title markers Forgejo and Gitea both treat as
// a draft. REST v1 has no draft flag on either host, so the prefix is the
// portable representation.
var workInProgressPrefixes = []string{"WIP:", "[WIP]", "DRAFT:", "[DRAFT]"}

// IsWorkInProgressTitle reports whether a title already carries one of the
// work-in-progress prefixes Forgejo and Gitea treat as a draft marker.
func IsWorkInProgressTitle(title string) bool {
	_, found := workInProgressPrefix(title)
	return found
}

// StripWorkInProgressPrefix removes a leading draft marker and reports whether
// one was there. It is the "mark ready for review" operation on a host with no
// draft flag.
func StripWorkInProgressPrefix(title string) (string, bool) {
	prefix, found := workInProgressPrefix(title)
	if !found {
		return strings.TrimSpace(title), false
	}
	return strings.TrimSpace(strings.TrimSpace(title)[len(prefix):]), true
}

// workInProgressPrefix returns the marker a title starts with, as it appears
// in the title rather than upper-cased, so it can be sliced off exactly.
func workInProgressPrefix(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	upper := strings.ToUpper(trimmed)
	for _, prefix := range workInProgressPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return trimmed[:len(prefix)], true
		}
	}
	return "", false
}

// ParsePullRequestReference accepts a full pull-request URL on the configured
// instance or a canonical "owner/repo#number" reference. It deliberately
// rejects a bare "#123": without a repository the identity is ambiguous.
func ParsePullRequestReference(scope, reference string) (string, string, int64, bool) {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return "", "", 0, false
	}
	if match := ownerRepoNumberPattern.FindStringSubmatch(trimmed); match != nil {
		number, err := strconv.ParseInt(match[3], 10, 64)
		if err != nil || number <= 0 {
			return "", "", 0, false
		}
		return match[1], strings.TrimSuffix(match[2], ".git"), number, true
	}
	if !strings.Contains(trimmed, "://") {
		return "", "", 0, false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", "", 0, false
	}
	scopeURL, err := url.Parse(scope)
	if err != nil || !strings.EqualFold(parsed.Hostname(), scopeURL.Hostname()) {
		return "", "", 0, false
	}
	path := strings.Trim(parsed.Path, "/")
	if base := strings.Trim(scopeURL.Path, "/"); base != "" {
		if !strings.HasPrefix(path, base+"/") {
			return "", "", 0, false
		}
		path = strings.TrimPrefix(path, base+"/")
	}
	segments := strings.Split(path, "/")
	// Both web ("/owner/repo/pulls/1") and API ("/owner/repo/pulls/1") shapes
	// carry the same four leading segments.
	if len(segments) < 4 || segments[2] != "pulls" {
		return "", "", 0, false
	}
	number, err := strconv.ParseInt(segments[3], 10, 64)
	if err != nil || number <= 0 {
		return "", "", 0, false
	}
	owner := strings.TrimSpace(segments[0])
	name := strings.TrimSuffix(strings.TrimSpace(segments[1]), ".git")
	if owner == "" || name == "" {
		return "", "", 0, false
	}
	return owner, name, number, true
}

// PullRequestURL builds the canonical web URL for a pull request. It is a
// display/routing fallback only, never an identity.
func PullRequestURL(scope, owner, name string, number int64) string {
	return fmt.Sprintf("%s/%s/%s/pulls/%d", strings.TrimSuffix(scope, "/"), owner, name, number)
}
