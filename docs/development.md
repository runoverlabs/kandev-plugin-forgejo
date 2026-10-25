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
| Minimum | `v0.95.1` | A second `test` leg: the oldest Kandev the manifest claims (`min_kandev_version`). Built with `-tags kandev_min`: that SDK has no exact archive command, so `internal/watches/archive_min.go` replaces `archive_exact.go` and cleanup only completes tasks |

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
make lint          # gofmt, go vet, tsc, golangci-lint (both SDK tags), ESLint, dependency-cruiser
make test          # Go unit tests (layer rules included) + UI tests (builds the bundle first)
make metrics       # the most complex functions, without failing
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

### Webhook deliveries

`TestLiveWebhookDeliveries` (`internal/forgejo/webhooks_live_test.go`) creates
its own repository, adds a webhook pointing at a listener in the test process,
causes every event the automation conditions use, and runs each delivery through
the verifier and the decoder. The instance must be able to reach the listener:
run the container on the host network (`--network host`, which also changes the
port to the instance's own `HTTP_PORT`) and open its allow-list with
`FORGEJO__webhook__ALLOWED_HOST_LIST=loopback` (`GITEA__…` on Gitea). Without
that the hook fails on the instance's side and the test times out naming the
missing deliveries. `KANDEV_FORGEJO_WEBHOOK_HOST` overrides the address the hook
points at, for a container that cannot use the host network.

Optional inputs: `KANDEV_FORGEJO_REVIEWER_TOKEN` adds a pull request from a fork,
and `KANDEV_FORGEJO_RUNNER_LABEL` names the label of an Actions runner registered
on the instance, in which case the test also adds a workflow and waits for run
events (Forgejo 12+). CI registers no runner.

**Re-capturing the fixtures.** `internal/forgejo/testdata/webhooks/<host>/`
holds one real delivery per scenario from each CI container, and
`TestDecodeCapturedWebhooks` runs them on every `go test`. Re-capture when a
release enters the matrix: start the container as above, register a runner
(`<binary> actions generate-runner-token` on the instance, then
`act_runner register --no-interactive --instance <url> --token <t> --labels host:host`
and `act_runner daemon`, with the binary from the `gitea/act_runner` image; host
mode needs no container runtime), and run

```sh
KANDEV_FORGEJO_RUNNER_LABEL=host \
KANDEV_FORGEJO_CAPTURE_DIR=$PWD/internal/forgejo/testdata/webhooks/forgejo-17 \
go test ./internal/forgejo -run TestLiveWebhookDeliveries -v
```

with the usual `KANDEV_FORGEJO_*` variables and the reviewer token. The
directory name is the host and its major release; `TestDecodeCapturedWebhooks`
expects one per CI container.

## Verified against

Provider behaviour the PR-actions work relies on, probed with throwaway
containers (Gitea 1.20.6 and 1.27.3, Forgejo 7.0.16 and 16.0.5) on 2026-10-09.
"Same" means identical on all four. Behaviour not listed here is still assumed.

| Fact | Result |
|---|---|
| Merge-style flags on `GET /repos/{o}/{r}` | `allow_merge_commits`, `allow_rebase`, `allow_rebase_explicit`, `allow_squash_merge`, `default_merge_style`, `default_delete_branch_after_merge` on all. `allow_fast_forward_only_merge` is **absent on Gitea 1.20** |
| `mergeable` on a PR | Present on all |
| `additions`, `deletions`, `changed_files` on a PR | **Absent on Gitea 1.20 and Forgejo 7**; present on Gitea 1.27 and Forgejo 16 |
| `requested_reviewers` / `assignees` on a PR | Null or `[]` depending on version; both decode to empty |
| Review events | `APPROVED` and `APPROVE` both accepted; `REQUEST_CHANGES`, `COMMENT` accepted. `GET .../reviews` returns `APPROVED`, `REQUEST_CHANGES`, `COMMENT` |
| Self-approval | Same: 422 `approve your own pull is not allowed` |
| Request the PR author as reviewer | Same: 422 `poster of pr can't be reviewer` |
| Request / remove reviewer | 201 with a list / 204 |
| Merge, bad `Do` | Same: 422 `[Do]: In` |
| Merge, stale `head_commit_id` | 409 `head out of date` (Gitea 1.20 answered the transient 405 below) |
| Merge while mergeability is computing | **Gitea 1.20: 405 `Please try again later`**, retryable. Newer hosts merged straight away |
| Merge an already-merged PR | 405 on all. Message is `The PR is already merged` on Gitea 1.27 and **empty** on Gitea 1.20 and both Forgejos, so it cannot be told from "not mergeable" by message: re-read the PR |
| `merge_when_checks_succeed` with no CI | 201, then `DELETE .../merge` cancels with 204 |
| Update branch, behind | 200 |
| Update branch, up to date | **500 `HeadBranch of PR is up to date` on Gitea and Forgejo 7**, 200 on Forgejo 16. The client treats it as success |
| Inline review comment (`new_position`) | Accepted, 200 |
| `GET .../branches/{b}` as a non-admin | `protected`, `required_approvals`, `enable_status_check`, `status_check_contexts`, `user_can_merge` on all |
| `GET .../branch_protections/{b}` as a non-admin writer | Same: 403 |
| Labels on `PUT .../issues/{n}/labels` | Numeric ids on all. **Names fail (422) on Gitea 1.20 and Forgejo 7** |
| Assignees via `PATCH .../issues/{n}` | 201; `[]` clears |
| `mergeable` right after a PR is opened | **Unreliable on Gitea.** 1.20 read `false` at t+0 and `true` 500 ms later; 1.27 was seen going true, false, true within three seconds. Forgejo 7 and 16 read `true` immediately. The details action reports a `false` newer than 30 s as unknown |
| Delete branch after merge (`delete_branch_after_merge`) | Head branch gone (404) afterwards on all four, same-repo head |
| Merge twice | 405 `not_mergeable` on all but Gitea 1.27 (`already_merged`); the caller re-reads the PR |
| Merge by a read-only collaborator | **405** `User not allowed to merge PR` on all four, not 403. Classified as forbidden |
| Labels, update branch by a read-only collaborator | 403 (empty body on most; `write permission is required` on Gitea 1.27; HTML on Forgejo 7) |
| Request reviewers as a read-only collaborator | 422 (`Doer can't choose reviewer` / the misleading `poster of pr can't be reviewer`) |
| Review and comment as a read-only collaborator | Both allowed (200 / 201) |
| `GET .../branches/{b}` for a read-only collaborator | 200 on all four |
| Files, commits, `.diff` | 200 on all; files carry per-file counts |

Review watches, probed the same way on 2026-10-09 (`TestLiveReviewRequestedSearch` pins them in CI):

| Fact | Result |
|---|---|
| `GET /repos/issues/search?type=pulls&state=open&review_requested=true` as the requested user | Same on all four: empty before the request, the pull request after it, empty again after an approval |
| The same search as the pull request's author | Same: empty (the author is never asked) |
| Search results' shape | Carry `number`, `title`, `html_url`, `updated_at`, `user.login`, `repository.full_name`. **No `draft`, no `requested_reviewers`**, so a detail request per new pull request is unavoidable |
| `limit=100` on the search | Accepted on all (the plugin asks for 50 and follows the raw page length) |
| Requesting a review bumps the PR's `updated_at` | Yes on all four (about 3 s later in the probe), though the watch does not rely on `since` |
| `draft` on a PR detail | **Absent on Gitea 1.20 and Forgejo 7**; present on Gitea 1.27 and Forgejo 16. The plugin also treats a `WIP:` or `[WIP]` title prefix as a draft, which is the only signal on the two that omit the field |
| `requested_reviewers_teams` on a PR detail | Present on Gitea 1.27 and Forgejo 16 only |
| `requested_reviewers` on a PR detail | Lists the direct request on all four |
| A closed or merged PR | `state` is `closed`; `merged` tells them apart, on all four |

Still unverified for review watches: a team request, AGit-flow head identity
(handled by failing closed), and the response when the token lacks `read:issue`.

Webhook deliveries, captured from Forgejo 7.0.16, 11.0.16, 15.0.10 and 17.0.0
and Gitea 1.27.3 on 2026-10-25 (`TestLiveWebhookDeliveries`; the fixtures in
`internal/forgejo/testdata/webhooks/` pin them):

| Fact | Result |
|---|---|
| Signature and event headers | `X-Gitea-Signature`, `X-Gitea-Event`, `X-Gitea-Event-Type` and `X-Hub-Signature-256` (`sha256=` prefixed) on all five; Forgejo adds `X-Forgejo-Signature`, `X-Forgejo-Event`, `X-Forgejo-Event-Type`. Signatures are lower-case hex |
| Pull request sub-events | `X-*-Event: pull_request` for opened, synchronized, labels, review requests, closed and merged; the payload's `action` tells them apart (`opened`, `synchronized`, `label_updated`, `review_requested`, `closed` with `merged`, `reopened`). A review is `pull_request_approved` |
| Comment on a pull request | `X-*-Event: issue_comment`, `is_pull: true`. **Forgejo 7 sends no `pull_request` object** with it; the others do |
| `draft` in a pull request payload | **Absent on Forgejo 7**; set from the `WIP:` prefix on the others |
| Fork pull request | `head.repo_id` differs from `base.repo_id` on all five |
| Branch deletion | A `delete` event only; no push with a zero `after` on any |
| `action_run_success` | Fires for **every** successful run (Forgejo 15 and 17), `prior_status: running` |
| Cancelled run | Forgejo 17 sends `action_run_failure` with `run.status: cancelled` (seen when a newer push cancelled a run) |
| Run payload | `{action, run, prior_status}`; the repository is `run.repository`, the number `run.index_in_repo`, `trigger_user` an object. `run.prettyref` is the branch or tag name for a push and `#N` for a pull request, so the branch is read from `run.event_payload` |
| A fork pull request's run | `is_fork_pull_request: true`, ran without approval for a collaborator's fork |
| Test delivery (`POST .../hooks/{id}/tests`) | A signed push to the default branch, the same body each time |

Token scopes and branch protection, probed on all four on 2026-10-09:

| Fact | Result |
|---|---|
| `read:repository,write:repository` token | Review, merge, update branch work. **Comment and labels are 403** (`write:issue` needed), and so is the review-requested search (`read:issue`). Request reviewers needs `write:repository` |
| Scope refusal body | 403 `token does not have at least one of required scope(s)…`, which contains "required": it must not read as branch protection |
| `write:issue` without `write:repository` | Comment and labels work; review, merge, update branch, request reviewers are 403 |
| Merge blocked by required approvals | **405** on all four. `not allowed to merge [reason: Does not have enough approvals]` on Gitea 1.20 and both Forgejos, bare `Does not have enough approvals` on Gitea 1.27. Classified as blocked by protection, ahead of "not allowed to merge" (a read-only user's message, which is forbidden) |
| `GET .../branches/main` on a protected branch | `protected`, `required_approvals`, `user_can_merge` as expected; a writer with an approval merges (200) |

Pull request automation, probed on Forgejo 7.0.16, 11.0.16, 15.0.10 and 17.0.0
and Gitea 1.27.3 on 2026-10-25. `TestLiveAutomationObservation` and
`TestLiveAutomationMergePinsTheHead` pin what the code relies on; the scheduled
merge rows are why it does not use that feature.

| Fact | Result |
|---|---|
| `merge_when_checks_succeed` when the head's checks are already green | **Forgejo 11, 15, 17: 201 and never merges** (no merge within 20 s, three runs each); it waits for a new status event. Forgejo 7: merged at once (200), once. Gitea 1.27: merges |
| `merge_when_checks_succeed` on a repository with no CI or statuses | **Forgejo: 201 and never merges.** Gitea 1.27: merges at once |
| Scheduled, then a new commit is pushed and goes green | **Merges the new head on all five**, though it was scheduled with the old `head_commit_id` |
| Scheduled with a stale `head_commit_id` | 201 (accepted) on Forgejo 11, 15, 17; 405 "try again later" on Forgejo 7 and Gitea, which is mergeability recomputing, not a check |
| Scheduling twice | 409 `pull request is already scheduled to auto merge…` on all |
| Cancelling a scheduled merge | 204, then 404 for a second cancel, on all |
| Plain merge with a stale `head_commit_id` | 409 `head out of date` (Forgejo 15, 17), or the transient 405 while mergeability recomputes (Forgejo 11); never merged |
| `GET .../collaborators/{user}/permission` | **403 for a non-admin token** asking about anyone but itself, on all five. Repository admins and the user itself get `permission` and `role_name` |
| `GET .../collaborators/{user}` | 204 / 404 to any collaborator, but says nothing about the level and misses owners and org members |
| A review's `official` | `true` for a collaborator with write access, `false` for a read-only one, on all five, visible to any token that can read the pull request |
| A review comment's `resolver` | Present (`null` until resolved) on all five. There is no API to resolve one. **Set only on the comment the conversation was resolved from** (Forgejo 16, Gitea 1.27); the web UI resolves from, and reads, the first comment |
| A reply in a review conversation | Filed under the review that opened it, with the same `path` and `position`/`original_position` (Forgejo 16, 17, even when the replier has a pending review). Gitea 1.27 files it under the replier's pending review when there is one, so a conversation can start in a review dismissed later and go on in a live one; the dismissed review's first comment still carries the resolver |
| `GET .../commits/{sha}/status` | **Paged**: 30 contexts by default, 50 at most, newest first. `total_count` and `state` describe the page alone; `X-Total-Count` is the number of contexts (Forgejo 16, Gitea 1.27). Without the header, a list that fills every page read counts as cut |
| `comments_count` on a review | Present on all five |
| Bot marker on users | None on any host: no `type` field. The Actions user (`-2`) and the ghost user (`-1`) have negative ids, read from source |
| A team review request on the pull request | `requested_reviewers_teams` lists the team on Forgejo 11, 15, 17 and Gitea; **Forgejo 7 lists neither the team nor its members** (`requested_reviewers: [null]`) |
| `GET /user/teams` | 200 with the plugin's documented scopes (Forgejo 17, Gitea 1.27); lists `organization.username` and `name` |

The CI `live` job seeds a write-access `reviewer` and a read-only `readonly`
user; `TestLivePullRequestActions`, `TestLiveReviewerFlow` and
`TestLiveReadOnlyUserIsForbidden` re-check the rest on each run.

## Layout

The packages are layered, and an import only points down. The layer table and
the rules that enforce it are in [architecture.md](architecture.md).

| Path | Contents |
| --- | --- |
| `manifest.yaml` | The declarative host contract: actions, provider ownership, reference source, capabilities, automation conditions, config schema. |
| `internal/scm/` | The shared vocabulary: `RepoRef`, the error kinds and write-refusal reasons every layer maps, the CI states. Standard library only. |
| `internal/kandev/` | Reaching Kandev: the late-bound host provider, the workspace walk, JSON action responses. `kandevtest/` is an in-memory host for tests. |
| `internal/sourcecontrol/` | The provider-neutral source-control recipe — the Kandev boundary, adapted from `kandev-plugin-template`: the model (`model.go`), the ports (`ports.go`, `connection.go`, `pullrequests.go`), the task association store, and the recipe's action handler (`recipe.go`). |
| `internal/watches/` | The provider-neutral watch half: the watch model (issue and review kinds), the dedup ledger, the poll loop, review placement and fork rules, and cleanup. Reaches Forgejo only through the `IssueSource` and `ReviewSource` ports. |
| `internal/automations/` | The provider-neutral half of the automation conditions: the condition keys and settings, the event model, the filters, the normalized `{{data.*}}` payload, and the `Instance` and `Webhooks` ports. |
| `internal/prautomation/` | The provider-neutral half of pull request automation: switches and workspace settings, the per pull request record and attempt ledger, feedback collection by trust level, the auto-fix, auto-merge and lifecycle decisions, prompt rendering, and the loop, one file per concern. Reaches Forgejo through the `Observer`, `Links` and `Merger` ports and Kandev through `Messenger`. |
| `internal/forgejo/rest/` | The REST v1 wire client: requests, authentication, paging, the wire types, the Actions/CI resolution chain, and classifying a refused write. Knows nothing about Kandev. `resttest/` is a scripted instance for tests. |
| `internal/forgejo/` | The port implementations: repositories, pull requests, reviews, references, git credentials, the issue and review sources, webhook signature checking and decoding (`webhooks.go`, with captured deliveries in `testdata/webhooks/`), the automation condition instance (`conditions.go`), and the pull request automation source (`automationsource.go`). |
| `internal/plugin/` | The transport: one handler type per surface (connection, pull request, watch and automation actions, the agent tools, the automation adapter), each holding only the ports it uses; `Runtime` routes the RPCs, and `wire.go` builds the graph. |
| `internal/archtest/` | The layer rules as a test. |
| `ui/src/` | The browser half: the recipe registration plus the Forgejo icon, reference parsing, detail adapter, review panel, connection panel, the watch panels, and the pull request automation section and settings (`automation.ts`). |
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
this plugin draws itself. The same holds for CI auto-fix, auto-merge and pull
request prompts: Kandev's are compiled into its GitHub integration (its outcome
tool accepts only `github` and `gitlab`, through v0.98.0), so the plugin runs
its own loop and reaches the agent through the message API.

### Ports in the neutral package, adapters in the provider package

`internal/sourcecontrol`, `internal/watches`, `internal/automations` and
`internal/prautomation` declare narrow interfaces and
own the decisions; `internal/forgejo` implements them and owns every URL,
pagination token, authentication detail and error mapping, with the wire calls
themselves in `internal/forgejo/rest`. The transport in `internal/plugin` holds
only those interfaces: `wire.go` is the one file there that names an adapter.

Nothing above the adapter forms a Forgejo URL, and nothing in the adapter
decides what a watch means or when a pull request counts as ready. That is what
makes the poller testable against a scripted `IssueSource` with no HTTP at all,
and the REST client testable against an `httptest` server with no Kandev host.

### One REST surface, both hosts

Forgejo is a hard fork of Gitea and both serve `/api/v1`. The client restricts
itself to endpoints and fields that exist on both, so one plugin serves either.
Flavor detection is for display, with one exception: the CI result automation
condition needs Forgejo Actions run events, which only Forgejo 12+ sends, so it
reports itself unavailable on Gitea and older Forgejo.

Where the hosts genuinely diverge, the code resolves through a chain and takes
the first surface that answers rather than assuming a shape: `ci` walks
`/actions/runs` → `/actions/tasks` → the combined commit status, because the
Actions API varies between releases and only the combined status sees CI running
outside the forge. The live contract matrix in CI is what keeps that claim
honest.

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
