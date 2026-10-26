package scm

import (
	"errors"
	"fmt"
)

// ErrNotFound is a 404 or 410 from the instance. Callers translate it into
// "not owned by this connection" rather than surfacing it as a transport error.
var ErrNotFound = errors.New("forgejo: resource not found")

// ErrUnauthorized is a 401/403 from the instance. It never carries the response
// body, which on some deployments echoes the presented token.
var ErrUnauthorized = errors.New("forgejo: not authorized")

// ErrNotConfigured means the operator has not supplied a usable instance URL
// and token yet. Callers surface it as a configuration problem rather than a
// provider outage.
var ErrNotConfigured = errors.New("forgejo: plugin is not configured with an instance URL and access token")

// ErrInvalidConfig means the configured instance URL cannot be used (not an
// absolute http or https URL). Like ErrNotConfigured it is fixed by editing
// the settings, never by retrying.
var ErrInvalidConfig = errors.New("forgejo: the configured instance URL is not valid")

// ErrNotLinked means the requested pull request is not among those linked to
// the task. It is the authorization failure for every pull request action: a
// number is only ever honoured when the task already owns it.
var ErrNotLinked = errors.New("forgejo: pull request is not linked to this task")

// ErrAmbiguous means the task links several pull requests and the caller named
// none of them.
var ErrAmbiguous = errors.New("forgejo: task links more than one pull request")

// InvalidError is a request the plugin refuses before it reaches the instance.
// Message is authored by the plugin, never copied from a provider, so it is
// safe to show.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return "forgejo: invalid request: " + e.Message }

// Invalid refuses a request with an operator-facing message.
func Invalid(message string) error { return &InvalidError{Message: message} }

// WriteError is a failed write with a classified reason. It carries neither
// the response body nor the request URL. 404 and 401 are not classified: they
// stay ErrNotFound and ErrUnauthorized, because a 401 really is a bad token
// whereas a 403 on a write is usually a missing scope or permission.
type WriteError struct {
	Status int
	Reason Reason
}

func (e *WriteError) Error() string {
	return fmt.Sprintf("forgejo: write refused (%d): %s", e.Status, e.Reason)
}

// Reason is a fixed, token-safe classification of a failed write. It is the
// only part of a provider error body that ever leaves the adapter: the body
// itself is read for classification and then dropped.
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
