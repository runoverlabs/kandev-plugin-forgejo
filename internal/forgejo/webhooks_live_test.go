package forgejo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kandev-plugin-forgejo/internal/automations"
	"kandev-plugin-forgejo/internal/forgejo/rest"

	"github.com/stretchr/testify/require"
)

// fixtureSecret signs the live deliveries and therefore the captured fixtures.
// It is a test value, not a credential.
const fixtureSecret = "kandev-webhook-fixture-secret"

// TestLiveWebhookDeliveries has the instance sign and send real deliveries for
// every scenario the conditions care about and runs each through the verifier
// and the decoder. It creates and deletes its own repository, so it does not
// disturb the seeded one.
//
// The instance must be able to reach a listener on this machine: CI runs the
// container on the host network and opens the webhook allow-list
// (`webhook.ALLOWED_HOST_LIST=*`). KANDEV_FORGEJO_WEBHOOK_HOST overrides the
// address the hook points at (default 127.0.0.1).
//
// Optional:
//   - KANDEV_FORGEJO_REVIEWER_TOKEN adds a pull request from a fork.
//   - KANDEV_FORGEJO_RUNNER_LABEL names a registered Actions runner label; on
//     Forgejo 12+ the test then adds a workflow and waits for run events.
//   - KANDEV_FORGEJO_CAPTURE_DIR writes every delivery there as a fixture
//     (`<scenario>.headers.json` and `<scenario>.body.json`).
func TestLiveWebhookDeliveries(t *testing.T) {
	client, _, _, _, _, _ := liveAdapters(t)
	ctx := context.Background()
	version, err := client.Version(ctx)
	require.NoError(t, err)
	flavor := client.DetectFlavor(ctx, version)
	user, err := client.CurrentUser(ctx)
	require.NoError(t, err)

	receiver := newWebhookReceiver(t)
	repo := fmt.Sprintf("webhooks-%d", time.Now().UnixNano())
	owner := user.Login
	require.NoError(t, client.Do(ctx, http.MethodPost, "/user/repos", nil, map[string]any{
		"name": repo, "auto_init": true, "default_branch": "main",
	}, nil))
	t.Cleanup(func() {
		_ = client.Do(context.Background(), http.MethodDelete, "/repos/"+owner+"/"+repo, nil, nil, nil)
	})
	base := "/repos/" + owner + "/" + repo

	reviewer := liveOptionalClient(t, "KANDEV_FORGEJO_REVIEWER_TOKEN")
	runnerLabel := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_RUNNER_LABEL"))
	withRuns := runnerLabel != "" && SupportsActionRunWebhooks(flavor, version)

	// Labels by id: Forgejo 7 refuses names (422).
	var label struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/labels", nil, map[string]any{"name": "bug", "color": "#ee0701"}, &label))
	hookType := "gitea"
	if flavor == rest.FlavorForgejo {
		hookType = "forgejo"
	}
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/hooks", nil, map[string]any{
		"type":   hookType,
		"active": true,
		"events": []string{"push", "pull_request", "issue_comment", "action_run_success", "action_run_failure"},
		"config": map[string]any{"url": receiver.url, "content_type": "json", "secret": fixtureSecret},
	}, nil))

	putFile := func(branch, newBranch, file, message string) {
		t.Helper()
		body := map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(message)), "message": message}
		if branch != "" {
			body["branch"] = branch
		}
		if newBranch != "" {
			body["new_branch"] = newBranch
		}
		require.NoError(t, client.Do(ctx, http.MethodPost, base+"/contents/"+file, nil, body, nil))
		time.Sleep(500 * time.Millisecond)
	}

	if withRuns {
		// A run fails on the branch named "fail" and succeeds everywhere else,
		// including the pull request run for that branch (its ref is the merge
		// ref).
		putFile("main", "", ".forgejo/workflows/ci.yml", "on: [push, pull_request]\n"+
			"jobs:\n  test:\n    runs-on: "+runnerLabel+"\n    steps:\n"+
			"      - run: test \"${GITHUB_REF_NAME}\" != fail\n")
	} else {
		putFile("main", "", "README-2.md", "push to main")
	}
	putFile("main", "feature/x", "x.txt", "feature commit")
	var pull rest.PullRequest
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/pulls", nil, map[string]any{
		"head": "feature/x", "base": "main", "title": "Add x", "body": "webhook capture",
	}, &pull))
	time.Sleep(500 * time.Millisecond)
	number := strconv.FormatInt(pull.Number, 10)
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/issues/"+number+"/labels", nil, map[string]any{"labels": []int64{label.ID}}, nil))
	if reviewer != nil {
		reviewerUser, err := reviewer.CurrentUser(ctx)
		require.NoError(t, err)
		require.NoError(t, client.Do(ctx, http.MethodPut, base+"/collaborators/"+reviewerUser.Login, nil, map[string]any{"permission": "write"}, nil))
		require.NoError(t, client.Do(ctx, http.MethodPost, base+"/pulls/"+number+"/requested_reviewers", nil,
			map[string]any{"reviewers": []string{reviewerUser.Login}}, nil))
	}
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/issues/"+number+"/comments", nil, map[string]any{"body": "a comment"}, nil))
	putFile("feature/x", "", "y.txt", "sync commit")
	time.Sleep(2 * time.Second)
	mergeEventually(t, client, base+"/pulls/"+number+"/merge")

	putFile("main", "fail", "z.txt", "draft change")
	var draft rest.PullRequest
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/pulls", nil, map[string]any{
		"head": "fail", "base": "main", "title": "WIP: draft change",
	}, &draft))
	time.Sleep(500 * time.Millisecond)
	draftPath := base + "/pulls/" + strconv.FormatInt(draft.Number, 10)
	require.NoError(t, client.Do(ctx, http.MethodPatch, draftPath, nil, map[string]any{"state": "closed"}, nil))
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, client.Do(ctx, http.MethodPatch, draftPath, nil, map[string]any{"state": "open"}, nil))
	require.NoError(t, client.Do(ctx, http.MethodPost, base+"/tags", nil, map[string]any{"tag_name": "v1", "target": "main"}, nil))

	if reviewer != nil {
		reviewerUser, _ := reviewer.CurrentUser(ctx)
		require.NoError(t, reviewer.Do(ctx, http.MethodPost, base+"/forks", nil, map[string]any{}, nil))
		forkBase := "/repos/" + reviewerUser.Login + "/" + repo
		t.Cleanup(func() { _ = reviewer.Do(context.Background(), http.MethodDelete, forkBase, nil, nil, nil) })
		time.Sleep(2 * time.Second)
		require.NoError(t, reviewer.Do(ctx, http.MethodPost, forkBase+"/contents/f.txt", nil, map[string]any{
			"content": base64.StdEncoding.EncodeToString([]byte("fork")), "message": "fork change",
			"new_branch": "fork-feature",
		}, nil))
		require.NoError(t, reviewer.Do(ctx, http.MethodPost, base+"/pulls", nil, map[string]any{
			"head": reviewerUser.Login + ":fork-feature", "base": "main", "title": "From a fork",
		}, nil))
	}

	required := []string{
		"push", "push_new_branch", "push_tag", "pr_opened", "pr_comment", "pr_synchronized",
		"pr_merged", "pr_draft_opened", "pr_closed", "pr_reopened",
	}
	if reviewer != nil {
		required = append(required, "pr_review_requested", "pr_fork_opened")
	}
	if withRuns {
		required = append(required, "run_success_push", "run_failure_push", "run_success_pull_request")
		if reviewer != nil {
			required = append(required, "run_success_pull_request_fork")
		}
	}
	deliveries := receiver.waitFor(t, required, pull.Number, draft.Number, 3*time.Minute)

	for name, delivery := range deliveries {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, VerifyWebhookSignature(fixtureSecret, delivery.body, delivery.headers))
			require.ErrorIs(t, VerifyWebhookSignature("another-secret", delivery.body, delivery.headers), ErrWebhookSignature)
			event, err := DecodeWebhook(delivery.body, delivery.headers)
			require.NoError(t, err)
			requireScenario(t, name, event, owner+"/"+repo, string(flavor))
		})
	}

	if dir := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_CAPTURE_DIR")); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for name, delivery := range deliveries {
			headers, err := json.MarshalIndent(delivery.headers, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, name+".headers.json"), append(headers, '\n'), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(dir, name+".body.json"), delivery.body, 0o644))
		}
		t.Logf("captured %d deliveries from %s %s into %s", len(deliveries), flavor, version, dir)
	}
}

