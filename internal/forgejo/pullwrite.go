package forgejo

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"kandev-plugin-forgejo/internal/sourcecontrol"
)

// InvalidError is a request the plugin refuses before it reaches the instance.
// Message is authored here, never copied from a provider, so it is safe to show.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return "forgejo: invalid request: " + e.Message }

func invalid(message string) error { return &InvalidError{Message: message} }

// attribution marks every review and comment as posted through Kandev: the
// Forgejo identity is always the operator's shared token, never the Kandev
// user, and a reader of the pull request should be able to tell.
const attribution = "_Posted via Kandev._"

var (
	_ sourcecontrol.ChangeRequestActions = (*PullActions)(nil)

	loginPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	shaPattern   = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// mergeChecking retries a merge the instance answered with "still computing
// mergeability", which Gitea 1.20 does for a moment after a pull request opens.
const (
	mergeCheckingTries = 4
	mergeCheckingWait  = time.Second
)

// requireHead refuses a call whose head SHA is missing or no longer the pull
// request's head, which is the stale-click protection every write relies on.
func requireHead(pull PullRequest, headSHA string, required bool) error {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		if required {
			return invalid("The commit you last saw (head_sha) is required.")
		}
		return nil
	}
	if !shaPattern.MatchString(headSHA) {
		return invalid("head_sha is not a commit SHA.")
	}
	current := strings.ToLower(strings.TrimSpace(pull.Head.Sha))
	if !strings.HasPrefix(current, strings.ToLower(headSHA)) {
		return &WriteError{Status: 409, Reason: ReasonHeadChanged}
	}
	return nil
}

// Merge merges, schedules, or cancels a scheduled merge.
func (a *PullActions) Merge(ctx context.Context, workspaceID, taskID string, request sourcecontrol.MergeRequest) (sourcecontrol.MergeResult, error) {
	target, err := a.resolveLinked(ctx, workspaceID, taskID, request.Number)
	if err != nil {
		return sourcecontrol.MergeResult{}, err
	}
	client, owner, name, number := target.client, target.repository.OwnerOrProject, target.repository.Name, target.identity.Number

	if request.CancelScheduled {
		if err := client.CancelAutoMerge(ctx, owner, name, number); err != nil {
			return sourcecontrol.MergeResult{}, err
		}
		return sourcecontrol.MergeResult{Number: number, Outcome: sourcecontrol.MergeOutcomeCancelled}, nil
	}

	pull, err := client.PullRequest(ctx, owner, name, number)
	if err != nil {
		return sourcecontrol.MergeResult{}, err
	}
	if pull.Merged {
		return sourcecontrol.MergeResult{}, &WriteError{Status: 405, Reason: ReasonAlreadyMerged}
	}
	if !strings.EqualFold(pull.State, "open") {
		return sourcecontrol.MergeResult{}, &WriteError{Status: 405, Reason: ReasonNotMergeable}
	}
	if err := requireHead(pull, request.HeadSHA, true); err != nil {
		return sourcecontrol.MergeResult{}, err
	}

	style, err := a.mergeStyle(ctx, target, request.Style)
	if err != nil {
		return sourcecontrol.MergeResult{}, err
	}
	input := MergeInput{
		Do:                     style,
		HeadCommitID:           pull.Head.Sha,
		DeleteBranch:           request.DeleteBranch,
		MergeWhenChecksSucceed: request.WhenChecksSucceed,
	}
	for attempt := 1; ; attempt++ {
		err = client.MergePullRequest(ctx, owner, name, number, input)
		var writeErr *WriteError
		if !errors.As(err, &writeErr) || writeErr.Reason != ReasonChecking || attempt == mergeCheckingTries {
			break
		}
		select {
		case <-ctx.Done():
			return sourcecontrol.MergeResult{}, ctx.Err()
		case <-time.After(mergeCheckingWait):
		}
	}
	if err != nil {
		return sourcecontrol.MergeResult{}, err
	}
	outcome := sourcecontrol.MergeOutcomeMerged
	if request.WhenChecksSucceed {
		outcome = sourcecontrol.MergeOutcomeScheduled
	}
	return sourcecontrol.MergeResult{Number: number, Outcome: outcome, Style: style}, nil
}

