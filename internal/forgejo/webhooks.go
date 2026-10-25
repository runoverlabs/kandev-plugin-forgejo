package forgejo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"kandev-plugin-forgejo/internal/automations"
)

// Webhook headers, lower-case as the host hands them over. Every host sends the
// x-gitea-* pair and x-hub-signature-256; Forgejo adds the x-forgejo-* pair
// (captured from Forgejo 7, 11, 15, 17 and Gitea 1.27). The sub-event headers
// (x-*-event-type) are not needed: the payload's `action` tells sub-events
// apart on every host.
const (
	headerForgejoSignature = "x-forgejo-signature"
	headerGiteaSignature   = "x-gitea-signature"
	headerHubSignature     = "x-hub-signature-256"
	headerForgejoEvent     = "x-forgejo-event"
	headerGiteaEvent       = "x-gitea-event"
)

// WebhookHeaders is the manifest's verification_headers: only these reach the
// plugin.
var WebhookHeaders = []string{
	headerForgejoSignature, headerGiteaSignature, headerHubSignature,
	headerForgejoEvent, headerGiteaEvent,
}

var (
	// ErrWebhookSignature means the delivery is not authenticated.
	ErrWebhookSignature = errors.New("forgejo: webhook signature missing or invalid")
	// ErrMalformedWebhook means an authenticated delivery is not a payload of
	// the type its headers announce.
	ErrMalformedWebhook = errors.New("forgejo: malformed webhook payload")
)

// VerifyWebhookSignature checks the HMAC-SHA256 of the raw body. At least one
// signature header must be present and every one present must verify: a
// delivery with one good and one bad signature was not produced by an
// instance holding the secret alone.
func VerifyWebhookSignature(secret string, body []byte, headers map[string]string) error {
	if secret == "" {
		return ErrWebhookSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	present := 0
	for _, header := range []string{headerForgejoSignature, headerGiteaSignature, headerHubSignature} {
		value, ok := headers[header]
		if !ok {
			continue
		}
		present++
		hexValue := value
		if header == headerHubSignature {
			var prefixed bool
			if hexValue, prefixed = strings.CutPrefix(value, "sha256="); !prefixed {
				return ErrWebhookSignature
			}
		}
		// Bare hex only: a "sha256=" prefix on the Forgejo or Gitea header is
		// not hex and fails here.
		got, err := hex.DecodeString(hexValue)
		if err != nil || !hmac.Equal(got, want) {
			return ErrWebhookSignature
		}
	}
	if present == 0 {
		return ErrWebhookSignature
	}
	return nil
}

// webhookEvent is the event name, Forgejo's header first. Gitea sends only its
// own.
func webhookEvent(headers map[string]string) string {
	if event := strings.TrimSpace(headers[headerForgejoEvent]); event != "" {
		return event
	}
	return strings.TrimSpace(headers[headerGiteaEvent])
}

// WebhookKind maps a delivery's event header onto the condition it can
// satisfy, or "" when no condition handles that event. A comment can be on an
// issue or a pull request; DecodeWebhook tells which.
func WebhookKind(headers map[string]string) string {
	switch webhookEvent(headers) {
	case "push":
		return automations.ConditionPush
	case "pull_request", "issue_comment":
		return automations.ConditionPullRequest
	case "action_run_success", "action_run_failure":
		return automations.ConditionCIResult
	}
	return ""
}

// DecodeWebhook turns an authenticated delivery into a neutral Event. An event
// no condition handles, or a comment on an issue, decodes to an Event with an
// empty Kind.
func DecodeWebhook(body []byte, headers map[string]string) (automations.Event, error) {
	provider := string(FlavorGitea)
	if strings.TrimSpace(headers[headerForgejoEvent]) != "" {
		provider = string(FlavorForgejo)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return automations.Event{}, ErrMalformedWebhook
	}
	var decode func([]byte) (automations.Event, error)
	switch webhookEvent(headers) {
	case "push":
		decode = decodePush
	case "pull_request":
		decode = decodePullRequest
	case "issue_comment":
		decode = decodeComment
	case "action_run_success", "action_run_failure":
		decode = decodeRun
	default:
		return automations.Event{}, nil
	}
	event, err := decode(body)
	if err != nil {
		return automations.Event{}, err
	}
	if event.Kind != "" {
		event.Provider = provider
	}
	return event, nil
}

type hookUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

func (u *hookUser) name() string {
	if u == nil {
		return ""
	}
	if u.Login != "" {
		return u.Login
	}
	return u.Username
}

type hookRepository struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

type hookPush struct {
	Ref          string          `json:"ref"`
	Before       string          `json:"before"`
	After        string          `json:"after"`
	CompareURL   string          `json:"compare_url"`
	TotalCommits int             `json:"total_commits"`
	Repository   *hookRepository `json:"repository"`
	Pusher       *hookUser       `json:"pusher"`
	HeadCommit   *struct {
		ID      string `json:"id"`
		Message string `json:"message"`
		URL     string `json:"url"`
		Author  *struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		} `json:"author"`
	} `json:"head_commit"`
}

