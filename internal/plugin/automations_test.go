package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"kandev-plugin-forgejo/internal/automations"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

const automationToken = "automation-secret-token"

// fakeInstance serves the endpoints Describe reads. Verify must reach none of
// them, which requests counts.
type fakeInstance struct {
	server   *httptest.Server
	requests atomic.Int64

	mu          sync.Mutex
	userID      int64
	version     string
	forgejo     bool
	repos       map[string]bool
	failUser    bool
	failRepo    bool
	failVersion bool
	failFlavor  bool
}

func newFakeInstance(t *testing.T, version string, forgejo bool) *fakeInstance {
	t.Helper()
	instance := &fakeInstance{userID: 7, version: version, forgejo: forgejo, repos: map[string]bool{"owner/repo": true}}
	instance.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instance.requests.Add(1)
		instance.mu.Lock()
		defer instance.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/user":
			if instance.failUser {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": instance.userID, "login": "kandev"})
		case r.URL.Path == "/api/v1/version":
			if instance.failVersion {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": instance.version})
		case r.URL.Path == "/api/forgejo/v1/version":
			if instance.failFlavor {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if !instance.forgejo {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": instance.version})
		case r.URL.Path == "/api/v1/repos/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": []any{
				map[string]any{"id": 1, "full_name": "owner/repo"},
				map[string]any{"id": 2, "full_name": "owner/other"},
			}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/"):
			if instance.failRepo {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
			if !instance.repos[name] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "full_name": name})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(instance.server.Close)
	return instance
}

func automationRuntime(t *testing.T, config map[string]any) (*Runtime, *configHost) {
	t.Helper()
	runtime := NewRuntime()
	host := newConfigHost(config)
	runtime.SetHost(host)
	t.Cleanup(runtime.Stop)
	return runtime, host
}

func describe(t *testing.T, runtime *Runtime, key string, settings map[string]any) (*pluginsdk.AutomationConditionResponse, error) {
	t.Helper()
	request := &pluginsdk.AutomationConditionRequest{WorkspaceId: "ws-1", ConditionKey: key}
	if settings != nil {
		raw, err := json.Marshal(settings)
		require.NoError(t, err)
		request.Config = raw
	}
	return runtime.DescribeAutomationCondition(context.Background(), request)
}

func TestDescribeListing(t *testing.T) {
	t.Parallel()
	t.Run("unconfigured", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, map[string]any{})
		response, err := describe(t, runtime, automations.ConditionPush, nil)
		require.NoError(t, err)
		require.False(t, response.Available)
		require.Equal(t, reasonNotConfigured, response.Reason)
	})
	t.Run("invalid instance URL asks for a settings fix", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, map[string]any{"base_url": "ftp://forge.example.com", "api_token": automationToken})
		for _, settings := range []map[string]any{nil, {"repository": "owner/repo"}} {
			response, err := describe(t, runtime, automations.ConditionPush, settings)
			require.NoError(t, err, "an error would make the host retry a delivery that cannot succeed")
			require.False(t, response.Available)
			require.Equal(t, reasonInvalidConfig, response.Reason)
		}
	})
	t.Run("unknown condition", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, map[string]any{})
		response, err := describe(t, runtime, "issues", nil)
		require.NoError(t, err)
		require.False(t, response.Available)
	})
	t.Run("suggests repositories", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		response, err := describe(t, runtime, automations.ConditionPush, nil)
		require.NoError(t, err)
		require.True(t, response.Available)
		require.Empty(t, response.ConnectionId, "a listing binds nothing")
		var options map[string][]string
		require.NoError(t, json.Unmarshal(response.ConfigOptions, &options))
		require.Equal(t, map[string][]string{"repository": {"owner/repo", "owner/other"}}, options)
	})
	t.Run("disabled workspace", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		setEnabled(t, runtime, "ws-1", false)
		response, err := describe(t, runtime, automations.ConditionPush, nil)
		require.NoError(t, err)
		require.False(t, response.Available)
		require.Equal(t, reasonDisabled, response.Reason)
	})
	t.Run("unreadable enabled state is an error", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		runtime, host := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		host.failState = true
		_, err := describe(t, runtime, automations.ConditionPush, map[string]any{"repository": "owner/repo"})
		require.Error(t, err)
	})
	for _, tc := range []struct {
		name, version string
		forgejo, ok   bool
	}{
		{"ci on forgejo 15", "15.0.10+gitea-1.22.0", true, true},
		{"ci on forgejo 11", "11.0.16+gitea-1.22.0", true, false},
		{"ci on gitea", "1.27.3", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			instance := newFakeInstance(t, tc.version, tc.forgejo)
			runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
			response, err := describe(t, runtime, automations.ConditionCIResult, nil)
			require.NoError(t, err)
			require.Equal(t, tc.ok, response.Available)
			if !tc.ok {
				require.Equal(t, reasonCIUnsupported, response.Reason)
			}
		})
	}
}

