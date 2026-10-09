# Plan: PR actions

Let agents and users act on a pull request, not just read it: merge it, submit a
review, request reviewers, see why it can or cannot merge, read its files and
diff, and comment, label and assign. Kandev's GitHub integration offers most of
this; the plugin today only reads PRs, reviews and CI, and can open a PR and
mark it ready.

Read [`README.md`](README.md) first (order of work, conventions, decisions) and
do [`00-hotfixes.md`](00-hotfixes.md) and the shared groundwork before this.

## Scope

**In**

- Merge: styles `merge`, `squash`, `rebase`, `rebase-merge`, `fast-forward-only`
  as the repository allows, delete-branch-after-merge, schedule a merge for when
  checks pass, and cancel a scheduled merge.
- Submit a review: approve, request changes, comment, with optional inline
  comments.
- Request and remove reviewers.
- Read: mergeability and the reason a merge is blocked, required checks and
  approvals where the token can see them, changed files, diff, commits.
- Update the branch from its base (merge or rebase).
- Comment on the PR; set labels and assignees.
- Surfaces: new ops on the existing agent tools, and actions and buttons in the
  plugin-owned review panel.

**Out**

- Webhooks and push-based refresh ([#7](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/7)).
- Automatic merge, CI auto-fix and lifecycle prompts ([#8](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/8)).
- Thread resolve/unresolve: not reliably in the REST API on the supported floor.
- Native "Create PR" and the host's change-request MCP tools: the host hard-codes
  those to GitHub and GitLab, so our own tools are the substitute.
- A merge queue, a draft flag (we keep the `WIP:` title convention), and anything
  needing a new `capabilities` entry.

**No new capability is needed.** Everything below uses `api_read`, `api_write:
tasks`, `state` and `secrets` as declared today, so operators are not asked to
re-approve. Keep it that way; if a step seems to need a capability, stop and
raise it.

## What the host gives us, and its limits

Findings from reading Kandev v0.97.0 that shape the design:

- **The review panel is ours to fill.** `ReviewPanel` in `ui/src/source-control.ts`
  (around lines 472-486) renders `host.ui.ChangeRequestDetail` and today passes no
  `actions`, `onAction`, `onRefresh` or `onAddContext`. That is the seam for
  buttons.
- **What the host component supports** (`kandev/apps/web/components/integrations/change-request-detail.tsx`,
  around lines 23-120):
  - `actions[]` with `placement` `header`, `comment` or `thread`, a `tone`
    (`default`, `success`, `danger`, `secondary`), `label`/`pendingLabel`, `busy`
    and `disabled`. `onAction({actionId, body?, threadId?, targetId?})` receives the
    click.
  - A `comment` action renders one textarea and submit (first one only); a
    `thread` action renders a reply box on each thread (first one only).
  - Per-review `actions` render buttons that send `targetId`.
  - `headerActions` and `notice` accept arbitrary React nodes, so a merge
    dropdown can live there.
  - `onRefresh`, `onRetry` and `onAddContext` exist.
- **What it does not support:** a structured review submit (approve / request
  changes / comment selector) and inline-comment authoring. Build those as a
  custom node or a `host.ui.Dialog`. The host docs say provider-specific markup is
  "not a supported parity path", so prefer `actions` + `onAction` and use custom
  nodes only where the model has no equivalent.
- **The SDK types the prop as `unknown`.** Nothing checks the shape at compile
  time; mirror the model by hand (see step 1).
- **`ReviewSummary` is narrow**: no mergeable flag, required checks or allowed
  merge methods. Anything extra comes from a **new action** the panel calls when
  it opens, cached in the panel's own React state, not from
  `registerReviewProvider.refresh` (which should stay light: the host calls it
  about every 90 seconds).
- **Actions:** a manifest `actions` entry has `key`, `scope`, `access` (default
  `authenticated`) and `max_body_bytes`. A `task`-scoped action gets a
  host-verified `VerifiedActionContext` (actor, workspace, task, repository,
  session, head branch). 15-second timeout, 1 MiB reply cap. A handler may return
  an explicit HTTP status 200-599; use 403/409/422 for expected failures instead
  of a Go error, which the host masks as an internal error.
- **A click is not an agent call.** Browser actions arrive through
  `HandleAction`; agent calls through `InvokeAgentTool`. They are different RPCs,
  so an action reachable only through `HandleAction` cannot be invoked by an agent
  through the tool path. Residual risk to check on a real host: whether an agent
  sandbox can reach the Kandev plugin-action REST route with some credential.

## Findings in our code that the plan must handle

- **`Client.do` flattens errors** (`internal/forgejo/client.go`, around lines
  125-157): 403 becomes `ErrUnauthorized`, rendered by `safeMessage`
  (`internal/plugin/runtime.go`, around line 424) as "The instance rejected the
  access token". That is wrong for a protected branch, a missing write permission
  or a self-approval. Other statuses ≥ 400 become `StatusError{Method, Path,
  Status}` with no body. Merge returns 405 (not mergeable), 409 (conflict, out of
  date, head mismatch) and 422; these must be told apart with fixed, token-safe
  reasons. `do` currently discards the body, so a write-specific path must read a
  bounded body to classify, and must never forward it.
- **There is no `put` or `delete` helper**, only `get`, `post`, `patch`. Add them
  beside `patch`. All path segments go through `pathSegment` (`api.go`).
- **`api.go` is already about 350 lines.** Put the new PR methods in a new
  `internal/forgejo/pullactions.go`.
- **`Repo` has no merge-style fields** (`allow_merge_commits`, `allow_squash_merge`
  and so on), `PullRequest` has no `mergeable`, line counts or requested
  reviewers, `EditPullRequestInput` omits assignees and labels, and `Reviews` has
  no pagination. `ReviewTaskReview.Required` and `.Requested` are never filled.
- **The `pr` agent tool's discriminator is `op`**, with values `get`, `open`,
  `ready` (`manifest.yaml`, around lines 145-174; `agenttools.go`).
- **Agent-tool budget is a hard test.** `internal/plugin/manifest_test.go`
  (around lines 147-190) pins two tools, each under 1,024 bytes of JSON and under
  1,536 together. Today `pr` costs about 177 tokens, `ci` 150. Every tool is
  injected into every matching agent session, so descriptions must stay terse.

## Steps

### 1. Fix the panel's data model (prerequisite)

Do [Fix C](00-hotfixes.md#fix-c-the-review-panel-may-not-render-verify-first)
first, including reproducing the problem on a live Kandev. Then go further: the
panel needs real detail, so add a read action (step 5) and rewrite
`toChangeRequestDetail` to emit the **full** `ChangeRequestDetailModel`, driven by
that action's response and not by the narrow `ReviewSummary`. Keep the local
mirror type so drift becomes a compile error. Add a test that exercises the
mapper against a complete fixture, and, if a rendering harness is feasible, a
render test; otherwise record the manual check in `testing-on-kandev.md`.

### 2. Probe the instances and record the results

Before encoding any behaviour below, verify it on the four live containers (see
the matrix) and write the outcomes into a "Verified against" table in
`docs/development.md`. Items marked "assumed" in this plan are exactly the ones to
pin. Feature-detect from response data, never from a version string (the repo
forbids branching on flavor).

### 3. Client layer (`internal/forgejo`)

- Add `put` and `delete` helpers.
- Add a typed write error, for example `WriteError{Status int; Reason Reason}`,
  where `Reason` is a fixed enum: `not_mergeable`, `conflict`, `out_of_date`,
  `head_changed`, `forbidden`, `blocked_by_protection`, `self_review`,
  `already_merged`, `invalid`. Derive it from the status plus a bounded parse of
  the response body in a write-only path; never return or log the body. Update
  `safeMessage` and the tool-side translation (the existing precedent is
  `translateIssueError` in `issuesource.go`). Extend `TestClientErrorsDoNotLeakToken`
  to the new path.
- Extend DTOs: `Repo` (merge-style flags, default merge style, default
  delete-branch), `PullRequest` (`mergeable`, `merged`, additions, deletions,
  changed files, requested reviewers, head and base SHA), `Branch` (`protected`,
  required approvals, status-check contexts, `user_can_merge` where present),
  `EditPullRequestInput` (assignees, labels).
- New methods in `pullactions.go`: `MergePullRequest`, `CancelAutoMerge`,
  `SubmitReview`, `RequestReviewers`, `RemoveReviewRequest`, `UpdateBranch`,
  `PullFiles`, `PullCommits`, `PullDiff`, `CreateComment`, `SetLabels`,
  `SetAssignees`, `BranchProtection`. `PullDiff` needs a **head-bounded** read
  (the existing `getTail` keeps the end); cap the size and report truncation.
  Paginate `PullFiles` and `PullCommits` with explicit caps.
- Tests: fake-server cases per status mapping using `apiServer` in
  `testsupport_test.go` (`handle`/`handleFunc`, keyed by `"METHOD /path"`;
  `changerequests_test.go` around lines 81-135 is a body-asserting template).
  Pin the live shapes from step 2 so the fakes stay honest.

### 4. Neutral port and wiring

Merge and review are provider-neutral concepts. Either declare a port in
`internal/sourcecontrol` (for example `PullRequestActions`) with the Forgejo
adapter behind it, or route plugin-owned action keys in `Runtime.HandleAction`
the way `connection.*` and `watches.*` are routed. The second is simpler and fits
existing code; the first is cleaner if the vocabulary is meant to be shared.
Either satisfies the architecture rule so long as every URL stays in
`internal/forgejo` and `internal/plugin` only wires. Recommended: the second, with
the request and result types defined in the neutral package so a second provider
could reuse them.

### 5. Actions (`manifest.yaml`, `internal/plugin`)

Add, all `scope: task`, access `authenticated`, each declared in the manifest and
counted in `manifest_test.go` with a comment:

| Key | Kind | Body limit (indicative) |
| --- | --- | --- |
| `change_requests.details` | read | 1 KiB |
| `change_requests.merge` | write | 4 KiB |
| `change_requests.review` | write | 32-64 KiB (inline comments) |
| `change_requests.request_reviewers` | write | 2 KiB |
| `change_requests.update_branch` | write | 1 KiB |
| `change_requests.comment` | write | 16 KiB |

`details` returns, in one bounded response: the PR, repository merge styles,
mergeability with a stated reason, protection and required checks where visible,
reviews, requested reviewers and comments. Budget it: the host allows 15 seconds
and the client timeout is 30, so a composite read must be bounded or split, and
must expose truncation. Load files, diff and commits separately and lazily, only
when the user opens that part.

Rules for every write action:

- **Resolve the PR from the task's linked set, never from a raw number.** Reuse
  the exact resolution the `ready` op uses (`taskPullRequests` in `agenttools.go`,
  around line 333, and `associations.ListForTask`): the `(connection scope,
  repository id, number)` tuple must be among the task's associations. Resolve the
  repository with `repositories.ResolveAttached` from the verified context. Never
  trust owner or name from the body. Without this, anything with the action can
  merge any PR the token can reach.
- **Check the workspace enable switch** (the default dispatch branch already
  does; do not bypass it).
- **Require an expected head SHA** from the caller (the SHA the user or agent
  last saw) and send it as `head_commit_id`. A mismatch is a 409, so a stale click
  cannot merge unseen commits.
- **Use a distinct key per write**, so the Kandev action audit shows what
  happened.
- **Return domain failures as explicit statuses** with fixed messages (409
  conflict or out of date, 403 forbidden or protected, 422 invalid), never as Go
  errors.
- **Attribution.** The Forgejo identity is always the operator's shared token,
  not the Kandev user. Record the verified `ActorID` in the log. Decision 3 in the
  README covers adding a "via Kandev (user)" footer.

### 6. UI (`ui/src/source-control.ts`, `detail.ts`, new components)

- `ReviewPanel` becomes a stateful component (`host.React.useState`, the same
  pattern as `watches-panel.ts`): on open it calls `change_requests.details`, keeps
  the result and a busy action id in state, and wires `onRefresh`, `onRetry` and
  `onAction`.
- **Merge** in `headerActions`: mirror Kandev's own `pr-merge-button.tsx`
  behaviour. The primary style comes from the repository's allowed styles
  (squash, then merge, then rebase), the rest in a dropdown. Offer only styles the
  repo reports. If the style list fails to load, still render the button and let
  the backend choose, so a failed lookup never locks the user out. Disable while
  merging; after success hide until refreshed state arrives; toast on success and
  failure; show "as <account>" using the connection's login.
- **Reason before button.** When a merge is not possible show why, using the
  mapping from `pr-mergeability-notice.tsx`: conflicts, checks failing, N
  approvals required, protected branch. Stay quiet while mergeability is unknown
  instead of flashing "not mergeable" (the `mergeable` flag is computed
  asynchronously and can be stale right after a push).
- **Review submit**: a dialog with the event (approve, request changes, comment),
  a body, and optional inline comments, since the host component has none. Use
  `host.ui.Dialog`.
- **Comment**: a `comment`-placement action.
- **After any mutation** call the existing `refreshAfterMutation`
  (`source-control.ts`, around line 283); there is no event push to refresh the
  panel.
- Tests in `ui/test/`: action wiring, busy and disabled states, error toasts,
  that no write button renders without a linked PR.

### 7. Agent tools (`manifest.yaml`, `internal/plugin/agenttools.go`)

Extend the existing tools rather than adding one; a third tool is a permanent
per-session tax. `docs/agent-tools.md` records that folding `ci_log` into `ci`
cut the cost from 419 to 327 tokens for the same reason.

- Add ops to `pr`: `merge`, `review`, `request_review`, `update`, `comment`,
  `label`, `assign`, and fold read detail (mergeability, files, commits, diff,
  checks, protection) into `get` through one `include` argument.
- **Measure before designing.** Marshal the `{name, description, inputSchema}`
  of both tools exactly as `manifest_test.go` does and see how much headroom is
  left under the 1,536-byte total; the current definitions already use roughly
  1.2-1.3 KB. Compress to one enum and a few shared optional arguments, keep
  descriptions terse, and put long semantics in `docs/agent-tools.md`. If it
  cannot fit, raising the ceiling or adding a third tool is a deliberate decision
  (README decision 4) with the test comment updated, not a quiet edit.
- **Authorization:** same linked-PR resolution and expected-head-SHA rule as the
  actions. `ready` already refuses when more than one PR is linked; keep that
  behaviour for every write op.
- **Agent merge is off by default.** Add a per-workspace switch in Host state
  (next to `enabledStateKey` in `runtime.go`, around line 41), default off for
  `merge`; review and comment may stay on. Surface it in the connection panel
  through a small action. Mind the host state rules in `AGENTS.md` (key
  vocabulary, int64 returning as float64). The switch is also the place to
  enforce "merge only when checks pass" server-side, independent of what an agent
  asks for.
- **Annotations:** the tools declare `destructive_hint:false` and
  `idempotent_hint:true`. Merge is destructive and review submit is not
  idempotent. Check in the Kandev MCP code whether the hints change any approval
  UX before deciding; do not leave them wrong by accident.
- **Output:** one line per fact, a fixed small budget for diff and file text, and
  explicit truncation markers. The tool result ceiling is 1 MiB for text plus
  structured content together.

### 8. Docs, tokens, changelog

- **Token scopes.** Today: `read:repository`, `write:repository` (open PRs only),
  `read:user`, `read:issue`. Merge, review, request reviewers and update branch
  need `write:repository`. Comments, labels and assignees likely need
  `write:issue`, which is new. Verify each on every container, because scope
  categories differ. A missing write scope shows up as 403; map it to
  "forbidden", not "bad token". Update the README token table, the
  `config_schema.api_token.description` in `manifest.yaml` and `docs/security.md`.
- Update `docs/agent-tools.md` (ops, token cost table, context cost),
  `docs/configuration.md` (action table and bodies), `docs/security.md` (write
  authority, confirmation model, attribution), `docs/development.md` (live
  matrix, CI users), `docs/testing-on-kandev.md` (manual checks for the host seam:
  buttons, toasts, refresh, panel render), README feature table, and the
  `AGENTS.md` host-behaviour notes. `CHANGELOG.md` under `## Unreleased`.

## Verify against live instances first

Record each outcome in `docs/development.md`. Assumed means unverified.

1. **Merge** `POST /repos/{o}/{r}/pulls/{n}/merge`: body field spelling (`Do`,
   `MergeTitleField`, `MergeMessageField`, `delete_branch_after_merge`,
   `force_merge`, `head_commit_id`, `merge_when_checks_succeed`); which styles
   exist on each version (`fast-forward-only` is the likeliest missing on the
   floor); 200 vs 204; 405 not mergeable; 409 conflict or head mismatch; 403
   protection or permission; 422; the result of merging twice.
2. **Per-repo styles** on `GET /repos/{o}/{r}`: `allow_merge_commits`,
   `allow_rebase`, `allow_rebase_explicit`, `allow_squash_merge`,
   `allow_fast_forward_only_merge`, `default_merge_style`,
   `default_delete_branch_after_merge`. Present on the 1.20 floor? What does a
   disabled style return?
3. **Scheduled merge** (`merge_when_checks_succeed`): the response, how the PR
   reports it, cancelling with `DELETE .../merge`, and behaviour with no CI
   configured.
4. **`mergeable` staleness**: measure how long after creating a PR or pushing the
   flag converges, and decide the "unknown" handling.
5. **Review submit** `POST .../pulls/{n}/reviews`: accepted event spelling
   (`APPROVED` vs `APPROVE`, `REQUEST_CHANGES`, `COMMENT`); omitting `event`
   leaving a pending review; self-approval rejection; `commit_id`; inline
   `new_position` semantics (a diff position, not a file line); and the state
   strings `GET .../reviews` returns (our `summarizeReviews` ignores everything
   but `APPROVED` and `REQUEST_CHANGES`; the host maps `COMMENTED`).
6. **Request reviewers** `POST/DELETE .../requested_reviewers`: 422 for a user
   without access or the author; differences between versions; where the
   requested list is read.
7. **Update branch** `POST .../pulls/{n}/update?style=merge|rebase`: availability,
   409 on conflict, the "already up to date" response, required permission.
8. **Read endpoints**: `.../pulls/{n}/files` (paging, limit cap, per-file counts),
   `.../commits`, `.diff` and `.patch` (content type, size), and the
   `additions`/`deletions`/`changed_files` fields on the PR (they feed the panel).
9. **Branch protection visibility** for a non-admin: compare
   `GET .../branch_protections/{name}` (likely 403 or 404) with
   `GET .../branches/{name}` (`protected`, required approvals, status-check
   contexts, `user_can_merge`). Is the second enough for required checks?
10. **Comments, labels, assignees** on PRs: ids vs names for labels by version;
    assignees through the issue or pull endpoint.
11. **Delete branch after merge**: same-repo only? Protected branches?
12. **Token scope matrix**: for each new endpoint, a token with only
    `read:repository` and one with `write:repository` but no `write:issue`.
13. **Error body shape** for 403, 405, 409, 422, and that none echoes the token.
14. **Idempotency**: double merge, double approve, double reviewer request,
    double comment.

### Live test matrix

Extend the `live` job (shared groundwork gives it the `reviewer` and `readonly`
users). Use a fresh head branch per run, as `TestLivePullRequestEditRoundTrips`
does, because both hosts reject a second open PR for the same head and base.

| Case | Setup | Expect (verify) |
| --- | --- | --- |
| merge: merge, squash, rebase, ff-only | enable the style via `PATCH /repos/{o}/{r}` | success |
| merge, style disabled | disable it | 405 or 422 |
| merge, conflict | two branches editing one line | 409 or 405 |
| merge, stale head | old `head_commit_id` | 409 |
| merge, delete branch | same-repo head | branch gone |
| merge, protected base, 1 approval required | `POST /branch_protections` | blocked until `reviewer` approves |
| merge when checks succeed | post a pending status, then flip it to success | scheduled, then merged |
| merge as `readonly` | | 403 |
| review: approve, request changes, comment | as `reviewer` | state strings as returned |
| review by the PR author | | rejection code |
| review with an inline comment | diff position | comment present |
| request reviewers | `reviewer`; `readonly` | 2xx; 422 |
| update branch | base moves ahead | success, then up to date |
| files, commits, diff | | paging and sizes |
| `mergeable` staleness | create a PR, poll | convergence time |
| branch protection read | as `reviewer`, as admin | which fields are visible |

Also unit-test every status mapping with `apiServer` fakes so `go test ./...`
stays hermetic.

## Acceptance criteria

- The review panel renders for a linked PR on a real Kandev, with the full detail
  model, and a test fails if the mapper drifts from it.
- A user can merge (style chosen from what the repo allows), approve, request
  changes, comment and request reviewers from the panel, and sees a clear reason
  when a merge is blocked.
- An agent can do the same through `pr`, only on PRs linked to its task, only with
  the current head SHA, and cannot merge unless the workspace switch allows it.
- Failures show fixed, token-safe messages; a 403 never says "bad token" unless it
  is one.
- No new capability, so no re-approval; the manifest and its tests are updated
  deliberately; token-scope docs are accurate.
- `make lint && make test` pass, the live matrix passes on all four containers,
  and the manual host checks in `testing-on-kandev.md` are done.

## Risks and open questions

- **Blast radius.** One shared operator token and an agent that can merge to the
  default branch. The linked-PR rule, the head-SHA precondition and the
  default-off switch are the controls; do not weaken them for convenience.
- **Stale data.** `mergeable` and check state lag; the head SHA precondition makes
  the merge fail safe.
- **Floor versions.** `fast-forward-only`, and some fields, may be missing on
  Gitea 1.20 and Forgejo 7. Offer only what the repo reports.
- **403 is overloaded** (scope, permission, protection). Classification needs the
  bounded body parse; without it the messages mislead.
- **Composite reads vs the 15-second action timeout.** Bound them or split them.
- **Admin-only protection reads.** A non-admin gets 403 or 404 from the full
  protection endpoint; the UI must say "unavailable to this token", not error.
- **Decisions 1 to 4 in the README** need an owner before steps 5 to 7.
