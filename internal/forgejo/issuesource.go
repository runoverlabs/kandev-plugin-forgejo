package forgejo

import (
	"context"
	"errors"
	"fmt"

	"kandev-plugin-forgejo/internal/watches"
)

// maxIssuePages bounds how many pages one repository contributes to a single
// poll. A watch's first run against a repository with thousands of open issues
// should surface the newest work and let the inflight budget meter the rest,
// not spend minutes paginating a backlog the board cannot absorb anyway.
const maxIssuePages = 10

// IssueSource adapts the Forgejo REST client to the provider-neutral port the
// watch poller depends on.
type IssueSource struct {
	connection *Connection
}

var _ watches.IssueSource = (*IssueSource)(nil)

// NewIssueSource returns the Forgejo-backed issue source.
func NewIssueSource(connection *Connection) *IssueSource {
	return &IssueSource{connection: connection}
}

// ListIssues returns every issue in a repository matching the query, following
// pagination up to maxIssuePages.
func (s *IssueSource) ListIssues(ctx context.Context, repo watches.RepoRef, query watches.IssueQuery) ([]watches.Issue, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return nil, err
	}

	collected := make([]watches.Issue, 0, maxIssuePageLimit)
	for page := 1; page <= maxIssuePages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		issues, raw, err := client.listIssuesPage(ctx, repo.Owner, repo.Name, IssueListOptions{
			State:  query.State,
			Labels: query.Labels,
			Query:  query.Query,
			Since:  query.Since,
			Page:   page,
			Limit:  maxIssuePageLimit,
		})
		if err != nil {
			// A failure on any page discards what earlier pages collected. A
			// partial walk would make the caller's "matched" count meaningless
			// and could silently drop issues it would then never revisit,
			// because a successful poll advances the incremental bound past
			// them.
			return nil, translateIssueError(repo, err)
		}
		for _, issue := range issues {
			collected = append(collected, watches.Issue{
				Number:    issue.Number,
				Title:     issue.Title,
				Body:      issue.Body,
				State:     issue.State,
				URL:       issue.HTMLURL,
				Labels:    issue.LabelNames(),
				Author:    issue.User.Login,
				UpdatedAt: issue.UpdatedAt,
			})
		}
		// The walk ends on a short page, judged by the rows the server sent, not
		// by what is left after pull requests are filtered out. A server that
		// ignores type=issues can fill a page with pull requests; counting only
		// the issues would make that full page look short and drop every issue
		// on the pages after it.
		if raw < maxIssuePageLimit {
			break
		}
	}
	return collected, nil
}

// translateIssueError maps a transport error onto an operator-facing message.
// Provider error bodies are never forwarded: on some deployments they echo the
// token.
func translateIssueError(repo watches.RepoRef, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%s is not visible to this token", repo.FullName())
	case errors.Is(err, ErrUnauthorized):
		return fmt.Errorf("the instance rejected the access token for %s", repo.FullName())
	default:
		return fmt.Errorf("could not read issues for %s", repo.FullName())
	}
}
