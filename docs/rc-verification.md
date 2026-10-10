# Verifying a release candidate on a real installation

A runbook for an agent (or a person) with shell access to a real Kandev and a
Forgejo or Gitea instance. It exercises what the unit tests and fake hosts
cannot: the host seam, the real approval flow, and the real UI. It complements
[testing-on-kandev.md](testing-on-kandev.md), which explains why each check
matters; this page says exactly what to run and what counts as a pass.

Written for `0.4.0-rc.1`. Report results with the template at the end.

## Ground rules

- **Use a throwaway Forgejo repository and a workspace you can spare.** Several
  checks merge, close and archive things.
- **Never print the access token.** Pass it through a variable.
- **Expect no capability prompt.** `0.4.0-rc.1` declares the same capabilities as
  0.3.x. If Kandev asks for re-approval, that is a failure: record it.
- **Kandev returns 409 for a version that is already installed.** If a candidate
  needs fixing, it needs a new version string, not a re-tag.
- A 503 `plugin action unavailable` hides the real cause. The reason is only in
  the Kandev server log (`podman logs <kandev>` or the service log).
- A plugin's own stderr does **not** reach Kandev's log on v0.97.0. Use the
  `connection.audit` action to see what the plugin did.

## 0. Setup

You need:

| Thing | Notes |
| --- | --- |
| Kandev ≥ 0.95.0 (0.97.0 recommended) | A workspace with a workflow that has **a column that starts an agent on entry** and **a column that does not** (Kandev's default "Development" workflow has both). |
| Forgejo or Gitea | Admin shell access is the easiest way to mint tokens and users. |
| Users | `kandev` (admin, the plugin's token user), `author` (writes to the repo and can fork it), optionally `readonly`. |
| A repository | e.g. `kandev/demo`, with a `main` branch, public or with `author` as a write collaborator. |

Mint tokens (Forgejo/Gitea CLI; adjust the container and binary):

```sh
podman exec -u git <forgejo> forgejo admin user generate-access-token \
  --username kandev --token-name rc1 --scopes all --raw
```

For scope checks you also need narrower tokens; see check 12.

Install the release asset and configure it:

```sh
curl -sL -o plugin.tar.gz \
  https://github.com/runoverlabs/kandev-plugin-forgejo/releases/download/v0.4.0-rc.1/kandev-plugin-forgejo-0.4.0-rc.1.tar.gz
curl -s -w '\nHTTP %{http_code}\n' -F package=@plugin.tar.gz http://<kandev>/api/plugins/install   # expect 201
```

Approve capabilities and set `base_url` and `api_token` through the Kandev UI
(Settings > Plugins) or the HTTP API as described in
[configuration.md](configuration.md). Then every action can be called headlessly:

```sh
A(){ curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"workspaceId\":\"$W\",${TASK:+\"taskId\":\"$TASK\",}\"body\":${2:-{\}}}" \
  http://<kandev>/api/plugins/kandev-plugin-forgejo/actions/$1; }
```

Workspace-scoped actions reject a `taskId` with HTTP 400; task-scoped actions
need one.

**Pass:** install returns 201 with `status: active`; `A connection.get` returns
`configured: true, connected: true` with the right `account`.

## 1. Upgrading an existing install

Only if the installation already ran 0.3.x with data.

- Before installing, record `A watches.list '{}'` and the output of the issue
  watch you rely on.
- Install the rc over the top.
- **Pass:** no approval prompt; `watches.list` returns the same issue watches
  with the same fields (no `kind`, no review fields); the watch still polls and
  does not re-file issues it already handled.

## 2. Settings screen renders

Open `/settings/workspaces/<workspace>/integrations`, then the **Forgejo** entry.

- **Pass:** three sections appear, in order: connection, **Issue watches**,
  **Review watches**. No uncaught page error. The review form (press the second
  "Add watch") shows "Whose requests", "Include draft pull requests", "Column for
  fork pull requests", "When a pull request is merged or closed", and
  "Repositories (optional)", and does **not** show "Issue state". The issue form
  is unchanged. Submitting an empty form shows "A name is required", not
  "Couldn't reach the Forgejo plugin".
- Playwright works headless against Kandev's web UI; WebKit needs no sandbox
  flags in the container image.

## 3. Issue watches (regression)

Run on a repository with labelled issues.

1. Create a watch with a label no issue carries; **Run now**. Pass: `0 matched,
   0 created`, no error.
2. A label no issue carries must match **nothing** (rc.1 matched everything; fixed after rc.1). Label one issue; **Run now**. Pass: exactly one card in the chosen column;
   its `source` is `plugin:kandev-plugin-forgejo`.
3. **Run now** again. Pass: `0 created, 1 already tracked`, no second card.
4. **Forget history**, run: exactly one new card.
5. With `max_inflight_tasks: 1` and three more matching issues: `created 1,
   throttled 3`; complete a card, run: one more.
6. Pause the watch: **Run now** is refused with a readable message. Resume.
   Turn the workspace integration off: **Run now** is refused again.
7. **Pausing must not erase anything.** Create a watch with a prompt, agent
   profile, repository and labels; `watches.update` with only `{id, enabled:false}`
   and then `{id, enabled:true}`; `watches.list`. Pass: every field is unchanged.
8. Set the interval to 30 s, label a fresh issue and wait two minutes without
   pressing anything. Pass: a card appears (the background poller works).
9. Change any plugin config field to restart it. Pass: the watch still polls and
   files nothing it already handled.

## 4. The review panel and pull request writes

Prepare a task linked to pull requests (`change_requests.link` with a pull
request URL or `owner/repo#N`). Open the task and the **Review** panel from the
`#N` button (or the "N PRs" menu if several are linked).

| Check | Pass |
| --- | --- |
| Details | Author, branches, line counts, description, reviews, checks and comments are the real ones; merge and review controls show "as <account>". |
| Conflict | Mergeability is computed asynchronously: right after a PR is opened `mergeable` is `null` and `blockers` empty, on purpose, so the panel stays quiet rather than flashing "not mergeable". Re-read after a few seconds. A PR with a conflict then shows "This branch has conflicts that must be resolved." with merge disabled. A clean PR has merge enabled, labelled with the repository's primary style; the caret lists only the styles the repository allows. |
| Review | The dialog submits approve / request changes / comment, with an inline comment row. A "Review submitted" toast appears and the review is listed (`COMMENT` shows as "Commented"). Self-approval is refused with a readable message. |
| Comment | Appears on Forgejo and ends with "Posted via Kandev." |
| Merge | Squash merge with "Delete branch" ticked: toast, state flips to merged, controls disappear; Forgejo shows it merged and the branch gone. |
| Stale head | Open the panel, push a new commit to the PR on Forgejo, then merge in the panel. Refused with a message; the panel reloads. |
| Audit | `A connection.audit` (no taskId) lists each write with the verified Kandev actor (the signed-in Kandev user, `default-user` on a single-user install; `agent:<session>` for an agent), number, action and outcome, and **no text**. The Forgejo account is the connection's, shown by `connection.get`; it is not repeated per entry. |
| Agent merge | Settings: "Let agents merge pull requests" is off by default and survives a reload. |

### Agent tool (needs an agent session on the task)

In a session, call the `pr` tool:

- `pr get` shows state, `sha`, checks. With several PRs linked, writes refuse
  rather than pick one.
- `pr comment` works. `pr merge` is **refused** while the switch is off. Turn
  it on, then `pr merge` with the `sha` from `get` merges; with a stale `sha`, or
  a PR with conflicts, missing approvals or failing/running checks, it refuses
  with a reason.
- **Pass:** the `ci` and `pr` tool definitions appear in the session once each;
  record the prompt-token cost your Kandev reports for them (the estimator is
  Kandev's, not ours).

## 5. Review watches

You need `author` to open pull requests that request a review from `kandev`.
Create them through the Forgejo API as `author`:

- **same-repo:** branch + file + PR in `kandev/demo`, then request `kandev`;
- **draft:** the same with a `WIP: ` title;
- **fork:** as `author`, `POST /repos/kandev/demo/forks`, push a branch and a file
  into `author/demo`, open the PR with head `author:<branch>`, request `kandev`.

Create the watch (`kind: "review"`, workflow, a column that does **not** start
agents, `fork_workflow_step_id` set to another non-starting column,
`cleanup_policy: "when_closed"`, `repos: ["kandev/demo"]`) and **Run now**.

| Check | Pass |
| --- | --- |
| Discovery | `matched: 3, created: 2, drafts: 1`. Titles read `PR #N: …`. The description has the prompt with number, title, link, author, repo and branches filled in, and no `{{`. |
| Placement | The same-repo card is in the main column; the fork card is in the fork column and carries `forgejo_fork: true` in its plugin metadata. The plugin passes the configured column to Kandev, but Kandev applies that column's own on_enter actions and workflow rules, which can move a card on or start an agent. If a card lands elsewhere than configured, read the column's actions in the workflow before suspecting the plugin; for a fork card the watch also reports "moved by workflow automation" in its last error. **No agent session started for the fork card.** |
| Idempotence | Run again: `created: 0, duplicates: 2`. |
| Drafts | Turn on "Include draft pull requests", run: the draft is filed. |
| Answering | Approve a PR as `kandev` (or a second reviewer): it stops matching on the next run. |
| Scope | With "Only me", a PR where only a team was asked is not filed (needs a team; if you have none, say so). |
| Fork guard, save time | `watches.create` with `fork_workflow_step_id` set to a column that starts agents is refused with **422** and "starts an agent". |
| Fork guard, run time | Make the **main** column start agents and clear the fork column: a fork PR is left out and counted (`skipped_forks: 1`); a same-repo PR is still filed. |
| Checkout (needs a Kandev repository attached to the watch) | A same-repo task has the PR's head branch checked out; a fork task has the repository but **not** the fork's branch. |
| Errors | Each refusal above arrives as its own status and message, never "plugin action unavailable". That includes a malformed body (`labels` as a string) and a `change_requests.link` body without `reference`. `repos` accepts `"owner/name"` strings or the `{"owner","name"}` objects `watches.list` returns. (Needs a build with PR #26; rc.1 itself returns 503 for these.) |
| Kinds | `watches.list {"kind":"review"}` and `{"kind":"issue"}` return disjoint sets; `{}` returns both. A watch's kind cannot be changed (422). |

### Cleanup

1. Merge one PR and close another. **Clean up now** (`watches.cleanup`). Pass:
   both tasks archived (`archived: 2`) on Kandev 0.97.0; the cards are not filed
   again on the next run.
2. **Clean up now** again. Pass: `archived` is absent or 0 and, if you can see
   Forgejo's access log, **no request** is made for the retired tasks.
3. **Fallback** (only on a host with no exact archive command, i.e. older than
   0.97.0, or one whose approval omits it): the task is marked complete, the
   watch shows "Tasks are completed, not archived…", and the task stops counting
   against the open task limit. Marking a task COMPLETED does not set
   `completed_at` on 0.97.0; confirm the budget still frees.
4. A PR whose task was deleted by hand: while the PR is open the ledger entry
   stays (it is not filed twice); once the PR is finished the entry is dropped.
5. **Never deleted:** no task is ever removed by cleanup.

## 6. Provider matrix

If you have more than one Forgejo/Gitea version, repeat checks 3–5 on each. The
repository's CI already does the REST-level matrix (Gitea 1.20/1.27, Forgejo
7/16); what you add is the real Kandev on top. Record the version of each.

## 7. Scope and permission behaviour

With a token limited to `read:repository,write:repository` (no `write:issue`),
in the review panel:

- review and merge work;
- comment and label changes are refused with a message naming `write:repository`
  and `write:issue`;
- the review-watch search fails with a readable error (`read:issue` is needed),
  not a crash, and the watch records `last_error`.

With a read-only collaborator as the token user, merge reads as a permission
problem, not "not mergeable". On a protected branch, merging without the
required approvals reads as branch protection.

## 8. Rollback (optional)

Install the previous release over the top. Pass: issue watches keep working and
review watches are simply absent (they are stored under `rwatch.`, which 0.3.x
does not read). Reinstalling the rc brings them back.

## Reporting

Return a table with one row per numbered check, in this form, and attach the
evidence named in the Pass column for anything that failed:

```text
Kandev version / Forgejo version / plugin version:
Check | Result (pass / fail / not run) | Evidence or reason
```

Mark **not run** rather than **pass** for anything you could not exercise (no
team, no older host, no attached Kandev repository), and say why. A failure that
shows only "plugin action unavailable" needs the Kandev server log line.

---

[← Back to the README](../README.md)
