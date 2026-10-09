# Plans towards 0.4

Implementation plans for work that separate sessions will pick up. Each plan is
written to be executed without the conversation that produced it: it names the
files, the seams, the tests, and the things that must be checked against a real
instance before they are encoded.

| Plan | What it delivers |
| --- | --- |
| [Step 0: hotfixes](00-hotfixes.md) | Two bugs in the released 0.3.0 that should ship first as 0.3.1 |
| [PR actions](pr-actions.md) | Merge, review, request reviewers, mergeability, diff and files, comments and labels: as agent-tool ops and as buttons in the review panel |
| [Review watches and cleanup](review-watches.md) | Watches that file a task for each PR awaiting your review, plus cleanup of the tasks they create |

Deferred, not planned here: [#7](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/7)
(webhook receiver and native automation triggers) and
[#8](https://github.com/runoverlabs/kandev-plugin-forgejo/issues/8) (CI auto-fix,
auto-merge and PR lifecycle prompts).

## How these were produced, and how far to trust them

The goal is parity with Kandev's built-in GitHub integration, as far as Forgejo
supports it. The plans rest on three surveys made against Kandev **v0.97.0**:
what the GitHub integration does, what a plugin can implement, and what the
Forgejo/Gitea API offers.

- **Kandev facts** were read from the source and cite files. They are solid for
  v0.97.0 and may shift in later releases.
- **Gitea facts** for `review_requested`, `reviewed` and similar search filters
  were checked against Gitea's published swagger. Where a plan says "verified from
  swagger" that is what it means.
- **Forgejo facts** are mostly **unverified**. codeberg.org is not reachable from
  the development sandbox. Anything about Forgejo 7 or 16, and anything a plan
  marks "assumed", must be pinned by a live test against the CI containers
  (Gitea 1.20 and 1.27, Forgejo 7 and 16) before it is encoded. This is the
  repo's own rule: verify a provider claim against a real instance first.

## Order of work

```text
Step 0  hotfixes (0.3.1)  ──┐
                            ├──►  shared groundwork  ──►  PR actions
                            │                        └─►  Review watches and cleanup
```

1. **Step 0** first. Both bugs are in released code, and one of them destroys
   user data. The review-watches plan also depends on one of the fixes.
2. **Shared groundwork** (below) before either feature plan, because both need a
   second user on the live CI containers.
3. **PR actions** and **review watches** are independent after that and can run
   in parallel in separate sessions. Where they touch the same files, merge order
   matters; see "Collisions".

### Shared groundwork

Both plans need to act as a second user, which the live CI seed does not have
today. Do this once, in its own change, and record the results.

- Extend the seed step in `.github/workflows/ci.yml` (the `live` job) to create
  a non-admin user `reviewer` with write access to `kandev/demo`, and a user
  `readonly` with read access only. The seed currently creates the admin user
  `kandev` with an `all`-scoped token, the repo `kandev/demo`, two feature
  branches and one PR.
- Pass `KANDEV_FORGEJO_REVIEWER_TOKEN` and `KANDEV_FORGEJO_READONLY_TOKEN` to
  the test step, and have the live tests skip when they are absent, like the
  existing ones.
- Add a "Verified against" table (the README already has one for versions) to
  `docs/development.md`, so each provider fact a plan lists as unverified ends up
  recorded once it is checked.

### Collisions

Both feature plans edit `manifest.yaml` and `internal/plugin/manifest_test.go`
(action counts and keys, pinned deliberately), `internal/plugin/runtime.go`,
`CHANGELOG.md`, `README.md` and several `docs/` pages. They do not share
implementation files otherwise: PR actions lives in `internal/forgejo/`
(a new `pullactions.go`), `ui/src/source-control.ts` and `agenttools.go`; review
watches lives in `internal/watches/`, `internal/plugin/watches.go` and
`ui/src/watches-panel.ts`.

Merge the shared groundwork first, then whichever feature finishes first;
the second rebases the manifest and test counts. Keep manifest-count edits in
their own small commit so the rebase is mechanical.

## Conventions every session must follow

These come from [`AGENTS.md`](../../AGENTS.md); read it before starting. The
points that bite:

- **Architecture rule.** `internal/forgejo` owns every URL, pagination detail,
  auth detail and error mapping. Neutral packages (`internal/sourcecontrol`,
  `internal/watches`) declare ports and own decisions, and contain no Forgejo
  detail. `internal/plugin` only wires.
- **The manifest is a contract.** Every routed action must be declared. Changing
  `capabilities` invalidates the operator's approval for every capability, so a
  plan that needs a new capability must say so loudly and add an upgrade note.
  **Neither feature plan needs a new capability in its recommended form.**
- **The token never reaches a response, log, cursor or error**, and provider
  error bodies are never forwarded. Map to fixed, token-safe strings.
- **Workspace id comes from `request.Context.WorkspaceID`**, never from a body.
- **Break the code and confirm the test fails.** Every behaviour change needs a
  test, and `make lint && make test` must pass. Run `make security` if
  dependencies change.
- **Docs and changelog in the same change**, under `## Unreleased`.
- **Conventional Commits.** No attribution lines in commits or PRs.
- A release needs its own version string (`0.4.0-rc.1`); Kandev returns 409 for
  a version that is already installed. Work through
  [`testing-on-kandev.md`](../testing-on-kandev.md) before a release; the fake
  hosts cannot exercise the host seam.

## Decisions that need an owner

Each is stated in the plan that raises it, with a recommendation. They are
collected here so none is decided by accident.

| # | Decision | Plan | Recommendation |
| --- | --- | --- | --- |
| 1 | Who may click merge and review: any authenticated task member, or Kandev admins only (`access: admin`) | PR actions | Any authenticated member, with a server-checked head SHA so a stale click cannot merge unseen commits |
| 2 | May agents merge by default | PR actions | No. A per-workspace switch, default off for merge; review and comment stay on |
| 3 | Attribution: the Forgejo identity is always the shared operator token. Add a "via Kandev (user)" footer to merges and reviews? | PR actions | Yes for reviews and comments; the UI also states the acting account |
| 4 | Agent tool budget: compress into the existing two tools, or raise the ceiling or add a third tool | PR actions | Compress first; measure before deciding |
| 5 | Cleanup vocabulary: `auto` needs to read task messages, which needs a new `api_read` resource and therefore re-approval | Review watches | Ship `never` and `when_closed` now; add `auto` later with the capability |
| 6 | Fork PRs: how to stop an unattended agent running fork-controlled content | Review watches | Dedicated no-auto-start column, otherwise skip fork PRs and report them |
| 7 | Archive needs an operator grant (`host.v2.write:tasks`) | Review watches | Use it when granted, fall back to completing the task, and show the state in the panel |
