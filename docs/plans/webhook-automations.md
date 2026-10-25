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
  authors, labels, drafts, check names, conclusions), plus fork handling.
- A normalized payload per condition, exposed to prompts as `{{data.*}}`.
- Setup documentation: the URL, the secret and the event to tick in Forgejo.
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

Read from `kandev/apps/backend` at v0.97.0. The surface is identical in v0.95.0,
our `min_kandev_version`, so **the floor does not move**.

- **Manifest.** `automation_conditions[]` (`internal/plugins/manifest/automation.go`):
  at most 32; each needs `access: public`, `config_version: 1`, a label, and
  `verification_headers` (at most 16, lower-case, `x-` prefixed, not `x-kandev-*`).
  The config schema dialect is small: `string` (optionally `enum`), `boolean`,
  and `array` of plain strings. **There is no array of enum**, so a multi-select
  of events has to be a set of booleans. At most 32 fields.
- **No capability.** Declaring conditions and implementing the adapter needs no
  entry in `capabilities`, so this **does not invalidate the operator's
  approval**. `manifest_test.go` still changes (condition count and keys).
- **Adapter.** `pluginsdk.AutomationAdapter` (`pkg/pluginsdk/automation.go`), two
  RPCs implemented on `Runtime`:
  - `DescribeAutomationCondition(workspace, key, config)`. With no config (the
    editor listing it) it reports availability and may return `config_options`,
    at most 100 suggestions per string field: the repository list. With config
    (saving, and configuring the binding) it validates the configuration and
    returns a `connection_id` and `connection_revision`; both must be non-empty
    or the host refuses to bind. 5 s deadline.
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
  (`version:installed_at`), the `connection_revision`, the trigger's config and
  the secret id. A mismatch is a 401. So **every plugin upgrade or reinstall
  breaks every binding** until the user clicks "Configure webhook" again and
  pastes the new secret into Forgejo. This needs a line in the docs and in the
  upgrade notes of every release from now on.
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
`application/json`, the revealed secret, and **only** the matching event ticked
("Push", "Pull request" and its sub-events, or "Commit status"). Ticking more
is harmless but costs an `ignored` receipt each time.

### Conditions and filters

Empty lists mean "all". The repository is required, which is fail-closed in the
same way as GitHub's `pr_merged` (`all_repos: false` with no repositories matches
nothing). Defaults are in brackets.

| Condition | Fields |
| --- | --- |
| `push` | `repository` (required, suggestions), `branches` (globs) |
| `pull_request` | `repository` (required), `base_branches` (globs), `authors`, `labels`, booleans `on_opened` [true], `on_reopened`, `on_synchronized`, `on_closed`, `on_merged`, `on_review_requested`, `on_commented`, `exclude_draft` [true], `include_forks` [false] |
| `ci_result` | `repository` (required), `branches` (globs), `workflows` (workflow file names, e.g. `ci.yml`), booleans `on_success`, `on_failure` [true], `include_forks` [false] |

Matching rules, all in the neutral package:

- Branch globs use `path.Match`, the same syntax as GitHub's `release/*`.
- `push`: only `refs/heads/*`. Tag pushes and branch deletions (`after` is all
  zeros) are `ignored`.
- `pull_request`: `on_merged` fires on `closed` with `merged: true`, and
  `on_closed` on `closed` without a merge, so one can be had without the other.
  A pull request from a fork is `ignored` unless `include_forks` is set: an
  automation that starts an agent on fork-controlled content must be an explicit
  choice, as in review watches.
- `ci_result`: Forgejo Actions run events, `action_run_success` and
  `action_run_failure`. A run from a fork pull request
  (`run.is_fork_pull_request`) is `ignored` unless `include_forks` is set, for
  the same reason as pull requests.
- Fork handling is a per-condition switch in the automation editor, off by
  default, so an operator who trusts everyone who can open a pull request on
  their instance can opt in.

### Verification

In order. Nothing is parsed before the signature passes.

1. Empty secret: `rejected`.
2. Signature. Accept `x-forgejo-signature`, `x-gitea-signature` (hex HMAC-SHA256
   of the raw body) and `x-hub-signature-256` (`sha256=` plus hex). **At least
   one must be present, and every one present must verify**, compared with
   `hmac.Equal`. Otherwise `rejected`.
3. Connection: the `connection_id` must equal the current instance digest (see
   below), otherwise `rejected`. The binding is to an instance that is no longer
   configured.
