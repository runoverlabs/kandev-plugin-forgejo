# Changelog

## 0.3.1

Fixes three defects in 0.3.0. No capability changes, so Kandev does not ask for
re-approval.

- **Pausing or resuming an issue watch no longer erases it.** The pause toggle
  sends only the new enabled state, but the update path overwrote the agent and
  executor profiles, prompt, repository, base branch, query, labels and the
  start-agent setting with empty values. An update now changes only the fields it
  carries, and a field can still be cleared by sending it empty on purpose.
  **A watch that was paused or resumed on 0.3.0 has already lost those settings
  and they cannot be recovered; open it and set them again.**
- **The review panel could not render.** The adapter handed Kandev's detail view
  a different shape from the one it renders, and the view throws on the first
  field it expects and does not find (the author, the reviewer list). The adapter
  now supplies every field. What the review snapshot does not carry is shown as
  a neutral placeholder rather than invented: the author reads "unknown", the
  branches are blank, the line counts are 0, and the reviewer and comment lists
  are empty. Loading the real values needs a detail request the panel does not
  make yet. State, draft, checks, review state and pending reviewers are real.
- **Issue watches could miss issues on older instances.** The watch walks a
  repository's issues a page at a time and stopped at the first page that looked
  short. A server that ignores the issue-only filter (older Gitea) mixes pull
  requests into the page, so a full page of mostly pull requests looked short and
  every issue after it was dropped. The walk now judges a page by the rows the
  server sent.

## 0.3.0

Issue watches. A watch polls Forgejo repositories on an interval and files each
new matching issue as a Kanban task.

- Kandev's native issue watches are compiled in per provider with no extension
  point, so this is a plugin-owned poll loop: the plugin process runs its own
  timer and creates tasks through the Host's `api_write:tasks` RPC. Everything
  stays in one repo and one release cycle.
- The manifest gains `api_write: ["tasks"]` and `api_read` on `workspaces`,
  `workflows`, `agent_profiles` and `executor_profiles`. Task creation is the
  plugin's only entity mutation; the reads let the configuration form offer real
  columns and profiles by name instead of asking for raw ids.
- The watch model reproduces the fourteen columns Kandev's six native providers
  share, plus `max_inflight_tasks`, `repository_id` and `base_branch` — which
  every native provider except GitHub carries, and whose absence there is an
  accident of history rather than a design decision.
- Deduplication is a ledger in plugin state keyed by repository and issue
  number, written after the task is created. Reserving the key first would
  avoid a duplicate on a crash between create and record, but would permanently
  suppress the issue if creation then failed; a duplicate card is visible and
  fixable, a silently skipped issue is neither.
- Duplicate handling is a choice rather than an assumption. The default matches
  the native providers, whose join tables are `UNIQUE(issue_watch_id, repo,
  number)` — two watches over the same issue each file a card. "One task per
  issue" scopes the ledger to the workspace instead.
- The open task limit counts this watch's tasks that Kandev has neither
  completed nor archived, via one page-walk rather than a `GetTask` per issue
  ever handled. It throttles rather than capping: finishing a task frees a slot.
- Polls are incremental. A watch passes its previous poll time as `since`,
  rewound by one interval — clock skew between the plugin and the instance is
  real, and re-reading a small overlap costs one deduplicated comparison while
  missing an issue costs a card that never appears.
- Pull requests are excluded. The repository issue endpoint returns both in one
  shape; `type=issues` asks the server to filter, and every result is checked
  again because older Gitea releases ignore the parameter.
- The free-text filter uses `q`, not `keyword` — the per-repository issue
  endpoint silently ignores the latter, so the wrong spelling filters nothing
  rather than erroring. `q` is served by the host's issue indexer, which ingests
  asynchronously and can be disabled; labels and state are evaluated directly
  and have neither caveat. Verified against live Gitea 1.27.
- `start_agent` defaults off, and a watch that turns it on must name an agent
  profile: an unattended watch should not depend on whichever profile the
  workspace happens to default to. Kandev treats the launch as best-effort, so a
  failure to start does not fail task creation.
- Every watch action is workspace-scoped, and the workspace comes from the
  verified action context rather than the request body. Upstream #3681 fixed a
  leak where native providers returned every workspace's watch configs, exposing
  filters, repository and profile ids, and spawn prompts.
- Both enable switches genuinely stop work: a workspace with the integration
  turned off is skipped by the poller, and a paused watch refuses a manual run.

**Upgrading:** this release changes the plugin's declared capabilities, so
Kandev requires the capability set to be re-approved. Until it is, the existing
repository, review and pull-request features are denied as well — Kandev checks
the approval against a digest of the whole capability list, not per capability.

### Security

- Adds `.github/workflows/security.yml`: secret detection with `gitleaks` over
  the working tree **and the full history**, `govulncheck` for Go, `npm audit`
  for the UI toolchain, and CodeQL with the `security-extended` queries for Go,
  TypeScript and the GitHub Actions workflows. It runs on push, on pull requests, and weekly — a dependency
  becomes vulnerable when an advisory is published, not when someone pushes.
