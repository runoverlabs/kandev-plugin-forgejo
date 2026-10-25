# Plan: webhook automation conditions

Forgejo and Gitea webhooks arrive as native **automation conditions** in Kandev's
automation editor: "push to a branch", "pull request event" and "CI result" for a
repository, comparable to Kandev's built-in `github_push`, `github_pr` and
`github_ci` triggers. Tracks [#7](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/7).

Read [`README.md`](README.md) first for the conventions every plan follows.

## Scope

**In**

- Three conditions: `push`, `pull_request`, `ci_result`. The first two serve
  Forgejo and Gitea; `ci_result` is **Forgejo only** (see "Host support").
- Mandatory, fail-closed signature verification of each delivery.
- Filters comparable to the GitHub triggers (repository, branches, events,
  authors, labels, drafts, workflow names, conclusions), plus fork handling.
- A normalized payload per condition, exposed to prompts as `{{data.*}}`.
- Setup documentation: the URL, the secret and the events to tick in Forgejo.
- Captured real deliveries from each CI container as test fixtures, and a live
  contract test that has the instance sign and send a real delivery.

**Out**

- Registering the webhook on the instance through the API (see "Who registers
  the webhook"). The operator creates it by hand.
- Organisation and system hooks: [#28](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/28).
- Issue and issue-comment conditions. Issue watches cover discovery; add them
  later if someone asks.
- Feeding these events into the plugin's own pollers (watches, #8). That needs a
  path from the host into the plugin, and an accepted delivery goes to the
  automation, not to us.
- Any plugin UI. The host renders the editor, the binding URL, the secret and the
  receipts list.

## How the host side works (Kandev v0.95.0 to v0.97.0)

Read from `kandev/apps/backend` at v0.97.0. The files involved are byte-identical
in v0.95.1, our CI minimum, so **the floor does not move**.

- **Manifest.** `automation_conditions[]` (`internal/plugins/manifest/automation.go`):
  at most 32; each needs `access: public`, `config_version: 1`, a label, and
  `verification_headers` (at most 16, lower-case, `x-` prefixed, not `x-kandev-*`).
  The config schema dialect is small: per field only `type`, `title`,
  `title_key`, `enum` and `items`; types `string` (optionally `enum`), `boolean`,
  and `array` of plain strings. **There is no array of enum**, so a multi-select
  of events has to be a set of booleans. At most 32 fields.
- **Defaults.** A field cannot carry a `default`; the condition has a
  `default_config` object instead, which the editor copies into a new trigger's
  settings when the condition is picked (`automation/handlers.go`). After that
  the settings are whatever the user saved. The editor renders a boolean as on
  only when its value is `true` (`web/components/automations/trigger-configs/plugin-event-config.tsx`),
  so **the plugin reads an absent boolean as false**, the same as the editor
  shows it, and every boolean is named so that false is the safe value
  (`include_forks`, not `exclude_forks`). Defaults live in `default_config` only.
- **No capability.** Declaring conditions and implementing the adapter needs no
  entry in `capabilities`, so this **does not invalidate the operator's
  approval**. `manifest_test.go` still changes (condition count and keys).
- **Adapter.** `pluginsdk.AutomationAdapter` (`pkg/pluginsdk/automation.go`), two
  RPCs implemented on `Runtime`:
  - `DescribeAutomationCondition(workspace, key, config)`. With no config (the
    editor listing it) it reports availability and may return `config_options`,
    at most 100 suggestions per string field: the repository list. With config
    it validates the configuration and returns a `connection_id` and
    `connection_revision`; both must be non-empty or the host refuses to bind.
    5 s deadline. The host calls it with config when the trigger is saved, when
    the binding is configured, and **again for every accepted delivery before
    dispatch and before admission** (`plugin_webhook_worker.go`), comparing both
    values with the ones stored on the binding. A mismatch or `available: false`
    cancels the receipt; an RPC error leaves it pending and the worker retries.
  - `VerifyAutomationWebhook(workspace, key, connection id and revision, config,
    body, headers, secret)`. Returns `rejected` (401), `malformed` (400),
    `ignored` (200) or `accepted` (202) with `event_kind` equal to the condition
    key and `data`, a JSON object up to 1 MiB. An RPC error is a 503 and the
    instance retries. 10 s deadline.
- **The host owns the secret and the URL.** One binding per saved condition, at
  `/api/v1/automations/webhook-bindings/{id}`, with a vault-backed secret the
  user sees only through "reveal". The plugin receives the secret **only inside
  `VerifyAutomationWebhook`**, and must never store or log it.
- **Replays are handled by the host.** Receipts are keyed by the SHA-256 of the
  raw body, per binding, for seven days (`commitWebhookReceipt` in
  `internal/automation/plugin_webhook.go`). A Forgejo redelivery resends the same
  bytes, so it is a duplicate (200) without reaching the automation. The delivery
  id header is unsigned and adds nothing, so the plugin does no deduplication of
  its own.
- **Binding fencing.** The binding revision hashes the plugin generation
  (`version:installed_at`), the `connection_revision` stored at configure time,
  the trigger's `updated_at` and its config, plus the secret id. A mismatch is a
  401. So **every plugin upgrade or reinstall, and every saved edit of the
  condition's settings, breaks the binding** until the user clicks "Configure
  webhook" again, which issues a new secret, and pastes that secret into Forgejo.
  The docs and the upgrade notes of every release say so.
- Only the declared `x-` headers reach the plugin, and a declared header sent
  twice is a 400. Compressed requests and bodies over 1 MiB are refused.
- A plugin-event automation cannot also have a schedule trigger.
- `{{webhook.<path>}}` exposes the original body and `{{data.<path>}}` the
  normalized output.

## Design

### Who registers the webhook

**The operator, by hand.** The plugin cannot do it: when the binding is
configured the host never tells the plugin the secret, so there is nothing to
put in the hook's `secret` field. It would also need `write:repository` (the
admin `hooks` endpoints need repository admin), which the token does not
otherwise need for reads.

Each saved condition has its own URL and secret, so the setup is one Forgejo
webhook per automation condition: type Forgejo or Gitea, content type
`application/json`, the revealed secret, and only the matching events:

| Condition | Events to tick |
| --- | --- |
| `push` | Push |
| `pull_request` | Pull request events (all of them, or at least "Pull request", "Pull request synchronized", "Pull request review requested" and "Pull request comments" for the switches in use) |
| `ci_result` | Action run success and Action run failure |

Ticking more is harmless but costs an `ignored` receipt each time. There is no
commit-status event on Forgejo (see "Host support").

### Conditions and filters

Empty lists mean "all". The repository is required, which is fail-closed in the
same way as GitHub's `pr_merged` (`all_repos: false` with no repositories matches
nothing). `default_config` values are in brackets; every other boolean starts
false.

| Condition | Fields |
| --- | --- |
| `push` | `repository` (required, suggestions), `branches` (globs) |
| `pull_request` | `repository` (required), `base_branches` (globs), `authors`, `labels`, booleans `on_opened` [true], `on_reopened`, `on_synchronized`, `on_closed`, `on_merged`, `on_review_requested`, `on_commented`, `exclude_draft` [true], `include_forks` |
| `ci_result` | `repository` (required), `branches` (globs), `workflows` (workflow file names, e.g. `ci.yml`), booleans `on_success`, `on_failure` [true], `on_cancelled`, `include_forks` |

Matching rules, all in the neutral package:

- Branch globs use `path.Match`, the same syntax as GitHub's `release/*`.
- `push`: only `refs/heads/*`. Tag pushes and branch deletions (`after` is all
  zeros) are `ignored`.
- `pull_request`: `on_merged` fires on `closed` with `merged: true`, and
  `on_closed` on `closed` without a merge, so one can be had without the other.
  `on_commented` fires on a new comment (`issue_comment` with `is_pull`, action
  `created`). `labels` matches a pull request carrying any of them; `authors`
  matches the pull request's author, not the sender. A pull request from a fork
  is `ignored` unless `include_forks` is set: an automation that starts an agent
  on fork-controlled content must be an explicit choice, as in review watches.
- `ci_result`: Forgejo Actions run events. The state comes from `run.status`
  (`success`, `failure`, `cancelled`), not from the event name: Forgejo 17
  delivers a cancelled run as `action_run_failure`. `branches` matches the
  pushed branch for a push run and the head branch for a pull request run (the
  run's `prettyref` is `#N` there, so the branch is read from the run's embedded
  `event_payload`); a run on a tag or with no branch matches only when
  `branches` is empty. A run from a fork pull request (`run.is_fork_pull_request`)
  is `ignored` unless `include_forks` is set, for the same reason as pull
  requests.
- Fork handling is a per-condition switch in the automation editor, off by
  default, so an operator who trusts everyone who can open a pull request on
  their instance can opt in.

### Verification

In order. Nothing is parsed before the signature passes.

1. Empty secret: `rejected`.
2. Signature. Accept `x-forgejo-signature`, `x-gitea-signature` (bare hex
   HMAC-SHA256 of the raw body, either case) and `x-hub-signature-256`
   (`sha256=` plus hex). **At least one must be present, and every one present
   must verify**, compared with `hmac.Equal` on the decoded bytes. A `sha256=`
   prefix on a bare-hex header is not hex and fails. Otherwise `rejected`.
3. Connection: the `connection_id` must equal the current instance digest (see
   below), otherwise `rejected`. The binding is to an instance that is no longer
   configured. The account half of the identity is checked by the host's
   re-`Describe` before dispatch, not here, because it needs a provider request.
4. Event type from `x-forgejo-event`, else `x-gitea-event` (Gitea sends only the
   latter). The captures show the payload's `action` is enough to tell
   sub-events apart (`synchronized`, `review_requested`, …), so the
   `-event-type` headers are not used. An event that is not this condition's is
   `ignored`: `push` for `push`; `pull_request` and `issue_comment` for
   `pull_request`; `action_run_success` and `action_run_failure` for
   `ci_result`.
5. Parse. A body that is not a JSON object, or lacks the fields the condition
   needs, is `malformed`.
6. Repository. The repository's `full_name` (case-insensitive) must equal the
   configured repository, otherwise `ignored`. It is at the top level for push,
   pull request and comment payloads and at `run.repository` for run payloads.
   The delivery is correctly signed, so it is not an authentication failure; the
   hook was put on another repository. The host's receipts list still shows it
   as ignored.
7. Workspace switch off (`connection.set_enabled`): `ignored`.
8. Filters: `ignored` on a miss, `accepted` with the normalized data on a match.

**No provider request is made in `Verify`.** It stays a pure function of its
input and the plugin config, which keeps it under the 10 s deadline, keeps it
testable without HTTP, and means a slow instance cannot hold webhook deliveries.

### Connection identity

- `connection_id`: a SHA-256 digest of the connection scope (the normalized
  instance URL, `Connection.Scope`). A digest so the host database holds no URL.
- `connection_revision`: a digest of the scope plus the authenticated user's id,
  taken from the `Describe` call, which already probes the instance to validate
  the repository. Changing the instance or the account changes it, and the host's
  per-delivery re-`Describe` then cancels deliveries for the old binding until
  it is reconfigured. Rotating the token for the same account deliberately does
  not: the webhook secret does not depend on the token, so a rotation must not
  break automations.

`Describe` with a configuration: condition fields valid (repository is
`owner/name`, globs compile), integration enabled, client configured, the
account readable (`GET /user`) and the repository reachable with the token
(`GET /repos/{owner}/{repo}`), and for `ci_result` a Forgejo 12 or later.
A definite answer (invalid field, switched off, not configured, token rejected,
repository not found, unsupported host) is `available: false` with a fixed,
token-safe `reason`. A transient one (network error, 5xx, timeout) is an RPC
error with a fixed message, so the host's worker retries instead of cancelling.
The account and the instance version are cached in memory for a minute: the
host calls `Describe` twice per delivery. Kandev restarts the plugin when its
config changes, so the cache cannot outlive a token change.

### Normalized data

Stable snake_case and provider-neutral, so a prompt written for one host works on
the other. Every payload carries `provider` (`forgejo` or `gitea`, for display:
Forgejo sends `x-forgejo-event`, Gitea does not), `repository` (`owner/name`)
and `html_url`.

- `push`: `branch`, `before`, `after`, `pusher`, `commit_count`, `compare_url`,
  `head_commit` (`id`, `message`, `url`, `author`).
- `pull_request`: `action` (the normalized one: `opened`, `reopened`,
  `synchronized`, `closed`, `merged`, `review_requested`, `commented`),
  `number`, `title`, `author`, `sender`, `base_branch`, `head_branch`,
  `head_sha`, `draft`, `fork`, `labels`, `requested_reviewer` (for
  `review_requested`), `comment` (`author`, `body` and `html_url`, for
  `commented`).
- `ci_result`: `state` (`success`, `failure` or `cancelled`), `workflow`,
  `run_number` (`index_in_repo`), `title`, `sha`, `branch`, `ref` (the run's
  `prettyref`), `event` (what triggered the run), `pull_request` (the number,
  for a pull request run), `trigger_user`, `fork`. Never the run's
  `event_payload`: it is the whole triggering event as a string, unbounded, and
  already reachable through `{{webhook.*}}`.

`data` is untrusted, like `webhook`: the host says so, and the docs should repeat
it for anyone templating a commit message or PR title into a prompt.

### Host support

Read from each project's `modules/webhook/type.go` and `routers/api/v1/utils/hook.go`
at the tags named, then confirmed by the step 1 captures on Forgejo 7.0.16,
11.0.16, 15.0.10 and 17.0.0 and Gitea 1.27.3.

| | Forgejo 7 to 11 | Forgejo 12 to 16 | Forgejo 17 | Gitea 1.27 |
| --- | --- | --- | --- | --- |
| `push`, `pull_request` | yes | yes | yes | yes |
| Commit status event | **no** | **no** | **no** | `status` |
| Actions run events | none | `action_run_{success,failure,recover}` | `action_run_{success,failure}`, plus `workflow_run_*` and `workflow_job_*` per state | `workflow_run`, `workflow_job` (different payloads) |

What the captures settled:

- Every host sends `x-gitea-signature`, `x-gitea-event` and
  `x-hub-signature-256`; Forgejo adds `x-forgejo-signature` and
  `x-forgejo-event`. Pull request sub-events share `x-*-event: pull_request`
  (`pull_request_approved` for reviews) and differ in `action`. A comment on a
  pull request is `x-*-event: issue_comment` with `is_pull: true`.
- **Forgejo 7 sends no `draft` field** on a pull request; 11 and later and Gitea
  do. On Forgejo 7 the draft flag is derived from the title, using the
  instance's default work-in-progress prefixes (`WIP:` and `[WIP]`, case
  insensitive).
