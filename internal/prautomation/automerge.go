package prautomation

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// autoMerge merges when every gate passes, pinned to the observed head.
func (r *evaluation) autoMerge() {
	record, obs := r.record, r.obs
	if !record.AutoMerge || obs.State != StateOpen {
		return
	}
	if blocker := mergeBlocker(*record, obs, r.now); blocker != "" {
		r.status("Auto-merge: " + blocker)
		return
	}
	signature := mergeSignature(obs)
	if record.MergeFailure != "" && record.MergeFailureSignature == signature && !record.MergeRetry {
		r.status("Auto-merge failed: " + record.MergeFailure + " Retry from the panel, or it tries again when the pull request changes.")
		return
	}
	err := r.engine.merger.Merge(r.ctx, r.workspaceID, record.Ref(), obs.HeadSHA)
	record.MergeRetry = false
	var refused *MergeError
	switch {
	case err == nil:
		record.MergedByPlugin = true
		record.MergeFailure, record.MergeFailureSignature = "", ""
		r.status("Auto-merge merged the pull request.")
	case errors.As(err, &refused) && refused.Transient:
		r.status("Auto-merge: the instance asked to try again; it will.")
	case errors.As(err, &refused):
		record.MergeFailure, record.MergeFailureSignature = refused.Message, signature
		r.status("Auto-merge failed: " + refused.Message)
	default:
		record.Error = r.engine.describe(err)
		r.status("Auto-merge could not reach the instance; it will try again.")
	}
}

// mergeBlocker returns why auto-merge waits, or "" when it may merge. The
// gates are checked in order and the first one that holds the merge back
// names the reason.
func mergeBlocker(record Record, obs Observation, now time.Time) string {
	if obs.Draft {
		return "the pull request is a draft."
	}
	if blocker := ciBlocker(obs, record.HeadSeenAt, now); blocker != "" {
		return blocker
	}
	if blocker := mergeabilityBlocker(obs); blocker != "" {
		return blocker
	}
	if blocker := agentBlocker(record, obs.HeadSHA, now); blocker != "" {
		return blocker
	}
	return reviewBlocker(obs)
}

func ciBlocker(obs Observation, headSeenAt, now time.Time) string {
	switch {
	case obs.CI.State == CIPending:
		return "waiting for CI."
	case obs.CI.State == CIFailure:
		return "CI is failing."
	case len(obs.CI.Missing) > 0:
		return "waiting for required checks to report: " + strings.Join(obs.CI.Missing, ", ") + "."
	case obs.CI.State == CINone && now.Sub(headSeenAt) < noCIGrace:
		return "waiting for CI to report on the new head."
	}
	return ""
}

func mergeabilityBlocker(obs Observation) string {
	switch {
	case obs.Mergeable == nil:
		return "waiting for the instance to compute mergeability."
	case !*obs.Mergeable:
		return "the branch has conflicts."
	}
	return ""
}

func agentBlocker(record Record, head string, now time.Time) string {
	switch {
	case record.Attempt != nil:
		return "an auto-fix round is in progress."
	case record.HoldHead == head && now.Before(record.HoldUntil):
		// The agent reported a change: the head from before it is not the
		// one to merge until a new head shows or the window passes.
		return "waiting for the agent's change to show up."
	case record.BlockedHead == head && now.Before(record.BlockedUntil):
		return "the agent reported that a change is needed but blocked."
	}
	return ""
}

func reviewBlocker(obs Observation) string {
	if obs.ReviewsIncomplete {
		return "the pull request has more reviews than automation reads; merge it by hand."
	}
	changes, approvals := reviewVerdicts(obs.Reviews, obs.Operator)
	if changes > 0 {
		return "changes are requested."
	}
	if required := obs.RequiredApprovals; required != nil && approvals < *required {
		return fmt.Sprintf("%d of %d required approvals.", approvals, *required)
	}
	if unresolvedComments(obs.Reviews) {
		return "review comments are unresolved."
	}
	return ""
}

func unresolvedComments(reviews []Review) bool {
	for _, review := range reviews {
		if review.Dismissed {
			continue
		}
		for _, comment := range review.Comments {
			if !comment.Resolved {
				return true
			}
		}
	}
	return false
}

// reviewVerdicts reads each reviewer's latest decisive review: how many
// request changes, and how many official reviewers approve. Reviews arrive
// oldest first from the instance.
//
// The operator's approval is not counted: agents post as that account, so it
// could be an agent approving its own work. Its request for changes still
// holds the merge back.
func reviewVerdicts(reviews []Review, operator string) (changes, approvals int) {
	operator = strings.ToLower(strings.TrimSpace(operator))
	latest := map[string]Review{}
	for _, review := range reviews {
		if review.Dismissed || (review.State != ReviewApproved && review.State != ReviewChangesRequested) {
			continue
		}
		latest[strings.ToLower(strings.TrimSpace(review.Author))] = review
	}
	for author, review := range latest {
		switch {
		case review.State == ReviewChangesRequested:
			changes++
		case review.Official && (operator == "" || author != operator):
			approvals++
		}
	}
	return changes, approvals
}

// mergeSignature identifies the state a merge was attempted in. A failure is
// not retried on its own until this changes.
func mergeSignature(obs Observation) string {
	items := []Item{{Key: "head:" + obs.HeadSHA}, {Key: "ci:" + string(obs.CI.State)}}
	for _, missing := range obs.CI.Missing {
		items = append(items, Item{Key: "missing:" + missing})
	}
	for _, review := range obs.Reviews {
		items = append(items, Item{Key: fmt.Sprintf("review:%d:%s:%t", review.ID, review.State, review.Dismissed)})
	}
	if obs.RequiredApprovals != nil {
		items = append(items, Item{Key: fmt.Sprintf("required:%d", *obs.RequiredApprovals)})
	}
	return Signature(items)
}
