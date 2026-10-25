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
	"time"

	"kandev-plugin-forgejo/internal/automations"
	"kandev-plugin-forgejo/internal/forgejo"

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

// automationInfoTTL bounds how long the account, version and repository
// checks behind Describe are reused. The host calls Describe with config twice per accepted delivery;
// Kandev restarts the plugin when its config changes, so a cached answer never
// outlives a token or URL change.
const automationInfoTTL = time.Minute

// maxRepositoryOptions is how many repositories the editor suggests. The host
// allows 100 per field.
const maxRepositoryOptions = 50

type instanceInfo struct {
	userID int64
	at     time.Time
}

// instanceVersion is what the instance reported about itself. An empty version
// is a definitive answer (the instance did not report one), never a failed
// request: those are not cached.
type instanceVersion struct {
	flavor  forgejo.Flavor
	version string
	at      time.Time
}

type repositoryOptions struct {
	names []string
	at    time.Time
}

// automationCache is keyed by the resolved client, which Connection rebuilds
// whenever the URL or token changes.
type automationCache struct {
	mu           sync.Mutex
	client       *forgejo.Client
	info         *instanceInfo
	version      *instanceVersion
	repositories *repositoryOptions
	// found maps a lowercased owner/name the token could read to when it
	// was checked. Only a found repository is cached, so creating a missing
	// one or granting the token access takes effect at once.
	found map[string]time.Time
}

func (c *automationCache) forClient(client *forgejo.Client) {
	if c.client != client {
		c.client, c.info, c.version, c.repositories, c.found = client, nil, nil, nil, nil
	}
}

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
	client, err := r.connection.Client(ctx)
	if errors.Is(err, forgejo.ErrNotConfigured) {
		return unavailable(reasonNotConfigured), nil
	}
	if errors.Is(err, forgejo.ErrInvalidConfig) {
		return unavailable(reasonInvalidConfig), nil
	}
	if err != nil {
		return nil, errInstanceUnreachable
	}
	if listing {
		return r.describeListing(ctx, client, key), nil
	}
	return r.describeConfigured(ctx, client, cfg)
}

// describeListing answers the editor's condition list. The host gives every
// condition of every plugin five seconds together, so a provider failure here
// leaves the condition available without suggestions; saving it runs the full
// check.
func (r *Runtime) describeListing(ctx context.Context, client *forgejo.Client, key string) *pluginsdk.AutomationConditionResponse {
	if key == automations.ConditionCIResult {
		if v, err := r.instanceVersion(ctx, client); err == nil && v.version != "" &&
			!forgejo.SupportsActionRunWebhooks(v.flavor, v.version) {
			return unavailable(reasonCIUnsupported)
		}
	}
	response := &pluginsdk.AutomationConditionResponse{Available: true}
	if names := r.repositoryOptions(ctx, client); len(names) > 0 {
		if encoded, err := json.Marshal(map[string][]string{"repository": names}); err == nil {
			response.ConfigOptions = encoded
		}
	}
	return response
}

