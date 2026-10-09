import type { ReviewSummary } from "@kandev/plugin-sdk";

/**
 * The shape `host.ui.ChangeRequestDetail` renders, mirrored by hand from the
 * host (`apps/web/components/integrations/change-request-detail.tsx`). The SDK
 * types the prop as `unknown`, so nothing else would notice this drifting: the
 * mapper once emitted a different shape entirely, and the host header threw on
 * the first missing field (`detail.author.name`, `detail.reviews.length`).
 * Typing the return value here makes a missing or renamed field a compile error.
 */
export type ChangeRequestDetailPerson = {
  name: string;
  url?: string;
  avatarUrl?: string;
  isBot?: boolean;
  kind?: "user" | "team";
};

export type ChangeRequestDetailReview = {
  id: string;
  author: ChangeRequestDetailPerson;
  state: string;
  body?: string;
  createdAt?: string;
};

export type ChangeRequestDetailCheck = {
  id: string;
  name: string;
  state: string;
  conclusion?: string;
  url?: string;
  output?: string;
  startedAt?: string;
  completedAt?: string;
};

export type ChangeRequestDetailComment = {
  id: string;
  parentId?: string;
  author: ChangeRequestDetailPerson;
  body: string;
  createdAt?: string;
  url?: string;
  path?: string;
  line?: number;
  resolved?: boolean;
};

export type ChangeRequestDetailModel = {
  providerId: string;
  reviewKey: string;
  number: string | number;
  title: string;
  url: string;
  state: string;
  draft?: boolean;
  author: ChangeRequestDetailPerson;
  createdAt?: string;
  mergedAt?: string;
  closedAt?: string;
  sourceBranch: string;
  targetBranch: string;
  additions: number;
  deletions: number;
  description?: string;
  reviewState?: string;
  pendingReviewCount?: number;
  reviews: ChangeRequestDetailReview[];
  requestedReviewers: ChangeRequestDetailPerson[];
  checks: ChangeRequestDetailCheck[];
  comments: ChangeRequestDetailComment[];
  lastSyncedAt?: string;
};

/**
 * The one provider-specific presentation adapter the recipe asks for: it turns
 * a normalized review snapshot into the model `host.ui.ChangeRequestDetail`
 * renders. The host owns the layout for both desktop and mobile.
 *
 * The snapshot is narrow. It carries state, checks and review counts, but not
 * the author, branches, line counts, reviewer list or comment bodies, so those
 * are neutral placeholders rather than invented values: an empty list, zero,
 * and "unknown". Showing real ones needs a detail request the review panel does
 * not make yet. The placeholders keep the panel rendering; they are not a
 * statement about the pull request.
 */
export function toChangeRequestDetail(review: ReviewSummary): ChangeRequestDetailModel {
  const status = review.taskStatus;
  const reported = status?.state ?? review.state;
  // The host shows "draft" for an open request flagged as a draft, so a draft
  // goes out as open plus the flag.
  const draft = reported === "draft";

  const detail: ChangeRequestDetailModel = {
    providerId: review.providerId,
    reviewKey: review.reviewKey,
    number: status?.number ?? review.changeRequestNumber,
    title: review.title,
    url: review.url,
    state: draft ? "open" : reported,
    author: { name: "unknown" },
    sourceBranch: "",
    targetBranch: "",
    additions: 0,
    deletions: 0,
    reviews: [],
    requestedReviewers: [],
    checks: (status?.checks ?? []).map((check) => ({
      id: check.id,
      name: check.label,
      state: check.state,
      ...(check.detail ? { output: check.detail } : {}),
      ...(check.url ? { url: check.url } : {}),
    })),
    comments: [],
  };

  if (draft) {
    detail.draft = true;
  }
  if (status?.review) {
    // The host upper-cases review states when it looks up their labels.
    detail.reviewState = status.review.state.toUpperCase();
    if (status.review.requested !== undefined) {
      detail.pendingReviewCount = status.review.requested;
    }
  }
  if (status?.updatedAt !== undefined) {
    detail.lastSyncedAt = new Date(status.updatedAt).toISOString();
  }
  return detail;
}
