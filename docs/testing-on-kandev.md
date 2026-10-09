# Testing on a real Kandev

The unit suite and the live contract suite between them cover both halves of
this plugin — but neither covers the seam where they meet. This is the stage
that does, and it is worth doing deliberately before any release that changes
the manifest.

## What the existing suites do not reach

Every Host RPC is faked in the unit tests and absent from the live contract
tests, which talk only to Forgejo:

| Host call | Used by | Capability |
| --- | --- | --- |
| `Tasks().Create` | Issue watches filing a card | `api_write:tasks` |
| `Tasks().Update` | Review-watch cleanup completing a task | `api_write:tasks` |
| `GetCapabilityContext`, `TaskCommands().Archive` | Review-watch cleanup archiving a task | `api_write:tasks`, and the `host.v2.write:tasks` approval that comes with it on 0.97.0 |
| `Tasks().List` / `.Get` | The open-task budget, cleanup (archived included), task→workspace lookup | `api_read:tasks` |
| `Workspaces().List` | The poller, finding due watches | `api_read:workspaces` |
| `Workflows().List` / `.ListSteps` | The watch form's column picker | `api_read:workflows` |
| `AgentProfiles().List` | The watch form's agent picker | `api_read:agent_profiles` |
| `ExecutorProfiles().List` | The watch form's executor picker | `api_read:executor_profiles` |
| `Repositories().List` | The attached-repository resolver | `api_read:repositories` |

Also untested anywhere but on a host: the capability **approval** flow, the
plugin's own process lifecycle (`SetHost` → the poller goroutine), and whether
the settings panel actually renders with the host's real component library
rather than merely typechecking against its types.

## Read this before upgrading an existing install

**Changing the capability set invalidates the existing approval, for every
capability — not just the new ones.**

Kandev records an approval against a digest of the manifest's capability list
(`ManifestCapabilityDigest`). On every gated call it re-checks that the digest
still matches the installed manifest; when it does not, the call is denied with
`unavailable_capability`. The check is global, so a manifest that adds
`api_write:tasks` invalidates the grant covering `api_read:repositories` too.

The practical consequence for a 0.2.0 → 0.3.0 upgrade:

> Until an operator re-approves the new capability set, **the repository picker,
> review panel and pull-request actions stop working as well** — not only the
> new issue watches.

That is not a bug, it is the design working: a plugin that quietly gained
task-write on upgrade would be worse. But it means the upgrade is not
transparent, the release notes have to say so, and re-approval is the first
thing to verify on a real host rather than something discovered by a user.

## The strategy: two instances, in this order

Use a **disposable Kandev first, your real one second**. They answer different
questions and the order matters.

### Stage A — a throwaway Kandev, where the risk is zero

This is where the Host RPC contract, the approval flow and the panel get shaken
out. Breakage here costs nothing, so test the destructive paths properly:
uninstall, reinstall, revoke approval, downgrade.

```sh
podman run -d --name kandev-test -p 8080:8080 \
  -v kandev-test-data:/data \
  <kandev-image>:0.95.0
```

Point it at a **scratch Forgejo** — the same disposable container the live
contract tests use is ideal, because you can create and label issues freely.

Work through the checks below in full. Everything that fails here would have
failed on your real instance too, and you find it with no cleanup.

### Stage B — your real Kandev, narrowly

Once Stage A is clean, the remaining questions are environmental: your Kandev
version, your Forgejo, your token's scopes, your workflows, your session
ceiling. Those cannot be reproduced on a throwaway.

Constrain the blast radius rather than trusting the plugin:

1. **A dedicated workspace**, not the one you work in. Watches are
   workspace-scoped, so this is real isolation rather than a convention.
2. **A dedicated repository** on your Forgejo, or at minimum a label no real
   issue carries.
3. **`max_inflight_tasks: 1`** for the first watch. Raise it once you have seen
   what the filter actually matches.
4. **"Start an agent immediately" off.** On an instance with a low session
   ceiling, a watch that launches agents is the one configuration that can
   disrupt work in progress.
