package forgejo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// References implements the recipe's ReferenceService port, backing the
// composer's `#` picker.
type References struct {
	connection *Connection
}

var _ sourcecontrol.ReferenceService = (*References)(nil)

// NewReferences returns the composer reference adapter.
func NewReferences(connection *Connection) *References {
	return &References{connection: connection}
}

// Search returns bounded display candidates. Candidate selection is not
// authorization — Authorize re-checks access at submission time.
func (r *References) Search(ctx context.Context, _ string, query string, limit int) ([]pluginsdk.EntityReferenceCandidate, error) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		if errors.Is(err, scm.ErrNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	results, err := client.SearchPullRequests(ctx, query, limit)
	if err != nil {
		if errors.Is(err, scm.ErrUnauthorized) {
			return nil, nil
		}
		return nil, err
	}
	candidates := make([]pluginsdk.EntityReferenceCandidate, 0, len(results))
	for _, result := range results {
		candidate, ok := referenceCandidate(client.Scope(), result)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate)
		if len(candidates) >= limit {
			break
		}
	}
	return candidates, nil
}

// referenceCandidate maps one search result, or reports it unusable: without a
// repository id, owner, name or number it cannot become a reference.
func referenceCandidate(scope string, result rest.IssueSearchResult) (pluginsdk.EntityReferenceCandidate, bool) {
	if result.Repository == nil || result.Number <= 0 {
		return pluginsdk.EntityReferenceCandidate{}, false
	}
	owner, name := repositoryName(result.Repository.Owner, result.Repository.Name, result.Repository.FullName)
	if owner == "" || name == "" || result.Repository.ID <= 0 {
		return pluginsdk.EntityReferenceCandidate{}, false
	}
	url := strings.TrimSpace(result.HTMLURL)
	if url == "" {
		url = PullRequestURL(scope, owner, name, result.Number)
	}
	label := fmt.Sprintf("%s/%s#%d", owner, name, result.Number)
	title := strings.TrimSpace(result.Title)
	if title == "" {
		title = label
	}
	return pluginsdk.EntityReferenceCandidate{
		ProviderLocalID: referenceID(result.Repository.ID, result.Number),
		Title:           title,
		URL:             url,
		Attributes: map[string]any{
			"label":         label,
			"repository_id": strconv.FormatInt(result.Repository.ID, 10),
			"number":        result.Number,
			"state":         referenceState(result),
		},
	}, true
}

// repositoryName is a search result's repository owner and name, from its full
// name when either field is empty.
func repositoryName(owner, name, fullName string) (string, string) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner != "" && name != "" {
		return owner, name
	}
	full := strings.TrimSpace(fullName)
	if full == "" {
		return owner, name
	}
	if parsedOwner, parsedName, found := strings.Cut(full, "/"); found {
		return parsedOwner, parsedName
	}
	return owner, name
}

// Authorize performs a live access check. It fails closed: any error, timeout,
// revoked token, or deleted pull request denies the reference.
func (r *References) Authorize(ctx context.Context, _ string, _ string, reference map[string]any) (bool, error) {
	client, err := r.connection.Client(ctx)
	if err != nil {
		if errors.Is(err, scm.ErrNotConfigured) {
			return false, nil
		}
		return false, err
	}
	repositoryID, number, ok := parseReferenceID(reference)
	if !ok {
		return false, nil
	}
	repo, err := client.RepoByID(ctx, repositoryID)
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) || errors.Is(err, scm.ErrUnauthorized) {
			return false, nil
		}
		return false, err
	}
	owner := repo.OwnerLogin()
	if owner == "" || strings.TrimSpace(repo.Name) == "" {
		return false, nil
	}
	if _, err := client.PullRequest(ctx, owner, repo.Name, number); err != nil {
		if errors.Is(err, scm.ErrNotFound) || errors.Is(err, scm.ErrUnauthorized) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// referenceState reports whether a candidate is open, merged, or closed so the
// composer can label it without a second provider call.
func referenceState(result rest.IssueSearchResult) string {
	if result.PullRequest != nil && result.PullRequest.Merged {
		return "merged"
	}
	if strings.EqualFold(strings.TrimSpace(result.State), "closed") {
		return "closed"
	}
	return "open"
}

// referenceID encodes the provider-local id carried through the composer. It
// uses immutable numeric ids so a rename cannot invalidate a stored reference.
func referenceID(repositoryID, number int64) string {
	return strconv.FormatInt(repositoryID, 10) + ":" + strconv.FormatInt(number, 10)
}

// parseReferenceID reads the provider-local id back out of a host reference
// payload. The host may hand back the id under `id` or `entity_id`.
func parseReferenceID(reference map[string]any) (int64, int64, bool) {
	if reference == nil {
		return 0, 0, false
	}
	var raw string
	// Kandev hands the plugin its own ProviderLocalID back as `id`, with
	// `key` carrying the same canonical value.
	for _, key := range []string{"id", "key"} {
		if value, ok := reference[key].(string); ok && strings.TrimSpace(value) != "" {
			raw = strings.TrimSpace(value)
			break
		}
	}
	if raw == "" {
		return 0, 0, false
	}
	repositoryPart, numberPart, found := strings.Cut(raw, ":")
	if !found {
		return 0, 0, false
	}
	repositoryID, err := strconv.ParseInt(strings.TrimSpace(repositoryPart), 10, 64)
	if err != nil || repositoryID <= 0 {
		return 0, 0, false
	}
	number, err := strconv.ParseInt(strings.TrimSpace(numberPart), 10, 64)
	if err != nil || number <= 0 {
		return 0, 0, false
	}
	return repositoryID, number, true
}
