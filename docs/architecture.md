# Architecture

This page is the design the Go backend and the browser bundle follow, the rules
that keep it that way, and the tools that check those rules. The tools catch the
mechanical part. The practices at the end cover what a linter cannot see.

## The guiding design: ports and adapters, by feature

The plugin sits between two systems it does not control: Kandev, which calls it,
and a Forgejo or Gitea instance, which it calls. Every package belongs to
exactly one of five layers. **An import may only point to a lower layer.**

```text
 L4  entry        server/                      pluginsdk.Serve(plugin.NewRuntime())
 L3  transport    internal/plugin              Kandev → plugin: actions, agent tools, events,
                                               automation RPCs; the enable switch; error → status
 L2  adapters     internal/forgejo             plugin → Forgejo: implements the features' ports
                  internal/forgejo/rest        the REST v1 wire client: HTTP, paging, wire types
 L1  features     internal/sourcecontrol       repositories, change requests, associations
                  internal/watches             issue and review watches
                  internal/automations         webhook automation conditions
                  internal/prautomation        auto-fix, auto-merge, lifecycle prompts
 L0  foundation   internal/scm                 shared vocabulary: RepoRef, error kinds, refusal
                                               reasons, CI states
                  internal/kandev              Kandev host helpers: the late-bound host, the
                                               workspace walk, action responses
```

Two test-support packages sit beside the code they fake and may only be
imported from `_test.go` files: `internal/kandev/kandevtest` (an in-memory
host) and `internal/forgejo/rest/resttest` (a scripted instance).

| Layer | May import | Must not import |
| --- | --- | --- |
| foundation | the standard library; `kandev` also the SDK | anything else in `internal/` |
| features | foundation, the SDK, and `sourcecontrol` (the base feature) | another feature, any adapter, `plugin` |
| `forgejo/rest` | `scm`, the standard library | features, `kandev`, the SDK, `forgejo`, `plugin` |
| `forgejo` | `forgejo/rest`, foundation, features, the SDK | `plugin` |
| `plugin` | foundation, features, the SDK. Only `wire.go` imports `forgejo` | `forgejo/rest` |

Three consequences are the point of the layering:

- **Features decide; adapters translate.** A feature owns what a watch means,
  when a pull request is ready to merge, and which feedback is trusted. It
  reaches the instance only through interfaces it declares itself (`IssueSource`,
  `Observer`, `ChangeRequestActions`, …). The poller is therefore testable
  against a scripted source with no HTTP at all.
- **Only the adapter knows Forgejo.** Every URL, pagination quirk, wire field,
  auth detail and provider error lives under `internal/forgejo`. The wire client
  (`forgejo/rest`) knows nothing about Kandev. The port implementations
  (`forgejo`) map wire types onto feature types.
- **Transport knows Kandev's shapes, nothing about Forgejo.** `plugin` turns an
  SDK request into a feature call and the result into an SDK response. Error
  kinds come from `scm`, so mapping a refusal onto a status needs no provider
  types. The composition root (`plugin/wire.go`) is the one file that names
  concrete adapters.

The browser bundle follows the same rule at a smaller scale:

```text
 bundle.ts                        entry: registers everything with the host
 source-control.ts,               components: React elements through host.jsx,
 repository-provider.ts,          each a hook or two for state and effects plus
 *-panel.ts, review-dialog.ts,    plain render helpers
 watch-form.ts, automation.ts,
 forgejo-icon.ts, workspace.ts
 *-model.ts, detail.ts,           pure models: parsing, drafts, formatting, view
 references.ts, review-store.ts   models. No React, no host calls
 host-contract.ts                 type-level contract with the host
```

Models never import components. Nothing imports `bundle.ts`. A panel is split
into hooks and render helpers called directly, not into child components: the
tests render one component against a stub host and walk the element tree it
returns, which a child component would hide.

## What enforces it

