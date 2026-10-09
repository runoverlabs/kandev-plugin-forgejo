import { describe, expect, it } from "vitest";
import {
  type DetailsResponse,
  mergeChoice,
  mergeDisabled,
  mergeNotice,
  mergeOffered,
  toDetailModel,
} from "../src/panel-model";

const full: DetailsResponse = {
  provider_id: "forgejo",
  review_key: "scope/5#7",
  number: 7,
  title: "Add x",
  url: "https://forge/o/r/pulls/7",
  state: "open",
  author: "alice",
  description: "Does x.",
  created_at: "2026-10-09T10:00:00Z",
  source_branch: "feat",
  target_branch: "main",
  head_sha: "abc1234",
  additions: 12,
  deletions: 3,
  merge: {
    styles: ["squash", "merge"],
    default_style: "squash",
    mergeable: true,
    approvals: 1,
    required_approvals: 1,
    blockers: [],
  },
  pipeline_state: "success",
  checks: [{ id: "1", label: "ci", state: "success", url: "https://ci/1" }],
  reviews: [
    { id: 1, author: "bob", state: "APPROVED", body: "lgtm", submitted_at: "2026-10-09T11:00:00Z" },
    { id: 2, author: "carol", state: "COMMENT" },
  ],
  requested_reviewers: ["dave"],
  comments: [{ id: 9, author: "alice", body: "hi", created_at: "2026-10-09T10:30:00Z" }],
};

describe("toDetailModel", () => {
  it("emits every field the host model requires", () => {
    const model = toDetailModel(full);
    expect(model).toMatchObject({
      providerId: "forgejo",
      number: 7,
      state: "open",
      author: { name: "alice" },
      sourceBranch: "feat",
      targetBranch: "main",
      additions: 12,
      deletions: 3,
      description: "Does x.",
    });
    expect(model.reviews).toHaveLength(2);
    expect(model.requestedReviewers).toEqual([{ name: "dave" }]);
    expect(model.checks[0]).toMatchObject({ name: "ci", state: "success", url: "https://ci/1" });
    expect(model.comments[0]).toMatchObject({ id: "9", body: "hi" });
    expect(model.reviewState).toBe("APPROVED");
  });

  // Forgejo says COMMENT; the host's review row looks up COMMENTED.
  it("maps Forgejo's COMMENT review state to the host's COMMENTED", () => {
    expect(toDetailModel(full).reviews.map((review) => review.state)).toEqual(["APPROVED", "COMMENTED"]);
  });

  it("survives an empty reply without throwing", () => {
    const model = toDetailModel({});
    expect(model.author.name).toBe("unknown");
    expect(model.reviews).toEqual([]);
    expect(model.additions).toBe(0);
  });

  it("turns a null line count into zero rather than NaN", () => {
    const model = toDetailModel({ ...full, additions: null, deletions: null });
    expect(model.additions).toBe(0);
    expect(model.deletions).toBe(0);
  });

  it("reports a draft as open plus the draft flag", () => {
    const model = toDetailModel({ ...full, state: "draft" });
    expect(model.state).toBe("open");
    expect(model.draft).toBe(true);
  });

  it("surfaces requested changes as the review decision", () => {
    const model = toDetailModel({ ...full, merge: { ...full.merge, blockers: ["changes_requested"] } });
    expect(model.reviewState).toBe("CHANGES_REQUESTED");
  });
});

describe("mergeNotice", () => {
  it("is quiet when nothing is known to be wrong", () => {
    expect(mergeNotice(full)).toBeNull();
  });

  // mergeable is computed asynchronously; null is "not yet", never "no".
  it("stays quiet while mergeability is unknown", () => {
    expect(mergeNotice({ ...full, merge: { ...full.merge, mergeable: null, blockers: [] } })).toBeNull();
  });

  it("explains each blocker the backend names", () => {
    const notice = mergeNotice({
      ...full,
      merge: { ...full.merge, blockers: ["conflicts", "checks_failing"], required_approvals: 2, approvals: 1 },
    });
    expect(notice).toContain("conflicts");
    expect(notice).toContain("checks are failing");
  });

  it("states the approval count", () => {
    const notice = mergeNotice({
      ...full,
      merge: { ...full.merge, blockers: ["approvals_required"], required_approvals: 2, approvals: 1 },
    });
    expect(notice).toBe("2 approvals required (1 so far).");
  });

  it("says a draft is a draft, and says nothing for a closed pull request", () => {
    expect(mergeNotice({ ...full, state: "draft" })).toContain("draft");
    expect(mergeNotice({ ...full, state: "merged", merge: { blockers: ["not_open"] } })).toBeNull();
  });
});

describe("merge choice", () => {
  it("prefers the repository default when it is allowed", () => {
    expect(mergeChoice(full)).toEqual({ primary: "squash", others: ["merge"], unreported: false });
    expect(mergeChoice({ merge: { styles: ["squash", "merge", "rebase"], default_style: "rebase" } })).toEqual({
      primary: "rebase",
      others: ["squash", "merge"],
      unreported: false,
    });
  });

  it("falls back to the first allowed style when the default is not allowed", () => {
    expect(mergeChoice({ merge: { styles: ["merge"], default_style: "squash" } }).primary).toBe("merge");
  });

  // A failed or old lookup must not lock the user out of merging.
  it("still offers a merge when the repository reported no styles", () => {
    expect(mergeChoice({ merge: { styles: [] } })).toEqual({ primary: "", others: [], unreported: true });
    expect(mergeChoice({})).toEqual({ primary: "", others: [], unreported: true });
  });
});

describe("merge availability", () => {
  it("is offered only for an open or draft pull request", () => {
    expect(mergeOffered(full)).toBe(true);
    expect(mergeOffered({ ...full, state: "merged" })).toBe(false);
    expect(mergeOffered({ ...full, state: "closed" })).toBe(false);
  });

  it("disables on a known blocker, a draft, or a token that cannot merge", () => {
    expect(mergeDisabled(full)).toBe(false);
    expect(mergeDisabled({ ...full, merge: { ...full.merge, blockers: ["conflicts"] } })).toBe(true);
    expect(mergeDisabled({ ...full, state: "draft" })).toBe(true);
    expect(mergeDisabled({ ...full, merge: { ...full.merge, can_merge: false } })).toBe(true);
  });

  it("does not disable on unknowns", () => {
    expect(mergeDisabled({ ...full, merge: { styles: [], mergeable: null, can_merge: null } })).toBe(false);
  });
});
