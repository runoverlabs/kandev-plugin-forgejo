# Review watches

A review watch files a Kanban task for every pull request that asks for your
review, and can retire the task when the pull request is merged or closed. It is
Kandev's GitHub review watch, for a Forgejo or Gitea instance.

Configure them at **Settings > Integrations > Forgejo**, under "Review watches",
next to the [issue watches](issue-watches.md). The two share one poller, one
ledger and the same enable switches.

"Your review" means the account behind the access token. The instance answers
the question itself: a pull request is returned while that account (or, by
default, a team it belongs to) has been asked to review it and has not answered.
Approving or requesting changes drops it out; so does merging or closing it.
Pull requests the account opened never appear.

Each watch holds:

| Setting | Meaning |
| --- | --- |
| Repositories | Optional `owner/name` pairs. Empty follows every repository the token can see. The filter is applied after the search, because the instance has no per-repository filter. |
| Whose requests | "Me and my teams" (default, what the instance returns) or "Only me", which also requires the account to be named directly on the pull request. |
| Include drafts | Off by default. A draft is the `draft` flag, or a `WIP:` or `[WIP]` title prefix: Gitea 1.20 and Forgejo 7 report no `draft` field, so there the prefix is all there is. |
| Labels, search | A pull request must carry every label; the search text is matched by the instance. |
| Workflow + column | Where a created card lands. |
| Column for fork pull requests | See [Fork pull requests](#fork-pull-requests). |
| Agent / executor profile | How the task runs once started. |
| Start an agent immediately | Off by default; never honoured for a fork pull request. |
| Prompt | The task description, and what the agent is asked. Empty uses the default below. |
| Poll interval | 60 s floor, 300 s default. The search covers the whole instance, so it is heavier than an issue watch's per-repository list. |
| Open task limit | How many of this watch's tasks may be open at once. Matches above it wait for a later run. |
| When a pull request is merged or closed | "Leave the task alone" (default) or "Archive the task". See [Cleanup](#cleanup). |

## What a task looks like

The title is `PR #12: <title>`. The description is the prompt with these
placeholders filled in: `{{pr.number}}`, `{{pr.title}}`, `{{pr.link}}`,
`{{pr.author}}`, `{{pr.repo}}`, `{{pr.branch}}` and `{{pr.base_branch}}`. The
placeholders are filled with text and never evaluated; anything else in braces is
left as written. The default prompt names the pull request and the three-dot diff
commands, and tells the agent not to review outside the diff.

For a pull request from the same repository the task attaches the repository
with the head branch checked out. The branch name is checked first: a name that
starts with a dash, contains `..`, whitespace or control characters, or ends in
`.lock` gets a task with no checkout rather than a failed watch.

The task's metadata records the repository, pull request number and link, the
watch, the author, both branches and, for a fork, `forgejo_fork`.

## Fork pull requests

A fork's head is content its author controls. A task that checks it out and
starts an agent runs it with the operator's executor environment, which is what
GitHub's review watches guard against by marking such a task so the host refuses
an automatic start. **A plugin cannot set that marker**, so this plugin guards
differently, and the guard is weaker: Kandev has no way to stop you starting the
task by hand, and the placement below is the only thing between a fork and an
unattended agent.

A pull request counts as a fork when its head repository is not the base
repository. When the head repository is missing (a deleted fork, an AGit-flow
pull request) or unnamed, it counts as a fork too: unknown fails closed.

A fork pull request's task:

- never starts an agent, whatever the watch says;
- never checks the fork's branch out;
- lands in the **fork column** if you chose one, which must not start an agent
  when a task enters it (saving a watch whose fork column does, is refused);
- otherwise lands in the main column if that does not start an agent either;
- and is **left out**, counted in the run summary as "from a fork left out", when
  the only column available would start one. The column is checked again on every
  run, because someone may add an auto-start action to it later.

## Cleanup

With "Archive the task" set, a pull request that is merged or closed retires its
task. Each pass:

1. reads the watch's ledger and the workspace's tasks (archived included) once;
2. skips every task that is already archived or complete, **without asking the
   instance anything**;
3. asks the instance about each remaining one, at most 40 per pass, and stops the
   whole pass at the first rate limit or refused token;
4. archives the task when the pull request is finished, or **completes** it when
   the host will not archive.

Nothing is ever deleted: a plugin cannot, and the card is the record of the
review. The ledger entry stays after archiving, so the pull request is not filed
again. If a task was deleted by hand and its pull request has since finished, the
ledger entry is dropped; while the pull request is open it is kept, because it is
what stops the request being filed a second time.

**Archiving needs a grant.** Archiving a task is a separate operator approval,
`host.v2.write:tasks`, per workspace, on top of the `tasks (write)` capability the
plugin already holds. Without it the plugin falls back to completing the task and
the watch says so ("Tasks are completed, not archived…"), so the difference is
visible rather than silent. "Clean up now" runs a pass on demand.

"Forget history" clears the ledger, so the next run files a card again for every
pull request that is still awaiting review. Use it only to start over.

## Existing watches

Issue watches are untouched: same keys, same stored shape, same behaviour.
Review watches are stored under their own `rwatch.` prefix, so an older version
of the plugin simply does not see them, rather than polling them as issue
watches.

## Capabilities this needs

None beyond what issue watches already declared. Reading and updating tasks, and
reading workflows and their steps, are covered by `api_read` and `api_write:
tasks`; archiving is covered by the separate per-workspace grant above, not by
the manifest.

---

[← Back to the README](../README.md)
