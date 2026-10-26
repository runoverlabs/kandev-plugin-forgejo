package rest

import (
	"net/http"
	"strings"

	"kandev-plugin-forgejo/internal/scm"
)

// maxErrorBodyBytes bounds how much of an error body is read to classify it.
const maxErrorBodyBytes = 4 << 10

// bodyReasons are the error-body phrases that decide a Reason whatever the
// status, in match order: the first entry with a phrase in the body wins.
var bodyReasons = []struct {
	phrases []string
	reason  scm.Reason
}{
	{[]string{"already merged", "has been merged"}, scm.ReasonAlreadyMerged},
	// A merge blocked by branch protection is a 405 on all four hosts, with
	// "not allowed to merge [reason: Does not have enough approvals]" (Gitea
	// 1.20, Forgejo) or just "Does not have enough approvals" (Gitea 1.27).
	// The reason must win over "not allowed to merge" below.
	{[]string{"[reason:", "enough approvals", "status check", "protected branch"}, scm.ReasonBlockedByProtection},
	// Gitea and Forgejo answer a merge by a user without permission with
	// 405, not 403 (probed with a read-only collaborator on all four).
	{[]string{"not allowed to merge", "not allowed to"}, scm.ReasonForbidden},
	// Gitea and Forgejo 7 answer 500 for a no-op branch update.
	{[]string{"up to date"}, scm.ReasonUpToDate},
	{[]string{"try again later"}, scm.ReasonChecking},
	{[]string{"own pull", "your own", "own pr"}, scm.ReasonSelfReview},
}

// classifyWrite derives a Reason from a status and a bounded error body. The
// phrases are matched case-insensitively against Gitea and Forgejo's messages;
// each is recorded in docs/development.md once verified on a live instance, and
// an unrecognised body falls back to the status alone.
func classifyWrite(status int, body string) scm.Reason {
	text := strings.ToLower(body)
	for _, entry := range bodyReasons {
		if containsAny(text, entry.phrases...) {
			return entry.reason
		}
	}
	switch status {
	case http.StatusMethodNotAllowed:
		return scm.ReasonNotMergeable
	case http.StatusConflict:
		return conflictReason(text)
	case http.StatusForbidden:
		return forbiddenReason(text)
	default:
		return scm.ReasonInvalid
	}
}

func conflictReason(text string) scm.Reason {
	switch {
	case containsAny(text, "head"):
		return scm.ReasonHeadChanged
	case containsAny(text, "out of date", "outofdate", "out-of-date"):
		return scm.ReasonOutOfDate
	default:
		return scm.ReasonConflict
	}
}

func forbiddenReason(text string) scm.Reason {
	// "write permission is required" (Gitea 1.27) is a permission failure
	// even though it contains "required".
	// A token without the scope is refused with "token does not have at
	// least one of required scope(s)", which also contains "required".
	if !containsAny(text, "permission", "scope") && containsAny(text, "protect", "approv", "required", "status check") {
		return scm.ReasonBlockedByProtection
	}
	return scm.ReasonForbidden
}

func containsAny(text string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(text, part) {
			return true
		}
	}
	return false
}
