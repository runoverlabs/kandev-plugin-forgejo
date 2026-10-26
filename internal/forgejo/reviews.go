package forgejo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// Reviews implements the recipe's ReviewReader port. It publishes semantic
// status only: Kandev owns every colour, glyph, and piece of chrome derived
// from it.
type Reviews struct {
	connection   *Connection
	repositories *Repositories
	associations *sourcecontrol.Associations
	providerID   string
}

var _ sourcecontrol.ReviewReader = (*Reviews)(nil)

// NewReviews returns the review adapter.
func NewReviews(connection *Connection, repositories *Repositories, associations *sourcecontrol.Associations, providerID string) *Reviews {
	return &Reviews{connection: connection, repositories: repositories, associations: associations, providerID: providerID}
}

// ForTask returns a normalized snapshot per pull request linked to the task.
func (r *Reviews) ForTask(ctx context.Context, workspaceID, taskID string) ([]sourcecontrol.ReviewSummary, error) {
	identities, err := r.associations.ListForTask(ctx, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	if len(identities) == 0 {
		return nil, nil
	}
	client, err := r.connection.Client(ctx)
	if err != nil {
		if errors.Is(err, scm.ErrNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	summaries := make([]sourcecontrol.ReviewSummary, 0, len(identities))
	for _, identity := range identities {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if identity.ConnectionScope != client.Scope() {
			// Recorded against a different instance than the one currently
			// configured; it is not this connection's to report.
			continue
		}
		summary, ok, err := r.summarize(ctx, client, identity)
		if err != nil {
			return nil, err
		}
		if ok {
			summaries = append(summaries, summary)
		}
	}
	return summaries, nil
}

// Associations returns the workspace-level task-to-review map that backs the
// sidebar, Kanban, and list pull-request glyphs. It is intentionally cheap: it
// reads stored identities and never calls the provider.
func (r *Reviews) Associations(ctx context.Context, workspaceID string) ([]sourcecontrol.ReviewAssociation, error) {
	identities, taskIDs, err := r.associations.ListForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := make([]sourcecontrol.ReviewAssociation, 0, len(identities))
	for i, identity := range identities {
		result = append(result, sourcecontrol.ReviewAssociation{
			ProviderID:          r.providerID,
			TaskID:              taskIDs[i],
			ReviewKey:           identityReviewKey(identity),
			ConnectionScope:     identity.ConnectionScope,
			RepositoryID:        identity.RepositoryID,
			ChangeRequestNumber: identity.Number,
		})
	}
	return result, nil
}

// summarize builds one review snapshot. A pull request that has disappeared is
// reported as absent rather than as an error, so one deleted PR cannot break a
// task's whole refresh.
func (r *Reviews) summarize(ctx context.Context, client *rest.Client, identity sourcecontrol.ChangeRequestIdentity) (sourcecontrol.ReviewSummary, bool, error) {
	repository, err := r.repositories.Resolve(ctx, "", sourcecontrol.RepositoryIdentity{
		ConnectionScope: identity.ConnectionScope,
		RepositoryID:    identity.RepositoryID,
	})
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) || errors.Is(err, scm.ErrUnauthorized) {
			return sourcecontrol.ReviewSummary{}, false, nil
		}
		return sourcecontrol.ReviewSummary{}, false, err
	}
	pull, err := client.PullRequest(ctx, repository.OwnerOrProject, repository.Name, identity.Number)
	if err != nil {
		if errors.Is(err, scm.ErrNotFound) || errors.Is(err, scm.ErrUnauthorized) {
			return sourcecontrol.ReviewSummary{}, false, nil
		}
		return sourcecontrol.ReviewSummary{}, false, err
	}

	status := sourcecontrol.ReviewTaskStatus{
		Number:        pull.Number,
		State:         pullRequestState(pull),
		PipelineState: "neutral",
		Checks:        []sourcecontrol.ReviewTaskStatusCheck{},
	}
	if updated := parseTimestampMillis(pull.UpdatedAt); updated > 0 {
		status.UpdatedAt = updated
	}
	// review_comments counts unresolved inline review comments on both hosts.
	if pull.ReviewComments > 0 {
		status.UnresolvedComments = pull.ReviewComments
	}

	if sha := strings.TrimSpace(pull.Head.Sha); sha != "" {
		combined, err := client.CombinedStatus(ctx, repository.OwnerOrProject, repository.Name, sha)
		switch {
		case err == nil:
			status.PipelineState = pipelineState(combined.State)
			status.Checks = normalizeChecks(combined.Statuses)
		case errors.Is(err, scm.ErrNotFound), errors.Is(err, scm.ErrUnauthorized):
			// No CI configured, or statuses not visible to this token.
		default:
			return sourcecontrol.ReviewSummary{}, false, err
		}
	}

	reviews, err := client.Reviews(ctx, repository.OwnerOrProject, repository.Name, identity.Number)
	switch {
	case err == nil:
		status.Review = summarizeReviews(reviews)
	case errors.Is(err, scm.ErrNotFound), errors.Is(err, scm.ErrUnauthorized):
		// Reviews not visible to this token; leave the field unset.
	default:
		return sourcecontrol.ReviewSummary{}, false, err
	}

	htmlURL := strings.TrimSpace(pull.HTMLURL)
	if htmlURL == "" {
		htmlURL = PullRequestURL(client.Scope(), repository.OwnerOrProject, repository.Name, pull.Number)
	}
	return sourcecontrol.ReviewSummary{
		ProviderID:          r.providerID,
		ReviewKey:           identityReviewKey(identity),
		Title:               strings.TrimSpace(pull.Title),
		URL:                 htmlURL,
		ConnectionScope:     identity.ConnectionScope,
		RepositoryID:        identity.RepositoryID,
		ChangeRequestNumber: pull.Number,
		State:               status.State,
		TaskStatus:          &status,
	}, true, nil
}

// pullRequestState maps Forgejo's state plus its merged/draft flags onto the
// host's open | merged | closed | draft vocabulary. Order matters: a merged
// pull request is reported as merged even though its state is "closed".
func pullRequestState(pull rest.PullRequest) string {
	switch {
	case pull.Merged:
		return "merged"
	case strings.EqualFold(strings.TrimSpace(pull.State), "closed"):
		return "closed"
	case pull.Draft || IsWorkInProgressTitle(pull.Title):
		return "draft"
	default:
		return "open"
	}
}

// pipelineState maps a combined commit status onto the host's
// success | failure | pending | neutral vocabulary.
func pipelineState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return "success"
	case "failure", "error":
		return "failure"
	case "pending", "running":
		return "pending"
	default:
		return "neutral"
	}
}