// requireScenario holds a decoded delivery to what the scenario that produced
// it means. The same assertions run against the committed fixtures.
func requireScenario(t *testing.T, name string, event automations.Event, repository, provider string) {
	t.Helper()
	if name == "pr_review_approved" {
		require.Empty(t, event.Kind, "reviews are not a condition event")
		return
	}
	require.True(t, strings.EqualFold(repository, event.Repository), "repository %q", event.Repository)
	require.Equal(t, provider, event.Provider)
	switch {
	case strings.HasPrefix(name, "push"):
		require.Equal(t, automations.ConditionPush, event.Kind)
		require.NotNil(t, event.Push)
		switch name {
		case "push_tag":
			require.True(t, strings.HasPrefix(event.Push.Ref, "refs/tags/"))
		case "push_new_branch":
			require.Equal(t, "refs/heads/feature/x", event.Push.Ref)
			require.Equal(t, strings.Repeat("0", 40), event.Push.Before)
		default:
			require.Equal(t, "refs/heads/main", event.Push.Ref)
			require.NotNil(t, event.Push.HeadCommit)
			require.NotEmpty(t, event.Push.Pusher)
		}
	case strings.HasPrefix(name, "pr_"):
		require.Equal(t, automations.ConditionPullRequest, event.Kind)
		pr := event.PullRequest
		require.NotNil(t, pr)
		require.Positive(t, pr.Number)
		want := map[string]string{
			"pr_opened": automations.ActionOpened, "pr_labeled": "label_updated",
			"pr_review_requested": automations.ActionReviewRequested, "pr_comment": automations.ActionCommented,
			"pr_synchronized": automations.ActionSynchronized, "pr_merged": automations.ActionMerged,
			"pr_draft_opened": automations.ActionOpened, "pr_closed": automations.ActionClosed,
			"pr_reopened": automations.ActionReopened, "pr_fork_opened": automations.ActionOpened,
		}[name]
		require.Equal(t, want, pr.Action)
		if name == "pr_comment" {
			require.NotNil(t, pr.Comment)
			require.Equal(t, "a comment", pr.Comment.Body)
			if !pr.Detailed {
				// Forgejo 7 sends the comment without the pull request.
				return
			}
		}
		require.True(t, pr.Detailed)
		require.Equal(t, "main", pr.BaseBranch)
		require.NotEmpty(t, pr.HeadSHA)
		require.Equal(t, name == "pr_fork_opened", pr.Fork)
		require.Equal(t, name == "pr_draft_opened" || name == "pr_closed" || name == "pr_reopened", pr.Draft)
		if name == "pr_review_requested" {
			require.NotEmpty(t, pr.RequestedReviewer)
		}
		if name == "pr_synchronized" || name == "pr_merged" || name == "pr_comment" {
			require.Equal(t, []string{"bug"}, pr.Labels)
		}
	case strings.HasPrefix(name, "run_"):
		require.Equal(t, automations.ConditionCIResult, event.Kind)
		run := event.Run
		require.NotNil(t, run)
		parts := strings.Split(strings.TrimPrefix(name, "run_"), "_")
		require.Equal(t, parts[0], run.State)
		require.Equal(t, "ci.yml", run.Workflow)
		require.Positive(t, run.RunNumber)
		require.Equal(t, strings.HasSuffix(name, "_fork"), run.Fork)
		switch {
		case strings.Contains(name, "pull_request"):
			require.Equal(t, "pull_request", run.Event)
			require.Positive(t, run.PullRequest)
			require.NotEmpty(t, run.Branch)
		case name == "run_failure_push":
			require.Equal(t, "fail", run.Branch)
		}
	default:
		t.Fatalf("unknown scenario %q", name)
	}
}