func decodePush(body []byte) (automations.Event, error) {
	var payload hookPush
	if err := json.Unmarshal(body, &payload); err != nil || payload.Repository == nil ||
		payload.Repository.FullName == "" || payload.Ref == "" {
		return automations.Event{}, ErrMalformedWebhook
	}
	push := &automations.Push{
		Ref:         payload.Ref,
		Before:      payload.Before,
		After:       payload.After,
		Pusher:      payload.Pusher.name(),
		CommitCount: payload.TotalCommits,
		CompareURL:  payload.CompareURL,
		HTMLURL:     payload.Repository.HTMLURL,
	}
	if commit := payload.HeadCommit; commit != nil {
		push.HeadCommit = &automations.Commit{ID: commit.ID, Message: commit.Message, URL: commit.URL}
		if commit.Author != nil {
			push.HeadCommit.Author = commit.Author.Username
			if push.HeadCommit.Author == "" {
				push.HeadCommit.Author = commit.Author.Name
			}
		}
	}
	return automations.Event{
		Kind:       automations.ConditionPush,
		Repository: payload.Repository.FullName,
		Push:       push,
	}, nil
}

type hookBranch struct {
	Ref    string          `json:"ref"`
	SHA    string          `json:"sha"`
	RepoID int64           `json:"repo_id"`
	Repo   *hookRepository `json:"repo"`
}