// mergeStyle picks the style to send. A style the repository reports as
// disabled is refused here with a clear message; when the repository reports
// none (an older instance), the caller's choice is passed through and the
// instance has the last word.
func (a *PullActions) mergeStyle(ctx context.Context, target linked, requested string) (string, error) {
	known := map[string]bool{
		MergeStyleMerge: true, MergeStyleRebase: true, MergeStyleRebaseMerge: true,
		MergeStyleSquash: true, MergeStyleFastForwardOnly: true,
	}
	requested = strings.TrimSpace(requested)
	if requested != "" && !known[requested] {
		return "", invalid("That merge style is not recognised.")
	}
	var allowed []string
	var fallback string
	if id, err := parseRepositoryID(target.identity.RepositoryID); err == nil {
		repo, err := target.client.RepoByID(ctx, id)
		switch {
		case err == nil:
			allowed, fallback = repo.MergeStyles(), repo.DefaultMergeStyle
		case !softFailure(err):
			return "", err
		}
	}
	contains := func(style string) bool {
		for _, candidate := range allowed {
			if candidate == style {
				return true
			}
		}
		return false
	}
	switch {
	case requested != "":
		if len(allowed) > 0 && !contains(requested) {
			return "", invalid("This repository does not allow that merge style.")
		}
		return requested, nil
	case known[fallback] && (len(allowed) == 0 || contains(fallback)):
		return fallback, nil
	case len(allowed) > 0:
		return allowed[0], nil
	default:
		return MergeStyleMerge, nil
	}
}

// Review submits a review as the token's account.
func (a *PullActions) Review(ctx context.Context, workspaceID, taskID string, request sourcecontrol.ReviewRequest) (sourcecontrol.ReviewResult, error) {
	var event string
	switch request.Event {
	case sourcecontrol.ReviewEventApprove:
		event = ReviewApprove
	case sourcecontrol.ReviewEventRequestChanges:
		event = ReviewRequestChanges
	case sourcecontrol.ReviewEventComment:
		event = ReviewComment
	default:
		return sourcecontrol.ReviewResult{}, invalid("The review event must be approve, request_changes or comment.")
	}
	body := strings.TrimSpace(request.Body)
	if body == "" && event != ReviewApprove && len(request.Comments) == 0 {
		return sourcecontrol.ReviewResult{}, invalid("This review needs a body or inline comments.")
	}
	if len([]rune(body)) > sourcecontrol.MaxReviewBodyRunes {
		return sourcecontrol.ReviewResult{}, invalid("The review body is too long.")
	}
	if len(request.Comments) > sourcecontrol.MaxReviewComments {
		return sourcecontrol.ReviewResult{}, invalid("Too many inline comments.")
	}
	inline := make([]InlineComment, 0, len(request.Comments))
	for _, comment := range request.Comments {
		text := strings.TrimSpace(comment.Body)
		path := strings.TrimSpace(comment.Path)
		if path == "" || text == "" || comment.Line <= 0 || len([]rune(text)) > sourcecontrol.MaxInlineCommentRune {
			return sourcecontrol.ReviewResult{}, invalid("Each inline comment needs a path, a line and a body.")
		}
		inline = append(inline, InlineComment{Path: path, Body: text, NewPosition: comment.Line})
	}

	target, err := a.resolveLinked(ctx, workspaceID, taskID, request.Number)
	if err != nil {
		return sourcecontrol.ReviewResult{}, err
	}
	client, owner, name, number := target.client, target.repository.OwnerOrProject, target.repository.Name, target.identity.Number
	pull, err := client.PullRequest(ctx, owner, name, number)
	if err != nil {
		return sourcecontrol.ReviewResult{}, err
	}
	if err := requireHead(pull, request.HeadSHA, false); err != nil {
		return sourcecontrol.ReviewResult{}, err
	}

	input := SubmitReviewInput{Event: event, Body: withAttribution(body), Comments: inline}
	if strings.TrimSpace(request.HeadSHA) != "" {
		input.CommitID = pull.Head.Sha
	}
	review, err := client.SubmitReview(ctx, owner, name, number, input)
	if err != nil {
		return sourcecontrol.ReviewResult{}, err
	}
	return sourcecontrol.ReviewResult{Number: number, State: strings.ToUpper(review.State)}, nil
}

