package watches

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// DefaultReviewPrompt is the task description for a review watch with no
// prompt of its own. It follows Kandev's own GitHub review watch.
const DefaultReviewPrompt = `Review Pull Request #{{pr.number}}: {{pr.title}}
Repository: {{pr.repo}}
PR: {{pr.link}}
Author: {{pr.author}}
Branch: {{pr.branch}} → {{pr.base_branch}}

To see ONLY the PR changes, use:
- git diff origin/{{pr.base_branch}}...HEAD (three-dot = only changes on the PR branch)
- git log --oneline origin/{{pr.base_branch}}..HEAD (list PR commits)
Do NOT review files outside this diff.`

// interpolate fills the {{pr.*}} placeholders. Anything else in braces is left
// alone, so an operator's own braces survive. The values are pull request
// metadata an author controls, so they are substituted as text and never
// evaluated.
func interpolate(template string, repo scm.RepoRef, number int64, detail PullDetail) string {
	return strings.NewReplacer(
		"{{pr.link}}", detail.URL,
		"{{pr.number}}", strconv.FormatInt(number, 10),
		"{{pr.title}}", detail.Title,
		"{{pr.author}}", detail.Author,
		"{{pr.repo}}", repo.FullName(),
		"{{pr.branch}}", detail.HeadBranch,
		"{{pr.base_branch}}", detail.BaseBranch,
	).Replace(template)
}

// createReviewTask turns one pull request into one task.
func (p *Poller) createReviewTask(ctx context.Context, host pluginsdk.Host, watch Watch, candidate PullRequest, detail PullDetail, where placement) (string, error) {
	template := watch.Prompt
	if template == "" {
		template = DefaultReviewPrompt
	}
	stepID := where.stepID
	input := pluginsdk.CreateTaskInput{
		WorkspaceID:    watch.WorkspaceID,
		WorkflowID:     watch.WorkflowID,
		WorkflowStepID: &stepID,
		Title:          truncateRunes(fmt.Sprintf("PR #%d: %s", candidate.Number, strings.TrimSpace(detail.Title)), maxTitleRunes),
		Description:    interpolate(template, candidate.Repo, candidate.Number, detail),
		StartAgent:     where.startAgent,
		Metadata:       reviewMetadata(watch, candidate, detail),
	}
	if watch.RepositoryID != "" {
		input.Repositories = []pluginsdk.PluginTaskRepository{reviewRepository(watch, candidate, detail, where)}
	}
	if launch := profileLaunch(watch); launch != nil {
		input.Launch = launch
	}
	created, err := host.Tasks().Create(ctx, input)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	if created == nil || strings.TrimSpace(created.ID) == "" {
		return "", errors.New("create task: the host returned no task")
	}
	return created.ID, nil
}

func reviewMetadata(watch Watch, candidate PullRequest, detail PullDetail) map[string]any {
	metadata := map[string]any{
		"forgejo_repo":      candidate.Repo.FullName(),
		"forgejo_pr_number": candidate.Number,
		"forgejo_pr_url":    detail.URL,
		"forgejo_watch_id":  watch.ID,
		"forgejo_author":    detail.Author,
		"forgejo_head":      detail.HeadBranch,
		"forgejo_base":      detail.BaseBranch,
	}
	if detail.Fork {
		metadata["forgejo_fork"] = true
	}
	return metadata
}

// reviewRepository attaches the watch's repository at the pull request's base,
// falling back to the watch's own base branch, and checks out the head only
// when the placement allows it.
func reviewRepository(watch Watch, candidate PullRequest, detail PullDetail, where placement) pluginsdk.PluginTaskRepository {
	attached := pluginsdk.PluginTaskRepository{RepositoryID: watch.RepositoryID}
	base := detail.BaseBranch
	if base == "" {
		base = watch.BaseBranch
	}
	if base != "" {
		attached.BaseBranch = &base
	}
	if where.checkout {
		head, number := detail.HeadBranch, candidate.Number
		attached.CheckoutBranch, attached.PullRequestNumber = &head, &number
	}
	return attached
}