4. Event type from `x-forgejo-event` or `x-gitea-event`, refined by
   `x-forgejo-event-type` or `x-gitea-event-type` (Gitea sends `pull_request`
   with type `pull_request_sync`, for instance; confirm in the capture step). An
   event that is not this condition's is `ignored`.
5. Parse. A body that is not a JSON object, or lacks the fields the condition
   needs, is `malformed`.
6. Repository. The payload's `repository.full_name` (case-insensitive) must equal
   the configured repository, otherwise `ignored`. The delivery is correctly
   signed, so it is not an authentication failure; the hook was put on another
   repository. The host's receipts list still shows it as ignored.
7. Workspace switch off (`connection.set_enabled`): `ignored`.
8. Filters: `ignored` on a miss, `accepted` with the normalized data on a match.

**No provider request is made in `Verify`.** It stays a pure function of its
input and the plugin config, which keeps it under the 10 s deadline, keeps it
testable without HTTP, and means a slow instance cannot hold webhook deliveries.

### Connection identity

- `connection_id`: a SHA-256 digest of the connection scope (the normalized
  instance URL, `Connection.Scope`). A digest so the host database holds no URL.
- `connection_revision`: the same digest plus the authenticated user's id, taken
  from the `Describe` call, which already probes the instance to validate the
  repository. Changing the instance or the account changes it and fences the old
  bindings. Rotating the token for the same account deliberately does not: the
  webhook secret does not depend on the token, so a rotation must not break
  automations.

`Describe` with a configuration: integration enabled, client configured, the
repository reachable with the token (`GET /repos/{owner}/{repo}`), and the
condition's fields valid (globs compile, lists bounded). Otherwise
`available: false` with a fixed, token-safe `reason`.

### Normalized data

Stable snake_case and provider-neutral, so a prompt written for one host works on
the other. Every payload carries `provider` (`forgejo` or `gitea`, for display),
`repository` (`owner/name`) and `html_url`.

- `push`: `branch`, `before`, `after`, `pusher`, `commit_count`, `compare_url`,
  `head_commit` (`id`, `message`, `url`, `author`).
- `pull_request`: `action` (the normalized one: `opened`, `merged`, …), `number`,
  `title`, `author`, `base_branch`, `head_branch`, `head_sha`, `draft`, `fork`,
  `labels`, `requested_reviewer` (for `on_review_requested`), `comment` (`author`
  and `body`, for `on_commented`).
- `ci_result`: `state` (`success` or `failure`), `workflow`, `run_number`,
  `title`, `sha`, `ref`, `event` (what triggered the run), `trigger_user`,
  `fork`. Never the run's `event_payload`: it is the whole triggering event as a
  string, unbounded, and already reachable through `{{webhook.*}}`.

`data` is untrusted, like `webhook`: the host says so, and the docs should repeat
it for anyone templating a commit message or PR title into a prompt.

### Host support

Read from each project's `modules/webhook/type.go` and `modules/structs/` at
the tags named. Still to be confirmed by the capture step.

| | Forgejo 7 to 11 | Forgejo 12 to 16 | Forgejo 17 | Gitea 1.25+ |
| --- | --- | --- | --- | --- |
| `push`, `pull_request` | yes | yes | yes | yes |
| Commit status event | **no** | **no** | **no** | `status` |
| Actions run events | none | `action_run_{success,failure,recover}` | `action_run_{success,failure}`, plus `workflow_run_*` and `workflow_job_*` per state | `workflow_run`, `workflow_job` (different payloads) |

Consequences:

- **Forgejo cannot report CI by webhook except through Actions run events.**
  A third-party CI that posts commit statuses (Woodpecker, for instance) cannot
  feed `ci_result` on Forgejo at all.
- `action_run_success` and `action_run_failure` are the only CI events common to
  Forgejo 12 through 17, so they are the first input. `action_run_recover` is
  gone in 17 and is not used. The payload is `{action, run, prior_status,
  last_run}`, with the repository inside `run.repository`, not at the top
  level.
- Forgejo 17's `workflow_run_*` and `workflow_job_*` events add cancelled,
  skipped and per-job results. They are a later addition to the same condition,
  once 17 is in the CI matrix.
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
| `internal/automations/` (new, neutral) | Condition keys, the typed config per condition and its validation, the neutral `Event` model, glob and filter matching, the `Normalize` output. No Forgejo |
| `internal/forgejo/webhooks.go` | Header names, signature schemes, event type mapping, decoding each payload into the neutral `Event`, the Forgejo/Gitea split |
| `internal/plugin/automations.go` | `Describe` and `Verify` on `Runtime`: enable switch, connection identity, wiring the two above |