// describeConfigured runs its lookups concurrently, so a cold cache costs one
// round-trip of the host's five-second budget rather than four, then judges
// them in a fixed order.
func (r *Runtime) describeConfigured(ctx context.Context, client *forgejo.Client, cfg automations.Config) (*pluginsdk.AutomationConditionResponse, error) {
	var (
		wg                           sync.WaitGroup
		info                         instanceInfo
		version                      instanceVersion
		infoErr, versionErr, repoErr error
	)
	wg.Go(func() { info, infoErr = r.instanceInfo(ctx, client) })
	if cfg.Key == automations.ConditionCIResult {
		wg.Go(func() { version, versionErr = r.instanceVersion(ctx, client) })
	}
	wg.Go(func() { repoErr = r.repositoryExists(ctx, client, cfg.Repository) })
	wg.Wait()

	if errors.Is(infoErr, forgejo.ErrUnauthorized) {
		return unavailable(reasonTokenRejected), nil
	}
	if infoErr != nil {
		return nil, errInstanceUnreachable
	}
	if cfg.Key == automations.ConditionCIResult {
		if versionErr != nil {
			return nil, errInstanceUnreachable
		}
		if version.version == "" {
			return unavailable(reasonVersionUnknown), nil
		}
		if !forgejo.SupportsActionRunWebhooks(version.flavor, version.version) {
			return unavailable(reasonCIUnsupported), nil
		}
	}
	if errors.Is(repoErr, forgejo.ErrNotFound) || errors.Is(repoErr, forgejo.ErrUnauthorized) {
		return unavailable(reasonRepoNotFound), nil
	}
	if repoErr != nil {
		return nil, errInstanceUnreachable
	}
	return &pluginsdk.AutomationConditionResponse{
		Available:          true,
		ConnectionId:       connectionID(client.Scope()),
		ConnectionRevision: connectionRevision(client.Scope(), info.userID),
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
	if err := forgejo.VerifyWebhookSignature(request.Secret, request.Body, request.Headers); err != nil {
		return outcome(outcomeRejected), nil
	}
	scope, err := r.connection.Scope(ctx)
	if errors.Is(err, forgejo.ErrNotConfigured) || errors.Is(err, forgejo.ErrInvalidConfig) {
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
	if forgejo.WebhookKind(request.Headers) != key {
		return outcome(outcomeIgnored), nil
	}
	event, err := forgejo.DecodeWebhook(request.Body, request.Headers)
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

// instanceInfo returns the account, from cache when fresh.
func (r *Runtime) instanceInfo(ctx context.Context, client *forgejo.Client) (instanceInfo, error) {
	cache := &r.automationCache
	cache.mu.Lock()
	cache.forClient(client)
	if info := cache.info; info != nil && time.Since(info.at) < automationInfoTTL {
		cache.mu.Unlock()
		return *info, nil
	}
	cache.mu.Unlock()

	user, err := client.CurrentUser(ctx)
	if err != nil {
		return instanceInfo{}, err
	}
	info := instanceInfo{userID: user.ID, at: time.Now()}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.forClient(client)
	cache.info = &info
	return info, nil
}

// instanceVersion returns the version and flavor, from cache when fresh. Only
// ci_result needs them, so other conditions never pay for the requests. A
// failed request is an error, not an empty version: the host cancels a
// delivery on available=false, so a blip must not read as "no version".
func (r *Runtime) instanceVersion(ctx context.Context, client *forgejo.Client) (instanceVersion, error) {
	cache := &r.automationCache
	cache.mu.Lock()
	cache.forClient(client)
	if v := cache.version; v != nil && time.Since(v.at) < automationInfoTTL {
		cache.mu.Unlock()
		return *v, nil
	}
	cache.mu.Unlock()

	result := instanceVersion{at: time.Now()}
	version, err := client.Version(ctx)
	switch {
	case errors.Is(err, forgejo.ErrNotFound), errors.Is(err, forgejo.ErrUnauthorized):
		// The endpoint is hidden: a lasting answer, not a failure.
	case err != nil:
		return instanceVersion{}, err
	case strings.TrimSpace(version) != "":
		flavor, err := client.ProbeFlavor(ctx, version)
		if err != nil {
			return instanceVersion{}, err
		}
		result.version, result.flavor = version, flavor
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.forClient(client)
	cache.version = &result
	return result, nil
}

// repositoryExists checks that the token can read a repository, from cache
// when it was found recently.
func (r *Runtime) repositoryExists(ctx context.Context, client *forgejo.Client, repository string) error {
	key := strings.ToLower(repository)
	cache := &r.automationCache
	cache.mu.Lock()
	cache.forClient(client)
	if at, ok := cache.found[key]; ok && time.Since(at) < automationInfoTTL {
		cache.mu.Unlock()
		return nil
	}
	cache.mu.Unlock()

	owner, name, _ := strings.Cut(repository, "/")
	if _, err := client.Repo(ctx, owner, name); err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.forClient(client)
	now := time.Now()
	for stale, at := range cache.found {
		if now.Sub(at) >= automationInfoTTL {
			delete(cache.found, stale)
		}
	}
	if cache.found == nil {
		cache.found = map[string]time.Time{}
	}
	cache.found[key] = now
	return nil
}

// repositoryOptions suggests the repositories the token sees, most recently
// updated first. A failure suggests nothing; the field still takes free text.
func (r *Runtime) repositoryOptions(ctx context.Context, client *forgejo.Client) []string {
	cache := &r.automationCache
	cache.mu.Lock()
	cache.forClient(client)
	if options := cache.repositories; options != nil && time.Since(options.at) < automationInfoTTL {
		cache.mu.Unlock()
		return options.names
	}
	cache.mu.Unlock()

	repos, err := client.SearchRepos(ctx, "", 1, maxRepositoryOptions)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo.FullName != "" && len(names) < maxRepositoryOptions {
			names = append(names, repo.FullName)
		}
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.forClient(client)
	cache.repositories = &repositoryOptions{names: names, at: time.Now()}
	return names
}

func unavailable(reason string) *pluginsdk.AutomationConditionResponse {
	return &pluginsdk.AutomationConditionResponse{Available: false, Reason: reason}
}

func outcome(value string) *pluginsdk.AutomationWebhookResponse {
	return &pluginsdk.AutomationWebhookResponse{Outcome: value}
}