// RequestReviewers adds and removes review requests.
func (a *PullActions) RequestReviewers(ctx context.Context, workspaceID, taskID string, request sourcecontrol.ReviewersRequest) (sourcecontrol.ActionResult, error) {
	add, err := cleanLogins(request.Add)
	if err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	remove, err := cleanLogins(request.Remove)
	if err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	if len(add)+len(remove) == 0 {
		return sourcecontrol.ActionResult{}, invalid("Name at least one reviewer to add or remove.")
	}
	target, err := a.resolveLinked(ctx, workspaceID, taskID, request.Number)
	if err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	client, owner, name, number := target.client, target.repository.OwnerOrProject, target.repository.Name, target.identity.Number
	if len(remove) > 0 {
		if err := client.RemoveReviewRequest(ctx, owner, name, number, remove); err != nil {
			return sourcecontrol.ActionResult{}, err
		}
	}
	if len(add) > 0 {
		if err := client.RequestReviewers(ctx, owner, name, number, add); err != nil {
			return sourcecontrol.ActionResult{}, err
		}
	}
	return sourcecontrol.ActionResult{Number: number, OK: true}, nil
}

// UpdateBranch brings the head branch up to date with its base.
func (a *PullActions) UpdateBranch(ctx context.Context, workspaceID, taskID string, request sourcecontrol.UpdateBranchRequest) (sourcecontrol.ActionResult, error) {
	style := strings.TrimSpace(request.Style)
	if style == "" {
		style = "merge"
	}
	if style != "merge" && style != "rebase" {
		return sourcecontrol.ActionResult{}, invalid("The update style must be merge or rebase.")
	}
	target, err := a.resolveLinked(ctx, workspaceID, taskID, request.Number)
	if err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	number := target.identity.Number
	if err := target.client.UpdateBranch(ctx, target.repository.OwnerOrProject, target.repository.Name, number, style); err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	return sourcecontrol.ActionResult{Number: number, OK: true}, nil
}

// Comment adds a conversation comment.
func (a *PullActions) Comment(ctx context.Context, workspaceID, taskID string, request sourcecontrol.CommentRequest) (sourcecontrol.ActionResult, error) {
	body := strings.TrimSpace(request.Body)
	if body == "" {
		return sourcecontrol.ActionResult{}, invalid("A comment needs a body.")
	}
	if len([]rune(body)) > sourcecontrol.MaxCommentBodyRunes {
		return sourcecontrol.ActionResult{}, invalid("The comment is too long.")
	}
	target, err := a.resolveLinked(ctx, workspaceID, taskID, request.Number)
	if err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	number := target.identity.Number
	if err := target.client.CreateComment(ctx, target.repository.OwnerOrProject, target.repository.Name, number, withAttribution(body)); err != nil {
		return sourcecontrol.ActionResult{}, err
	}
	return sourcecontrol.ActionResult{Number: number, OK: true}, nil
}

func withAttribution(body string) string {
	if body == "" {
		return attribution
	}
	return body + "\n\n" + attribution
}

// cleanLogins validates reviewer logins. They go into a JSON body, not a path,
// but a login is still provider data the caller chose, so it is held to the
// characters a login can have.
func cleanLogins(logins []string) ([]string, error) {
	if len(logins) > sourcecontrol.MaxReviewers {
		return nil, invalid("Too many reviewers in one request.")
	}
	clean := make([]string, 0, len(logins))
	for _, login := range logins {
		login = strings.TrimSpace(login)
		if !loginPattern.MatchString(login) {
			return nil, invalid("A reviewer login is not valid.")
		}
		clean = append(clean, login)
	}
	return clean, nil
}
