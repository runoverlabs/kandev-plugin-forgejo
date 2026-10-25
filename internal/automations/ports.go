package automations

import "context"

// Instance is what describing a condition needs to know about the configured
// instance. Every method may answer from a short-lived cache.
type Instance interface {
	// Scope names the configured instance. It fails with
	// scm.ErrNotConfigured or scm.ErrInvalidConfig when there is none.
	Scope(ctx context.Context) (string, error)
	// AccountID is the account the token acts as; scm.ErrUnauthorized when
	// the instance refuses the token.
	AccountID(ctx context.Context) (int64, error)
	// CIEvents reports whether the instance sends the Actions run events
	// ConditionCIResult needs. known is false when the instance does not
	// report its version; a failed request is an error, not unknown.
	CIEvents(ctx context.Context) (supported, known bool, err error)
	// RepositoryExists checks that the token can read owner/name:
	// scm.ErrNotFound or scm.ErrUnauthorized when it cannot.
	RepositoryExists(ctx context.Context, fullName string) error
	// RepositoryNames suggests repositories for the editor. A failure
	// suggests nothing.
	RepositoryNames(ctx context.Context) []string
}

// Webhooks authenticates, classifies and decodes the instance's deliveries
// without contacting it.
type Webhooks interface {
	// Verify checks the delivery's signature against secret.
	Verify(secret string, body []byte, headers map[string]string) error
	// Kind is the condition key a delivery's headers name, or "".
	Kind(headers map[string]string) string
	// Decode reads a verified delivery into the neutral event.
	Decode(body []byte, headers map[string]string) (Event, error)
}