- **Forgejo 7's comment payload has no `pull_request` object**, so the base
  branch, draft and fork filters cannot be evaluated: `on_commented` is
  `ignored` there and its minimum is Forgejo 11.
- Fork pull requests have a different `head.repo_id` from `base.repo_id` on
  every host; a missing head repository (a deleted fork) counts as a fork.
- `action_run_success` fires for **every** successful run, not only the first
  after a failure. The payload is `{action, run, prior_status}` (no `last_run`),
  with the repository at `run.repository` and `trigger_user` an object.
- Forgejo 17 also sends `action_run_failure` for a cancelled run, with
  `run.status: cancelled`.

Consequences:

- **Forgejo cannot report CI by webhook except through Actions run events.**
  A third-party CI that posts commit statuses (Woodpecker, for instance) cannot
  feed `ci_result` on Forgejo at all.
- `action_run_success` and `action_run_failure` are the only CI events common to
  Forgejo 12 through 17, so they are the input. `action_run_recover` is gone in
  17 and is not used. Forgejo 17's `workflow_run_*` and `workflow_job_*` events
  add per-job results; they are a later addition to the same condition.
- **`ci_result` does not support Gitea.** Its run events have other names and
  shapes. This follows the project's direction: target recent Forgejo, and stop
  supporting Gitea for a feature where supporting it gets in the way. Push and
  pull request payloads are shared, so those two conditions keep Gitea.