func TestDescribeConfigured(t *testing.T) {
	t.Parallel()
	settings := map[string]any{"repository": "owner/repo"}

	t.Run("binds to the instance and account", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		response, err := describe(t, runtime, automations.ConditionPush, settings)
		require.NoError(t, err)
		require.True(t, response.Available, response.Reason)
		require.Equal(t, connectionID(instance.server.URL), response.ConnectionId)
		require.Equal(t, connectionRevision(instance.server.URL, 7), response.ConnectionRevision)
		require.NotContains(t, response.ConnectionId, "127.0.0.1", "the host stores a digest, not the URL")

		// The host describes twice per delivery: the second is served from
		// cache, and push never asks for the version.
		before := instance.requests.Load()
		_, err = describe(t, runtime, automations.ConditionPush, settings)
		require.NoError(t, err)
		require.Equal(t, int64(0), instance.requests.Load()-before)
	})
	t.Run("a missing repository is not cached", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		missing := map[string]any{"repository": "owner/new"}
		response, err := describe(t, runtime, automations.ConditionPush, missing)
		require.NoError(t, err)
		require.Equal(t, reasonRepoNotFound, response.Reason)

		instance.mu.Lock()
		instance.repos["owner/new"] = true
		instance.mu.Unlock()
		response, err = describe(t, runtime, automations.ConditionPush, missing)
		require.NoError(t, err)
		require.True(t, response.Available, response.Reason)
	})
	t.Run("token rotation keeps the revision, an account change moves it", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		first, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": "token-a"})
		rotated, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": "token-b"})
		a, err := describe(t, first, automations.ConditionPush, settings)
		require.NoError(t, err)
		b, err := describe(t, rotated, automations.ConditionPush, settings)
		require.NoError(t, err)
		require.Equal(t, a.ConnectionRevision, b.ConnectionRevision)

		instance.mu.Lock()
		instance.userID = 8
		instance.mu.Unlock()
		other, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": "token-c"})
		c, err := describe(t, other, automations.ConditionPush, settings)
		require.NoError(t, err)
		require.Equal(t, a.ConnectionId, c.ConnectionId)
		require.NotEqual(t, a.ConnectionRevision, c.ConnectionRevision)
	})
	for name, tc := range map[string]struct {
		key      string
		settings map[string]any
		version  string
		forgejo  bool
		reason   string
	}{
		"invalid settings":    {automations.ConditionPush, map[string]any{"repository": "nope"}, "15.0.0", true, ""},
		"missing repository":  {automations.ConditionPush, map[string]any{"repository": "owner/missing"}, "15.0.0", true, reasonRepoNotFound},
		"ci on gitea":         {automations.ConditionCIResult, settings, "1.27.3", false, reasonCIUnsupported},
		"ci on forgejo 7":     {automations.ConditionCIResult, settings, "7.0.16+gitea-1.21.11", true, reasonCIUnsupported},
		"ci without version":  {automations.ConditionCIResult, settings, "", true, reasonVersionUnknown},
		"unknown field":       {automations.ConditionPush, map[string]any{"repository": "owner/repo", "x": true}, "15.0.0", true, ""},
		"pull request on old": {automations.ConditionPullRequest, map[string]any{"repository": "owner/missing"}, "7.0.16", true, reasonRepoNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			instance := newFakeInstance(t, tc.version, tc.forgejo)
			runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
			response, err := describe(t, runtime, tc.key, tc.settings)
			require.NoError(t, err)
			require.False(t, response.Available)
			require.Empty(t, response.ConnectionId)
			if tc.reason != "" {
				require.Equal(t, tc.reason, response.Reason)
			}
			require.NotContains(t, response.Reason, automationToken)
		})
	}
	t.Run("ci on forgejo 15", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
		runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		response, err := describe(t, runtime, automations.ConditionCIResult, settings)
		require.NoError(t, err)
		require.True(t, response.Available, response.Reason)
	})
	// The host's delivery worker cancels a receipt on available=false and
	// retries on an error, so a transient failure must be an error.
	for name, mutate := range map[string]func(*fakeInstance){
		"user endpoint down": func(f *fakeInstance) { f.failUser = true },
		"repository 500":     func(f *fakeInstance) { f.failRepo = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
			mutate(instance)
			runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
			_, err := describe(t, runtime, automations.ConditionPush, settings)
			require.ErrorIs(t, err, errInstanceUnreachable)
			require.NotContains(t, err.Error(), automationToken)
			require.NotContains(t, err.Error(), instance.server.URL)
		})
	}
	// A failed version or flavor probe must not be cached as "no version" or
	// "Gitea": ci_result would read unavailable, and the host would cancel the
	// delivery instead of retrying it.
	for name, mutate := range map[string]func(*fakeInstance, bool){
		"version endpoint down": func(f *fakeInstance, fail bool) { f.failVersion = fail },
		"flavor probe down":     func(f *fakeInstance, fail bool) { f.failFlavor = fail },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// No "+gitea-" suffix: only the namespace probe can tell Forgejo.
			instance := newFakeInstance(t, "15.0.10", true)
			runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
			instance.mu.Lock()
			mutate(instance, true)
			instance.mu.Unlock()
			_, err := describe(t, runtime, automations.ConditionCIResult, settings)
			require.ErrorIs(t, err, errInstanceUnreachable)

			instance.mu.Lock()
			mutate(instance, false)
			instance.mu.Unlock()
			response, err := describe(t, runtime, automations.ConditionCIResult, settings)
			require.NoError(t, err)
			require.True(t, response.Available, response.Reason)
		})
	}
	t.Run("version endpoint down does not affect push", func(t *testing.T) {
		t.Parallel()
		instance := newFakeInstance(t, "15.0.10", true)
		instance.failVersion = true
		runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
		response, err := describe(t, runtime, automations.ConditionPush, settings)
		require.NoError(t, err)
		require.True(t, response.Available, response.Reason)
	})
}

