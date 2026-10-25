<p align="center">
  <img src="assets/icon.svg" alt="kandev-plugin-forgejo: a git branch in Forgejo orange merging into a Kandev-indigo node, drawn in dots" width="140" height="140">
</p>

# kandev-plugin-forgejo

Connect a self-hosted [Forgejo](https://forgejo.org/) or [Gitea](https://about.gitea.com/)
instance to Kandev's **native** repository, pull-request, task-link and review
surfaces.

This is a Kandev runtime plugin. Repositories, pull requests and reviews are
supplied through the source-control extension contracts and rendered by Kandev
with the same UI it uses for the built-in code hosts. The only screens the
plugin draws itself are on the integrations card: a connection panel, and the
issue-watch list — Kandev's own issue-watch surfaces are compiled in per
provider and have no extension point a plugin can register against.

## What it adds

| Surface | What you get |
| --- | --- |
| Remote repository picker | Forgejo repositories appear alongside the built-in providers, with server-side search, paging, and branch lists. |
| Create PR dialog | Opens a pull request from the task's verified worktree branch. Draft is honored with the portable `WIP:` title marker. |
| Task **Link** menu | A "Forgejo pull request" entry that accepts a pull-request URL or `owner/repo#number`. |
| Sidebar / Kanban / list glyphs | Pull-request status per task, from one workspace-level association map. |
| Integrations card | Connection status and a per-workspace enable switch the plugin renders itself. |
| Review panel + CI popover | Review state, approval counts, individual commit statuses, and unresolved review comments, on desktop and mobile. |
| Composer `#` references | Search pull requests from the composer; access is re-checked live at submit time. |
| [Agent tools (MCP)](docs/agent-tools.md) | Two tools on task sessions — read CI for a ref (optionally with each failing job's log) and get/open/ready the task's pull request. |
| [Issue watches](docs/issue-watches.md) | Poll repositories for matching issues and file each new one as a Kanban task, with the column, profiles and prompt you choose. |
| [Automation conditions](docs/automations.md) | Forgejo and Gitea webhooks as native triggers in Kandev's automation editor: a push, a pull request event, or (Forgejo 12+) a finished Actions run, with branch, author, label, draft, workflow and fork filters. Every delivery's signature is verified. |
| [Review watches](docs/review-watches.md) | File a task for every pull request that asks for your review, with an interpolated prompt and a checkout of the branch; fork pull requests never start an agent on their own; finished pull requests can have their task archived. |
| Git credentials | HTTPS clone and push credentials for task sessions, via Kandev's Git credential broker. The configured token is issued only for `owner/repo` paths on the configured instance, only to complete task/session/repository scopes, and never while the workspace integration is switched off. Without this, Kandev cannot start a task on a Forgejo repository. |

## Requirements

- Kandev **0.95.0** or newer, which is where plugin `agent_tools` are served over
  Kandev's MCP endpoint. The source-control half alone needs only `v0.88.0`, and
  a `0.88`–`0.94` host will happily run release `0.1.2`; it would also install
  `0.2.0` and silently never show the agent tools, which is why the floor moved
  rather than staying put.
- **Forgejo 7.0+** or **Gitea 1.27+**, reachable from the Kandev backend and
  serving the REST v1 API at `<instance URL>/api/v1`. Some features need a newer
  release; see "Supported versions" for the table, and "Older instances" for what
  to install on an older Gitea.
- A personal access token. Every supported host enforces token scopes, and the
  plugin needs all four of:

  | Scope | Needed for |
  | --- | --- |
  | `read:repository` | Repository search, branches, pull requests, reviews, commit statuses |
  | `write:repository` | Opening pull requests (omit for a read-only install) |
  | `read:user` | The connection test and the account shown in settings |
  | `read:issue` | Composer `#` pull-request search, and reading issues for issue watches |

  A token missing `read:user` reports "not connected" even though everything
  else works, and one missing `read:issue` returns an empty composer picker.

## Install

1. Download `kandev-plugin-forgejo-<version>.tar.gz` from
   [Releases](https://github.com/naerymdan/kandev-plugin-forgejo/releases).
2. In Kandev: **Settings → Plugins → Install from file**.
3. Open **Settings → Plugins → Forgejo** and set:
   - **Instance URL** — e.g. `https://codeberg.org` or `http://forge.lan:3000`.
   - **Access token** — with the scopes above. Stored in Kandev's encrypted
     vault and readable only inside the plugin process.
4. Use **Test connection** in **Settings → Integrations → Forgejo** to confirm
   the instance is reachable.

Changing the configuration restarts the plugin; that is expected.

> **Upgrading from 0.2.0 or earlier?** Releases that change the plugin's
> declared capabilities require re-approving them in Kandev, and the existing
> repository, review and pull-request features stay denied until you do. See
> [Testing on a real Kandev](docs/testing-on-kandev.md#read-this-before-upgrading-an-existing-install).

## Forgejo and Gitea

Forgejo is a hard fork of Gitea. Forgejo has diverged substantially in its web
UI and features, but it still serves the Gitea-compatible REST surface at
`/api/v1` and continues to declare its compatibility level in the version
string (`16.0.5+gitea-1.22.0`). Every feature so far uses only endpoints and
fields that exist on both, so one plugin serves either host; see "Supported
versions" for what happens when that stops being possible.

Every request/response shape this plugin depends on was compared field by field
across Forgejo 13.0.5, Forgejo 16.0.5, and Gitea 1.24.7. They are identical;
the only difference is the version string itself.

The connection panel labels which flavor it detected. **One behaviour branches
on it**: the CI result automation condition, which only Forgejo 12+ can serve
(see "Supported versions"). Detection asks for Forgejo's own `/api/forgejo/v1/version`
namespace, which Gitea does not serve, rather than matching on the
`+gitea-<compat>` suffix — that suffix is a compatibility declaration Forgejo
could stop publishing as it diverges further, and the namespace probe keeps
working if it does.

Two places where the shared surface differs from what a GitHub-shaped client
would assume, and which this plugin handles explicitly:

- A combined commit status uses `state`, but each entry inside it uses
  `status`. Reading `state` on the entries yields no checks at all.
- REST v1 has **no draft flag** when creating a pull request. Draft is expressed
  with a `WIP:` title prefix, which both web UIs recognize; an existing marker
  is not doubled.

### Supported versions

**Floor: Forgejo 7.0, Gitea 1.27.** Newer is always fine.

The plugin targets **recent Forgejo** first. Where a feature can be built on
Forgejo but not on Gitea, it ships for Forgejo only, rather than being held back
to what both hosts share.

Each feature has **one minimum version per host**. Below it the feature reports
itself unavailable, with the reason; there is no reduced mode. The one
exception predates this rule: the `ci` tool reads commit statuses everywhere and
adds Actions jobs and their logs where the host serves them, because the
commit status is also the only place CI from outside the forge appears.

| Feature | Forgejo | Gitea |
| --- | --- | --- |
| Repository picker, pull request creation, linking, glyphs, review panel, composer references, git credentials | 7.0 | 1.27 |
| Pull request actions (merge, review, request reviewers, comment, update branch) | 7.0 | 1.27 |
| Issue watches, review watches | 7.0 | 1.27 |
| `ci` agent tool: overall state and each check | 7.0 | 1.27 |
| `ci` agent tool: Actions jobs and their logs | 16.0 | 1.27 |
| `pr` agent tool | 7.0 | 1.27 |
| Push and pull request automation conditions | 7.0 | 1.27 |
| Pull request condition: "when commented on" | 11.0 | 1.27 |
| CI result automation condition | 12.0 | not supported |

How the matrix is tested: CI runs the live contract suite against **every
Forgejo long-term-support release from the floor up (7, 11, 15) and the current
Forgejo release (17)**, plus the current Gitea (1.27), and asserts the detected
flavor matches the host under test. A release between two LTS releases is
assumed to behave like the LTS before it. A feature whose minimum falls between
them names that release, read from Forgejo's source, and is tested at the next
LTS.

Forgejo 7 and 11 are past their end of life upstream. Supported here means the
plugin keeps working with them; it is not a recommendation to run a Forgejo
release that no longer gets security fixes.

### Older instances

**Plugin 0.4 is the last release line that supports Gitea 1.20 to 1.26.** Gitea
supports only its most recent release upstream, so the plugin's Gitea floor
follows it. On an older Gitea, install the newest 0.4.x release and stay on it.
A raised floor is always a breaking change with its own changelog entry, and
names the last plugin release for the hosts it leaves behind.

What 0.4 was verified against, with the full live contract suite (repository
discovery, review snapshot with CI status and approvals, composer reference
search and authorization, and pull-request creation):

| Host | Versions verified | Result |
| --- | --- | --- |
| Gitea | 1.14.7, 1.15.11, 1.16.9, 1.17.4, 1.18.5, 1.19.4, 1.20.6, 1.21.11, 1.22.6, 1.23.8, 1.24.7, 1.25.5, 1.26.4, 1.27.3 | 14/14 fully working |
| Forgejo | 7.0.16, 8.0.3, 9.0.3, 10.0.3, 11.0.16, 12.0.4, 13.0.5, 14.0.5, 15.0.9, 16.0.5 | 10/10 fully working |

Gitea before 1.20 also worked with 0.4 but was never supported, because its
token scopes differ:

| Gitea range | Token behavior |
| --- | --- |
| ≤ 1.18 | No scope system at all. The `scopes` field is ignored and a token has **full account access**, strictly worse for an integration credential. |
| 1.19 | Scopes exist but use an incompatible vocabulary: `read:repository` is rejected outright, and a token minted through the API comes back with `scopes: null` and is then **denied every write**. |

## Documentation

| Document | What is in it |
| --- | --- |
| [Issue watches](docs/issue-watches.md) | Turning Forgejo issues into Kanban tasks: every setting, what the feature guarantees, and the capabilities it needs. |
| [Automation conditions](docs/automations.md) | Webhook triggers for Kandev automations: setting up the Forgejo webhook, every filter, the normalized `{{data.*}}` fields, and why an upgrade means reconfiguring the webhook. |
| [Review watches](docs/review-watches.md) | Filing a task per pull request awaiting your review: scope, drafts, fork handling, and cleanup by archiving or, where the host cannot, completing the task. |
| [Agent tools](docs/agent-tools.md) | The two MCP tools on task sessions, what CI data each host version can actually serve, and what the pair costs in prompt tokens. |
| [Configuration and operations](docs/configuration.md) | Connection scope, the per-workspace enable switch, headless install and action bodies over HTTP, and the unsigned badge. |
| [Verifying a release candidate](docs/rc-verification.md) | A runbook for an agent with a real Kandev and Forgejo: what to run, what counts as a pass, how to report. |
| [Security](docs/security.md) | Credential handling, the capability surface, and what the security pipeline checks. |
| [Development](docs/development.md) | Repo setup against the sibling SDK checkout, the live contract tests, the code layout, and the design decisions behind it. |
| [Testing on a real Kandev](docs/testing-on-kandev.md) | The pre-release verification stage the automated suites cannot cover. |

## License

MIT — see [LICENSE](LICENSE).
