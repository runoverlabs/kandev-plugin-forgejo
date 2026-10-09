import { describe, expect, it } from "vitest";
import type { ReviewSummary } from "@kandev/plugin-sdk";
import { toChangeRequestDetail, type ChangeRequestDetailModel } from "../src/detail";

function review(overrides: Partial<ReviewSummary> = {}): ReviewSummary {
  return {
    providerId: "forgejo",
    reviewKey: "https://forge.example.com/repositories/7/pulls/42",
    title: "Add widgets",
    url: "https://forge.example.com/acme/widgets/pulls/42",
    connectionScope: "https://forge.example.com",
    repositoryId: "7",
    changeRequestNumber: 42,
    state: "open",
    ...overrides,
  } as ReviewSummary;
}

/**
 * What the host's ChangeRequestDetail dereferences unconditionally while it
 * renders (header, reviews, checks and comments sections). A model missing any
 * of these throws inside the host and takes the panel down, which is what the
 * previous shape did. Keep this list in step with the host component.
 */
function renderFootprint(detail: ChangeRequestDetailModel) {
  return {
    authorName: detail.author.name,
    reviewCount: detail.reviews.length,
    pendingCount: detail.requestedReviewers.length || detail.pendingReviewCount || 0,
    checkCount: detail.checks.length,
    checkNames: detail.checks.map((check) => check.name.toLowerCase()),
    commentCount: detail.comments.filter((comment) => !comment.author.isBot).length,
    stats: `${detail.additions}/${detail.deletions}`,
    branches: `${detail.sourceBranch}->${detail.targetBranch}`,
    key: detail.reviewKey,
    providerId: detail.providerId,
  };
}

describe("toChangeRequestDetail", () => {
  it("emits every field the host renders, even from a bare snapshot", () => {
    const detail = toChangeRequestDetail(review());

    // Throws on any missing field, as the host would.
    expect(() => renderFootprint(detail)).not.toThrow();
    expect(detail.providerId).toBe("forgejo");
    expect(detail.reviewKey).toBe("https://forge.example.com/repositories/7/pulls/42");
    expect(detail.author).toEqual({ name: "unknown" });
    expect(detail.reviews).toEqual([]);
    expect(detail.requestedReviewers).toEqual([]);
    expect(detail.comments).toEqual([]);
    expect(detail.additions).toBe(0);
    expect(detail.deletions).toBe(0);
  });

  it("maps a full snapshot onto the host model", () => {
    const detail = toChangeRequestDetail(
      review({
        taskStatus: {
          number: 42,
          state: "open",
          pipelineState: "success",
          checks: [
            { id: "1", label: "ci/build", state: "success", detail: "ok", url: "https://ci/1" },
          ],
          review: { state: "approved", approved: 2, requested: 1 },
          unresolvedComments: 3,
          updatedAt: 1755002400000,
        },
      }),
    );

    expect(() => renderFootprint(detail)).not.toThrow();
    expect(detail.number).toBe(42);
    expect(detail.title).toBe("Add widgets");
    expect(detail.state).toBe("open");
    expect(detail.checks).toEqual([
      { id: "1", name: "ci/build", state: "success", output: "ok", url: "https://ci/1" },
    ]);
    // The host looks review labels up by upper-case state.
    expect(detail.reviewState).toBe("APPROVED");
    expect(detail.pendingReviewCount).toBe(1);
    expect(detail.lastSyncedAt).toBe(new Date(1755002400000).toISOString());
  });

  it("falls back to summary fields when no task status is present", () => {
    const detail = toChangeRequestDetail(review());
    expect(detail.number).toBe(42);
    expect(detail.state).toBe("open");
    expect(detail.checks).toEqual([]);
    // Absent optional fields must be omitted, not set to undefined.
    expect("reviewState" in detail).toBe(false);
    expect("pendingReviewCount" in detail).toBe(false);
    expect("lastSyncedAt" in detail).toBe(false);
    expect("draft" in detail).toBe(false);
  });

  it("omits optional check fields that are absent", () => {
    const detail = toChangeRequestDetail(
      review({
        taskStatus: {
          number: 42,
          state: "merged",
          pipelineState: "neutral",
          checks: [{ id: "1", label: "ci/build", state: "pending" }],
        },
      }),
    );
    expect(detail.state).toBe("merged");
    expect(detail.checks[0]).toEqual({ id: "1", name: "ci/build", state: "pending" });
  });

  it("sends a draft as an open request flagged draft, which is how the host shows it", () => {
    const detail = toChangeRequestDetail(
      review({
        taskStatus: { number: 42, state: "draft", pipelineState: "neutral", checks: [] },
      }),
    );
    expect(detail.state).toBe("open");
    expect(detail.draft).toBe(true);
  });
});
