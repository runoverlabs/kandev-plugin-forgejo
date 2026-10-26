package forgejo

import (
	"context"
	"errors"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// prListPages and prListLimit bound the search for a pull request by head
// branch. Neither Gitea nor older Forgejo can filter by head server-side, so
// the newest pages are scanned and anything older is treated as absent.
const (
	prListPages = 3
	prListLimit = 50
)

// PullRequests serves the agent tools' reads: a pull request, the open one for
// a head branch, readying a draft, and CI for a ref.
type PullRequests struct {
	connection *Connection
}

// NewPullRequests returns the agent-facing reads over connection.
func NewPullRequests(connection *Connection) *PullRequests {
	return &PullRequests{connection: connection}
}

// PullRequest reads one pull request.
func (p *PullRequests) PullRequest(ctx context.Context, repository sourcecontrol.Repository, number int64) (sourcecontrol.PullRequestSummary, error) {
	client, err := p.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.PullRequestSummary{}, err
	}
	pull, err := client.PullRequest(ctx, repository.OwnerOrProject, repository.Name, number)
	if err != nil {
		return sourcecontrol.PullRequestSummary{}, err
	}
	return summarizePull(pull), nil
}

// OpenPullRequestForHead looks for an open pull request whose head is branch.
// Only current Forgejo can filter by head server-side, so the newest pages are
// scanned and compared locally.
func (p *PullRequests) OpenPullRequestForHead(ctx context.Context, repository sourcecontrol.Repository, branch string) (*sourcecontrol.PullRequestSummary, error) {
	wanted := strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	if wanted == "" {
		return nil, nil
	}
	client, err := p.connection.Client(ctx)
	if err != nil {
		return nil, err
	}
	for page := 1; page <= prListPages; page++ {
		pulls, err := client.ListPullRequests(ctx, repository.OwnerOrProject, repository.Name, "open", page, prListLimit)
		if errors.Is(err, scm.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, pull := range pulls {
			if strings.EqualFold(strings.TrimSpace(pull.Head.Ref), wanted) {
				summary := summarizePull(pull)
				return &summary, nil
			}
		}
		if len(pulls) < prListLimit {
			break
		}
	}
	return nil, nil
}

// MarkReady clears the work-in-progress title marker. Forgejo and Gitea have no
// draft flag in REST v1, so a draft is a "WIP:" title prefix and readying one
// is a title edit.
func (p *PullRequests) MarkReady(ctx context.Context, repository sourcecontrol.Repository, pull sourcecontrol.PullRequestSummary) (sourcecontrol.PullRequestSummary, bool, error) {
	stripped, changed := StripWorkInProgressPrefix(pull.Title)
	if !changed {
		return pull, false, nil
	}
	client, err := p.connection.Client(ctx)
	if err != nil {
		return sourcecontrol.PullRequestSummary{}, false, err
	}
	updated, err := client.EditPullRequest(ctx, repository.OwnerOrProject, repository.Name, pull.Number,
		rest.EditPullRequestInput{Title: stripped})
	if err != nil {
		return sourcecontrol.PullRequestSummary{}, false, err
	}
	return summarizePull(updated), true, nil
}

// CIStatus resolves CI for a ref through the instance's surfaces.
func (p *PullRequests) CIStatus(ctx context.Context, repository sourcecontrol.Repository, ref string) (scm.CIStatus, error) {
	client, err := p.connection.Client(ctx)
	if err != nil {
		return scm.CIStatus{}, err
	}
	return client.CIStatusFor(ctx, repository.OwnerOrProject, repository.Name, ref)
}

// JobLogTail reads the end of one job's log.
func (p *PullRequests) JobLogTail(ctx context.Context, repository sourcecontrol.Repository, jobID int64, maxBytes int) (string, bool, error) {
	client, err := p.connection.Client(ctx)
	if err != nil {
		return "", false, err
	}
	return client.JobLogTail(ctx, repository.OwnerOrProject, repository.Name, jobID, maxBytes)
}

// summarizePull reports merged separately from closed, which the REST state
// field does not.
func summarizePull(pull rest.PullRequest) sourcecontrol.PullRequestSummary {
	state := strings.TrimSpace(pull.State)
	switch {
	case pull.Merged:
		state = "merged"
	case state == "":
		state = "unknown"
	}
	return sourcecontrol.PullRequestSummary{
		Number:  pull.Number,
		State:   state,
		URL:     pull.HTMLURL,
		Title:   pull.Title,
		Head:    pull.Head.Ref,
		Base:    pull.Base.Ref,
		HeadSHA: pull.Head.Sha,
		Draft:   IsWorkInProgressTitle(pull.Title),
	}
}
