# kandev-plugin-forgejo — agent guide

> Conventions and gotchas for AI coding agents working in this repo. Human
> documentation lives in [`README.md`](README.md) and [`docs/`](docs/); this
> file is only what an agent needs that those do not say.

## What this is

A Kandev runtime plugin connecting a self-hosted Forgejo or Gitea instance. Two
halves shipped as one tarball:

- a **Go binary** Kandev spawns as a gRPC subprocess (`server/`, `internal/`)
- a **browser bundle** Kandev loads into its own UI (`ui/`)

`manifest.yaml` is the contract between them and the host. It is not
configuration — read "The manifest is a contract" below before touching it.

## Setup: the sibling checkout

The Kandev SDK is not a published module. This repo **only builds next to a
Kandev monorepo checkout**:

```text
parent/
├── kandev/                  # github.com/kdlbs/kandev
└── kandev-plugin-forgejo/   # this repo
```

`go.mod` has `replace github.com/kandev/kandev => ../kandev/apps/backend`, and
`package.json` resolves `@kandev/plugin-sdk` from
`../kandev/apps/packages/plugin-sdk`.

If a build fails with an unresolvable `github.com/kandev/kandev`, the sibling
checkout is missing — do not "fix" it by editing `go.mod`.

**The sibling checkout is also the authority on host behaviour.** When you need
to know what a Host RPC does, what a capability gates, or what a proto field
means, read it in `../kandev/apps/backend/` rather than inferring from the SDK's
type signatures. Several important behaviours are only visible there — see
"Host behaviour worth knowing".

## Commands

```sh
npm install                # once, after cloning
make claude-md             # restores the local-only CLAUDE.md pointer
make lint                  # gofmt, vet, tsc, golangci-lint, ESLint, dependency-cruiser
make test                  # Go tests (layer rules included) + UI tests (builds the bundle first)
make metrics               # the most complex functions, without failing
make security              # gitleaks, govulncheck, npm audit
make verify-package-host   # host-platform tarball + checksums
make package               # all five platforms, for a release
```

Run `make lint && make test` before claiming a change is done. Run
`make security` when you touch dependencies.

`go test ./...` is hermetic: the live contract tests in
`internal/forgejo/integration_test.go` skip unless `KANDEV_FORGEJO_URL` and
`KANDEV_FORGEJO_TOKEN` are set. See [docs/development.md](docs/development.md)
for running them against a disposable container.

## Architecture rule

Ports and adapters, by feature, in five layers; an import only points down.
[docs/architecture.md](docs/architecture.md) has the layer table, the
thresholds, and the practices to follow. `internal/archtest` fails the tests on
an import that crosses a layer, and on a new package missing from its table.

| Layer | Package | Owns |
| --- | --- | --- |
| foundation | `internal/scm/` | `RepoRef`, the error kinds and refusal reasons, CI states: standard library only |
| foundation | `internal/kandev/` | The late-bound host, the plugin config, the workspace walk, JSON action responses |
| feature | `internal/sourcecontrol/` | The source-control recipe and the task association store: ports, no Forgejo |
| feature | `internal/watches/` | Issue and review watch model, dedup ledger, poll loop, fork placement, cleanup: ports, no Forgejo |
| feature | `internal/automations/` | Automation condition settings, the neutral webhook event, the filters, the `{{data.*}}` payload: no Forgejo |
| feature | `internal/prautomation/` | Pull request automation: switches, settings, the per pull request record, feedback and trust, auto-fix rounds, merge gates, lifecycle prompts, the loop: ports, no Forgejo |
| adapter | `internal/forgejo/rest/` | The REST v1 wire client: every URL, pagination token, auth detail, wire type and write classification. Knows nothing of Kandev |
| adapter | `internal/forgejo/` | The port implementations: provider quirks, webhook header and payload shape, mapping wire types onto the features' |
| transport | `internal/plugin/` | One handler type per RPC surface, each holding only the ports it uses; `wire.go` is the only file that names an adapter |
| — | `ui/src/` | The browser half: components over pure models, checked by dependency-cruiser |

