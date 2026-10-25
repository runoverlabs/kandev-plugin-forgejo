package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	"kandev-plugin-forgejo/internal/automations"
	"kandev-plugin-forgejo/internal/scm"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

var _ pluginsdk.AutomationAdapter = (*Runtime)(nil)

// Verification outcomes the host maps onto HTTP statuses: rejected 401,
// malformed 400, ignored 200, accepted 202.
const (
	outcomeRejected  = "rejected"
	outcomeMalformed = "malformed"
	outcomeIgnored   = "ignored"
	outcomeAccepted  = "accepted"
)

// errInstanceUnreachable is the RPC error for a transient failure in
// Describe. The host's delivery worker retries an error but cancels the
// receipt on available=false, so a blip on the instance must not be reported
// as unavailable. The message is fixed: it can reach the editor.
var errInstanceUnreachable = errors.New("kandev-plugin-forgejo: could not reach the Forgejo instance; try again")

// Fixed, token-safe reasons for an unavailable condition.
const (
	reasonDisabled       = "The Forgejo integration is turned off for this workspace."
	reasonNotConfigured  = "Set the instance URL and access token in Settings > Plugins > Forgejo."
	reasonInvalidConfig  = "The instance URL is not a valid http or https URL. Fix it in Settings > Plugins > Forgejo."
	reasonTokenRejected  = "The instance rejected the access token."
	reasonRepoNotFound   = "The repository was not found, or the access token cannot read it."
	reasonUnknown        = "This condition is not offered by this version of the plugin."
	reasonCIUnsupported  = "CI results need Forgejo 12 or later: they come from Forgejo Actions run events, which Gitea and older Forgejo releases do not send."
	reasonVersionUnknown = "CI results need Forgejo 12 or later, and the instance did not report its version."
)

// DescribeAutomationCondition reports whether a condition can be used and, for
// a saved configuration, the connection identity the host binds it to.
func (r *Runtime) DescribeAutomationCondition(ctx context.Context, request *pluginsdk.AutomationConditionRequest) (*pluginsdk.AutomationConditionResponse, error) {
	if request == nil {
		return nil, errors.New("kandev-plugin-forgejo: condition request is required")
	}
	key := request.ConditionKey
	if !automations.Known(key) {
		return unavailable(reasonUnknown), nil
	}
	listing := len(bytes.TrimSpace(request.Config)) == 0
	var cfg automations.Config
	if !listing {
		parsed, err := automations.Parse(key, request.Config)
		if err != nil {
			// The message is fixed text plus a field name or the operator's own
			// pattern; nothing from the instance.
			return unavailable(err.Error()), nil
		}
		cfg = parsed
	}
	enabled, err := r.integrationEnabledStrict(ctx, request.WorkspaceId)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return unavailable(reasonDisabled), nil
	}
	scope, err := r.conditions.Scope(ctx)
	if errors.Is(err, scm.ErrNotConfigured) {
		return unavailable(reasonNotConfigured), nil
	}
	if errors.Is(err, scm.ErrInvalidConfig) {
		return unavailable(reasonInvalidConfig), nil
	}
	if err != nil {
		return nil, errInstanceUnreachable
	}
	if listing {
		return r.describeListing(ctx, key), nil
	}
	return r.describeConfigured(ctx, scope, cfg)
}

// describeListing answers the editor's condition list. The host gives every
// condition of every plugin five seconds together, so a provider failure here
// leaves the condition available without suggestions; saving it runs the full
// check.
func (r *Runtime) describeListing(ctx context.Context, key string) *pluginsdk.AutomationConditionResponse {
	if key == automations.ConditionCIResult {
		if supported, known, err := r.conditions.CIEvents(ctx); err == nil && known && !supported {
			return unavailable(reasonCIUnsupported)
		}
	}
	response := &pluginsdk.AutomationConditionResponse{Available: true}
	if names := r.conditions.RepositoryNames(ctx); len(names) > 0 {
		if encoded, err := json.Marshal(map[string][]string{"repository": names}); err == nil {
			response.ConfigOptions = encoded
		}
	}
	return response
}