- `Describe` reports `ci_result` as unavailable with a clear reason when the
  instance is Gitea or a Forgejo older than 12. Flavor and version come from the
  existing connection probe. This is the first behaviour that branches on
  flavor, so `AGENTS.md` ("no behaviour branches on it") changes in the same
  pull request.

### Packages

Following the architecture rule.

| Package | Owns |
| --- | --- |
| `internal/automations/` (new, neutral) | Condition keys, the typed config per condition and its validation, the neutral `Event` model, glob and filter matching, the normalized output. No Forgejo |
| `internal/forgejo/webhooks.go` | Header names, signature schemes, event type mapping, decoding each payload into the neutral `Event`, the Forgejo/Gitea split, the Forgejo 7 draft fallback |
| `internal/plugin/automations.go` | `Describe` and `Verify` on `Runtime`: enable switch, connection identity, wiring the two above |

The provider package exposes `VerifyWebhookSignature(secret, body, headers)` and
`DecodeWebhook(body, headers) (automations.Event, error)`; the neutral package
decides.

## Steps

0. **CI matrix.** Done in #29: Forgejo 7, 11, 15 and 17 and Gitea 1.27, and the
   README has a table of each feature's minimum release. `ci_result` gets the
   row "Forgejo 12, not Gitea"; 12 falls between two LTS releases, so it is read
   from source and tested on 15 and 17.