type hookPullRequest struct {
	Number  int64     `json:"number"`
	Title   string    `json:"title"`
	HTMLURL string    `json:"html_url"`
	Draft   *bool     `json:"draft"`
	Merged  bool      `json:"merged"`
	User    *hookUser `json:"user"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Base *hookBranch `json:"base"`
	Head *hookBranch `json:"head"`
}

type hookPullRequestEvent struct {
	Action            string           `json:"action"`
	Number            int64            `json:"number"`
	PullRequest       *hookPullRequest `json:"pull_request"`
	RequestedReviewer *hookUser        `json:"requested_reviewer"`
	Repository        *hookRepository  `json:"repository"`
	Sender            *hookUser        `json:"sender"`
}

func decodePullRequest(body []byte) (automations.Event, error) {
	var payload hookPullRequestEvent
	if err := json.Unmarshal(body, &payload); err != nil || payload.Repository == nil ||
		payload.Repository.FullName == "" || payload.PullRequest == nil || payload.Action == "" {
		return automations.Event{}, ErrMalformedWebhook
	}
	pr := pullRequestOf(payload.PullRequest, payload.Repository)
	pr.Action = payload.Action
	if payload.Action == "closed" && payload.PullRequest.Merged {
		pr.Action = automations.ActionMerged
	}
	if pr.Number == 0 {
		pr.Number = payload.Number
	}
	pr.Sender = payload.Sender.name()
	if payload.Action == automations.ActionReviewRequested {
		pr.RequestedReviewer = payload.RequestedReviewer.name()
	}
	return automations.Event{
		Kind:        automations.ConditionPullRequest,
		Repository:  payload.Repository.FullName,
		PullRequest: pr,
	}, nil
}

func pullRequestOf(payload *hookPullRequest, repository *hookRepository) *automations.PullRequest {
	pr := &automations.PullRequest{
		Number:   payload.Number,
		Detailed: payload.Base != nil && payload.Head != nil,
		Title:    payload.Title,
		Author:   payload.User.name(),
		HTMLURL:  payload.HTMLURL,
		Draft:    draftOf(payload),
		Labels:   []string{},
	}
	for _, label := range payload.Labels {
		pr.Labels = append(pr.Labels, label.Name)
	}
	if pr.Detailed {
		pr.BaseBranch = payload.Base.Ref
		pr.HeadBranch = payload.Head.Ref
		pr.HeadSHA = payload.Head.SHA
		pr.Fork = hookIsFork(payload.Base, payload.Head, repository)
	}
	return pr
}

// draftOf reads the draft flag. Forgejo 7 does not send one; the title prefix
// is the signal there, as in review watches.
func draftOf(payload *hookPullRequest) bool {
	if payload.Draft != nil {
		return *payload.Draft
	}
	return hasWIPPrefix(payload.Title)
}

// hookIsFork compares repository ids, which every captured host sends. A head
// with no repository is a fork that has since been deleted, and counts as one.
func hookIsFork(base, head *hookBranch, repository *hookRepository) bool {
	if head.RepoID != 0 && base.RepoID != 0 {
		return head.RepoID != base.RepoID
	}
	if head.Repo == nil || head.Repo.FullName == "" {
		return true
	}
	return !strings.EqualFold(head.Repo.FullName, repository.FullName)
}

type hookComment struct {
	Action      string           `json:"action"`
	IsPull      bool             `json:"is_pull"`
	PullRequest *hookPullRequest `json:"pull_request"`
	Issue       *struct {
		Number  int64     `json:"number"`
		Title   string    `json:"title"`
		HTMLURL string    `json:"html_url"`
		User    *hookUser `json:"user"`
	} `json:"issue"`
	Comment *struct {
		Body    string    `json:"body"`
		HTMLURL string    `json:"html_url"`
		User    *hookUser `json:"user"`
	} `json:"comment"`
	Repository *hookRepository `json:"repository"`
	Sender     *hookUser       `json:"sender"`
}

// decodeComment handles a comment on a pull request. Forgejo 7 sends no
// pull_request object with it, only the issue; the event then names the pull
// request but is not Detailed, and no condition can match it.
func decodeComment(body []byte) (automations.Event, error) {
	var payload hookComment
	if err := json.Unmarshal(body, &payload); err != nil || payload.Repository == nil ||
		payload.Repository.FullName == "" || payload.Comment == nil || payload.Action == "" {
		return automations.Event{}, ErrMalformedWebhook
	}
	if !payload.IsPull {
		return automations.Event{}, nil
	}
	var pr *automations.PullRequest
	switch {
	case payload.PullRequest != nil:
		pr = pullRequestOf(payload.PullRequest, payload.Repository)
	case payload.Issue != nil:
		pr = &automations.PullRequest{
			Number:  payload.Issue.Number,
			Title:   payload.Issue.Title,
			Author:  payload.Issue.User.name(),
			HTMLURL: payload.Issue.HTMLURL,
			Labels:  []string{},
		}
	default:
		return automations.Event{}, ErrMalformedWebhook
	}
	pr.Action = "comment_" + payload.Action
	if payload.Action == "created" {
		pr.Action = automations.ActionCommented
	}
	pr.Sender = payload.Sender.name()
	pr.Comment = &automations.Comment{
		Author:  payload.Comment.User.name(),
		Body:    payload.Comment.Body,
		HTMLURL: payload.Comment.HTMLURL,
	}
	return automations.Event{
		Kind:        automations.ConditionPullRequest,
		Repository:  payload.Repository.FullName,
		PullRequest: pr,
	}, nil
}

type hookRun struct {
	Run *struct {
		Status            string          `json:"status"`
		WorkflowID        string          `json:"workflow_id"`
		IndexInRepo       int64           `json:"index_in_repo"`
		Title             string          `json:"title"`
		CommitSHA         string          `json:"commit_sha"`
		PrettyRef         string          `json:"prettyref"`
		Event             string          `json:"event"`
		TriggerUser       *hookUser       `json:"trigger_user"`
		IsForkPullRequest bool            `json:"is_fork_pull_request"`
		HTMLURL           string          `json:"html_url"`
		Repository        *hookRepository `json:"repository"`
		// The whole triggering event, as a JSON string. Read only for the
		// branch, which the run itself does not carry for a pull request.
		EventPayload string `json:"event_payload"`
	} `json:"run"`
}

// decodeRun handles Forgejo's action_run_success and action_run_failure. The
// state comes from the run, not the event name: Forgejo 17 sends a cancelled
// run as action_run_failure.
func decodeRun(body []byte) (automations.Event, error) {
	var payload hookRun
	if err := json.Unmarshal(body, &payload); err != nil || payload.Run == nil ||
		payload.Run.Repository == nil || payload.Run.Repository.FullName == "" || payload.Run.Status == "" {
		return automations.Event{}, ErrMalformedWebhook
	}
	run := payload.Run
	branch, number := runBranch(run.Event, run.EventPayload)
	return automations.Event{
		Kind:       automations.ConditionCIResult,
		Repository: run.Repository.FullName,
		Run: &automations.Run{
			State:       run.Status,
			Workflow:    run.WorkflowID,
			RunNumber:   run.IndexInRepo,
			Title:       run.Title,
			SHA:         run.CommitSHA,
			Branch:      branch,
			Ref:         run.PrettyRef,
			Event:       run.Event,
			PullRequest: number,
			TriggerUser: run.TriggerUser.name(),
			Fork:        run.IsForkPullRequest,
			HTMLURL:     run.HTMLURL,
		},
	}, nil
}

// runBranch reads the branch a run tested from its triggering event: the
// pushed branch, or a pull request's head branch. The run's prettyref cannot
// say: it is "#N" for a pull request and a bare name for a branch and a tag
// alike. Anything unreadable is no branch, which a branch filter never
// matches.
func runBranch(event, eventPayload string) (string, int64) {
	var trigger struct {
		Ref         string `json:"ref"`
		PullRequest *struct {
			Number int64 `json:"number"`
			Head   *struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if eventPayload == "" || json.Unmarshal([]byte(eventPayload), &trigger) != nil {
		return "", 0
	}
	switch event {
	case "push":
		branch, _ := strings.CutPrefix(trigger.Ref, "refs/heads/")
		if branch == trigger.Ref {
			return "", 0
		}
		return branch, 0
	case "pull_request", "pull_request_target":
		if trigger.PullRequest == nil || trigger.PullRequest.Head == nil {
			return "", 0
		}
		return trigger.PullRequest.Head.Ref, trigger.PullRequest.Number
	}
	return "", 0
}

// minActionRunWebhookMajor is the first Forgejo release with action_run_*
// webhook events (modules/webhook/type.go at v12.0).
const minActionRunWebhookMajor = 12

// SupportsActionRunWebhooks reports whether an instance sends the Actions run
// events ci_result is built on. Gitea's run events have other names and
// payloads, so it never does. An unreadable version is unsupported: the caller
// reports the condition unavailable rather than binding a hook that may never
// fire.
func SupportsActionRunWebhooks(flavor Flavor, version string) bool {
	if flavor != FlavorForgejo {
		return false
	}
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= minActionRunWebhookMajor
}