**Nothing above the adapter forms a Forgejo URL. Nothing in the adapter decides
what a watch means.** If you find yourself importing `internal/forgejo` into a
neutral package or into `internal/plugin` outside `wire.go`, the design is
wrong — add a port instead.

This is what makes the poller testable against a scripted `IssueSource` with no
HTTP, and the REST client testable against `resttest` with no Kandev host.

**The lint gate is a ceiling, not a target.** `.golangci.yml` and
`eslint.config.mjs` cap complexity, length and nesting. When a change hits a
limit, split the function into named steps; do not raise the limit or add a
`nolint` without a reason on the line.

## The manifest is a contract

`manifest.yaml` declares actions, capabilities, agent tools and config schema.
Changing it has consequences beyond this repo:

- **Every routed action must be declared**, or Kandev rejects the call before
  the handler runs.
- **Changing the capability set invalidates the operator's existing approval —
  for every capability, not just the new one.** Kandev stores an approval
  against a digest of the whole capability list and re-checks it per call. After
  an upgrade that changes capabilities, *all* gated calls are denied until an
  operator re-approves. Any such change needs a CHANGELOG note saying so.
- **Every agent tool is injected into every matching agent session, forever.**
  A new tool is a permanent context cost paid by every user.

`internal/plugin/manifest_test.go` asserts the action count, the exact
capability set, and a byte ceiling on the agent-tool definitions. **These tests
fail on purpose when the manifest changes.** Update them deliberately, with a
comment explaining the new entry — do not adjust a number to make CI green.

Least privilege is the standing rule: never add `agent_invoke` or `auth`.
`auth` is the highest-privilege capability a manifest can hold.

## Conventions

### Comments say why, not what

This repo is dense with rationale on non-obvious decisions and silent on obvious
ones. A comment restating the line below it is noise. A comment recording a
measurement, a failure mode, a rejected alternative, or a provider quirk is why
the next person does not redo the work.

State the invariant, not the argument for it. No review-round narration, no
bug history — that belongs in the PR body.

### The token is the thing to protect

The operator's Forgejo token is in memory in the plugin process and in Kandev's
vault. It must never reach a response, a log, a cursor, a repository
descriptor, or an error message.

**Provider error bodies are never forwarded** — some deployments echo the
presented token. Map adapter errors onto operator-facing messages
(`safeMessage`, `translateIssueError`) and let the raw error go to the log.

### Workspace scoping

Every action is `scope: "workspace"`. Take the workspace from
`request.Context.WorkspaceID` — the host verified it — and **never from the
request body**. Upstream #3681 was a leak where native providers returned every
workspace's watch configs, exposing filters, repository and profile ids, and
spawn prompts.

### Host state keys

Keys must match `[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}`. Repository names alone can
exceed that, so long components are **hashed, not truncated** (`repoDigest` in
`internal/watches/store.go`). Keys share one workspace scope across features, so
new prefixes must not collide with existing ones.

Host state round-trips through JSON: an `int64` written comes back as
`float64`. Decode accordingly, and make fakes do the same.

### Tests

Every behaviour change needs a test. Go tests live beside the source; UI tests
in `ui/test/`.

Before trusting a new test, **break the code it covers and confirm it fails.**
A test that passes against a deliberately broken implementation is worse than no
test, because it is claimed as coverage.

What the suites cannot reach: every Host RPC is faked, and the live contract
tests talk only to Forgejo. The capability approval flow, the plugin's process
lifecycle, and the real UI component library are only exercised on a running
Kandev — see [docs/testing-on-kandev.md](docs/testing-on-kandev.md).

## Host behaviour worth knowing

Verified in the sibling checkout; each cost real debugging time.

- `Tasks().Update` **rejects a workflow step change**. Use `Tasks().Move` to
  transition a task between columns.
- `CreateTaskInput` carries more than the obvious fields: `Launch`
  (agent/executor profile, prompt, plan mode), `Repositories` (repository id,
  base branch), `Metadata`, `Priority`.
- Kandev stamps `source = "plugin:<id>"` on rows a plugin creates and **the
  plugin cannot set it**. Tasks a watch files are permanently attributable.
- `StartAgent` is best-effort: a launch failure does not fail task creation, so
  a silent non-launch is possible.