1. **Capture real deliveries.** A live test (`TestLiveWebhookDeliveries`) runs a
   receiver on the test host, creates a hook with a known secret, and causes a
   push, a tag push, a pull request open, label, review request, comment, sync,
   merge, close and reopen, a draft and a fork pull request, and, where a runner
   is registered, Actions runs that succeed and fail. It checks every delivery
   against the verifier and decoder, and with `KANDEV_FORGEJO_CAPTURE_DIR` set it
   writes them as `testdata` fixtures, one directory per container. The fixtures
   in the repository were captured locally with `gitea/act_runner` in host mode
   for the run events (the CI job registers no runner, so it does not wait for
   runs). The container needs `ALLOWED_HOST_LIST` opened for the receiver.
2. **Verifier and `push`, end to end.** Manifest entry, the neutral package,
   provider decoding, the adapter, `manifest_test.go`, docs and changelog.
3. **`pull_request`.**
4. **`ci_result`** from Forgejo Actions run events.
5. **Real-Kandev check** per [`testing-on-kandev.md`](../testing-on-kandev.md):
   build an automation on each condition, configure the binding, paste the secret
   into Forgejo, and see a task created. Then upgrade the plugin and confirm the
   binding fails until reconfigured, as the host source says.

Steps 2 to 4 shipped together.