5. **Snapshot the database first.** `docker cp` the data directory, or stop the
   container and copy the volume. Uninstalling a plugin purges its config,
   state and vault secrets with no confirmation step.

## The checks, in order

Each one is cheap and each one can only fail after the previous passed, so stop
at the first failure rather than pressing on.

### 1. Install and re-approve

Install the release candidate. Confirm Kandev prompts for the **new** capability
set, and that the prompt lists `tasks (write)` alongside the reads. Approve it.

Then confirm the *pre-existing* features still work — open the repository picker
and a task's review panel. If those are empty, the approval did not take and
nothing below will work.

### 2. A read-only shakedown, before creating anything

Open **Settings → Integrations → Forgejo**. The "Issue watches" section calls
`watches.options`, which exercises `Workspaces`, `Workflows`, `AgentProfiles`
and `ExecutorProfiles` in one go — and creates nothing.

- The workflow dropdown lists your real workflows.
- Choosing one populates the column dropdown with its real columns.
- The agent and executor pickers list real profiles.

An empty dropdown here is a denied capability, not an empty account. This is the
cheapest possible place to discover that.

### 3. A watch that matches nothing

Create a watch against your test repository with a label no issue carries.
Press **Run now**.

Expect: `0 matched — 0 created`, and no error. This proves the whole path —
action → poller → Forgejo → back — without creating a single task.

### 4. Exactly one issue

Add the label to one issue. **Run now**.

Expect exactly one card, in the column you chose. Then check the provenance,
which is the part the UI does not show:

```sh
# Read-only; adjust the path to your deployment.
docker exec <kandev-container> python3 -c "
import sqlite3
db = sqlite3.connect('file:/data/data/kandev.db?mode=ro', uri=True)
for row in db.execute('SELECT id, title, source, workflow_step_id FROM tasks ORDER BY created_at DESC LIMIT 5'):
    print(row)
"
```

`source` must be `plugin:kandev-plugin-forgejo`. The plugin cannot set that
field itself, so it is the honest answer to "where did this card come from".

### 5. Run it again — the one most likely to fail

Press **Run now** a second time, changing nothing.

Expect: `1 matched — 0 created, 1 already tracked`, and **no second card**. This
is the dedup ledger, and a duplicate here is the failure mode that would be most
annoying in production.

Then press **Forget history** and run again: exactly one *new* card. That proves
the ledger is what was suppressing the duplicate, rather than the filter quietly
matching nothing.

### 6. The budget

Label three more issues with `max_inflight_tasks: 1` still set. **Run now**.

Expect: `created 1`, `throttled 3`. Move one card to a done column, run again,
and expect exactly one more. The budget throttles; it does not cap for life.

### 7. Both switches

- Pause the watch → **Run now** refuses.
- Re-enable it, turn the whole integration off for the workspace → **Run now**
  refuses, and the timer stops too.

### 8. Let the timer run

Everything above used the manual action. The poll loop itself — `SetHost`
firing, the goroutine starting, a due watch being picked up — is only proven by
waiting. Set the interval to its 30 s floor, label a fresh issue, and leave it
alone for two minutes.

If a card appears without you pressing anything, the background half works. This
is the check most easily skipped and the one with no substitute.

### 9. Restart the plugin

Change the plugin config (any field) to force a restart, or restart Kandev.
Confirm the watch still polls afterwards and does **not** re-file the issues it
already handled — the ledger lives in Host state and must survive the process.

### 10. The review panel and pull request writes

Added with the PR actions work. Use a throwaway Forgejo repository and a task
with linked pull requests (link with `change_requests.link`). The unit tests
cannot reach any of this: it is the host seam. Last checked on Kandev 0.97.0
with Forgejo 16 and WebKit.

- [ ] Open the task's Review panel from the `#N` button (or the "N PRs" menu).
      It shows the real author, branches, line counts, description, reviews,
      checks and comments, and the Review and merge controls with "as <account>".
- [ ] A pull request with a conflict shows "This branch has conflicts that must
      be resolved." and a disabled merge button. A clean one has an enabled
      button labelled with the repository's primary style, and the caret lists
      the other allowed styles.