// describeConfigured runs its lookups concurrently, so a cold cache costs one
// round-trip of the host's five-second budget rather than four, then judges
// them in a fixed order.
func (r *Runtime) describeConfigured(ctx context.Context, scope string, cfg automations.Config) (*pluginsdk.AutomationConditionResponse, error) {
	var (
		wg                           sync.WaitGroup
		accountID                    int64
		ciSupported, ciKnown         bool
		infoErr, versionErr, repoErr error
	)
	wg.Go(func() { accountID, infoErr = r.conditions.AccountID(ctx) })
	if cfg.Key == automations.ConditionCIResult {
		wg.Go(func() { ciSupported, ciKnown, versionErr = r.conditions.CIEvents(ctx) })
	}
	wg.Go(func() { repoErr = r.conditions.RepositoryExists(ctx, cfg.Repository) })
	wg.Wait()

	if errors.Is(infoErr, scm.ErrUnauthorized) {
		return unavailable(reasonTokenRejected), nil
	}
	if infoErr != nil {
		return nil, errInstanceUnreachable
	}
	if cfg.Key == automations.ConditionCIResult {
		if versionErr != nil {
			return nil, errInstanceUnreachable
		}
		if !ciKnown {
			return unavailable(reasonVersionUnknown), nil
		}
		if !ciSupported {
			return unavailable(reasonCIUnsupported), nil
		}
	}
	if errors.Is(repoErr, scm.ErrNotFound) || errors.Is(repoErr, scm.ErrUnauthorized) {
		return unavailable(reasonRepoNotFound), nil
	}
	if repoErr != nil {
		return nil, errInstanceUnreachable
	}
	return &pluginsdk.AutomationConditionResponse{
		Available:          true,
		ConnectionId:       connectionID(scope),
		ConnectionRevision: connectionRevision(scope, accountID),
	}, nil
}

// VerifyAutomationWebhook authenticates and classifies one delivery. It makes
// no provider request: a slow instance must not hold the host's ten-second
// webhook window, and the outcome stays a function of the request and the
// plugin config.
func (r *Runtime) VerifyAutomationWebhook(ctx context.Context, request *pluginsdk.AutomationWebhookRequest) (*pluginsdk.AutomationWebhookResponse, error) {
	if request == nil || !automations.Known(request.ConditionKey) {
		return outcome(outcomeRejected), nil
	}
	key := request.ConditionKey
	// Nothing is parsed before the signature verifies.
	if err := r.webhooks.Verify(request.Secret, request.Body, request.Headers); err != nil {
		return outcome(outcomeRejected), nil
	}
	scope, err := r.conditions.Scope(ctx)
	if errors.Is(err, scm.ErrNotConfigured) || errors.Is(err, scm.ErrInvalidConfig) {
		return outcome(outcomeRejected), nil
	}
	if err != nil {
		// A host config read failure is transient. An error makes the host
		// answer 503; neither the host nor Forgejo retries it, but the failed
		// delivery stays on the hook's page for a manual redelivery.
		return nil, err
	}
	if request.ConnectionId != connectionID(scope) {
		return outcome(outcomeRejected), nil
	}
	if r.webhooks.Kind(request.Headers) != key {
		return outcome(outcomeIgnored), nil
	}
	event, err := r.webhooks.Decode(request.Body, request.Headers)
	if err != nil {
		return outcome(outcomeMalformed), nil
	}
	if event.Kind != key {
		return outcome(outcomeIgnored), nil
	}
	cfg, err := automations.Parse(key, request.Config)
	if err != nil {
		// The host validated these settings with Describe when they were
		// saved. Settings this version cannot read are not ones it can honour.
		return outcome(outcomeRejected), nil
	}
	// Correctly signed but for another repository: the hook was put on the
	// wrong repository, not forged.
	if !strings.EqualFold(event.Repository, cfg.Repository) {
		return outcome(outcomeIgnored), nil
	}
	enabled, err := r.integrationEnabledStrict(ctx, request.WorkspaceId)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return outcome(outcomeIgnored), nil
	}
	data, matched := automations.Match(cfg, event)
	if !matched {
		return outcome(outcomeIgnored), nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &pluginsdk.AutomationWebhookResponse{Outcome: outcomeAccepted, EventKind: key, Data: encoded}, nil
}

// connectionID names the configured instance. A digest, so the host's
// database holds no URL.
func connectionID(scope string) string {
	sum := sha256.Sum256([]byte("forgejo-instance\x00" + scope))
	return hex.EncodeToString(sum[:])
}

// connectionRevision changes with the instance or the account, never with a
// token rotation for the same account: the webhook secret does not depend on
// the token, so rotating it must not break automations.
func connectionRevision(scope string, userID int64) string {
	sum := sha256.Sum256([]byte("forgejo-account\x00" + scope + "\x00" + strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(sum[:])
}

func unavailable(reason string) *pluginsdk.AutomationConditionResponse {
	return &pluginsdk.AutomationConditionResponse{Available: false, Reason: reason}
}

func outcome(value string) *pluginsdk.AutomationWebhookResponse {
	return &pluginsdk.AutomationWebhookResponse{Outcome: value}
}