func liveOptionalClient(t *testing.T, envVar string) *rest.Client {
	t.Helper()
	token := strings.TrimSpace(os.Getenv(envVar))
	if token == "" {
		return nil
	}
	baseURL, _, _, _ := liveConfig(t)
	client, err := rest.NewClient(baseURL, token, &http.Client{Timeout: 30 * time.Second})
	require.NoError(t, err)
	return client
}

// mergeEventually merges, retrying while the instance is still computing
// mergeability (Gitea answers 405 for a moment after a push).
func mergeEventually(t *testing.T, client *rest.Client, path string) {
	t.Helper()
	var err error
	for range 20 {
		if err = client.Do(context.Background(), http.MethodPost, path, nil, map[string]any{"Do": "merge"}, nil); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
}

type webhookDelivery struct {
	headers map[string]string
	body    []byte
}

type webhookReceiver struct {
	url string

	mu         sync.Mutex
	deliveries []webhookDelivery
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	receiver := &webhookReceiver{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		// Keep what the host would hand the plugin: the declared headers,
		// lower-cased, one value each.
		headers := map[string]string{}
		for _, name := range WebhookHeaders {
			if value := r.Header.Get(name); value != "" {
				headers[name] = value
			}
		}
		receiver.mu.Lock()
		receiver.deliveries = append(receiver.deliveries, webhookDelivery{headers: headers, body: body})
		receiver.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimSpace(os.Getenv("KANDEV_FORGEJO_WEBHOOK_HOST"))
	if host == "" {
		host = "127.0.0.1"
	}
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	receiver.url = "http://" + net.JoinHostPort(host, port) + "/"
	return receiver
}

// waitFor collects deliveries until every required scenario has arrived,
// naming each by what it is. The first delivery of a scenario wins.
func (r *webhookReceiver) waitFor(t *testing.T, required []string, pull, draft int64, timeout time.Duration) map[string]webhookDelivery {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		named := map[string]webhookDelivery{}
		r.mu.Lock()
		for _, delivery := range r.deliveries {
			if name := classifyDelivery(delivery, pull, draft); name != "" {
				if _, seen := named[name]; !seen {
					named[name] = delivery
				}
			}
		}
		r.mu.Unlock()
		var missing []string
		for _, name := range required {
			if _, ok := named[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return named
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries never arrived for %v (got %d deliveries)", missing, len(r.deliveries))
		}
		time.Sleep(time.Second)
	}
}

// classifyDelivery names a delivery from its own content, so arrival order
// does not matter.
func classifyDelivery(delivery webhookDelivery, pull, draft int64) string {
	var payload struct {
		Action      string `json:"action"`
		Ref         string `json:"ref"`
		Before      string `json:"before"`
		Number      int64  `json:"number"`
		PullRequest *struct {
			Merged bool   `json:"merged"`
			Title  string `json:"title"`
		} `json:"pull_request"`
		Run *struct {
			Status       string `json:"status"`
			Event        string `json:"event"`
			Fork         bool   `json:"is_fork_pull_request"`
			EventPayload string `json:"event_payload"`
		} `json:"run"`
	}
	if json.Unmarshal(delivery.body, &payload) != nil {
		return ""
	}
	switch webhookEvent(delivery.headers) {
	case "push":
		switch {
		case strings.HasPrefix(payload.Ref, "refs/tags/"):
			return "push_tag"
		case strings.Trim(payload.Before, "0") == "":
			return "push_new_branch"
		case payload.Ref == "refs/heads/main":
			return "push"
		}
	case "issue_comment":
		return "pr_comment"
	case "pull_request_approved":
		return "pr_review_approved"
	case "pull_request":
		if payload.PullRequest != nil && payload.PullRequest.Title == "From a fork" {
			if payload.Action == "opened" {
				return "pr_fork_opened"
			}
			return ""
		}
		switch {
		case payload.Number == pull && payload.Action == "closed" && payload.PullRequest.Merged:
			return "pr_merged"
		case payload.Number == pull && payload.Action == "label_updated":
			return "pr_labeled"
		case payload.Number == pull:
			return map[string]string{
				"opened": "pr_opened", "review_requested": "pr_review_requested", "synchronized": "pr_synchronized",
			}[payload.Action]
		case payload.Number == draft:
			return map[string]string{
				"opened": "pr_draft_opened", "closed": "pr_closed", "reopened": "pr_reopened",
			}[payload.Action]
		}
	case "action_run_success", "action_run_failure":
		if payload.Run == nil {
			return ""
		}
		var trigger struct {
			Ref string `json:"ref"`
		}
		_ = json.Unmarshal([]byte(payload.Run.EventPayload), &trigger)
		if strings.HasPrefix(trigger.Ref, "refs/tags/") {
			return "run_" + payload.Run.Status + "_tag"
		}
		name := "run_" + payload.Run.Status + "_" + payload.Run.Event
		if payload.Run.Fork {
			name += "_fork"
		}
		return name
	}
	return ""
}