| Check | Tool | Where |
| --- | --- | --- |
| Layer rules above (Go) | `internal/archtest`: a test that parses every import and fails on an edge the layer table does not list | `make test`, CI |
| Module rules (UI): no cycles, models import no component, nothing imports `bundle.ts`, no orphans | `dependency-cruiser` (`.dependency-cruiser.cjs`) | `make lint-ui`, CI |
| Cyclomatic complexity ≤ 15 per function | `gocyclo` via golangci-lint (`.golangci.yml`) | `make lint-go`, CI |
| Cognitive complexity ≤ 20 per function | `gocognit` via golangci-lint | `make lint-go`, CI |
| Nesting ≤ 5, ≤ 50 statements per function | `nestif`, `funlen` via golangci-lint. Statements, not lines: the comments carry the reasoning | `make lint-go`, CI |
| Copy-paste of 150 tokens or more | `dupl` via golangci-lint | `make lint-go`, CI |
| Bugs and API misuse | `staticcheck`, `govet`, `errorlint`, `errcheck`, `unparam`, `unused`, `ineffassign`, `unconvert` | `make lint-go`, CI |
| Formatting and import grouping | `gofmt`, `goimports` with the module as the local prefix | `make lint-go`, CI |
| UI complexity ≤ 12, ≤ 100 lines per function, ≤ 400 lines per file, nesting ≤ 4, ≤ 4 parameters | ESLint with `typescript-eslint` (`eslint.config.mjs`) | `make lint-ui`, CI |
| Dead code across packages | `deadcode` (reachability from `server/`) | `make deadcode`, before a release |

Tests are exempt from the length and complexity rules: a table-driven test is
long by design.

The thresholds are ceilings, not targets. A function at the limit is a warning
to split before the next feature lands in it.

`make metrics` prints the worst offenders without failing (`gocyclo`,
`gocognit`, and the ESLint complexity report), so a pull request can show what
it moved. To see the package graph itself, `goda graph ./...:noroot` (from
`github.com/loov/goda`) renders it for Graphviz.

## Practices a linter cannot check

### Structure

- **Put a decision in the feature, a quirk in the adapter.** If an `if` exists
  because Forgejo 7 answers differently from Forgejo 15, it belongs in
  `internal/forgejo`. If it exists because a fork author's comment must not
  start an agent, it belongs in the feature. A change that needs both is two
  changes: a port method, then its use.
- **Declare an interface where it is consumed, not where it is implemented.**
  Keep it to what that consumer calls. A port with one implementation and one
  caller is normal here: it is the seam the tests use.
- **A new shared helper goes in the lowest layer that every caller can
  import, once.** Before writing `firstNonEmpty` again, look in `scm` and
  `kandev`. Two copies of a helper in two packages are a missing foundation
  function.
- **No `util`, `common` or `helpers` packages.** A foundation package is named
  for what it holds (`scm`: source-control vocabulary, `kandev`: talking to
  Kandev).
- **One file, one concern.** Split a file that needs a table of contents. The
  name says what is in it (`autofix.go`, not `engine2.go`).
- **Each handler gets its own dependencies.** A transport handler gets the
  ports it uses as fields. It does not get the whole `Runtime`. A handler that
  needs everything is doing too much.

### Functions

- **Keep the happy path at the left margin.** Return early on refusal. A
  function that reads top to bottom as "check, check, check, act" rarely needs
  splitting.
- **Name the step, not the mechanism.** Extract `mergeGates(obs)` rather than
  `checkStuff2`, so the caller reads as the policy it implements.
- **Make a big `switch` a table** when its cases do the same thing with
  different data. Examples are the reason-to-message maps and the action-key
  routing.
- **Push I/O to the edges.** A function that decides should take the
  observation and return the decision, so a test can call it without fakes.

### Changes

- **A refactor changes no behaviour and no test expectation.** If a test has to
  change, the change was not a refactor: split it out and say why.
- **Tests go through the outermost stable surface.** The plugin tests drive
  `NewRuntime()` with a fake host and an `httptest` instance. That is what lets
  the packages underneath move without the tests moving with them.
- **Do not raise a threshold to make CI green.** Split the function. If a
  function really cannot be split, a `//nolint:<linter> // <reason>` on it is
  the record of that decision.
