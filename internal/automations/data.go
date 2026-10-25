package automations

import "strings"

// The normalized payloads below are the stable, provider-neutral prompt
// surface: snake_case, the same keys on Forgejo and Gitea. They are as
// untrusted as the raw webhook body they come from.

func baseData(event Event, htmlURL string) map[string]any {
	return map[string]any{
		"provider":   event.Provider,
		"repository": event.Repository,
		"html_url":   htmlURL,
	}
}

func pushData(event Event) map[string]any {
	push := event.Push
	data := baseData(event, push.HTMLURL)
	data["branch"] = strings.TrimPrefix(push.Ref, "refs/heads/")
	data["before"] = push.Before
	data["after"] = push.After
	data["pusher"] = push.Pusher
	data["commit_count"] = push.CommitCount
	data["compare_url"] = push.CompareURL
	if commit := push.HeadCommit; commit != nil {
		data["head_commit"] = map[string]any{
			"id":      commit.ID,
			"message": commit.Message,
			"url":     commit.URL,
			"author":  commit.Author,
		}
	}
	return data
}

func pullRequestData(event Event) map[string]any {
	pr := event.PullRequest
	data := baseData(event, pr.HTMLURL)
	labels := pr.Labels
	if labels == nil {
		labels = []string{}
	}
	data["action"] = pr.Action
	data["number"] = pr.Number
	data["title"] = pr.Title
	data["author"] = pr.Author
	data["sender"] = pr.Sender
	data["base_branch"] = pr.BaseBranch
	data["head_branch"] = pr.HeadBranch
	data["head_sha"] = pr.HeadSHA
	data["draft"] = pr.Draft
	data["fork"] = pr.Fork
	data["labels"] = labels
	if pr.RequestedReviewer != "" {
		data["requested_reviewer"] = pr.RequestedReviewer
	}
	if comment := pr.Comment; comment != nil {
		data["comment"] = map[string]any{
			"author":   comment.Author,
			"body":     comment.Body,
			"html_url": comment.HTMLURL,
		}
	}
	return data
}

func runData(event Event) map[string]any {
	run := event.Run
	data := baseData(event, run.HTMLURL)
	data["state"] = run.State
	data["workflow"] = run.Workflow
	data["run_number"] = run.RunNumber
	data["title"] = run.Title
	data["sha"] = run.SHA
	data["branch"] = run.Branch
	data["ref"] = run.Ref
	data["event"] = run.Event
	data["trigger_user"] = run.TriggerUser
	data["fork"] = run.Fork
	if run.PullRequest > 0 {
		data["pull_request"] = run.PullRequest
	}
	return data
}
