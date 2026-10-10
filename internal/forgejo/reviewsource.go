package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"kandev-plugin-forgejo/internal/watches"
)

// maxReviewPages bounds the pages one review search reads. Fifty a page, so a
// watch sees the 500 most recently updated requests; the in-flight budget
// meters what becomes a task.
const maxReviewPages = 10

// ReviewSource adapts the Forgejo REST client to the port review watches
// depend on.
type ReviewSource struct {
	connection *Connection
}

var _ watches.ReviewSource = (*ReviewSource)(nil)

// NewReviewSource returns the Forgejo-backed review source.
func NewReviewSource(connection *Connection) *ReviewSource {
	return &ReviewSource{connection: connection}
}

// SearchReviewRequests returns the open pull requests awaiting the token
// user's review.
//
// One instance-wide search answers it, then the repository list narrows the
// result here: the search has an owner filter but none for a single
// repository. The walk continues on the raw page length so a full page is
// never mistaken for the last one.
func (s *ReviewSource) SearchReviewRequests(ctx context.Context, q watches.ReviewQuery) ([]watches.PullRequest, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(q.Repos))
	for _, repo := range q.Repos {
		wanted[strings.ToLower(repo.FullName())] = struct{}{}
	}

	var found []watches.PullRequest
	for page := 1; page <= maxReviewPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, err := client.SearchReviewRequested(ctx, ReviewSearchOptions{
			Labels: q.Labels, Query: q.Query, Page: page, Limit: maxIssuePageLimit,
		})
		if err != nil {
			return nil, translateReviewError("the review search", err)
		}
		for _, result := range results {
			if result.Repository == nil {
				continue
			}
			owner, name, ok := strings.Cut(result.Repository.FullName, "/")
			if !ok {
				continue
			}
			// Same guard as for issues: an unknown label name filters nothing.
			names := make([]string, 0, len(result.Labels))
			for _, label := range result.Labels {
				names = append(names, label.Name)
			}
			if !hasAllLabels(names, q.Labels) {
				continue
			}
			if len(wanted) > 0 {
				if _, listed := wanted[strings.ToLower(result.Repository.FullName)]; !listed {
					continue
				}
			}
			found = append(found, watches.PullRequest{
				Repo:      watches.RepoRef{Owner: owner, Name: name},
				Number:    result.Number,
				Title:     result.Title,
				URL:       result.HTMLURL,
				Author:    result.User.Login,
				UpdatedAt: result.UpdatedAt,
			})
		}
		if len(results) < maxIssuePageLimit {
			break
		}
	}
	return found, nil
}

// PullDetail reads one pull request for a task about to be filed.
func (s *ReviewSource) PullDetail(ctx context.Context, repo watches.RepoRef, number int64, scope watches.ReviewScope) (watches.PullDetail, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return watches.PullDetail{}, err
	}
	pull, err := client.PullRequest(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return watches.PullDetail{}, translateReviewError(fmt.Sprintf("%s#%d", repo.FullName(), number), err)
	}
	detail := watches.PullDetail{
		Title:      pull.Title,
		URL:        pull.HTMLURL,
		Author:     pull.User.Login,
		HeadBranch: pull.Head.Ref,
		BaseBranch: pull.Base.Ref,
		Open:       pull.State == "open" && !pull.Merged,
		Draft:      pull.Draft || hasWIPPrefix(pull.Title),
		Fork:       isFork(pull),
	}
	if scope == watches.ReviewScopeUser {
		me, err := client.CurrentUser(ctx)
		if err != nil {
			return watches.PullDetail{}, translateReviewError("the current user", err)
		}
		for _, requested := range pull.Requested {
			if strings.EqualFold(requested.Login, me.Login) {
				detail.RequestedFromMe = true
			}
		}
	}
	return detail, nil
}

// PullState reports whether a pull request is still open.
func (s *ReviewSource) PullState(ctx context.Context, repo watches.RepoRef, number int64) (watches.PullState, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return "", err
	}
	pull, err := client.PullRequest(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return "", translateReviewError(fmt.Sprintf("%s#%d", repo.FullName(), number), err)
	}
	switch {
	case pull.Merged:
		return watches.PullMerged, nil
	case pull.State == "open":
		return watches.PullOpen, nil
	default:
		return watches.PullClosed, nil
	}
}

// isFork reports whether the head lives somewhere other than the base
// repository. A head with no repository (a deleted fork, or an AGit-flow pull
// request) is treated as a fork: unknown identity fails closed.
func isFork(pull PullRequest) bool {
	head, base := pull.Head.Repo, pull.Base.Repo
	if head == nil || base == nil || head.FullName == "" || base.FullName == "" {
		return true
	}
	return !strings.EqualFold(head.FullName, base.FullName)
}

// hasWIPPrefix is the draft signal on instances that predate the draft field
// (Gitea 1.20 and Forgejo 7 report none): the configured work-in-progress
// title prefixes, whose defaults are "WIP:" and "[WIP]".
func hasWIPPrefix(title string) bool {
	upper := strings.ToUpper(strings.TrimSpace(title))
	return strings.HasPrefix(upper, "WIP:") || strings.HasPrefix(upper, "[WIP]")
}

// translateReviewError maps a transport error onto an operator-facing message.
// Provider error bodies are never forwarded: on some deployments they echo the
// token. A refusal that more requests would not cure is marked so a batch
// stops instead of repeating it.
func translateReviewError(what string, err error) error {
	var status *StatusError
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%s is not visible to this token", what)
	case errors.Is(err, ErrUnauthorized):
		return fmt.Errorf("%w: the instance rejected the access token for %s", watches.ErrBackoff, what)
	case errors.As(err, &status) && status.Status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: rate limited while reading %s", watches.ErrBackoff, what)
	default:
		return fmt.Errorf("could not read %s", what)
	}
}