// fixture loads a delivery captured from a real instance.
func fixture(t *testing.T, host, name string) ([]byte, map[string]string, string) {
	t.Helper()
	dir := filepath.Join("..", "forgejo", "testdata", "webhooks", host)
	body, err := os.ReadFile(filepath.Join(dir, name+".body.json"))
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, name+".headers.json"))
	require.NoError(t, err)
	var headers map[string]string
	require.NoError(t, json.Unmarshal(raw, &headers))
	var payload struct {
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Run *struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"run"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	repository := ""
	if payload.Repository != nil {
		repository = payload.Repository.FullName
	} else {
		repository = payload.Run.Repository.FullName
	}
	return body, headers, repository
}

// The secret the fixtures were signed with (webhooks_live_test.go).
const fixtureSecret = "kandev-webhook-fixture-secret"

func resign(body []byte, secret string) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sum := hex.EncodeToString(mac.Sum(nil))
	return map[string]string{"x-forgejo-signature": sum, "x-gitea-signature": sum, "x-hub-signature-256": "sha256=" + sum}
}

func verify(t *testing.T, runtime *Runtime, request *pluginsdk.AutomationWebhookRequest) *pluginsdk.AutomationWebhookResponse {
	t.Helper()
	response, err := runtime.VerifyAutomationWebhook(context.Background(), request)
	require.NoError(t, err)
	return response
}

