# Step 0: hotfixes (target: 0.3.1)

Two defects in the released 0.3.0 were found while planning 0.4. Both were
checked against the source, not just reported. Ship them before the 0.4 work so
users stop losing data, and so the feature plans start from a sound base.

## Fix A: pausing a watch erases its configuration

**Severity: data loss. Do this first.**

`watchFromRequest` in `internal/plugin/watches.go` (around lines 273-310) folds a
request body onto the stored watch. Its comment says "a field the form omits
keeps its stored value", but these fields are assigned unconditionally:

```go
watch.AgentProfileID = body.AgentProfileID
watch.ExecutorProfileID = body.ExecutorProfileID
watch.Prompt = body.Prompt
watch.RepositoryID = body.RepositoryID
watch.BaseBranch = body.BaseBranch
watch.Query = body.Query
watch.Labels = body.Labels
watch.StartAgent = body.StartAgent
```

The pause/resume toggle in `ui/src/watches-panel.ts` (around lines 291-297) calls
`watches.update` with only `{ id, enabled }`. So pausing or resuming a watch
resets its agent profile, executor profile, prompt, repository, base branch,
query, labels and `start_agent` to empty. The edit form sends everything, so only
the toggle is affected.

The existing test does not catch it: `TestWatchActionsRoundTrip`
(`internal/plugin/watches_test.go`, around lines 134-178) claims "the rest
survive" but asserts only the name and workflow id.

**Fix**

- Make the update path presence-aware. The clearable fields need to distinguish
  "absent" from "empty string": use pointer fields on `watchRequest`
  (`*string`, `*[]string`, `*bool`) or decode into a `map[string]json.RawMessage`
  and apply only keys that are present. Keep the documented rule that an operator
  can clear a prompt or detach a repository by sending an empty value
  explicitly.
- Keep create behaviour unchanged (zero base, `Enabled` true).
- Alternative, if a smaller change is preferred: have the toggle send the whole
  draft. This fixes the symptom but leaves the trap for the next partial caller,
  so prefer the presence-aware body.
- Review watches (see the review-watches plan) add more fields. The same
  presence-aware handling must cover them, so do this fix before that work.

**Tests**

- Strengthen `TestWatchActionsRoundTrip`: create a watch with every field set,
  send `{id, enabled:false}`, and assert **every** field is unchanged.
- Add a case that an explicit empty prompt and an explicit empty `repository_id`
  still clear.
- UI: a test that the toggle sends only `{id, enabled}` is fine once the backend
  is presence-aware; keep the payload small.
- Break the fix and confirm the strengthened test fails.

**Changelog note.** Anyone who paused or resumed a watch on 0.3.0 has lost those
fields and cannot get them back; say so plainly, and tell them to re-edit the
watch. Do not pretend it can be recovered.

## Fix B: issue paging stops early when the server ignores `type=issues`

`ListIssues` in `internal/forgejo/issuesource.go` (around line 76) stops paging
when a page, after being filtered of pull requests, has fewer than
`maxIssuePageLimit` items. On an instance that ignores `type=issues` (older
Gitea does), a full page that contains pull requests ends the walk early and
silently drops issues. The comment at lines 71-75 admits the shape of the
problem.

**Fix.** Decide whether to continue on the **raw** page length (before the pull
request filter), and keep walking until a raw page comes back short or empty,
bounded by the existing page cap. Do not copy the current pattern for pull
requests in the review-watches work.

**Tests.** A fake server that ignores `type` and returns full pages mixing
issues and pull requests; assert every issue across all pages is returned and
that the page cap still bounds the walk. Break the fix and confirm it fails.

## Fix C: the review panel may not render (verify first)

`toChangeRequestDetail` in `ui/src/detail.ts` emits one shape; the host's
`host.ui.ChangeRequestDetail` expects another.

| | Fields |
| --- | --- |
| We emit | `provider`, `providerLabel`, `number`, `title`, `url`, `state`, `pipelineState`, `checks[{id,label,state}]`, `review`, `unresolvedComments`, `updatedAt` |
| Host requires (`ChangeRequestDetailModel`, `kandev/apps/web/components/integrations/change-request-detail.tsx`) | `providerId`, `reviewKey`, `number`, `title`, `url`, `state`, `author`, `sourceBranch`, `targetBranch`, `additions`, `deletions`, `reviews[]`, `requestedReviewers[]`, `checks[{name,state}]`, `comments[]` |

The host header dereferences `detail.author` and `detail.reviews.length`
(`change-request-detail-header.tsx`, around lines 87 and 119), so the real
component likely throws or renders blank. Our tests never render it:
`ui/test/detail.test.ts` checks the mapper against its own output, and
`registration.test.ts` only checks that `ReviewPanel` is a function. The host
model has been unchanged since roughly v0.88, so this may never have worked.

**This has not been reproduced on a live host.** Do not write a fix on the
strength of the source reading alone.

1. **Reproduce.** On a Kandev with a linked Forgejo PR, open the task's Review
   panel. Record whether it is blank, shows an error boundary, or renders.
2. **If it fails**, ship a defensive mapper in 0.3.1: emit every required field,
   with safe values where the snapshot has no data. The snapshot is narrow
   (`ReviewSummary`/`ReviewTaskStatus` in the SDK carry state, checks, review
   counts and unresolved comments, but no author, branches, line counts, reviewer
   list or comment bodies), so empty `reviews`, `requestedReviewers` and
   `comments`, a neutral `author` placeholder, and zero line counts are the honest
   fallbacks. Map `checks[].label` to `name`.
3. **Prevent recurrence.** Add a compile-time guard: mirror `ChangeRequestDetailModel`
   as a local TypeScript type and make `toChangeRequestDetail` return it. The SDK
   types the prop as `unknown`, so nothing else will catch drift.
4. **The real fix is in the PR-actions plan**, which loads full detail through a
   new action and renders it. Keep this hotfix minimal and do not build the new
   data path here.

## Releasing 0.3.1

- Bump `manifest.yaml` to `0.3.1`; the release workflow refuses a tag that does
  not match. Add a `## 0.3.1` changelog section (move anything from `Unreleased`
  that should not ship out of the way, or release from a branch cut at `v0.3.0`
  plus the fixes).
- No capability change, so no re-approval prompt.
- Work through [`../testing-on-kandev.md`](../testing-on-kandev.md): at minimum,
  pause and resume a watch with a prompt and profiles set and confirm nothing
  changes, and open the Review panel.
- Kandev returns 409 for a version that is already installed, so a failed 0.3.1
  needs a new number, not a re-tag.
