# Security

What this plugin does with an operator's credential, what it claims from the
host, and what the pipeline checks on every change.

## Identity and handling

- **Repository identity is the instance's immutable numeric id**, never
  `owner/name`. A rename or transfer does not break a stored link.
- **A task↔pull-request link is the full tuple** `(instance URL, repository id,
  pull request number)`, and each link is its own Host state entry — Host state
  has no compare-and-swap, so a shared per-task list would lose a concurrent
  link.
- `matchesURL` is only a hint; `inspectURL` performs an authenticated,
  workspace-scoped lookup and returns "not owned" rather than guessing.
- Pull-request creation takes its repository, session, and head branch from
  Kandev's `VerifiedActionContext`. The browser body may supply only title,
  description, destination, and draft state.
- Composer reference selection is not authorization: submission re-checks
  access against the instance and fails closed on any error or revocation.
- The access token never appears in responses, logs, cursors, repository
  descriptors, or error messages — provider error bodies are not forwarded,
  because some deployments echo the presented token.
- **Declared capabilities are the whole trust surface.** `api_read` covers
  `tasks`, `repositories`, `workspaces`, `workflows`, `agent_profiles`,
  `executor_profiles` and `sessions`; `state` and `secrets` are held;
  `api_write` is `["tasks", "messages"]`; `events` subscribes to
  `turn.completed` and `task_session.state_changed`. Task creation (watches
  filing a card) and prompts to a task's agent ([pull request
  automation](pr-automation.md)) are the plugin's only entity mutations; Kandev
  owns every other one. The manifest claims neither `agent_invoke` nor `auth`,
  and `auth` in particular is the highest-privilege capability a manifest can
  hold.
- **A capability grant is a permission gate, not a sandbox.** Plugin code runs
  privileged inside a Kandev installation, so `api_write: tasks` is a real trust
  step rather than a checkbox. `internal/plugin/manifest_test.go` asserts the
  capability set exactly, so widening it cannot pass review unnoticed.
- **Tasks a watch creates are permanently attributable.** Kandev stamps
  `source = "plugin:kandev-plugin-forgejo"` on rows this plugin creates and the
  plugin cannot set that field itself.

## Review watches

- **Fork pull requests never start an agent on their own.** A fork's head is
  content its author controls, and a task that checks it out and starts an agent
  runs it with the operator's executor environment. GitHub's watches mark such a
  task so the host refuses an automatic start; **a plugin cannot set that marker**
  (the host's top-level `fork_pr_requires_manual_start` is outside the plugin's
  namespaced metadata), so this protection is weaker. The plugin instead never
  sets the start flag, never attaches the fork's branch for checkout, and places
  the task in a column that does not start an agent on entry, or leaves the pull
  request out. It re-checks that column every run. Starting the task by hand
  remains possible, and is the operator's decision. A head repository that is
  missing or unnamed counts as a fork.
- **Branch names are checked before any checkout.** A head branch that could be
  read as an option or a path trick gets a task with no checkout.
- **Pull request text is data.** Titles, authors and branch names are substituted
  into the prompt as text and never evaluated.
- **Cleanup never deletes.** The plugin cannot delete tasks, and does not try.
  It archives with the host's exact archive command, which needs the
  `host.v2.write:tasks` approval (part of approving `api_write: tasks` on Kandev
  0.97.0), and otherwise completes the task. It acts only on task ids in its own ledger, never on
  another task in the workspace.
- **Cleanup is opt-in and bounded.** A watch cleans up only when set to, makes no
  instance request for a task that is already archived or complete, checks at
  most 40 pull requests per pass, and stops the whole pass at the first rate
  limit or refused token.

## Automation conditions

[Automation conditions](automations.md) are the plugin's only inbound path: a
Forgejo webhook posts to a Kandev URL and Kandev asks the plugin to verify it.
They add no capability.

- **Verification fails closed.** The HMAC-SHA256 of the raw body must match in
  every signature header present (`X-Forgejo-Signature`, `X-Gitea-Signature`,
  `X-Hub-Signature-256`), at least one must be present, and the comparison is
  constant-time. A missing or wrong signature, an empty secret, or a binding to
  an instance the plugin is no longer configured for is a 401, decided before
  the body is parsed.
- **The secret is Kandev's.** Kandev generates one per condition, keeps it in its
  vault, and passes it to the plugin only for the verification call. The plugin
  does not store or log it, and cannot register a webhook with it, which is why
  the operator creates the Forgejo webhook by hand.
- **Verification makes no provider request.** The outcome depends only on the
  delivery and the plugin config, so a slow instance cannot hold Kandev's webhook
  handling. The access token is not involved.
- **A signed delivery for another repository is ignored**, not accepted: the
  repository in the payload must equal the one configured on the condition.
- **Forks are out by default.** Pull requests and Actions runs from forks are
  ignored unless the condition opts in, for the same reason review watches never
  start an agent on one.
- **Delivery content is data.** The normalized summary (`{{data.*}}`) and the
  raw body (`{{webhook.*}}`) are untrusted input, chosen by whoever pushed or
  opened the pull request.
- **Replays** are Kandev's: it records the SHA-256 of each delivered body per
  binding for seven days and answers a repeat as a duplicate.

## Pull request automation

[Pull request automation](pr-automation.md) prompts a task's agent and can
merge. It is what `api_write: messages`, `api_read: sessions` and the two event
subscriptions are for; adding them means an operator re-approves the whole
capability set after upgrading.

- **People turn it on.** Each switch is set per pull request in the review
  panel, or copied from workspace defaults when a pull request is first linked.
  The agent tools cannot set any switch. Nothing runs while the workspace
  integration is off, and the loop checks that switch failing closed.