func TestVerifyAutomationWebhook(t *testing.T) {
	t.Parallel()
	instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
	config := map[string]any{"base_url": instance.server.URL, "api_token": automationToken}
	connection := connectionID(instance.server.URL)

	pushBody, pushHeaders, repository := fixture(t, "forgejo-15", "push")
	settings := func(key string, extra map[string]any) []byte {
		values := map[string]any{"repository": repository}
		for name, value := range extra {
			values[name] = value
		}
		raw, err := json.Marshal(values)
		require.NoError(t, err)
		return raw
	}
	request := func(key string, body []byte, headers map[string]string, cfg []byte) *pluginsdk.AutomationWebhookRequest {
		return &pluginsdk.AutomationWebhookRequest{
			WorkspaceId: "ws-1", ConditionKey: key, ConnectionId: connection, ConnectionRevision: "rev",
			Config: cfg, Body: body, Headers: headers, Secret: fixtureSecret,
		}
	}

	t.Run("accepts a captured push", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		response := verify(t, runtime, request(automations.ConditionPush, pushBody, pushHeaders, settings(automations.ConditionPush, nil)))
		require.Equal(t, outcomeAccepted, response.Outcome)
		require.Equal(t, automations.ConditionPush, response.EventKind)
		var data map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &data))
		require.Equal(t, "main", data["branch"])
		require.Equal(t, "forgejo", data["provider"])
		require.NotContains(t, string(response.Data), fixtureSecret)
	})

	t.Run("each condition accepts its captured event", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		for _, tc := range []struct {
			host, name, key string
			extra           map[string]any
		}{
			{"gitea-1.27", "pr_opened", automations.ConditionPullRequest, map[string]any{"on_opened": true}},
			{"forgejo-7", "pr_merged", automations.ConditionPullRequest, map[string]any{"on_merged": true}},
			{"forgejo-11", "pr_comment", automations.ConditionPullRequest, map[string]any{"on_commented": true}},
			{"forgejo-17", "run_failure_push", automations.ConditionCIResult, map[string]any{"on_failure": true}},
			{"forgejo-15", "run_success_pull_request", automations.ConditionCIResult, map[string]any{"on_success": true, "branches": []any{"feature/*"}}},
		} {
			body, headers, repo := fixture(t, tc.host, tc.name)
			values := map[string]any{"repository": repo}
			for name, value := range tc.extra {
				values[name] = value
			}
			cfg, err := json.Marshal(values)
			require.NoError(t, err)
			response := verify(t, runtime, request(tc.key, body, headers, cfg))
			require.Equal(t, outcomeAccepted, response.Outcome, tc.host+"/"+tc.name)
			require.Equal(t, tc.key, response.EventKind, "event_kind always equals the condition key")
		}
	})

	t.Run("rejections come before anything is parsed", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		cfg := settings(automations.ConditionPush, nil)
		otherRepo := []byte(strings.Replace(string(pushBody), repository, "someone/else", -1))
		for name, req := range map[string]*pluginsdk.AutomationWebhookRequest{
			"empty secret": func() *pluginsdk.AutomationWebhookRequest {
				r := request(automations.ConditionPush, pushBody, pushHeaders, cfg)
				r.Secret = ""
				return r
			}(),
			"wrong secret": func() *pluginsdk.AutomationWebhookRequest {
				r := request(automations.ConditionPush, pushBody, pushHeaders, cfg)
				r.Secret = "guess"
				return r
			}(),
			"no signature":                     request(automations.ConditionPush, pushBody, map[string]string{"x-forgejo-event": "push"}, cfg),
			"bad signature for other repo":     request(automations.ConditionPush, otherRepo, pushHeaders, cfg),
			"bad signature on unparsable body": request(automations.ConditionPush, []byte("{"), pushHeaders, cfg),
			"connection mismatch": func() *pluginsdk.AutomationWebhookRequest {
				r := request(automations.ConditionPush, pushBody, pushHeaders, cfg)
				r.ConnectionId = connectionID("https://elsewhere.example")
				return r
			}(),
			"unknown condition": request("issues", pushBody, pushHeaders, cfg),
			"unreadable settings": request(automations.ConditionPush, pushBody, pushHeaders,
				[]byte(`{"repository":"`+repository+`","branch":"main"}`)),
		} {
			response := verify(t, runtime, req)
			require.Equal(t, outcomeRejected, response.Outcome, name)
			require.Empty(t, response.Data, name)
		}
	})

	t.Run("signed but not for this condition is ignored", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		otherRepo := []byte(strings.Replace(string(pushBody), repository, "someone/else", -1))
		headers := resign(otherRepo, fixtureSecret)
		headers["x-forgejo-event"] = "push"
		prBody, prHeaders, _ := fixture(t, "forgejo-15", "pr_opened")
		tagBody, tagHeaders, _ := fixture(t, "forgejo-15", "push_tag")
		for name, req := range map[string]*pluginsdk.AutomationWebhookRequest{
			"another repository":   request(automations.ConditionPush, otherRepo, headers, settings(automations.ConditionPush, nil)),
			"another event":        request(automations.ConditionPush, prBody, prHeaders, settings(automations.ConditionPush, nil)),
			"pull request filters": request(automations.ConditionPullRequest, prBody, prHeaders, settings(automations.ConditionPullRequest, nil)),
			"tag push":             request(automations.ConditionPush, tagBody, tagHeaders, settings(automations.ConditionPush, nil)),
			"branch filter miss": request(automations.ConditionPush, pushBody, pushHeaders,
				settings(automations.ConditionPush, map[string]any{"branches": []any{"release/*"}})),
		} {
			response := verify(t, runtime, req)
			require.Equal(t, outcomeIgnored, response.Outcome, name)
			require.Empty(t, response.EventKind, name)
		}
	})

	t.Run("signed garbage is malformed", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		body := []byte(`{"ref":42}`)
		headers := resign(body, fixtureSecret)
		headers["x-forgejo-event"] = "push"
		response := verify(t, runtime, request(automations.ConditionPush, body, headers, settings(automations.ConditionPush, nil)))
		require.Equal(t, outcomeMalformed, response.Outcome)
	})

	t.Run("disabled workspace is ignored", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		setEnabled(t, runtime, "ws-1", false)
		response := verify(t, runtime, request(automations.ConditionPush, pushBody, pushHeaders, settings(automations.ConditionPush, nil)))
		require.Equal(t, outcomeIgnored, response.Outcome)
	})

	// The switch is a promise that nothing runs while it is off: an
	// unreadable switch fails the delivery rather than reading as on.
	t.Run("unreadable enabled state fails", func(t *testing.T) {
		t.Parallel()
		runtime, host := automationRuntime(t, config)
		host.failState = true
		_, err := runtime.VerifyAutomationWebhook(context.Background(), request(automations.ConditionPush, pushBody, pushHeaders, settings(automations.ConditionPush, nil)))
		require.Error(t, err)
	})

	t.Run("invalid instance URL rejects", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, map[string]any{"base_url": "ftp://forge.example.com", "api_token": automationToken})
		response := verify(t, runtime, request(automations.ConditionPush, pushBody, pushHeaders, settings(automations.ConditionPush, nil)))
		require.Equal(t, outcomeRejected, response.Outcome)
	})

	t.Run("unconfigured plugin rejects", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, map[string]any{})
		response := verify(t, runtime, request(automations.ConditionPush, pushBody, pushHeaders, settings(automations.ConditionPush, nil)))
		require.Equal(t, outcomeRejected, response.Outcome)
	})

	t.Run("nil request rejects", func(t *testing.T) {
		t.Parallel()
		runtime, _ := automationRuntime(t, config)
		response, err := runtime.VerifyAutomationWebhook(context.Background(), nil)
		require.NoError(t, err)
		require.Equal(t, outcomeRejected, response.Outcome)
	})
}

// Verify is a pure function of its input and the plugin config: no request
// reaches the instance, whatever the outcome.
func TestVerifyMakesNoProviderRequest(t *testing.T) {
	t.Parallel()
	instance := newFakeInstance(t, "15.0.10+gitea-1.22.0", true)
	runtime, _ := automationRuntime(t, map[string]any{"base_url": instance.server.URL, "api_token": automationToken})
	body, headers, repository := fixture(t, "forgejo-15", "pr_opened")
	cfg, err := json.Marshal(map[string]any{"repository": repository, "on_opened": true})
	require.NoError(t, err)
	for _, secret := range []string{fixtureSecret, "wrong"} {
		_, err := runtime.VerifyAutomationWebhook(context.Background(), &pluginsdk.AutomationWebhookRequest{
			WorkspaceId: "ws-1", ConditionKey: automations.ConditionPullRequest,
			ConnectionId: connectionID(instance.server.URL), Config: cfg, Body: body, Headers: headers, Secret: secret,
		})
		require.NoError(t, err)
	}
	require.Zero(t, instance.requests.Load())
}
