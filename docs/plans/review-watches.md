# Plan: review watches and cleanup

A **review watch** polls Forgejo or Gitea for pull requests where the token's
user has been asked to review, and files a review task for each new one, as
Kandev's built-in GitHub review watches do. **Cleanup** retires the tasks a watch
created once their PR is merged or closed.

Read [`README.md`](README.md) first (order of work, conventions, decisions) and do
[`00-hotfixes.md`](00-hotfixes.md) Fix A first: this plan adds more watch fields,
and the update path currently erases fields a partial request omits.

## Scope

**In**

- Review watches: scope (requested of me, or me and my teams), repository
  filter, draft handling, custom text filter, prompt template with PR
  placeholders, workflow and step, agent and executor profile, interval, enable
  switch, in-flight limit, fork handling.
- Cleanup: a per-watch policy, a reconcile loop, a manual sweep, and graceful
  behaviour when the operator has not granted archive.
- Settings UI for review watches next to the existing issue watches.

**Out**

- Webhook-driven discovery or cleanup ([#7](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/7)).
- CI auto-fix, auto-merge and PR lifecycle prompts ([#8](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/8)).
- Deleting tasks. A plugin cannot (see below).
- Using the notifications API as a source (see "Source of truth for discovery").
- Event-driven cleanup through `OnEvent` in the first version (optional later).

## What GitHub does, and where we differ

Kandev's `ReviewWatch` (`kandev/apps/backend/internal/github/models.go`, around
lines 1056-1078; service in `service_reviews.go`, poller in `poller.go`,
cleanup in `service_cleanup.go` and `poller_review_cleanup*.go`):

- Fields: workspace, workflow and step, repositories (empty means all), agent and
  executor profile, prompt template, `review_scope` (`user` or `user_and_teams`,
  default the latter), `custom_query` (overrides the scope), `target_login`,
  enabled, poll interval (default 300 s, minimum 60 s, but the loop actually uses
  a fixed 5-minute ticker), `cleanup_policy`, and last-poll status.
- Default query excludes drafts: `type:pr state:open review-requested:@me -is:draft`.
- Each poll fetches the whole matching set (not incremental), skips PRs already
  recorded, fetches PR detail (branches and head repository) for new ones, and
  creates one task per PR. The title is `PR #N: title`, and the description is the
  **interpolated prompt** using `{{pr.link}}`, `{{pr.number}}`, `{{pr.title}}`,
  `{{pr.author}}`, `{{pr.repo}}`, `{{pr.branch}}` and `{{pr.base_branch}}`. The
  task attaches the repository with the PR's head branch for checkout.
- Cleanup policies: `auto` (default), `always`, `never`. A task is terminal when
  its PR is merged or closed, or when the user already approved it. `never` makes
  no provider requests; `always` deletes on terminal; `auto` deletes unless the
  user wrote a message in the task or a PR lifecycle prompt option is on. The
  action is a **hard delete**. Archived tasks are retained and not rechecked.
- Reset deletes every task the watch created, clears the record of seen PRs and
  re-imports.
- Fork PRs get a host marker that blocks automatic agent start until the user
  starts the task manually.

Where we differ, deliberately:

| | GitHub | Us |
| --- | --- | --- |
| Poll interval | stored but ignored (fixed 5 min) | honoured; floor 30 s (consider 60 s for review watches: the search call is heavier) |
| In-flight limit | none | keep `max_inflight_tasks` (default 5) as the safety net |
| Deduplication | reserves the key before creating the task | records after creating the task, so a crash leaves a visible duplicate rather than a silently skipped PR; documented in `store.go` |
| Cleanup action | hard delete | archive or complete; **never delete** |
| Reset | cascade-deletes the created tasks | `Forget` clears the record only and re-files cards for tasks that still exist; document it, and see risk 4 |

## Constraints from the host (read before designing)

- **A plugin cannot delete tasks.** `PluginOwnedTaskTrees().Delete` is
  hard-denied (`kandev/apps/backend/internal/plugins/host_write.go`, around lines
  701-704) and the v1 `TaskReader` has no delete. So `always` cannot mean delete.
- **Archive exists but needs an operator grant.** Only the exact v2 command
  `HostTaskCommands(host).Archive(...)` archives, and it requires the manifest's
  `api_write: tasks` (we have it) **plus** a per-installation, per-workspace
  operator grant `host.v2.write:tasks`, the capability context's
  `ApprovalRevision` and `ManifestDigest` from `HostV2(host).GetCapabilityContext`,
  an `ExpectedResourceVersion` equal to the task's current version, and an
  idempotency key. Results are Applied, AlreadyApplied, Conflict (re-read and
  retry), Denied and Unsupported (older host). So archive can be denied at runtime
  and the code must degrade.
- **Without the grant, a plugin can still end a task's life.** `Tasks().Update`
  can set the state to `COMPLETED` or `CANCELLED`, and `Tasks().Move` can move it
  to a Done step, both under `api_write: tasks`, which we already hold.
- **Ownership.** The task service does not check plugin ownership on archive. The
  plugin must restrict itself to task ids in its own ledger, and may also check
  the `source` the host stamps (`plugin:<id>`, which the plugin cannot override).
- **Plugin metadata is namespaced** under `metadata["plugin:<id>"]`. A plugin
  cannot set the host's top-level `fork_pr_requires_manual_start`.
- **`StartAgent=false` is not enough** to stop an agent starting: a destination
  step with `on_enter: auto_start_agent` still launches it. The step's
  `OnEnterActionTypes` is exposed to plugins (`data_types.go`, around lines
  587-606), so we can read it.
- **Events are lossy.** `capabilities.events` plus `Plugin.OnEvent` receive host
  bus subjects (`task.created`, `task.updated`, `task.state_changed`,
  `task.deleted`, `task.moved`). There is no `task.archived`: archival arrives as
  `task.updated` with a non-null `archived_at`. Delivery is one sequential worker
  per plugin, a bounded queue of 100 that drops when full, and retries at 5, 15
  and 45 seconds; it is not durable across restarts. Events can only be an
  optimization. A **reconcile poll is mandatory**, and events cannot say whether a
  Forgejo PR merged; that needs polling Forgejo.
- **Adding `events` probably does not change the capability approval digest**
  (it is built from `api_read` and `api_write` plus managed agent tools; see
  `internal/plugins/approval.go`), but verify on a live Kandev and still add a
  changelog note.

## Design

### One model with a `kind`, not a second subsystem

Add `Kind` (`"issue"` default, `"review"`) to `Watch`
(`internal/watches/watch.go`, around lines 97-152), with `omitempty`. Keep the
shared machinery: tick, workspace gate, due check, in-flight count, budget, store,
ledger, and the `watches.*` actions. Split only the part that differs:

```go
// Sketch, not a prescription.
type runner interface {
    fetch(ctx context.Context, w Watch, repo RepoRef, since time.Time) ([]candidate, error)
    build(w Watch, c candidate) pluginsdk.CreateTaskInput
}
```

with an issue runner and a review runner, and a second port `ReviewSource`
beside `IssueSource` that `internal/forgejo` implements. A separate model, store
and action set would duplicate roughly 60 percent of the code and add at least
seven action keys.

New review-only fields: `review_scope`, `include_drafts`, `cleanup_policy`,
`fork_workflow_step_id`, and the free-text `query` and `labels` already present.
Every field the update path handles must be presence-aware (Fix A).

### Migration and downgrade

- Existing v0.3.0 records have no `kind`. `Normalize` maps empty to `"issue"`; no
  state rewrite is needed. Existing issue watches must default to cleanup `never`
  (empty means never for issues), so an upgrade never starts archiving tasks
  without consent. New review watches default to the policy in the form.
- The ledger is unchanged. Issues and PRs share one per-repository number space,
  so the `<repo-digest>.<number>` key cannot collide even in workspace dedup
  scope. Offer per-watch dedup only for review watches, as GitHub does.
- **Downgrade hazard.** A v0.3.0 binary would read a review watch as an issue
  watch and poll it with irrelevant filters. If downgrade safety matters, store
  review watch config under its own prefix (for example `rwatch.`, which v0.3.0
  skips because it lacks the `watch.` prefix) and keep the shared ledger
  prefixes. It is cheap and recommended. Keep the existing key-vocabulary tests
  (`TestWatchKeysDoNotCollideWithExistingState`,
  `TestStateKeysStayWithinTheHostVocabulary`) passing.

### Source of truth for discovery

Use the search endpoint, not notifications.

- `GET /repos/issues/search?type=pulls&state=open&review_requested=true`.
  **Verified from Gitea's swagger and source** (1.20 and 1.25): `review_requested`
  exists from Gitea 1.14, `type=pulls` and `since` exist throughout, and the
  filter matches the latest direct request to me **or** a request to any of my
  teams. It excludes PRs I authored, and a PR drops out once I approve or reject.
  So the Gitea equivalent of GitHub's `review-requested:@me` is the default, with
  teams included. There is no server-side "me only" filter.
- **"Me only" scope** is a client-side check that the PR's `requested_reviewers`
  contains the token user's login (from `GET /user`, which the connection probe
  already calls; `Client.CurrentUser` in `api.go`). That needs per-PR detail,
  which we fetch anyway for new PRs.
- **Do not narrow with `team`.** The `team` parameter (Gitea 1.16+) narrows which
  repositories are searched; it is not "requested from team X".
- **Do not rely on `since`.** Whether a new review request bumps the PR's
  `updated_at` is unverified, so fetch all open review-requested PRs each tick
  (bounded pages) and de-duplicate by ledger, as GitHub does.
- **Repository filter is client-side**: the search has `owner` but no per-repo
  filter, so filter on the result's repository full name. A watch with no
  repositories means every repository visible to the token; the in-flight limit
  is the safety net.
- **Custom filter**: there is no query language. Define the custom scope as free
  text `q` plus labels plus the repository list, on top of `review_requested`;
  do not accept a raw query string.
- **Not notifications.** Read state is shared with the web UI, so a user who opens
  a PR hides it, and notifications have no clean "review requested" reason.
- **Drafts.** `PullRequest.draft` arrives in Gitea 1.22; before that the only
  signal is the configured `WIP:` title prefix. Default to excluding drafts, and
  fail to the title heuristic where the field is absent.
- **Forgejo is unverified.** Forgejo is a hard fork of Gitea 1.21+, hosted on
  codeberg.org, which could not be reached while planning. Assume Forgejo 7 is
  close to Gitea 1.22 and Forgejo 16 later, but pin all of it on the live
  containers.
- The composer search already calls this endpoint (`SearchPullRequests`,
  `internal/forgejo/api.go`, around lines 296-310), so auth and the response shape
  are proven. `IssueSearchResult` lacks the author, `draft`, `updated_at` and
  labels; extend it.
- Do **not** copy the paging pattern flagged in Fix B. Continue on the raw page
  length.

### Task creation

- Title `PR #N: title` (cap at 160 runes as issues do). Description is the
  prompt, **interpolated**. The current issue-watch code passes the prompt
  verbatim into `Launch.Prompt`; review watches need `{{pr.*}}` expansion with the
  placeholders above. Ship a default prompt modelled on Kandev's
  `pr-review-watch-default.md`: PR number and title, repository, link, author,
  branch to base, and the three-dot diff commands, with an instruction not to
  review outside the diff.
- Attach the repository with `CheckoutBranch` set to the PR head branch and
  `PullRequestNumber` set (`PluginTaskRepository`, `data_types.go`, around lines
  1224-1230), **for same-repository PRs only**. Validate the head branch name
  before use; a name that fails a safe-ref check creates the task without a
  checkout.
- Task metadata (host-namespaced): repository, PR number and URL, watch id,
  author, head and base branch, and `forgejo_fork` when applicable.
- The review runner needs per-new-PR detail: one `GET /repos/{o}/{r}/pulls/{n}` for
  head and base, head repository, `draft` and `requested_reviewers`.

### Fork pull requests

GitHub blocks automatic start for fork PRs through a host marker we cannot set.
Equivalent protection:

1. Detect a fork from the PR detail: `head.repo` is nil or its full name differs
   (case-insensitively) from `base.repo`. **Fail closed**: unknown identity counts
   as a fork. Test AGit-flow PRs on a live instance.
2. For a fork PR: `StartAgent=false`, and do **not** set `CheckoutBranch` or fetch
   the PR ref (that is the fork-controlled content that would reach a setup
   script holding executor environment values).
3. Handle the destination step:
   - Preferred: a per-watch `fork_workflow_step_id` pointing at a step without
     `auto_start_agent`. Reject at save time any configured fork step whose
     `OnEnterActionTypes` contains `auto_start_agent`.
   - Otherwise, if the main step auto-starts and no safe fork step is set, **skip
     fork PRs** and report the count in the run result (a new `Skipped`).
   - Last resort: create the task with no repository attached and the link in the
     description.
4. Say plainly in `docs/security.md` that a plugin cannot set the host's manual
   start marker, so this protection is weaker than GitHub's.

### Cleanup

Policy vocabulary (README decision 5): GitHub's `auto` needs to know whether the
user wrote a message in the task, which needs `api_read: messages`, a new
capability that forces re-approval. Recommended for this release:

| Value | Meaning |
| --- | --- |
| `never` | Never touch the task. Default for existing issue watches. |
| `when_closed` | When the PR is merged or closed, archive the task. |

Add `auto` (keep the task if a person engaged with it) later together with the
messages capability and its upgrade note. Do not ship `auto` with different
meaning than GitHub's.

Action on a terminal PR, in order of preference:

1. Archive through the exact v2 command, if the operator has granted
   `host.v2.write:tasks`.
2. Otherwise mark the task complete (state `COMPLETED`, or move to a Done step).
3. Never delete. Say so in the docs.

The reconcile loop, per tick, per review watch with a cleanup policy, after
discovery:

1. Read the watch's ledger records (`Store.Records`).
2. One `Tasks().List` page-walk with `IncludeArchived: true` gives the live,
   archived and missing task sets together.
3. Skip archived or already-completed tasks: **zero provider requests**.
4. For each remaining record, `GET /repos/{o}/{r}/pulls/{n}` (existing
   `Client.PullRequest`). One request per live record, no feedback fetch. Bound
   the per-tick budget, and stop the whole batch on a rate-limit or 403.
5. If the PR is closed or merged, apply the policy.
6. Keep the ledger record after archiving, so the PR is not filed again. For a
   missing task, drop the record only once the PR is terminal, so an open PR is
   not re-filed.
7. Optional, for parity with GitHub's "already approved by me" terminal reason:
   `GET .../pulls/{n}/reviews` plus the current user.

Archive needs `ExpectedResourceVersion`: on Conflict, re-read the task and retry
once. Treat Unsupported and Denied as "fall back to complete" and record that in
the watch's status so the panel can say why.

Provide a manual sweep, `watches.cleanup` (one new action; a deliberate
`manifest_test.go` count change). `watches.options` gains the review scopes, the
cleanup values, the token user's login and whether the archive grant is present,
so the panel can show "Cleanup needs the Host v2 tasks grant".

## Steps

1. **Fix A and Fix B** from the hotfix plan (Fix A is a hard prerequisite).
2. **Shared groundwork**: a `reviewer` user on the live containers. The review
   request search must be exercised as that user.
3. **Probe and record** (below).
4. **Model and store**: `Kind`, review fields, `Normalize` and `Validate` per kind,
   the optional `rwatch.` prefix, presence-aware update. Tests in
   `internal/watches/watch_test.go`, `store_test.go`.
5. **`ReviewSource` port and the Forgejo adapter**: search, repository filter, PR
   detail enrichment, fork detection, draft detection, scope check. Extend
   `IssueSearchResult`. Error redaction in the style of `translateIssueError`.
6. **Runner split in the poller**, then the review runner: build the task input,
   interpolate the prompt, apply the fork rules, respect the in-flight budget.
7. **Cleanup**: policy, reconcile loop, archive via `HostV2` with the fallbacks,
   manual sweep action, status reporting.
8. **Actions and manifest**: `kind` accepted on create (immutable on update),
   `watches.list` with an optional kind filter, `watches.options` additions,
   `watches.cleanup`. Update `manifest.yaml`, its comments and `manifest_test.go`
   (action count and keys, capability set) deliberately.
9. **UI**: parametrize `createWatchesPanel(host, {kind})` in
   `ui/src/watches-panel.ts`, and render two sections in `ui/src/bundle.ts` ("Issue
   watches" is hard-coded around line 58). Show or hide fields by kind; add the
   review fields, the cleanup control, the fork step selector and the grant notice.
10. **Docs and changelog**: generalise or split `docs/issue-watches.md`, update the
    README feature table and index, `docs/security.md`, `docs/development.md`
    (package table, strategy split), `docs/configuration.md`,
    `docs/testing-on-kandev.md` (capability table and manual steps), `AGENTS.md`
    (new host behaviours), and `CHANGELOG.md` under `## Unreleased`, with an
    upgrade note if any capability changes.

## Verify against live instances first

Assumed means unverified. Record each in `docs/development.md`.

- `review_requested` is accepted, includes team requests, and honours
  `type=pulls`, on Gitea 1.20 and 1.27 and Forgejo 7 and 16.
- Whether a new review request bumps the PR's `updated_at`.
- `draft` and `requested_reviewers_teams` presence by version (Gitea 1.22 and 1.23
  respectively, per swagger; Forgejo unknown).
- Page-size clamping and the maximum `limit` on the search endpoint.
- The `requested_reviewers` shape, and that an approved review drops the PR out of
  results.
- The response when the token lacks `read:issue`.
- AGit-flow PR head repository identity.
- Search results for a repository the token cannot see.

## Tests

- **Unit, fake server** (`internal/forgejo`, in the style of `issues_test.go`):
  search parameters and paging that does not stop early, repository filtering,
  draft detection, the detail enrichment, fork detection (head repo nil, differing
  name), error redaction.
- **Poller** (`internal/watches`, `fakeHost` plus a scripted `ReviewSource`): task
  input shape, interpolated prompt, checkout only for same-repo PRs, no start for
  forks, the fork-step guard, de-duplication, throttling, per-repo failure
  isolation. Extend `fakeHost` for `Tasks().Move` and `Update` and add a `HostV2`
  fake returning Applied, Conflict, Denied and Unsupported.
- **Cleanup**: policy against merged, closed and open, archived and missing
  tasks; archived tasks cause zero provider calls; a rate limit aborts the batch
  and leaves state intact; the ledger survives archiving.
- **Regression**: Fix A's toggle test, and that issue watches are unaffected.
- **Manifest tests**: action count and keys, capability set.
- **UI**: kind switching, the new fields, the grant notice.
- **Live contract test** (`internal/forgejo/integration_test.go`):
  `TestLiveReviewRequestedSearch`: create a PR, request review from `reviewer`,
  search with that user's token; also a team request if feasible and the approval
  drop-out.
- **Manual host checks** in `testing-on-kandev.md`: the archive grant flow, a fork
  PR landing in a non-auto-start step, and that cleanup degrades without the
  grant. Fake hosts cannot prove these.
- Break each fix and confirm the test fails.

## Acceptance criteria

- A review watch files one task per open PR where the token user's review is
  requested, with an interpolated prompt and, for same-repo PRs, a checkout of the
  head branch.
- Fork PRs never start an agent automatically, or are skipped and counted.
- With a cleanup policy set, terminal PRs have their task archived (or completed
  if archive is not granted); archived tasks cost no provider requests; nothing is
  ever deleted.
- Existing issue watches behave exactly as in 0.3.x, including cleanup `never`.
- Pausing any watch preserves every other field.
- No new capability in the recommended form; if `auto` or `events` are added, the
  upgrade note says what operators must re-approve.
- `make lint && make test` pass, the live test passes on all four containers, and
  the manual host checks are done.

## Risks and open questions

1. **Archive depends on a separate grant.** Without it cleanup silently degrades
   to completing the task. Make that visible.
2. **Lost events.** Treat the reconcile poll as the source of truth; events, if
   added, only save latency.
3. **Rate.** One request per live ledger record per tick. Cap pages and budget,
   stop on 429 or 403, and add a circuit breaker like GitHub's.
4. **Reset re-files cards.** `Forget` followed by a poll re-creates tasks for
   every still-open PR, which is more painful for review watches. Consider making
   reset drop ledger entries only for terminal PRs, or show a count first. Document
   at least.
5. **Interval floor.** The search endpoint is heavier than a per-repo list;
   consider 60 s for review watches.
6. **Repository filter is client-side**, and a watch with no repositories reads
   everything visible to the token.
7. **Drafts before Gitea 1.22** are detected only by the `WIP:` prefix.
8. **Author exclusion.** The search excludes PRs the token user authored, which
   differs from GitHub only in edge cases.
9. **Test churn.** A `kind` field touches `ui/test/watches-panel.test.ts`,
   `internal/plugin/watches_test.go`, `internal/watches/*_test.go` and
   `manifest_test.go`.
10. **Decisions 5, 6 and 7 in the README** need an owner before steps 6 and 7.