- There is **no `SubscribeEvent` RPC**. Declare `events:` in the manifest and
  implement `OnEvent`; the host pushes.
- `EmitEvent` publishes to bus subject `plugin.<id>.<name>`, which **nothing
  currently consumes**. It is not a path into Kandev's automations — those are
  fed by webhook receipts via `automation_conditions`.
- **Archiving a task from a plugin goes through one exact command.** Only
  `HostTaskCommands(host).Archive` archives; it needs the `host.v2.write:tasks`
  approval, the capability context's `ApprovalRevision` and `ManifestDigest`, the
  task's current `ResourceVersion` and an idempotency key. On Kandev 0.97.0 that
  approval is declared with `api_write: tasks` and cannot be approved
  separately (a partial approval returns 400). `PluginOwnedTaskTrees().Delete` is
  hard-denied and the task reader has no delete: a plugin **cannot delete a
  task**. `Tasks().Update` can set the state to `COMPLETED`, the fallback, which
  does **not** set `completed_at`: check the state too.
- **The host masks every Go error** from an action as `plugin action unavailable`
  (503). An operator-actionable failure has to be returned as a response with its
  own status and message, as the pull request and watch handlers do.
- A workflow step's `OnEnterActionTypes` is visible to plugins. `StartAgent:
  false` does **not** stop an agent when the destination step has
  `auto_start_agent`, so anything that must not start an agent has to check the
  step, not just the flag.
- A plugin cannot set the host's top-level `fork_pr_requires_manual_start`
  marker; plugin task metadata is namespaced under `plugin:<id>`.
- Review watches use the instance's own `review_requested=true` search, which
  returns neither `draft` nor `requested_reviewers`; per-pull-request detail is
  needed for both. They are stored under `rwatch.`, not `watch.`, so an older
  binary does not read them as issue watches.
- **Automation conditions** (`automation_conditions` in the manifest) need no
  capability. The host calls `DescribeAutomationCondition` with the saved
  settings when a trigger is saved, when its binding is configured, and **twice
  more for every accepted delivery** before running it; a changed
  `connection_id`/`connection_revision` or `available: false` cancels the
  delivery, an RPC error makes the worker retry. So a transient provider failure
  in `Describe` must be an error, not `available: false`. The binding revision
  also hashes the plugin version and install time and the trigger's
  `updated_at` and settings: **every plugin upgrade, and every edit of a
  condition, breaks its webhook** until the user reconfigures it and pastes the
  new secret into Forgejo.
- The automation editor renders a boolean setting as on only when it is `true`,
  and a field schema cannot carry `default` (the host refuses the manifest);
  defaults go in the condition's `default_config`, which is copied once into a
  new trigger. Read an absent boolean as false.
- **`Messages().Send` (`api_write: messages`) is the only way to prompt an
  existing task's agent.** An empty session id targets the primary session
  (`NotFound` when there is none); a RUNNING or STARTING session queues the
  text; a never-launched one (CREATED, or WAITING_FOR_INPUT with no execution)
  is **started** with it; an idle or completed one is prompted. Its idempotency
  key is derived from task, session, source and **text**, so an identical text
  is a replay: put an id in every prompt. It carries no metadata and cannot
  replace a queued message. `Sessions().List` (`api_read: sessions`) gives
  states but not which session is primary.
- **Event delivery is best-effort.** `capabilities.events` patterns are
  subscribed and pushed to `OnEvent` from an in-memory queue that drops on
  overflow and does not survive a restart, so an event is a hint and polling
  stays the source of truth. `events` is part of the capability digest.
  `turn.completed` carries `task_id`, `session_id`, `started_at`;
  `task_session.state_changed` carries `task_id`, `session_id`, `new_state`.
- Kandev's CI auto-fix, auto-merge and pull request prompts are GitHub-only in
  the host through v0.98.0 (the outcome tool accepts `github` and `gitlab`); the
  plugin runs its own (`internal/prautomation`).
- **Saving the plugin config restarts the plugin process**
  (`restartForConfigChange`), so the config cannot change under a running
  process. `kandev.Config` reads it once per process; a test that changes the
  config models the restart with a fresh `Connection` or `Runtime`.
- `SetHost` is the only lifecycle hook, called once from a background goroutine
  after the broker dial. It is where background work starts.

## Forgejo and Gitea

The floor is **Forgejo 7 and Gitea 1.27** (Gitea supports only its newest
release; 0.4 is the last plugin line for Gitea 1.20 to 1.26). CI tests every
Forgejo LTS from the floor up (7, 11, 15), the current Forgejo, and the current
Gitea. README.md's "Supported versions" table, the CI matrix and this paragraph
move together; raising a floor is a breaking change with a CHANGELOG entry that
names the last plugin release for the hosts it drops.

**Recent Forgejo comes first.** Every feature but one uses only the REST v1
surface both hosts share. The exception is the `ci_result` automation condition:
it needs Forgejo Actions run events (Forgejo 12+), so `Describe` reports it
unavailable on Gitea and older Forgejo (`forgejo.SupportsActionRunWebhooks`).
That is the only behaviour that branches on flavor. A new feature has one minimum release per host,
recorded in the README table. Below it, report the feature unavailable with a
reason; do not build a reduced mode. Where a feature can be built on Forgejo but
not on Gitea, ship it Forgejo-only rather than holding it back.

Where the hosts diverge, resolve through a chain and take the first surface that
answers rather than assuming a shape (`ci` walks `/actions/runs` →
`/actions/tasks` → combined commit status).

Verify a provider claim against a real instance before encoding it. Some found
that way and easy to get wrong:

- **Do not use `merge_when_checks_succeed` for unattended merges.** On Forgejo
  it never fires when checks were already green or the repository has no CI,
  and on every host it merges a head pushed after scheduling. A plain merge
  with `head_commit_id` is refused (409) when the head moved.
- **An issue's conversation comments are not paged** (Forgejo 7 to 17, Gitea
  1.27): `page` and `limit` are ignored and every request returns them all, so
  walking pages repeats the list. Reviews are paged. Both send
  `X-Total-Count`, which `pageNewest` uses to keep the newest entries.
- **The combined commit status pages** (30 contexts by default, 50 at most), and
  its `total_count` and `state` describe the page alone: `X-Total-Count` is the
  real count. A failing check can sit on page 2.
- A review comment's `resolver` is set only on the comment the conversation
  was resolved from; the web UI uses the first. Replies are not marked.
- A non-admin token gets 403 from `collaborators/{user}/permission` for anyone
  but itself. A review's `official` flag is the trust signal every token sees.
- The per-repository issues endpoint takes **`q`**, not `keyword` — and
  *silently ignores* `keyword`, so the wrong spelling filters nothing instead of
  erroring.
- `q` is served by the host's issue indexer, which ingests **asynchronously** and
  can be disabled. A just-created issue may not match for seconds. Labels and
  state are evaluated directly and have neither caveat.

## Commits and releases

Conventional Commits (`type: description`) — `feat`, `fix`, `docs`, `refactor`,
`chore`, `ci`, `build`, `test`. Match the existing log.

Releases are gated: `.github/workflows/release.yml` fails unless the `v*` tag
matches `manifest.yaml`'s `version`. Installing a version that already exists
returns **409**, so a release candidate needs its own version (`0.3.0-rc.1`),
never a rebuild of the target version.

Before a release, work through
[docs/testing-on-kandev.md](docs/testing-on-kandev.md). Automated suites do not
cover the host seam.

## Documentation

Keep the split: the README carries what the plugin does, which versions it
works with, and how to install it. Everything else belongs in `docs/`.

| Change | Update |
| --- | --- |
| A feature's behaviour | Its `docs/` page, and the README's feature table if the one-line summary changed |
| Capabilities, credential handling, the pipeline | `docs/security.md` |
| Setup, commands, layout, design decisions | `docs/development.md` |
| Actions over HTTP, connection scope, the enable switch | `docs/configuration.md` |
| Anything user-visible | `CHANGELOG.md` under `## Unreleased` |

## Maintaining this file

If a change makes a section here wrong — a new package, a changed convention, a
host behaviour that turns out different — **update it in the same change**.
Keep it concise and factual. Do not add aspirational content.