- `make security` runs the same checks locally, minus CodeQL.
- Bumps `google.golang.org/grpc` to v1.83.2 for GO-2026-6443, a server panic
  reachable from `pluginsdk.Serve` via missing authority or Host headers. Found
  by the new pipeline on its first run.
- Bumps `golang.org/x/net` to v0.60.0 for GO-2026-6617, an HTTP/2 server crash
  (HPACK encoder race) reachable from `pluginsdk.Serve`. Published after the
  first run of the new pipeline, which is the case the weekly scan exists for.
- Every workflow now pins Go to the exact patch release 1.26.9 instead of
  "1.26", which resolved to the runner image's cached 1.26.8. GO-2026-6617 is
  also in the standard library's `net/http` (fixed in go1.26.9), which a
  dependency bump cannot reach. The release workflow in particular builds the
  binary operators run. Bump the pin when `govulncheck` reports a newer fix.
- Bumps `vitest` to 4.1.11, clearing the open Dependabot alerts in `vitest`,
  `@vitest/mocker` and `tinypool` (two critical, one of them a remote code
  execution gadget), and refreshes `source-map-js`. `npm audit` is clean. This
  also moves `vite` to 8. Every npm dependency here is a devDependency and the
  published package ships a pre-built bundle with no `node_modules`, so none of
  it reached an operator; `npm audit` stays gated at `high` for that reason.

- CI, the security workflow and the release build now check out the Kandev SDK
  at pinned tags instead of `main`. An unpinned SDK let an upstream dependency
  bump fail every Go job with "updates to go.mod needed" and no change here.
  The standard target is `v0.97.0`, the newest Kandev this plugin is verified
  on, and it is set once per workflow. The `test` job also builds against
  `v0.95.1`, the oldest the manifest claims, so an API newer than
  `min_kandev_version` cannot slip in. The SDK has only grown between the two,
  so one `go.mod` serves both. `go.sum` is tidied against the standard target.

### Documentation

- Splits the README, which had grown to 482 lines, into a functionality-focused
  README plus `docs/`: issue watches, agent tools, configuration and operations,
  security, development and approach, and testing on a real Kandev.
- Adds `docs/testing-on-kandev.md`, the pre-release verification stage neither
  the unit suite nor the live contract suite can cover: every Host RPC is faked
  in one and absent from the other.

## 0.2.1

- Task sessions can clone and push over HTTPS. Kandev asks the owning plugin
  for transient Git credentials through its credential broker, and the plugin
  implemented neither `ResolveGitCredential` nor `GetGitCredentialBinding`, so
  every task on a Forgejo or Gitea repository failed to start with "plugin does
  not implement git credential resolver". The configured token is issued only
  for a single `owner/repo` path on the configured instance, only to requests
  that carry complete workspace, task, session and repository identity, and
  never while the workspace integration is switched off. The binding is a
  digest over the instance URL, token and lease scope, so a rotated token
  revokes the leases issued under the old one without a network call.
- `repositories.inspect` now emits `provider_repository_id`, the field Kandev's
  inspection contract reads for the immutable repository identifier. The
  response only carried `repository_id`, so the host parsed an empty ID and
  rejected every picker selection with "The selected repository could not be
  verified"; task creation from a Forgejo or Gitea repository failed on Kandev
  0.95.1. `repository_id` stays, because the plugin UI reads it.

## 0.2.0

Agent-facing MCP tools. Task agents can now drive Forgejo directly instead of
being handed `curl` recipes in a workflow step prompt.

