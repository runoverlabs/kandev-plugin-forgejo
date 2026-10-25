# Pull request automation

For a pull request linked to a task, the plugin can:

- **Auto-fix**: prompt the task's agent when CI fails, the branch stops merging
  cleanly, or reviewers ask for changes;
- **Auto-merge**: merge the pull request once CI passes and reviews allow it;
- **Lifecycle prompts**: tell the agent when its review is requested, or when
  the pull request is merged or closed.

These are the plugin's counterparts of Kandev's built-in GitHub CI auto-fix,
auto-merge and pull request prompts. Kandev compiles those into its GitHub
integration with no extension point for another code host, so the plugin runs
its own loop and reaches the agent through Kandev's message API.

## Turning it on

Each linked pull request has five switches, in the **Automation** section of
its review panel:

| Switch | What it does |
| --- | --- |
| Auto-fix | Sends the agent new CI failures, conflicts and review feedback, one round at a time |
| Auto-merge | Merges when every gate below passes |
| Review requested | Tells the agent when the plugin's account is asked to review |
| Merged | Tells the agent when the pull request is merged |
| Closed | Tells the agent when the pull request is closed without merging |

Workspace defaults live in **Settings → Integrations → Forgejo → Pull request
automation**. They are copied onto a pull request the first time the plugin sees
it linked; changing a default later does not touch pull requests already seen.
**A default never turns on auto-fix or auto-merge for a pull request from a
fork** (see [Forks](#forks)).

Nothing happens while the workspace's Forgejo integration is switched off. The
switches stay as they are and take effect again when it is back on.

The plugin looks at each pull request with a switch on about once a minute, and
every 20 seconds while a fix round is with the agent or auto-merge is armed. A
merged pull request is no longer polled.

## Auto-fix

When CI on the head commit has finished, the plugin collects the feedback on the
pull request:

- **failing checks**: commit statuses in `failure` or `error`. A cancelled or
  skipped check asks for no change, so it is not feedback;
- **a conflict**: the instance reports the branch no longer merges cleanly;
- **review feedback**: reviews requesting changes, reviews with a comment,
  their unresolved inline comments, and conversation comments, filtered by the
  workspace's [feedback setting](#review-feedback-and-trust).

Feedback the plugin's own account wrote is never included: the agent comments
and reviews as that account, and would otherwise be prompted by its own replies.
Comments from the instance's bots (the Actions user, a deleted account) only
come along with something else.

Only feedback that is **new since the last round** is sent. A failing check on
a new head commit counts as new, as on GitHub, so a fix that did not work is
reported again.

The prompt goes to the task's agent with the [auto-fix prompt](#the-prompt).
Then:

- **One round at a time.** Kandev's message API cannot replace a prompt that is
  still queued, so nothing else is sent until the agent reports an outcome or
  its turn ends.
- **The agent reports an outcome** through the `pr` tool (`op: "outcome"`):
  - `action_taken`: it changed something. The plugin waits for a new head
    commit, or two minutes, before sending new feedback.
  - `non_actionable`: the feedback asks for nothing it can do.
  - `blocked`: a change is needed, but something outside the task prevents it.
    Auto-merge holds off for an hour on that head.

  Either way, the feedback in that round counts as handled.
- **The turn ends without a report.** The round is retried, no sooner than two
  minutes after it was sent, and the retry carries the whole current feedback.
  The plugin hears about the turn ending from Kandev at once, or notices within
  about two minutes of the session going idle.
- **At most 10 rounds per pull request.** Then auto-fix pauses. **Reset rounds**
  in the panel, or turning auto-fix off and on, starts again.

**Which session.** The round goes to the session the last round went to if it
can still take a prompt, otherwise to the task's most recently started session.
A busy session queues it behind the current turn. If the task has no session,
the plugin waits for one.

### The prompt

The default prompt is modelled on Kandev's own auto-fix prompt. You can replace
it per workspace in the settings, with these placeholders:

- `{{pr.feedback}}` (appended if your prompt leaves it out);
- `{{pr.url}}`, `{{pr.number}}`, `{{pr.title}}`;
- `{{pr.repository}}`, `{{pr.head_branch}}`, `{{pr.base_branch}}`;
- `{{attempt.round}}`.

The outcome instructions are always appended and cannot be edited: the round
bookkeeping depends on them. Each prompt starts with a line naming the round and
an attempt id. Kandev treats two identical messages as one, and the id keeps
two rounds with the same feedback apart.

## Review feedback and trust

Review text goes into the agent's prompt, so whoever writes it is talking to
the agent. The workspace setting **Review feedback the agent sees** decides
whose words count:

| Setting | Review text that reaches the agent |
| --- | --- |
| **Everyone** (default) | Every review and comment, as with Kandev's GitHub integration |
| **Official reviewers** | Reviews that count towards the branch's required approvals, with their inline comments, and conversation comments by someone who left such a review |
| **No review text** | None: auto-fix acts on CI failures and conflicts only |

"Official" is the instance's own flag on a review. On every supported host it
means the reviewer has write access. An operator token that is not a repository
admin cannot read other users' permission levels, so this is the only trust
signal the plugin can rely on.

**At the default, anyone who can comment on the pull request can put text in
front of the agent.** On a repository open to outside contributors, choose
Official reviewers. The prompt marks review text as data rather than
instructions, but that is a mitigation, not a boundary.

Whatever a narrower setting leaves out is counted, and the panel says how many
comments were left out.

## Auto-merge

The plugin merges the pull request itself once all of these hold:

- the pull request is open and not a draft (`WIP:` and `[WIP]` titles count as
  drafts);
- CI on the head has finished and nothing failed. A head with no checks at
  all passes once it has stayed that way for two minutes, so CI that has not
  reported yet after a push is not mistaken for a repository without CI;
- the instance reports the branch merges cleanly;
- nobody's latest review requests changes;
- official approvals reach the branch's required approvals;
- no inline review comment is unresolved;
- no auto-fix round is in flight, and the agent has not reported `blocked` on
  this head within the hour.

**The merge.**

- It uses the repository's default merge style and its "delete branch after
  merge" default.
- It is pinned to the head commit the gates were checked on. If someone pushed
  in between, the instance refuses it and the plugin checks the new head
  instead, so commits nobody evaluated are never merged.
- The audit trail (`connection.audit`) records it as `automation.merge` by
  `auto-merge`.

**If the instance refuses.** The plugin shows why (branch protection, missing
permission) and does not try again until the pull request changes. **Retry
merge** in the panel makes one more attempt.

**Not Forgejo's "merge when checks succeed".** The plugin does not use the
instance's scheduled merge, which was probed on Forgejo 7 to 17 and Gitea 1.27:

- on Forgejo it never fires when the checks were already green, or on a
  repository without CI, because it waits for a new status event;
- on every host it merges a commit pushed after it was scheduled.

**Who decides.** Only a person turns auto-merge on, in the review panel. It is
separate from the workspace's "agents may merge" setting, which governs the
`pr` tool's `merge`; agents cannot switch automation on.

## Lifecycle prompts

Each fires once per event, as a one-line message to the task's agent:

- **Review requested**: the plugin's account was asked to review: directly, or
  (Forgejo 11+, Gitea) through a team it belongs to. A request that was already
  open when the switch was turned on does not fire.
- **Merged**: once. A merged pull request never reverts.
- **Closed**: once, without a merge. If the pull request is reopened and closed
  again, it fires again.

If the task has no session to take the prompt, it waits and is delivered when
one appears.

## Forks

A fork's code and comments are controlled by its author. An agent acting on
them runs with your executor's environment. So:

- Workspace defaults never turn on auto-fix or auto-merge for a fork's pull
  request. Turning either on is a person's choice in the panel, which asks for
  confirmation first.
- **The plugin never starts an agent for a fork's pull request.** Kandev starts
  a session that has never run when it receives a message, so for a fork the
  plugin only prompts a session that is already running or idle. Until someone
  starts the task, the panel says so and the prompt waits.

## After upgrading: re-approve the capabilities

This feature adds three capabilities to the plugin's manifest:

- `api_write: messages`, to prompt agents;
- `api_read: sessions`, to see which sessions can take a prompt;
- the `turn.completed` and `task_session.state_changed` events, to notice a
  turn ending without a report.

**Kandev re-checks the whole capability set, so after the upgrade every gated
feature of the plugin is refused until an administrator re-approves it** in
**Settings → Plugins → Forgejo**. Until then the automation section shows that
approval is needed.

## What the panel tells you

| Line | Meaning |
| --- | --- |
| Watching. | A switch is on and nothing needs doing |
| Auto-fix is waiting for CI to finish. | Checks on the head are still running |
| Auto-fix round N is with the agent. | A round was delivered and no outcome yet |
| Auto-fix: nothing new to fix. | All feedback present was already sent |
| Auto-fix paused after 10 rounds. | Reset rounds to continue |
| Auto-merge: … | The first gate that is not met |
| Auto-merge failed: … | The instance refused; Retry merge or wait for a change |
| Waiting for a session on the task. | The task has no session that can take a prompt |
| The task's agent has not been started… | A fork's task: start it to let automation prompt it |
| Paused: the Forgejo integration is off | The workspace switch is off |
