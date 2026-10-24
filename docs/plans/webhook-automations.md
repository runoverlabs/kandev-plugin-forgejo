# Plan: webhook automation conditions

Forgejo and Gitea webhooks arrive as native **automation conditions** in Kandev's
automation editor: "push to a branch", "pull request event" and "CI result" for a
repository, comparable to Kandev's built-in `github_push`, `github_pr` and
`github_ci` triggers. Tracks [#7](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/7).

Read [`README.md`](README.md) first for the conventions every plan follows.

## Scope

**In**

- Three conditions: `push`, `pull_request`, `ci_result`.
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
| `ci_result` | `repository` (required), `branches` (globs), `contexts` (status context or check names), booleans `on_success`, `on_failure` [true], `on_error` [true] |

Matching rules, all in the neutral package:

- Branch globs use `path.Match`, the same syntax as GitHub's `release/*`.
- `push`: only `refs/heads/*`. Tag pushes and branch deletions (`after` is all
  zeros) are `ignored`.
- `pull_request`: `on_merged` fires on `closed` with `merged: true`, and
  `on_closed` on `closed` without a merge, so one can be had without the other.
  A pull request from a fork is `ignored` unless `include_forks` is set: an
  automation that starts an agent on fork-controlled content must be an explicit
  choice, as in review watches.
- `ci_result`: the commit-status event first. Every CI that reports to Forgejo,
  Forgejo Actions included, posts a commit status, so this works at each host's
  supported floor. `pending` is never a result and is `ignored`. Actions
  `workflow_run` and `workflow_job` events exist only on newer releases; add
  them later as a second input to the same condition, only once the capture
  step shows their shape.

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
- `ci_result`: `state`, `context`, `sha`, `branches`, `target_url`,
  `description`.

`data` is untrusted, like `webhook`: the host says so, and the docs should repeat
it for anyone templating a commit message or PR title into a prompt.

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

1. **Capture real deliveries.** Add webhook config to the live CI containers
   (`FORGEJO__webhook__ALLOWED_HOST_LIST=*`, and the `GITEA__` spelling) and a
   live test that runs an `httptest` receiver reachable from the container,
   creates a hook with a known secret, causes a push, a pull request open, sync,
   label, review request, comment, merge and close, and a commit status, and
   records each request's headers and body. Commit them as `testdata` fixtures,
   one directory per container (Gitea 1.20 and 1.27, Forgejo 7 and 16). Record in
   `docs/development.md`'s "Verified against" table which headers and event
   types each version sends, and whether `workflow_run` exists. **Settle open
   questions 3 and 6 of the issue here, before writing the decoders.**
2. **Verifier and `push`, end to end.** Manifest entry, the neutral package,
   provider decoding, the adapter, `manifest_test.go`, docs and changelog. This is
   the smallest slice a user can try on a real Kandev.
3. **`pull_request`.**
4. **`ci_result`** from commit statuses.
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
  split, forks, drafts, tag pushes, branch deletions, `pending` statuses.
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
  with the filters above.
- Verification fails closed: no signature, a bad one, or an unknown connection is
  a 401, and nothing is parsed first.
- A redelivered event does not start a second run (the host's body-hash dedup,
  covered by the step 5 check).
- Fixture-based unit tests for all four containers and a live contract test.
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

Still open:

| # | Decision | Recommendation |
| --- | --- | --- |
| 4 | Pull request events from forks. Kandev's GitHub `github_pr` trigger does not distinguish forks at all | Ignored unless `include_forks` is set |
| 5 | `ci_result` source on hosts with Actions run events | Commit status only in the first version; revisit once the capture step shows what `workflow_run` carries |

## Risks

- **Upgrades break bindings.** This is the host's design, not ours, but users will
  experience it as the plugin breaking. Say it in the docs, the changelog of
  every release, and the GitHub release notes.
- **Network reachability.** A Forgejo instance on the public internet cannot reach
  a Kandev on a laptop, and Forgejo's default `ALLOWED_HOST_LIST` refuses private
  addresses. Neither can be fixed from the plugin; the docs have to state it.
- **Unverified provider claims.** Header names, the `_sync` event type split, the
  status event shape and Gitea 1.20's coverage are taken from documentation and
  the issue text. Step 1 exists to replace them with captures.