The provider package exposes something like
`VerifyDelivery(secret, body, headers) (Delivery, error)` and
`DecodeEvent(Delivery) (automations.Event, error)`; the neutral package decides.

## Steps

0. **CI matrix.** Done separately on `chore/raise-host-floor`: the matrix is
   now Forgejo 7, 11, 15 and 17 and Gitea 1.27, and the README has a table of
   each feature's minimum release. `ci_result` gets the row "Forgejo 12, not
   Gitea"; 12 falls between two LTS releases, so it is read from source and
   tested on 15 and 17.
1. **Capture real deliveries.** Add webhook config to the live CI containers
   (`FORGEJO__webhook__ALLOWED_HOST_LIST=*`, and the `GITEA__` spelling) and a
   live test that runs an `httptest` receiver reachable from the container,
   creates a hook with a known secret, causes a push, a pull request open, sync,
   label, review request, comment, merge and close, and an Actions run that
   succeeds and one that fails (Forgejo 15 and 17 need a runner, or a run
   status set through the API if the capture step finds a way), and
   records each request's headers and body. Commit them as `testdata` fixtures,
   one directory per container (Forgejo 7, 11, 15 and 17, Gitea 1.27). Record in
   `docs/development.md`'s "Verified against" table which headers and event
   types each version sends, and when exactly `action_run_success` fires (every
   success, or only the first after a failure). **Settle the decoders' input
   here, before writing them.**
2. **Verifier and `push`, end to end.** Manifest entry, the neutral package,
   provider decoding, the adapter, `manifest_test.go`, docs and changelog. This is
   the smallest slice a user can try on a real Kandev.
3. **`pull_request`.**
4. **`ci_result`** from Forgejo Actions run events.
5. **Real-Kandev check** per [`testing-on-kandev.md`](../testing-on-kandev.md):
   build an automation on each condition, configure the binding, paste the secret
   into Forgejo, and see a task created. Then upgrade the plugin and confirm the
   binding fails until reconfigured, as the host source says.

Steps 2 to 4 can each be their own pull request.

## Tests

- **Signature table** (provider package): each header alone, all three, one bad
  among good ones, a `sha256=` prefix on the hex headers, upper-case hex, an empty
  secret, a body changed by one byte, no signature header. Each must produce the
  expected outcome; break `hmac.Equal` into a prefix compare and confirm the
  suite fails.
- **Decoding** against every captured fixture from step 1, per container.
- **Filters** (neutral package): globs, empty lists, defaults, the merged/closed
  split, forks (pull requests and runs), drafts, tag pushes, branch deletions,
  workflow names.
- **Adapter** (plugin package): outcome per verification step in order (a bad
  signature on a body for another repository is `rejected` before anything is
  parsed), a delivery for another repository is `ignored`, `event_kind` always
  equals the key, disabled workspace, connection id
  mismatch, `Describe` with and without config, and that the secret appears in
  no log line or response (a capturing logger, as the token tests do).
- **Live contract test**: the instance signs and sends a real delivery and the
  adapter accepts it; the same delivery with a different secret is rejected.
  Skips without `KANDEV_FORGEJO_URL`, like the others.

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
- `docs/automations.md` (new) covers setup, the event to tick, the
  reconfigure-after-upgrade rule, the placeholders, and the network caveats
  (the instance must reach Kandev; `ALLOWED_HOST_LIST` blocks private addresses
  by default). The README feature table and the changelog are updated.
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

## Risks

- **Upgrades break bindings.** This is the host's design, not ours, but users will
  experience it as the plugin breaking. Say it in the docs, the changelog of
  every release, and the GitHub release notes.
- **Network reachability.** A Forgejo instance on the public internet cannot reach
  a Kandev on a laptop, and Forgejo's default `ALLOWED_HOST_LIST` refuses private
  addresses. Neither can be fixed from the plugin; the docs have to state it.
- **Unverified provider claims.** Header names, the `_sync` event type split and
  the run event payloads come from reading source, not from a delivery. Step 1
  exists to replace them with captures.
- **Run events need a runner to capture.** If a CI container cannot run a real
  Forgejo Actions job, the `ci_result` fixtures need another way to produce a
  finished run, or are captured once by hand on a real instance and committed.
