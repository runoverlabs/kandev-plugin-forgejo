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
make lint                  # gofmt, go vet, tsc
make test                  # Go tests + UI tests (builds the bundle first)
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

Neutral packages declare ports and own decisions; the provider package
implements them and owns every Forgejo detail.

| Package | Owns |
| --- | --- |
| `internal/sourcecontrol/` | The source-control recipe: ports, no Forgejo |
| `internal/watches/` | Issue-watch model, dedup ledger, poll loop: ports, no Forgejo |
| `internal/forgejo/` | Every URL, pagination token, auth detail, error mapping |
| `internal/plugin/` | Wires adapters to the SDK, owns actions and agent tools |
| `ui/src/` | The browser half |

**Nothing above the adapter forms a Forgejo URL. Nothing in the adapter decides
what a watch means.** If you find yourself importing `internal/forgejo` into a
neutral package, the design is wrong — add a port instead.

This is what makes the poller testable against a scripted `IssueSource` with no
HTTP, and the REST client testable against `httptest` with no Kandev host.

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
- `SetHost` is the only lifecycle hook, called once from a background goroutine
  after the broker dial. It is where background work starts.

## Forgejo and Gitea

One plugin serves both by restricting itself to the REST v1 surface they share.
Flavor detection is **for display only — no behaviour branches on it.**

Where the hosts diverge, resolve through a chain and take the first surface that
answers rather than assuming a shape (`ci` walks `/actions/runs` →
`/actions/tasks` → combined commit status). At the supported floor of either
host there is no Actions API at all.

Verify a provider claim against a real instance before encoding it. Two found
that way and easy to get wrong:

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
