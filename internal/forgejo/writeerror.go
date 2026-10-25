package forgejo

import (
	"kandev-plugin-forgejo/internal/scm"
	"net/http"
	"strings"
)

// maxErrorBodyBytes bounds how much of an error body is read to classify it.
const maxErrorBodyBytes = 4 << 10

// classifyWrite derives a Reason from a status and a bounded error body. The
// phrases are matched case-insensitively against Gitea and Forgejo's messages;
// each is recorded in docs/development.md once verified on a live instance, and
// an unrecognised body falls back to the status alone.
func classifyWrite(status int, body string) scm.Reason {
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
		return scm.ReasonAlreadyMerged
	case has("[reason:", "enough approvals", "status check", "protected branch"):
		// A merge blocked by branch protection is a 405 on all four hosts, with
		// "not allowed to merge [reason: Does not have enough approvals]" (Gitea
		// 1.20, Forgejo) or just "Does not have enough approvals" (Gitea 1.27).
		// The reason must win over "not allowed to merge" below.
		return scm.ReasonBlockedByProtection
	case has("not allowed to merge", "not allowed to"):
		// Gitea and Forgejo answer a merge by a user without permission with
		// 405, not 403 (probed with a read-only collaborator on all four).
		return scm.ReasonForbidden
	case has("up to date"):
		// Gitea and Forgejo 7 answer 500 for a no-op branch update.
		return scm.ReasonUpToDate
	case has("try again later"):
		return scm.ReasonChecking
	case has("own pull", "your own", "own pr"):
		return scm.ReasonSelfReview
	}
	switch status {
	case http.StatusMethodNotAllowed:
		return scm.ReasonNotMergeable
	case http.StatusConflict:
		switch {
		case has("head"):
			return scm.ReasonHeadChanged
		case has("out of date", "outofdate", "out-of-date"):
			return scm.ReasonOutOfDate
		default:
			return scm.ReasonConflict
		}
	case http.StatusForbidden:
		// "write permission is required" (Gitea 1.27) is a permission failure
		// even though it contains "required".
		// A token without the scope is refused with "token does not have at
		// least one of required scope(s)", which also contains "required".
		if !has("permission", "scope") && has("protect", "approv", "required", "status check") {
			return scm.ReasonBlockedByProtection
		}
		return scm.ReasonForbidden
	default:
		return scm.ReasonInvalid
	}
}
