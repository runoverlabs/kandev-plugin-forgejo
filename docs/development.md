# Development

Setting the repo up, the commands, the live contract tests, and the shape of
the code.

## Getting set up

The Kandev SDK is not yet published as a standalone module, so this repo builds
against a **sibling checkout** of the Kandev monorepo:

```text
parent/
├── kandev/                  # github.com/kdlbs/kandev
└── kandev-plugin-forgejo/   # this repo
```

`go.mod` has `replace github.com/kandev/kandev => ../kandev/apps/backend`, and
`package.json` resolves `@kandev/plugin-sdk` from
`../kandev/apps/packages/plugin-sdk`. Both paths change once the SDK ships as a
versioned module.

CI checks the SDK out at **pinned tags**, never at `main`:

| Target | Tag | Used for |
|---|---|---|
| Standard | `v0.97.0` | Local development, the live contract jobs, the security workflow, and the release build — the newest Kandev this plugin is verified on |
| Minimum | `v0.95.1` | A second `test` leg: the oldest Kandev the manifest claims (`min_kandev_version`) |

Build locally against the standard target (`git -C ../kandev checkout v0.97.0`)
so your build matches CI. The minimum leg is the guard that matters: building
only against the newest SDK would let a call to an API added after 0.95 compile
and pass, then fail on a 0.95 host. The SDK has so far only grown, so one
`go.mod` builds against both.

An unpinned SDK breaks the build whenever upstream moves a dependency, which is
how the Go jobs went red without a change here. To move the standard target when
a new Kandev ships, change `KANDEV_REF` at the top of `ci.yml`, `security.yml`
and `release.yml`, run `go mod tidy` against that checkout, and commit the
`go.mod` and `go.sum` it produces. Then run `make lint && make test` against the
minimum tag too before raising `min_kandev_version`.

```sh
npm install
make claude-md     # local-only CLAUDE.md pointing at AGENTS.md (gitignored)
make lint          # gofmt, go vet, tsc
make test          # Go unit tests + UI tests (builds the bundle first)
make verify-package-host   # host-platform tarball + checksum verification
make package       # all five platforms, for a release
```

## Running the live contract tests

The tests in `internal/forgejo/integration_test.go` skip unless an instance is
supplied. Use a **disposable** instance — never a production one:

```sh
podman run -d --name forgejo -p 3000:3000 \
  -e FORGEJO__security__INSTALL_LOCK=true \
  -e FORGEJO__database__DB_TYPE=sqlite3 \
  codeberg.org/forgejo/forgejo:16

# Create a user and a repo with a pull request, then mint the token through
# the admin CLI -- `--scopes all` is the one spelling that works on every
# supported version:
#   <binary> admin user generate-access-token --username kandev \
#     --token-name dev --scopes all --raw
KANDEV_FORGEJO_URL=http://localhost:3000 \
KANDEV_FORGEJO_TOKEN=... \
KANDEV_FORGEJO_OWNER=kandev KANDEV_FORGEJO_REPO=demo \
KANDEV_FORGEJO_HEAD_BRANCH=feature/kandev-create \
go test ./internal/forgejo -run TestLive -v
```

`KANDEV_FORGEJO_EXPECT_FLAVOR=forgejo|gitea` additionally asserts flavor
detection. The create and edit tests branch off a fresh head each run, so they
are safe to re-run against a long-lived instance. `TestLiveActionsSurfaceMatrix`
prints which Actions endpoints the instance serves, which is the table under
"What CI data you get, by version". Run the suite against Forgejo and Gitea
containers before releasing; CI does this across the supported range.

## Layout