- **Review text is untrusted input.** It reaches the agent's prompt marked as
  data. The workspace's trust setting decides whose text counts. At the
  default (everyone, as with Kandev's GitHub integration), anyone who can
  comment on the pull request can put text in front of the agent; "official
  reviewers" limits it to reviewers whose approval counts, which needs write
  access. The operator account's own words are never fed back.
- **Forks.** Defaults never turn on auto-fix or auto-merge for a fork's pull
  request; a person must, after a confirmation. Kandev starts a never-run
  session when it is messaged, so for a fork the plugin reads the task's session
  states first and only prompts one already running or idle. As with review
  watches, this is the plugin's own guard, not Kandev's manual-start marker,
  which a plugin cannot set.
- **Bounded.** At most one auto-fix prompt is in flight per pull request and at
  most 10 rounds before it pauses; every prompt carries an attempt id, so a
  resend after a crash is a replay Kandev collapses rather than a duplicate.
- **Merges are pinned and audited.** Auto-merge checks its gates on one head
  commit and sends that commit as `head_commit_id`, so a commit pushed in
  between is refused by the instance rather than merged unseen. It does not use
  the instance's scheduled merge, which on every supported host merges a head
  pushed after scheduling. Each attempt is recorded in the audit trail as
  `automation.merge` by `auto-merge`, and every switch change with the Kandev
  user who made it.
- **Outcome reports are bound.** The `pr` tool's `outcome` op only resolves the
  attempt in flight on the calling task, sent to the calling session; with none,
  it is refused.

## Write authority

The review panel can merge, review, request reviewers, update the branch and
comment. Every write uses the operator's one shared Forgejo token, so the
Forgejo account is the same whichever Kandev user clicked.

- **Only linked pull requests.** A write resolves the pull request from the
  task's own stored links and the repository from the stored identity. A number
  the task does not link, or an owner or repository in the body, is never
  honoured, so holding the action does not let anyone act on any pull request
  the token can reach.
- **The head must match.** Merge requires the commit SHA the caller last saw and
  is refused if the pull request has moved; the same SHA is sent to the instance
  as `head_commit_id`, so a race is refused there too.
- **Disclosed attribution.** Reviews and comments end with "Posted via Kandev."
  so a reader of the pull request can tell the shared account was driven from
  Kandev.
- **An audit trail per write.** Every write, from the panel or from an agent,
  records the verified actor (`agent:<session>` for an agent), workspace, task,
  pull request number, action and outcome, never a body, a title or a token. The
  last 100 per workspace are kept in Host state and listed newest first by
  `connection.audit`. It is stored rather than only logged because on Kandev
  v0.97.0 a plugin's stderr is dropped before it reaches the backend log, so a
  log line alone is unreadable when it is needed. This trail is the only place a
  write is tied to a Kandev user: Forgejo shows the shared account.
- **Fixed failure messages.** A failed write maps to a fixed reason; the
  instance's response body is read only to classify it and is never forwarded. A
  403 on a write is reported as a permission problem, not as a bad token.
- **Agents cannot merge unless an operator allows it.** The `pr` tool's merge op
  is behind a per-workspace switch that is off by default and fails closed. With
  it on, an agent still cannot merge a pull request with failing or running
  checks, conflicts, missing approvals or requested changes, whatever the
  repository's own protection says. Review, comment, reviewer requests and
  branch updates are not behind the switch.
- **Token scope.** Writes need `write:repository`; comments, labels and
  assignees may also need `write:issue` on some versions.

## Supply chain and scanning

`.github/workflows/security.yml` runs on every push and pull request **and on a
weekly schedule**. The schedule is the point: a dependency becomes vulnerable
when an advisory is published, not when someone pushes, so a repo that only
scans on commit stops being scanned the moment it stops changing.

| Check | Tool | Gate |
| --- | --- | --- |
| Secret detection | `gitleaks` over the working tree **and the full history** | Any finding fails |
| Go dependencies | `govulncheck` | Any vulnerability on a reachable call path fails |
| npm dependencies | `npm audit` | `high` and `critical` fail |
| Static analysis | CodeQL, `security-extended` queries, Go, TypeScript and GitHub Actions workflows | Findings surface in the Security tab |

Everything but CodeQL runs locally too:

```sh
make security          # secrets + dependencies
make security-secrets  # needs gitleaks on PATH
make security-deps
```

A few deliberate choices:

- **History is scanned, not just the tree.** A credential that was committed and
  later removed is still a leaked credential, and a shallow clone cannot see it.
  The secret job checks out with `fetch-depth: 0` for this reason.
- **gitleaks is a checksummed binary, not `gitleaks-action`.** That action
  requires a paid licence key for organization accounts; moving this repo under
  an org should not silently disable its secret scanning.
- **`npm audit` is gated at `high`, not `moderate`.** Every npm dependency here
  is a `devDependency`, and the published package ships a pre-built `ui/bundle.js`
  with no `node_modules`, so a moderate finding in a test runner never reaches an
  operator. A high or critical one in a build tool can, and fails the run.
- **`govulncheck` over `go list -m -u`.** It reports only vulnerabilities on a
  reachable call path, so a finding is one this plugin can actually hit rather
  than one merely present in the module graph.
- **Actions are pinned by commit SHA**, tag in a trailing comment, as in
  `ci.yml`. A tag can be moved; a SHA cannot. The gitleaks release is pinned by
  version *and* SHA-256.

## Reporting a vulnerability

Open a private security advisory on the repository rather than a public issue.
If the finding involves a live instance, do not include the token, the instance
URL, or a log excerpt that contains either.

---

[← Back to the README](../README.md)
