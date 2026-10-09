# Issue watches

A watch turns Forgejo issues into Kanban tasks. You name one or more
repositories and a filter; the plugin polls them on an interval and files a
card for each new issue that matches.

Configure them at **Settings > Integrations > Forgejo**, under "Issue watches".
Kandev's own issue-watch screens are compiled in per provider and have no
extension point a plugin can register against, so this panel is the entire UI
for the feature — unlike repositories and pull requests, which render in
Kandev's native surfaces.

Each watch holds:

| Setting | Meaning |
| --- | --- |
| Repositories | One or more `owner/name` pairs on the connected instance. |
| Labels | An issue must carry **every** label listed. Empty matches any. |
| Issue state | `open` (default), `closed`, or `all`. |
| Search | Optional free text over title and body. Served by the instance's issue indexer, so a brand-new issue may not match immediately, and nothing matches if indexing is off. Labels and state have neither caveat. |
| Workflow + column | Where a created card lands. |
| Agent / executor profile, prompt | How the task runs once started. |
| Start an agent immediately | Off by default. Leave it off to let the column's own `on_enter` auto-start decide. |
| Poll interval | 30 s floor, 300 s default. |
| Open task limit | How many of this watch's tasks may be open at once. Matched issues above the limit wait for a later run. |
| Duplicate handling | "One task per watch" (default, matching Kandev's native providers) or "One task per issue" across every watch in the workspace. |

## What it guarantees

- **One issue, one task.** Every created task is recorded in plugin state
  keyed by repository and issue number, so re-polling never files a second
  card. "Forget history" clears that ledger deliberately; deleting a watch
  clears its own entries too.
- **A bounded first run.** A watch polling a repository with a long backlog is
  metered by the open task limit, not by luck. Raise it once you have seen what
  a watch actually matches.
- **Both switches bite.** Turning the integration off for a workspace stops its
  watches polling, and a paused watch stops on its own. Neither merely hides a
  badge.
- **Attribution.** Kandev stamps `source = "plugin:kandev-plugin-forgejo"` on
  every task a watch creates and the plugin cannot set that itself, so a card
  from a watch is permanently distinguishable from one a person filed.

Pull requests are never turned into tasks by an issue watch; to file a task for
a pull request awaiting your review, use a [review watch](review-watches.md),
under the same settings screen.

Pull requests are never turned into tasks, even though Forgejo returns them
from the same endpoint — this plugin already has a first-class pull request
surface.

## Capabilities this needs

Issue watches are why the manifest declares `api_write: ["tasks"]`, plus
`api_read` on `workspaces`, `workflows`, `agent_profiles` and
`executor_profiles`. Task creation is the plugin's only entity mutation; the
reads exist so the configuration form can offer real columns and profiles by
name. Capability grants are permission gates rather than a sandbox, so this is
a genuine trust step — weigh it before installing on a shared instance.

---

[← Back to the README](../README.md)