- [ ] Review: the dialog opens, the three review types toggle, an inline comment
      row can be added and removed, submit shows a "Review submitted" toast and
      the review appears (Forgejo's `COMMENT` shows as "Commented").
- [ ] Comment through the panel's box: the comment appears and ends with
      "Posted via Kandev."
- [ ] Merge with a non-default style and "Delete branch" ticked: toast, the
      state becomes merged, the controls disappear, and Forgejo shows the pull
      request merged and the branch gone.
- [ ] Move the head on Forgejo while the panel is open, then merge: the stale
      merge is refused with a toast and the panel reloads.
- [ ] `connection.audit` lists each write with the verified actor.
- [ ] Settings > Forgejo: "Let agents merge pull requests" is off by default and
      survives a reload.

### 11. Review watches

Added with the review watches work. Needs a second Forgejo user to request a
review from, a throwaway repository, and a workflow that has one column which
starts an agent on entry and one that does not.

- [ ] Settings > Forgejo shows a "Review watches" section below the issue
      watches, with its own list. An issue watch does not appear in it, and the
      reverse.
- [ ] Create a review watch (empty repository list, default scope). Open a pull
      request as the second user, ask the token's account for a review, press
      **Run now**: exactly one card, titled `PR #N: …`, with the prompt filled in
      and, for a same-repository pull request, the branch checked out.
- [ ] Run again: no second card. Approve the pull request as the token's account
      and confirm it no longer matches.
- [ ] A draft pull request is left out ("1 draft(s) left out") until "Include
      draft pull requests" is on.
- [ ] A pull request from a fork: with the main column starting agents and no fork
      column, it is left out and counted; with a fork column that does not start
      agents, its card lands there, no agent starts, and the repository is
      attached without the fork's branch checked out. Saving a watch whose fork
      column starts agents is refused.
- [ ] Cleanup: set "Archive the task", merge a pull request, **Clean up now**:
      the task is archived (checked on Kandev 0.97.0, where the approval of
      `api_write: tasks` includes `host.v2.write:tasks`; a partial approval was
      refused), the card is not filed again on the next run, and a second
      cleanup makes no instance request for it.
- [ ] Cleanup fallback: needs a host without the exact archive command (older
      than 0.97.0) or one whose approval leaves it out. The task is completed, the
      watch shows "Tasks are completed, not archived…", and the task stops
      counting against the open task limit. Marking a task COMPLETED sets no
      `completed_at` on 0.97.0, so this is worth seeing once.
- [ ] Pausing a review watch changes only `enabled`; open it and confirm every
      other field is as it was.
- [ ] Downgrade check (optional): install the previous release over the top. Issue
      watches still work and the review watches are simply absent.

## Rolling back

Know the exit before you need it:

| Goal | Action | Keeps |
| --- | --- | --- |
| Stop tasks appearing, keep everything | Pause the watch | All config |
| Stop the integration for one workspace | The enable switch | All config |
| Remove a watch and its ledger | Delete the watch | Connection, task links |
| Return to the previous release | Install the older package over the top | Config, secrets, task links |
| Remove everything | Uninstall | **Nothing** — config, state and secrets are purged |

Installing a version that is already installed returns **409**, so give the
candidate its own version (`0.3.0-rc.1`) rather than reusing `0.3.0`. That also
keeps the released `0.3.0` tag honest: it should be the artifact you tested plus
a version bump, not a rebuild.

## The release gate

Ship `0.3.0` when all of these are true:

- [ ] `make lint`, `make test`, `make security`, `make verify-package` pass.
- [ ] CI's live contract matrix is green across the supported range.
- [ ] Stage A: every check above passes on a throwaway Kandev.
- [ ] Stage B: checks 1–5 and 8 pass on your real instance, in an isolated
      workspace.
- [ ] The release notes say that upgrading requires re-approving capabilities,
      and that the existing source-control features stay denied until it is done.
- [ ] `manifest.yaml`'s `version` is bumped and `CHANGELOG.md`'s `Unreleased`
      heading has become `0.3.0`.

---

[← Back to the README](../README.md)