## Tests

- **Signature table** (provider package): each header alone, all three, one bad
  among good ones, a `sha256=` prefix on a bare-hex header (rejected), upper-case
  hex (accepted), an empty secret, a body changed by one byte, no signature
  header. Break `hmac.Equal` into a prefix compare and confirm the suite fails.
- **Decoding** against every captured fixture from step 1, per container.
- **Filters** (neutral package): globs, empty lists, absent booleans, the
  merged/closed split, forks (pull requests and runs), drafts, tag pushes,
  branch deletions, workflow names, cancelled runs, pull request run branches.
- **Adapter** (plugin package): outcome per verification step in order (a bad
  signature on a body for another repository is `rejected` before anything is
  parsed), a delivery for another repository is `ignored`, `event_kind` always
  equals the key, disabled workspace, connection id mismatch, `Describe` with
  and without config, transient failures as RPC errors, and that the secret
  appears in no response or error.
- **Manifest**: the condition keys and headers, and `default_config` equal to
  the code's defaults.
- **Live contract test**: the instance signs and sends real deliveries and the
  verifier and decoder accept them; the same delivery checked with a different
  secret is rejected. Skips without `KANDEV_FORGEJO_URL`, like the others.

## Acceptance criteria

The issue's, restated against this design:

- `push`, `pull_request` and `ci_result` are selectable in the automation editor
  with the filters above; `ci_result` is unavailable, with its reason, on Gitea
  and on Forgejo before 12.
- Verification fails closed: no signature, a bad one, or an unknown connection is
  a 401, and nothing is parsed first.
- A redelivered event does not start a second run (the host's body-hash dedup,
  covered by the step 5 check).
- Fixture-based unit tests for every container in the matrix and a live
  contract test.
- `docs/automations.md` (new) covers setup, the events to tick, the
  reconfigure-after-upgrade-or-edit rule, the placeholders, and the network
  caveats (the instance must reach Kandev; `ALLOWED_HOST_LIST` blocks private
  addresses by default). The README feature table and the changelog are updated.
- No capability change, `min_kandev_version` unchanged.

## Decisions

Settled:

- A correctly signed delivery for another repository is `ignored`, not
  `rejected`.
- `connection_revision` does not change when the token is rotated for the same
  account.
- Per-repository hooks first; organisation hooks are
  [#28](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/28).
- Pull requests and Actions runs from forks are `ignored` unless the
  condition's `include_forks` switch is on. Kandev's GitHub trigger does not
  distinguish forks at all; we do.
- `ci_result` uses Forgejo Actions run events, not commit statuses, and is
  Forgejo only.
- An absent boolean setting is false. Defaults are `default_config`, nothing
  else.

## Risks

- **Upgrades and edits break bindings.** This is the host's design, not ours, but
  users will experience it as the plugin breaking. Say it in the docs, the
  changelog of every release, and the GitHub release notes.
- **Network reachability.** A Forgejo instance on the public internet cannot reach
  a Kandev on a laptop, and Forgejo's default `ALLOWED_HOST_LIST` refuses private
  addresses. Neither can be fixed from the plugin; the docs have to state it.
- **Run events are captured locally, not in CI.** The CI job has no runner, so a
  change in the run payload shows up only when the fixtures are re-captured.
  Re-capture when a Forgejo release enters the matrix.