// normalizeChecks converts individual commit statuses into host checks. The
// per-entry field is `status`, not `state`.
func normalizeChecks(statuses []rest.CommitStatus) []sourcecontrol.ReviewTaskStatusCheck {
	checks := make([]sourcecontrol.ReviewTaskStatusCheck, 0, len(statuses))
	for _, status := range statuses {
		label := strings.TrimSpace(status.Context)
		if label == "" {
			label = "check"
		}
		id := strconv.FormatInt(status.ID, 10)
		if status.ID <= 0 {
			id = label
		}
		checks = append(checks, sourcecontrol.ReviewTaskStatusCheck{
			ID:     id,
			Label:  label,
			State:  pipelineState(status.Status),
			Detail: strings.TrimSpace(status.Description),
			URL:    strings.TrimSpace(status.TargetURL),
		})
	}
	return checks
}

// summarizeReviews reduces submitted reviews to the host's review summary.
// Stale and dismissed reviews are ignored, and only the most recent decision
// per reviewer counts, matching how both hosts render the review box.
func summarizeReviews(reviews []rest.Review) *sourcecontrol.ReviewTaskReview {
	latest := make(map[string]string, len(reviews))
	order := make([]string, 0, len(reviews))
	for _, review := range reviews {
		if review.Stale || review.Dismissed {
			continue
		}
		state := strings.ToUpper(strings.TrimSpace(review.State))
		if state != "APPROVED" && state != "REQUEST_CHANGES" {
			// COMMENT and PENDING carry no approval decision.
			continue
		}
		login := strings.TrimSpace(review.User.Login)
		if login == "" {
			login = strconv.FormatInt(review.ID, 10)
		}
		if _, seen := latest[login]; !seen {
			order = append(order, login)
		}
		latest[login] = state
	}
	if len(latest) == 0 {
		return nil
	}
	approved, changesRequested := 0, 0
	for _, login := range order {
		switch latest[login] {
		case "APPROVED":
			approved++
		case "REQUEST_CHANGES":
			changesRequested++
		}
	}
	state := "pending"
	switch {
	case changesRequested > 0:
		state = "changes_requested"
	case approved > 0:
		state = "approved"
	}
	return &sourcecontrol.ReviewTaskReview{
		State:    state,
		Approved: approved,
	}
}

// identityReviewKey derives the review key used by both the per-task snapshot
// and the workspace association map. It is built purely from the immutable
// stored identity: the workspace glyph refresh must not call the provider, and
// a key containing owner/name would change under a repository rename and stop
// correlating the two sides.
func identityReviewKey(identity sourcecontrol.ChangeRequestIdentity) string {
	return strings.TrimSuffix(identity.ConnectionScope, "/") + "/repositories/" +
		identity.RepositoryID + "/pulls/" + strconv.FormatInt(identity.Number, 10)
}

// parseTimestampMillis converts an RFC 3339 timestamp into Unix milliseconds.
func parseTimestampMillis(value string) int64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}