- Adds two tools on the `kanban-task` surface: `ci` (CI result for a branch or
  commit, optionally with the tail of each failed job's log) and `pr` (get,
  open, or ready the task's pull request).
- Log fetching is an argument on `ci` rather than a tool of its own. A separate
  `ci_log` cost 419 tokens for the pair and answered "what failed and why" in
  two round trips; folding it in costs 327 and answers in one. Logs stay
  opt-in, so the plain read stays cheap, and are bounded to five failed jobs
  under a shared 256 KiB budget. The log text goes only into the result text,
  never into the structured content, because the host counts both against one
  1 MiB ceiling.
- `pr op=open` is idempotent rather than retried. Kandev never retries an agent
  tool, because it cannot know whether the side effect already landed, so open
  returns an existing pull request for the same head instead of failing — and
  checks again if the create itself errors.
- `pr op=open` records the Kandev task association, so a pull request an agent
  opened appears in the review sidebar exactly like one opened from the UI.
- `pr op=ready` clears the `WIP:` title prefix. REST v1 has no draft flag on
  either host, so that prefix is the draft and a title edit is the way out of it.
- `ci` resolves through `/actions/runs`, then `/actions/tasks`, then the
  combined commit status, taking the first that answers. The endpoints differ
  by release far more than the rest of the REST v1 surface does: Gitea 1.20 has
  none of them, Gitea gained `/actions/runs` after 1.24, and Forgejo 13 lists
  runs but serves neither their jobs nor job logs. The commit status is present
  everywhere and is also the only surface that sees CI running outside the
  forge, which on self-hosted Forgejo is common.
- A ref with no CI reports `none`, never `failure`. "Nothing ran" and "something
  broke" are not the same answer to give an agent.
- Job logs are streamed through a tail buffer and requested with a suffix
  `Range`, so a long log is never materialized and a host that ignores `Range`
  still yields the end of the log rather than the start.
- Normalizes one measured host difference: Gitea answers an unknown job id with
  HTTP 500 where Forgejo answers 404 (1.24.7 against 16.0.5). Reporting that as
  a server fault would send an agent chasing an outage instead of a stale id.
  A log that cannot be read marks that job alone and never fails the CI read.
- The tools honor the per-workspace enable switch and make no request to the
  instance while it is off.
- `min_kandev_version` moves to **0.95.0**, where plugin tools are served over
  Kandev's MCP endpoint. An older host ignores an unknown manifest block rather
  than refusing the install, so leaving the floor at 0.88.0 would have installed
  a plugin whose tools silently never appeared. A host on 0.88–0.94 can still
  run release 0.1.2 for the source-control surfaces.
- The two tool definitions cost 327 tokens together, measured with Kandev's own
  estimator; a test holds a ceiling on the set so a third has to be argued for.
  The catalog is built from the installed manifest and the only dynamic input
  is whether the plugin is active, so a plugin cannot withhold a tool from an
  instance too old to serve it — which is one more reason log fetching is an
  argument rather than a tool.

## 0.1.2

- The integrations card now renders its own enable/disable switch. Kandev does
  not supply one for a plugin integration, which is why this card had no toggle
  while native integrations did.
- The switch is honored, not decorative. The choice is stored per workspace and
  the backend enforces it: while off, repository, branch, review and association
  reads return nothing and pull-request create/link/unlink refuse. A workspace
  with no stored choice is enabled.
- Adds the `connection.set_enabled` action, and `connection.get` now reports
  `enabled` alongside connection state.

## 0.1.1

- The workspace integrations panel invoked its actions without a `workspaceId`.
  Every action is `scope: "workspace"`, so Kandev rejected each call at the
  envelope before it reached the plugin process; the panel then showed "not
  configured" and surfaced the raw host error, against a backend that was
  working. It now uses the workspace the host routes to it, falls back to the
  active workspace, and makes no call when neither resolves.
- `connection.get` always reported `connected: false` because it never probed,
  so the panel read as disconnected on every mount even right after a
  successful **Test connection**. It now serves a probe result cached per
  workspace for 60s and probes when that is stale. The cache is bound to the
  configured instance URL and dropped when a probe fails.
- Host and transport errors are no longer shown verbatim. The panel renders one
  actionable sentence and logs the cause to the console.
- Publishes the integration's enabled state per workspace via
  `host.setIntegrationEnabled`, which drives Kandev's enabled badge.
- `registerIntegrationSettings` is behind a capability check, so a host without
  that hook cannot abort `initialize` and lose the source-control registrations.
- Release binaries are built with `-trimpath -ldflags="-s -w"`: the package drops
  from 50.6 MB to 27.8 MB, and binaries no longer embed the build machine's
  filesystem paths. Panic traces keep function names and line numbers.
- Documents the HTTP install/config/action surface for headless setups, and
  states explicitly that one connection is shared by every workspace.

## 0.1.0

Initial release.

- Forgejo/Gitea repository provider: server-side search, paging, branch lists,
  and authenticated URL ownership checks.
- Pull-request creation from a task's verified worktree branch, with draft
  expressed as a portable `WIP:` title marker.
- Task **Link** action accepting a pull-request URL or `owner/repo#number`,
  resolved server-side before it is stored.
- Review snapshots with semantic state, commit-status checks, approval counts,
  and unresolved review comments, plus a workspace-level association map.
- Composer `#` pull-request references with fail-closed submit-time
  authorization.
- Distinguishes Forgejo from Gitea by probing Forgejo's `/api/forgejo/v1`
  namespace, so the label survives Forgejo dropping its `+gitea-` version
  suffix. Display only; no behavior branches on it.
- Requires a token with `read:repository`, `read:user` and `read:issue`, plus
  `write:repository` to open pull requests. Without `read:user` the connection
  test reports "not connected"; without `read:issue` the composer `#` picker
  returns nothing.
- Declares a supported floor of Gitea 1.20 / Forgejo 7.0. The floor is a
  token-scope boundary, not a capability one: Gitea ≤1.18 has no scope system
  (tokens carry full account access), and 1.19 uses an incompatible vocabulary
  whose API-minted tokens cannot write.
- Verified with the full live contract suite against 14 Gitea releases
  (1.14.7 through 1.27.3) and 10 Forgejo releases (7.0.16 through 16.0.5).
  All 24 returned complete data.
