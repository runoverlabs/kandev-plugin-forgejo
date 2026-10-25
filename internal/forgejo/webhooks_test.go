package forgejo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kandev-plugin-forgejo/internal/automations"
	"kandev-plugin-forgejo/internal/forgejo/rest"

	"github.com/stretchr/testify/require"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main"}`)
	good := sign("s3cret", body)
	bad := sign("other", body)

	for name, tc := range map[string]struct {
		secret  string
		body    []byte
		headers map[string]string
		ok      bool
	}{
		"forgejo header alone":      {"s3cret", body, map[string]string{"x-forgejo-signature": good}, true},
		"gitea header alone":        {"s3cret", body, map[string]string{"x-gitea-signature": good}, true},
		"hub header alone":          {"s3cret", body, map[string]string{"x-hub-signature-256": "sha256=" + good}, true},
		"all three":                 {"s3cret", body, map[string]string{"x-forgejo-signature": good, "x-gitea-signature": good, "x-hub-signature-256": "sha256=" + good}, true},
		"upper-case hex":            {"s3cret", body, map[string]string{"x-gitea-signature": strings.ToUpper(good)}, true},
		"one bad among good":        {"s3cret", body, map[string]string{"x-forgejo-signature": good, "x-gitea-signature": bad}, false},
		"bad hub among good":        {"s3cret", body, map[string]string{"x-forgejo-signature": good, "x-hub-signature-256": "sha256=" + bad}, false},
		"prefix on bare-hex header": {"s3cret", body, map[string]string{"x-gitea-signature": "sha256=" + good}, false},
		"hub header without prefix": {"s3cret", body, map[string]string{"x-hub-signature-256": good}, false},
		"empty secret":              {"", body, map[string]string{"x-gitea-signature": sign("", body)}, false},
		"body changed by one byte":  {"s3cret", []byte(`{"ref":"refs/heads/mair"}`), map[string]string{"x-gitea-signature": good}, false},
		"no signature header":       {"s3cret", body, map[string]string{"x-gitea-event": "push"}, false},
		"truncated signature":       {"s3cret", body, map[string]string{"x-gitea-signature": good[:32]}, false},
		"right prefix, wrong tail":  {"s3cret", body, map[string]string{"x-gitea-signature": good[:32] + bad[32:]}, false},
		"empty signature":           {"s3cret", body, map[string]string{"x-gitea-signature": ""}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := VerifyWebhookSignature(tc.secret, tc.body, tc.headers)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrWebhookSignature)
			}
		})
	}
}

func TestWebhookKind(t *testing.T) {
	t.Parallel()
	for event, want := range map[string]string{
		"push":                  automations.ConditionPush,
		"pull_request":          automations.ConditionPullRequest,
		"issue_comment":         automations.ConditionPullRequest,
		"action_run_success":    automations.ConditionCIResult,
		"action_run_failure":    automations.ConditionCIResult,
		"action_run_recover":    "",
		"pull_request_approved": "",
		"status":                "",
		"create":                "",
	} {
		require.Equal(t, want, WebhookKind(map[string]string{"x-gitea-event": event}), event)
	}
	// Forgejo's own header wins.
	require.Equal(t, automations.ConditionPush,
		WebhookKind(map[string]string{"x-forgejo-event": "push", "x-gitea-event": "create"}))
}

// Every delivery captured from the CI containers (TestLiveWebhookDeliveries
// with KANDEV_FORGEJO_CAPTURE_DIR) verifies and decodes to what its scenario
// means. Re-capture when a release enters the matrix.
func TestDecodeCapturedWebhooks(t *testing.T) {
	t.Parallel()
	dirs, err := filepath.Glob(filepath.Join("testdata", "webhooks", "*"))
	require.NoError(t, err)
	require.Len(t, dirs, 5, "one fixture directory per CI container")
	for _, dir := range dirs {
		host := filepath.Base(dir)
		provider := string(rest.FlavorForgejo)
		if strings.HasPrefix(host, "gitea") {
			provider = string(rest.FlavorGitea)
		}
		bodies, err := filepath.Glob(filepath.Join(dir, "*.body.json"))
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(bodies), 13, host)
		repository := ""
		for _, path := range bodies {
			name := strings.TrimSuffix(filepath.Base(path), ".body.json")
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			var headers map[string]string
			raw, err := os.ReadFile(strings.TrimSuffix(path, ".body.json") + ".headers.json")
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &headers))
			if repository == "" {
				var payload struct {
					Repository struct {
						FullName string `json:"full_name"`
					} `json:"repository"`
				}
				require.NoError(t, json.Unmarshal(body, &payload))
				repository = payload.Repository.FullName
			}
			t.Run(host+"/"+name, func(t *testing.T) {
				require.NoError(t, VerifyWebhookSignature(fixtureSecret, body, headers))
				event, err := DecodeWebhook(body, headers)
				require.NoError(t, err)
				requireScenario(t, name, event, repository, provider)
			})
		}
	}
}