| Path | Contents |
| --- | --- |
| `manifest.yaml` | The declarative host contract: actions, provider ownership, reference source, capabilities, config schema. |
| `internal/sourcecontrol/` | The provider-neutral source-control recipe — the Kandev boundary. Adapted from `kandev-plugin-template`. |
| `internal/forgejo/` | Concrete Forgejo/Gitea adapters: REST client, repositories, pull requests, reviews, references, associations, and the Actions/CI resolution chain. |
| `internal/watches/` | The provider-neutral issue-watch half: the watch model, the dedup ledger, and the poll loop. Reaches Forgejo only through the `IssueSource` port. |
| `internal/plugin/` | Wires the adapters into the extension, owns the connection and watch actions, and serves the agent tools. |
| `ui/src/` | The browser half: the recipe registration plus the Forgejo icon, reference parsing, detail adapter, connection panel, and the issue-watch panel. |
| `server/` | The `pluginsdk.Serve` entry point Kandev spawns. |

## Approach

Three decisions explain most of the code, and each one is load-bearing rather
than stylistic.

### A plugin, not a core provider

Kandev's six native connectors (GitHub, GitLab, Jira, Linear, Sentry, Azure
DevOps) are compiled into the host, one table and one poller per provider. This
is not the pattern to copy: those predate the mature plugin contracts, and the
project's own rule is that core changes must be **provider-neutral extension
points** while provider-specific logic belongs in a plugin. The reference for a
non-native forge is `kdlbs/kandev-plugin-bitbucket`, which is a plugin.

The practical consequence is that anything Kandev renders per provider at
compile time has no extension point a plugin can register against. Repositories,
pull requests and reviews do have neutral contracts and therefore appear in
Kandev's own UI; issue watches do not, which is why their entire UI is a panel
this plugin draws itself.

### Ports in the neutral package, adapters in the provider package

`internal/sourcecontrol` and `internal/watches` declare narrow interfaces and
own the decisions; `internal/forgejo` implements them and owns every URL,
pagination token, authentication detail and error mapping.

Nothing above the adapter forms a Forgejo URL, and nothing in the adapter
decides what a watch means or when a pull request counts as ready. That is what
makes the poller testable against a scripted `IssueSource` with no HTTP at all,
and the REST client testable against an `httptest` server with no Kandev host.

### One REST surface, both hosts

Forgejo is a hard fork of Gitea and both serve `/api/v1`. The client restricts
itself to endpoints and fields that exist on both, so one plugin serves either.
Flavor detection exists for display only — **no behaviour branches on it**.

Where the hosts genuinely diverge, the code resolves through a chain and takes
the first surface that answers rather than assuming a shape: `ci` walks
`/actions/runs` → `/actions/tasks` → the combined commit status, because at the
supported floor of either host there is no Actions API at all. The live contract
matrix in CI is what keeps that claim honest.

## Conventions worth matching

- **Comments say why, not what.** The repo is dense with rationale on the
  non-obvious decisions and silent on the obvious ones. A comment that restates
  the line below it is noise; one that records a measurement, a failure mode, or
  a rejected alternative is the reason the next person does not redo the work.
- **Guard tests assert exact sets.** `internal/plugin/manifest_test.go` pins the
  action count, the capability list, and a byte ceiling on the agent-tool
  definitions. These fail on purpose when the manifest changes: the point is
  that widening the plugin's privileges or its permanent context cost has to be
  a deliberate edit to a test that explains why, not a diff nobody reads.
- **Provider error bodies are never forwarded.** Some deployments echo the
  presented token. Adapter errors map onto operator-facing messages; the raw
  error goes to the log.
- **Host state keys stay inside `[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}`.** Long
  components are hashed rather than truncated — see `repoDigest` in
  `internal/watches/store.go`.

## Before releasing

Run the full pipeline locally, then verify on a real Kandev instance — the unit
and live contract suites cover this plugin's halves, but neither exercises the
Host RPCs it depends on. See [Testing on a real Kandev](testing-on-kandev.md).

```sh
make lint
make test
make security
make verify-package-host
```

Agent-facing conventions — the architecture rule, the manifest contract, the
host behaviours that cost real debugging time — are in
[`AGENTS.md`](../AGENTS.md). Keep it current in the same change that
invalidates it.

---

[← Back to the README](../README.md)
