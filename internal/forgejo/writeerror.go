package forgejo

import (
	"fmt"
	"net/http"
	"strings"
)

// Reason is a fixed, token-safe classification of a failed write. It is the
// only part of a provider error body that ever leaves this package: the body
// itself is read for classification and then dropped, because on some
// deployments it echoes the presented token.
type Reason string

const (
	// ReasonNotMergeable means the pull request cannot be merged as it stands.
	ReasonNotMergeable Reason = "not_mergeable"
	// ReasonConflict means the branches conflict.
	ReasonConflict Reason = "conflict"
	// ReasonOutOfDate means the head branch is behind its base.
	ReasonOutOfDate Reason = "out_of_date"
	// ReasonHeadChanged means the head moved since the caller last saw it.
	ReasonHeadChanged Reason = "head_changed"
	// ReasonForbidden means the token lacks the scope or permission.
	ReasonForbidden Reason = "forbidden"
	// ReasonBlockedByProtection means branch protection refuses the action.
	ReasonBlockedByProtection Reason = "blocked_by_protection"
	// ReasonSelfReview means an author tried to review their own pull request.
	ReasonSelfReview Reason = "self_review"
	// ReasonAlreadyMerged means the pull request is already merged.
	ReasonAlreadyMerged Reason = "already_merged"
	// ReasonChecking means the instance is still computing mergeability (Gitea
	// answers 405 "Please try again later"). It is transient: retry shortly.
	ReasonChecking Reason = "checking"
	// ReasonUpToDate means there was nothing to update.
	ReasonUpToDate Reason = "up_to_date"
	// ReasonInvalid means the instance rejected the request as malformed or
	// unsupported, for example a merge style the repository disables.
	ReasonInvalid Reason = "invalid"
)

// maxErrorBodyBytes bounds how much of an error body is read to classify it.
const maxErrorBodyBytes = 4 << 10

// WriteError is a failed write with a classified reason. It carries neither
// the response body nor the request URL. 404 and 401 are not classified: they
// keep returning ErrNotFound and ErrUnauthorized, because a 401 really is a bad
// token whereas a 403 on a write is usually a missing scope or permission.
type WriteError struct {
	Status int
	Reason Reason
}

func (e *WriteError) Error() string {
	return fmt.Sprintf("forgejo: write refused (%d): %s", e.Status, e.Reason)
}

// classifyWrite derives a Reason from a status and a bounded error body. The
// phrases are matched case-insensitively against Gitea and Forgejo's messages;
// each is recorded in docs/development.md once verified on a live instance, and
// an unrecognised body falls back to the status alone.
func classifyWrite(status int, body string) Reason {
	text := strings.ToLower(body)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(text, part) {
				return true
			}
		}
		return false
	}
	switch {
	case has("already merged", "has been merged"):
		return ReasonAlreadyMerged
	case has("[reason:", "enough approvals", "status check", "protected branch"):
		// A merge blocked by branch protection is a 405 on all four hosts, with
		// "not allowed to merge [reason: Does not have enough approvals]" (Gitea
		// 1.20, Forgejo) or just "Does not have enough approvals" (Gitea 1.27).
		// The reason must win over "not allowed to merge" below.
		return ReasonBlockedByProtection
	case has("not allowed to merge", "not allowed to"):
		// Gitea and Forgejo answer a merge by a user without permission with
		// 405, not 403 (probed with a read-only collaborator on all four).
		return ReasonForbidden
	case has("up to date"):
		// Gitea and Forgejo 7 answer 500 for a no-op branch update.
		return ReasonUpToDate
	case has("try again later"):
		return ReasonChecking
	case has("own pull", "your own", "own pr"):
		return ReasonSelfReview
	}
	switch status {
	case http.StatusMethodNotAllowed:
		return ReasonNotMergeable
	case http.StatusConflict:
		switch {
		case has("head"):
			return ReasonHeadChanged
		case has("out of date", "outofdate", "out-of-date"):
			return ReasonOutOfDate
		default:
			return ReasonConflict
		}
	case http.StatusForbidden:
		// "write permission is required" (Gitea 1.27) is a permission failure
		// even though it contains "required".
		// A token without the scope is refused with "token does not have at
		// least one of required scope(s)", which also contains "required".
		if !has("permission", "scope") && has("protect", "approv", "required", "status check") {
			return ReasonBlockedByProtection
		}
		return ReasonForbidden
	default:
		return ReasonInvalid
	}
}