// What the captures pinned per host, asserted directly so a re-capture that
// changes them fails loudly.
func TestCapturedHostDifferences(t *testing.T) {
	t.Parallel()
	decode := func(host, name string) automations.Event {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("testdata", "webhooks", host, name+".body.json"))
		require.NoError(t, err)
		var headers map[string]string
		raw, err := os.ReadFile(filepath.Join("testdata", "webhooks", host, name+".headers.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &headers))
		event, err := DecodeWebhook(body, headers)
		require.NoError(t, err)
		return event
	}
	// Forgejo 7 sends a comment without its pull request; later hosts with it.
	require.False(t, decode("forgejo-7", "pr_comment").PullRequest.Detailed)
	for _, host := range []string{"forgejo-11", "forgejo-15", "forgejo-17", "gitea-1.27"} {
		require.True(t, decode(host, "pr_comment").PullRequest.Detailed, host)
	}
	// Forgejo 7 has no draft field: the WIP title prefix stands in.
	raw, err := os.ReadFile(filepath.Join("testdata", "webhooks", "forgejo-7", "pr_draft_opened.body.json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"draft"`)
	require.True(t, decode("forgejo-7", "pr_draft_opened").PullRequest.Draft)
	// Gitea sends no x-forgejo-* headers.
	headers, err := os.ReadFile(filepath.Join("testdata", "webhooks", "gitea-1.27", "push.headers.json"))
	require.NoError(t, err)
	require.NotContains(t, string(headers), "x-forgejo")
}

func TestDecodeWebhookEdges(t *testing.T) {
	t.Parallel()
	forgejo := func(event string) map[string]string { return map[string]string{"x-forgejo-event": event} }

	t.Run("not a JSON object is malformed", func(t *testing.T) {
		for _, body := range []string{`[]`, `"x"`, `{`, ``, `null`} {
			_, err := DecodeWebhook([]byte(body), forgejo("push"))
			require.ErrorIs(t, err, ErrMalformedWebhook, body)
		}
	})
	t.Run("missing repository is malformed", func(t *testing.T) {
		_, err := DecodeWebhook([]byte(`{"ref":"refs/heads/main"}`), forgejo("push"))
		require.ErrorIs(t, err, ErrMalformedWebhook)
		_, err = DecodeWebhook([]byte(`{"action":"success","run":{"status":"success","repository":{}}}`), forgejo("action_run_success"))
		require.ErrorIs(t, err, ErrMalformedWebhook)
	})
	t.Run("unhandled event decodes to nothing", func(t *testing.T) {
		event, err := DecodeWebhook([]byte(`{}`), forgejo("create"))
		require.NoError(t, err)
		require.Empty(t, event.Kind)
	})
	t.Run("comment on an issue is not a pull request event", func(t *testing.T) {
		event, err := DecodeWebhook([]byte(`{"action":"created","is_pull":false,"comment":{"body":"x"},"repository":{"full_name":"o/r"}}`),
			forgejo("issue_comment"))
		require.NoError(t, err)
		require.Empty(t, event.Kind)
	})
	t.Run("edited comment keeps a non-matching action", func(t *testing.T) {
		event, err := DecodeWebhook([]byte(`{"action":"edited","is_pull":true,"issue":{"number":1},"comment":{"body":"x"},"repository":{"full_name":"o/r"}}`),
			forgejo("issue_comment"))
		require.NoError(t, err)
		require.Equal(t, "comment_edited", event.PullRequest.Action)
	})
	t.Run("deleted fork head counts as a fork", func(t *testing.T) {
		event, err := DecodeWebhook([]byte(`{"action":"opened","number":3,"pull_request":{"number":3,"base":{"ref":"main","repo_id":1},"head":{"ref":"x","repo_id":0}},"repository":{"full_name":"o/r"}}`),
			forgejo("pull_request"))
		require.NoError(t, err)
		require.True(t, event.PullRequest.Fork)
	})
	t.Run("cancelled run delivered as a failure keeps its state", func(t *testing.T) {
		// Shape of a Forgejo 17.0.0 delivery: a run cancelled by a newer push
		// arrives as action_run_failure.
		body := `{"action":"failure","prior_status":"waiting","run":{"status":"cancelled","workflow_id":"ci.yml",` +
			`"index_in_repo":2,"commit_sha":"abc","prettyref":"feature/x","event":"push","is_fork_pull_request":false,` +
			`"repository":{"full_name":"o/r"},"event_payload":"{\"ref\":\"refs/heads/feature/x\"}"}}`
		event, err := DecodeWebhook([]byte(body), forgejo("action_run_failure"))
		require.NoError(t, err)
		require.Equal(t, automations.StateCancelled, event.Run.State)
		require.Equal(t, "feature/x", event.Run.Branch)
	})
	t.Run("run branch", func(t *testing.T) {
		for _, tc := range []struct {
			event, payload, branch string
			number                 int64
		}{
			{"push", `{"ref":"refs/heads/release/1"}`, "release/1", 0},
			{"push", `{"ref":"refs/tags/v1"}`, "", 0},
			{"pull_request", `{"pull_request":{"number":4,"head":{"ref":"feature"}}}`, "feature", 4},
			{"pull_request", `{}`, "", 0},
			{"schedule", `{"ref":"refs/heads/main"}`, "", 0},
			{"push", `not json`, "", 0},
		} {
			branch, number := runBranch(tc.event, tc.payload)
			require.Equal(t, tc.branch, branch, tc.payload)
			require.Equal(t, tc.number, number, tc.payload)
		}
	})
}

func TestSupportsActionRunWebhooks(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]bool{
		"7.0.16+gitea-1.21.11": false,
		"11.0.16+gitea-1.22.0": false,
		"12.0.0+gitea-1.22.0":  true,
		"15.0.10+gitea-1.22.0": true,
		"17.0.0-dev-123":       true,
		"":                     false,
		"garbage":              false,
	} {
		require.Equal(t, want, SupportsActionRunWebhooks(rest.FlavorForgejo, version), version)
	}
	require.False(t, SupportsActionRunWebhooks(rest.FlavorGitea, "1.27.3"))
}
