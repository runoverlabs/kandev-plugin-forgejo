package forgejo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"kandev-plugin-forgejo/internal/automations"
	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/scm"
)

// ConditionInstance answers what describing an automation condition needs to
// know about the instance, caching each answer briefly: the host calls
// Describe several times for every delivery.
type ConditionInstance struct {
	connection *Connection
	cache      automationCache
}

var _ automations.Instance = (*ConditionInstance)(nil)

// NewConditionInstance returns the condition-facing view of connection.
func NewConditionInstance(connection *Connection) *ConditionInstance {
	return &ConditionInstance{connection: connection}
}

// Scope names the configured instance.
func (s *ConditionInstance) Scope(ctx context.Context) (string, error) {
	return s.connection.Scope(ctx)
}

// AccountID is the id of the account the token acts as.
func (s *ConditionInstance) AccountID(ctx context.Context) (int64, error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return 0, err
	}
	info, err := s.instanceInfo(ctx, client)
	return info.userID, err
}

// CIEvents reports whether the instance sends Actions run events.
func (s *ConditionInstance) CIEvents(ctx context.Context) (supported, known bool, err error) {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return false, false, err
	}
	v, err := s.instanceVersion(ctx, client)
	if err != nil || v.version == "" {
		return false, false, err
	}
	return SupportsActionRunWebhooks(v.flavor, v.version), true, nil
}

// RepositoryExists checks that the token can read owner/name.
func (s *ConditionInstance) RepositoryExists(ctx context.Context, fullName string) error {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return err
	}
	return s.repositoryExists(ctx, client, fullName)
}

// RepositoryNames suggests the repositories the token sees.
func (s *ConditionInstance) RepositoryNames(ctx context.Context) []string {
	client, err := s.connection.Client(ctx)
	if err != nil {
		return nil
	}
	return s.repositoryOptions(ctx, client)
}

// Webhooks verifies and decodes deliveries; see webhooks.go.
type Webhooks struct{}

var _ automations.Webhooks = Webhooks{}

// Verify checks the delivery's HMAC signature.
func (Webhooks) Verify(secret string, body []byte, headers map[string]string) error {
	return VerifyWebhookSignature(secret, body, headers)
}

// Kind is the condition key the delivery's event header names.
func (Webhooks) Kind(headers map[string]string) string { return WebhookKind(headers) }

// Decode reads a verified delivery.
func (Webhooks) Decode(body []byte, headers map[string]string) (automations.Event, error) {
	return DecodeWebhook(body, headers)
}

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
	flavor  rest.Flavor
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
	client       *rest.Client
	info         *instanceInfo
	version      *instanceVersion
	repositories *repositoryOptions
	// found maps a lowercased owner/name the token could read to when it
	// was checked. Only a found repository is cached, so creating a missing
	// one or granting the token access takes effect at once.
	found map[string]time.Time
}

func (c *automationCache) forClient(client *rest.Client) {
	if c.client != client {
		c.client, c.info, c.version, c.repositories, c.found = client, nil, nil, nil, nil
	}
}

// instanceInfo returns the account, from cache when fresh.
func (s *ConditionInstance) instanceInfo(ctx context.Context, client *rest.Client) (instanceInfo, error) {
	cache := &s.cache
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
func (s *ConditionInstance) instanceVersion(ctx context.Context, client *rest.Client) (instanceVersion, error) {
	cache := &s.cache
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
	case errors.Is(err, scm.ErrNotFound), errors.Is(err, scm.ErrUnauthorized):
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
func (s *ConditionInstance) repositoryExists(ctx context.Context, client *rest.Client, repository string) error {
	key := strings.ToLower(repository)
	cache := &s.cache
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
func (s *ConditionInstance) repositoryOptions(ctx context.Context, client *rest.Client) []string {
	cache := &s.cache
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
