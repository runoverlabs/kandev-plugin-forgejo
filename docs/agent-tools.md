# Agent tools

Task agents reach Forgejo through two MCP tools, exposed on the `kanban-task`
surface as `kandev_kandev_plugin_forgejo_ci` and `…_pr`. They exist to replace
hand-written `curl` recipes in workflow step prompts — a PR step that had to
scrape a token out of `~/.git-credentials` and hand-roll `/actions/runs`
filtering can ask for `ci` instead.

| Tool | Arguments | Answers |
| --- | --- | --- |
| `ci` | `ref` (branch or SHA), `logs?` (tail lines per failed job), `repo?` | Overall state (`success`/`failure`/`running`/`pending`/`none`) and each job, with the tail of each failed job's log when `logs` is set. |
| `pr` | `op` = `get` / `open` / `ready` / `merge` / `review` / `request_review` / `update` / `comment`, plus `head`, `base`, `title`, `body`, `draft`, `sha`, `event`, `style`, `reviewers`, `delete_branch`, `repo` | The task's pull request. `open` records the Kandev task association, so the review sidebar sees it too. The last five act on the task's linked pull request; see "Acting on a pull request". |

A few properties worth knowing before you write a prompt against them:

- **`open` is idempotent.** Kandev never retries an agent tool — it cannot know
  whether a side effect already landed — so `open` checks for an existing open
  pull request on the same head first, and checks again if the create fails.
  Calling it twice returns the same pull request, marked `already open`.
- **`ready` is the draft flag Forgejo does not have.** REST v1 exposes no draft
  boolean on either host, so a draft is a `WIP:` title prefix and `ready` is a
  title edit. Readying an already-ready pull request sends nothing.
- **`repo` is only needed for a task with more than one Forgejo repository.**
  With several attached, the tools refuse rather than guess, and name them.
- **`logs` is opt-in and bounded.** Without it `ci` fetches no logs at all. With
  it, the tails of at most five failed jobs are inlined under a shared 256 KiB
  budget, and a job whose log the release does not serve says so in place of
  its own section rather than failing the read.
- **They respect the workspace toggle.** With the integration switched off the
  tools return an error and make no request to the instance.
- **A ref with no CI reports `none`, not failure.** An agent must not read
  "nothing ran" as "something broke".

## What CI data you get, by version

`ci` resolves through three surfaces and takes the first that answers, because
this is where Forgejo and Gitea have genuinely diverged. Measured directly:

| Endpoint | Gitea 1.20 | Gitea 1.24 | Gitea 1.27 | Forgejo 7 | Forgejo 13 | Forgejo 16 |
| --- | --- | --- | --- | --- | --- | --- |
| `/actions/runs` | — | — | yes | — | yes | yes |
| `/actions/runs/{id}/jobs` | — | — | yes | — | — | yes |
| `/actions/tasks` | — | yes | yes | — | yes | yes |
| `/actions/jobs/{id}/logs` | — | yes | yes | — | — | yes |
| `/commits/{ref}/status` | yes | yes | yes | yes | yes | yes |

Read from each release's own `swagger.v1.json` and confirmed against running
instances. At the supported floor of either host there is no Actions API at
all, and Forgejo 13 lists runs without serving their jobs or logs — so a chain
that assumed one shape would be wrong on most of the range.

The combined commit status is last and present everywhere. It is not only the
floor fallback: it is the **only** surface that sees CI running outside the
forge, which on self-hosted Forgejo is common — Woodpecker and Drone report
there and appear in `ci` like any other check.

Only a job with an id has a log, which is why the summary prints `job=<id>` for
some entries and not others — a commit-status check has nothing to fetch. Where
a release serves no job logs at all, the job's section reads `(no log
available)` instead of implying the id was wrong. Gitea answers an unknown job
id with HTTP 500 rather than 404 (measured on 1.24.7, against 404 on Forgejo
16.0.5); that is normalized on this one endpoint so an agent chases a stale id
instead of an imagined outage.

## Acting on a pull request

`merge`, `review`, `request_review`, `update` and `comment` act on the pull
request the task has linked. There is no number argument: with one linked pull
request the tool acts on it, with several it refuses and says so (the same rule
`ready` follows), so an agent cannot be pointed at a pull request the task does
not own. `get` now prints `sha=` for the head commit.

- **`merge` needs `sha`**, the head commit from `get`. A pull request that has
  moved since is refused (409) before anything is sent, and the SHA is also sent
  to the instance. `style` must be one the repository allows; omitted, the
  repository's own default is used. `delete_branch` deletes the head branch.
- **Agents cannot merge by default.** A per-workspace switch, shown in the
  workspace's connection panel as "Let agents merge pull requests", is off until
  an operator turns it on. Review, comment, request_review and update are not
  behind it. The switch fails closed: if its state cannot be read, merging is
  refused.
- **Merging is stricter for agents than for people.** With the switch on, the
  tool still refuses a pull request that is not open, has conflicts, is missing
  approvals or has changes requested, or whose CI is failing or still running,
  even when the repository's own protection does not require checks. A pull
  request with no CI at all can be merged. The reply names every reason.
- **`review`** takes `event` (`approve`, `request_changes`, `comment`) and
  `body`; `sha` pins it to the commit that was read. **`request_review`** takes
  `reviewers` (logins). **`update`** takes `style` (`merge` or `rebase`) and
  treats "already up to date" as success. **`comment`** takes `body`.
- **Reviews and comments say they came from Kandev**, as the Forgejo account is
  the operator's shared one.
- **The annotations are now honest.** The tool declares `destructive_hint: true`
  and `idempotent_hint: false`, because merge destroys a branch and a review is
  not repeatable. Kandev passes them on to the agent's MCP client as hints; they
  can change how a client prompts for approval.

## Context cost

Every plugin tool is added to the agent's prompt and nothing can be removed to
make room, so the set is deliberately two tools and the descriptions are written
to be read once. Measured with Kandev's own estimator
(`o200k_base:mcp-tool-json-v1`, the same one behind `EstimatedTokens` in the MCP
attachment evidence):

| Tool | Tokens |
| --- | --- |
| `ci` | 150 |
| `pr` | 177 before the write ops |
| **Total** | **327** before the write ops |

The write ops grow `pr` from 591 to 999 bytes of definition JSON, roughly 300
tokens by the byte ratio (re-measure with the estimator before a release). That
fits the existing ceilings (1,024 bytes a tool, 1,536 together), which were not
raised: the descriptions were compressed instead, and the semantics live in this
page.

For scale, Kandev's own `show_rich_output_kandev` is around 2k tokens by itself.

Log fetching is an argument on `ci` rather than a tool of its own. As a separate
`ci_log` the pair cost 419 tokens and answered "what failed and why" in two
round trips; folding it in costs 327 and answers in one. No `output_schema` is
declared either: it would be shipped to every session for results that are
already self-describing. `internal/plugin/manifest_test.go` holds a byte ceiling
on the set so a third tool has to be argued for.

The catalog is static, so this is the cost on every host. A plugin cannot
withhold a tool from an instance too old to serve it: Kandev builds the catalog
from the installed manifest and the only dynamic input is whether the plugin is
active. On Gitea 1.20 or Forgejo 7 the `logs` argument is therefore declared but
inert, which is one more reason it is an argument rather than a tool.

Whether this is a net win depends on how much prompt text it lets you delete.
Start an Autopilot task and read the recorded MCP attachment evidence, which
reports per-tool `EstimatedTokens`, rather than taking the table above on faith.

---

[← Back to the README](../README.md)
