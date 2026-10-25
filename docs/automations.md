# Automation conditions

Forgejo and Gitea webhooks can start a Kandev automation. The plugin adds three
conditions to Kandev's automation editor, the counterparts of its built-in
GitHub triggers:

| Condition | Fires on | Hosts |
| --- | --- | --- |
| **Forgejo push** | A push to a branch | Forgejo 7+, Gitea 1.27 |
| **Forgejo pull request** | A pull request opened, reopened, given new commits, closed, merged, given a review request, or commented on | Forgejo 7+, Gitea 1.27 ("commented on": Forgejo 11+) |
| **Forgejo CI result** | A Forgejo Actions run finished: succeeded, failed or was cancelled | Forgejo 12+ only |

Kandev owns the rest: the editor, the webhook URL and signing secret of each
condition, the list of received deliveries, and what the automation does. The
plugin checks each delivery's signature, decides whether it matches the
condition, and hands Kandev a normalized summary of the event.

## Setting one up

1. In Kandev, create an automation and add one of the Forgejo conditions as its
   trigger. Fill in the **repository** as `owner/name`; the field suggests the
   repositories the plugin's token can see. Set the filters (below) and save.
   Saving checks the repository with the instance, so a typo or a repository
   the token cannot read is refused here.
2. Click **Configure webhook**. Kandev shows the **Webhook URL** and, behind
   **Reveal secret**, the **Signing secret**. The URL is
   `<your Kandev>/api/v1/automations/webhook-bindings/<id>`.
3. In Forgejo, open the repository's **Settings > Webhooks > Add webhook** and
   choose **Forgejo** (or **Gitea** on Gitea). Set:
   - **Target URL**: the webhook URL, with your Kandev's public address.
   - **HTTP method** POST, **POST content type** `application/json`.
   - **Secret**: the signing secret.
   - **Trigger on**: custom events, ticking only the ones this condition uses:

     | Condition | Events to tick |
     | --- | --- |
     | Push | Push |
     | Pull request | Pull request events: at least "Pull request", "Pull request synchronized", "Pull request review requested" and "Pull request comments" for the switches you turned on |
     | CI result | Action run success and Action run failure |

   Ticking more is harmless; each extra delivery shows up in Kandev as ignored.
   There is no "commit status" event on Forgejo, so CI that reports through
   commit statuses (Woodpecker, for instance) cannot feed the CI result
   condition.
4. Use **Test delivery** in Forgejo, or cause the event, and check **Refresh
   deliveries** in Kandev. The test delivery is a push to the default branch: a
   push condition whose branch filter matches accepts it, and any other
   condition ignores it, which still proves the URL, the network path and the
   secret. It sends the same body every time, so Kandev reports a second test
   as a duplicate.

Each condition has its own URL and secret, so it needs its own Forgejo webhook.
Two automations on the same repository mean two webhooks.

### After an upgrade, and after editing a condition

**Kandev ties each webhook binding to the installed plugin version and to the
condition's saved settings.** After any plugin upgrade or reinstall, and after
you save a change to a condition's filters, its webhook answers 401 until you
click **Configure webhook** again. That issues a **new secret**: reveal it and
paste it into the Forgejo webhook. The URL stays the same.

Rotating the plugin's access token does not break bindings, as long as it
belongs to the same account. Pointing the plugin at another account does: Kandev
still accepts the deliveries, then cancels each one before it runs ("connection
changed" in the deliveries list) until the binding is configured again.
Pointing it at another instance refuses them outright (401).

### Network

The instance has to reach Kandev. A Forgejo on the public internet cannot reach
a Kandev on a laptop or a private network without a tunnel or a reverse proxy.

Forgejo also filters webhook targets itself: `[webhook] ALLOWED_HOST_LIST`
defaults to `external`, which **refuses private and loopback addresses**. If
Kandev is on your LAN, add its address or network to that list on the instance
(for example `ALLOWED_HOST_LIST = external,192.168.1.0/24`), or the deliveries
fail on the Forgejo side and never reach Kandev.

## Filters

Every condition needs the repository. A delivery from any other repository is
ignored, even when correctly signed: the webhook was put on the wrong
repository. Empty lists mean "all". A switch you have never touched is off;
new conditions start with the defaults marked below.

### Push

| Setting | Meaning |
| --- | --- |
| Branches | Glob patterns, one per line, in Go's `path.Match` syntax: `main`, `release/*`. `*` does not cross a `/`. Empty: every branch. |

Tag pushes and branch deletions never fire.

### Pull request

| Setting | Meaning |
| --- | --- |
| When opened (default on), reopened, new commits are pushed, closed without merging, merged, a review is requested, commented on | Which actions fire. Merged and closed are separate, so one can be had without the other. |
| Base branches | Globs for the branch the pull request targets. |
| Authors | Logins of the pull request's author (not of whoever caused the event). |
| Labels | The pull request carries any of them. |
| Skip drafts (default on) | A draft is the `draft` flag, or on Forgejo 7, which sends none, a `WIP:` or `[WIP]` title prefix. |
| Include pull requests from forks | Off by default. See [Forks](#forks). |

"Commented on" is off by default. It fires for a new comment on the pull
request's conversation, **whoever wrote it**: the Authors filter checks the pull
request's author, not the commenter. On a repository where anyone can comment,
turning it on lets anyone start a run and put text in front of the agent
(`{{data.comment.body}}`), which is a prompt-injection path the fork default
does not close. Turn it on only when you trust everyone who can comment, and
write the prompt to treat the comment as data.

On Forgejo 7 the comment delivery does not include the pull request, so the
branch, draft and fork filters cannot be checked and the comment is ignored;
Forgejo 11 and later, and Gitea, include it.

### CI result

| Setting | Meaning |
| --- | --- |
| When the run succeeds, fails (default on), is cancelled | Which results fire. |
| Branches | Globs. For a pull request run, the pull request's head branch. A run on a tag, a schedule or a manual dispatch has no branch, and fires only while this list is empty. |
| Workflow files | The workflow's file name as Forgejo reports it, e.g. `ci.yml`. |
| Include runs for pull requests from forks | Off by default. See [Forks](#forks). |

Every finished run is delivered, not only a change from failing to passing.
Forgejo 17 delivers a cancelled run as a failure; the plugin reads the run's
own status, so "fails" does not fire for it.

The condition is unavailable on Gitea, whose run events have other names and
shapes, and on Forgejo before 12, which has no run events. The editor shows it
disabled there, with the reason.

### Forks

A pull request from a fork, and a CI run for one, carry content its author
controls. An automation that starts an agent on it runs that content with your
executor's environment. Both conditions therefore ignore forks unless you turn
on the switch, which is the right choice only when you trust everyone who can
open a pull request on the repository.

## In the prompt

Kandev exposes the original delivery as `{{webhook.<path>}}` and the plugin's
summary as `{{data.<path>}}`. The summary has the same keys on Forgejo and
Gitea, so a prompt written for one works on the other. **Both are untrusted
input**: a commit message or pull request title is text its author chose, and
a prompt that pastes it in should treat it as data.

Every summary has `provider` (`forgejo` or `gitea`), `repository`
(`owner/name`) and `html_url`.

| Condition | Fields |
| --- | --- |
| Push | `branch`, `before`, `after`, `pusher`, `commit_count`, `compare_url`, `head_commit.id`, `head_commit.message`, `head_commit.url`, `head_commit.author` |
| Pull request | `action` (`opened`, `reopened`, `synchronized`, `closed`, `merged`, `review_requested`, `commented`), `number`, `title`, `author`, `sender`, `base_branch`, `head_branch`, `head_sha`, `draft`, `fork`, `labels`, `requested_reviewer` (review requests), `comment.author`, `comment.body`, `comment.html_url` (comments) |
| CI result | `state` (`success`, `failure`, `cancelled`), `workflow`, `run_number`, `title`, `sha`, `branch`, `ref`, `event` (what started the run), `pull_request` (its number, for a pull request run), `trigger_user`, `fork` |

## How a delivery is checked

In this order, and nothing is parsed before the signature passes:

1. **Signature.** Forgejo signs the raw body with HMAC-SHA256 and sends it in
   `X-Forgejo-Signature`, `X-Gitea-Signature` and `X-Hub-Signature-256`. At
   least one must be present and every one present must match. Otherwise 401.
2. **Instance.** The binding must be for the instance the plugin is configured
   with. Otherwise 401.
3. **Event.** An event this condition does not use is ignored (200).
4. **Payload.** A signed body that is not the event it claims is a 400.
5. **Repository, enable switch, filters.** A miss is ignored (200); a match is
   accepted (202) and the automation runs.

A delivery Kandev cannot check (the plugin is stopped, or a host read fails)
gets a 503. Neither Kandev nor Forgejo retries it: the failed delivery stays
under the webhook's recent deliveries in Forgejo, which can send it again.

Kandev answers a delivery it has already seen (the same bytes, within seven
days) as a duplicate, so a redelivery from Forgejo's webhook page does not run
the automation twice. The plugin makes no request to the instance while
checking a delivery.

The plugin switched off for the workspace (Settings > Integrations > Forgejo)
means every delivery is ignored; if the switch cannot be read, the delivery
fails rather than run. The secret reaches the plugin only while a
delivery is checked; it is never stored or logged.
