import type {
  ChangeRequestDetailCheck,
  ChangeRequestDetailComment,
  ChangeRequestDetailModel,
  ChangeRequestDetailPerson,
  ChangeRequestDetailReview,
} from "./detail";

/**
 * The `change_requests.details` response, as the backend serializes it
 * (`sourcecontrol.ChangeRequestDetails`). Every field is optional here: the
 * panel must survive an older or partial reply, and `null` means "the instance
 * did not say", which is not the same as zero or false.
 */
export type DetailsResponse = {
  provider_id?: string;
  review_key?: string;
  number?: number;
  title?: string;
  url?: string;
  state?: string;
  author?: string;
  description?: string;
  created_at?: string;
  source_branch?: string;
  target_branch?: string;
  head_sha?: string;
  additions?: number | null;
  deletions?: number | null;
  merge?: MergeResponse;
  pipeline_state?: string;
  checks?: { id?: string; label?: string; state?: string; detail?: string; url?: string }[];
  reviews?: { id?: number; author?: string; state?: string; body?: string; submitted_at?: string }[];
  requested_reviewers?: string[];
  comments?: { id?: number; author?: string; body?: string; created_at?: string }[];
  truncated?: string[];
  actor?: string;
};

export type MergeResponse = {
  styles?: string[];
  default_style?: string;
  delete_branch_default?: boolean;
  mergeable?: boolean | null;
  can_merge?: boolean | null;
  required_approvals?: number | null;
  approvals?: number;
  protection_visible?: boolean;
  blockers?: string[];
};

function person(name: string | undefined): ChangeRequestDetailPerson {
  return { name: (name ?? "").trim() || "unknown" };
}

/**
 * Review states in the host's vocabulary. Forgejo says COMMENT where the host
 * expects COMMENTED; mapping it here is what keeps the review row from showing
 * a raw provider string.
 */
function reviewState(state: string | undefined): string {
  const upper = (state ?? "").toUpperCase();
  return upper === "COMMENT" ? "COMMENTED" : upper || "COMMENTED";
}

/**
 * Turns the details response into the model `host.ui.ChangeRequestDetail`
 * renders. The host requires numbers for the line counts, so an instance that
 * does not report them (older Gitea and Forgejo) shows zero; that is a limit of
 * the host model, not a claim about the pull request.
 */
export function toDetailModel(details: DetailsResponse): ChangeRequestDetailModel {
  const draft = details.state === "draft";
  const reviews: ChangeRequestDetailReview[] = (details.reviews ?? []).map((review, index) => ({
    id: String(review.id ?? index),
    author: person(review.author),
    state: reviewState(review.state),
    ...(review.body ? { body: review.body } : {}),
    ...(review.submitted_at ? { createdAt: review.submitted_at } : {}),
  }));
  const checks: ChangeRequestDetailCheck[] = (details.checks ?? []).map((check, index) => ({
    id: check.id ?? String(index),
    name: check.label ?? "check",
    state: check.state ?? "neutral",
    ...(check.detail ? { output: check.detail } : {}),
    ...(check.url ? { url: check.url } : {}),
  }));
  const comments: ChangeRequestDetailComment[] = (details.comments ?? []).map((comment, index) => ({
    id: String(comment.id ?? index),
    author: person(comment.author),
    body: comment.body ?? "",
    ...(comment.created_at ? { createdAt: comment.created_at } : {}),
  }));

  const model: ChangeRequestDetailModel = {
    providerId: details.provider_id ?? "forgejo",
    reviewKey: details.review_key ?? "",
    number: details.number ?? 0,
    title: details.title ?? "",
    url: details.url ?? "",
    state: draft ? "open" : (details.state ?? "open"),
    author: person(details.author),
    sourceBranch: details.source_branch ?? "",
    targetBranch: details.target_branch ?? "",
    additions: details.additions ?? 0,
    deletions: details.deletions ?? 0,
    reviews,
    requestedReviewers: (details.requested_reviewers ?? []).map((name) => person(name)),
    checks,
    comments,
  };
  if (draft) model.draft = true;
  if (details.description) model.description = details.description;
  if (details.created_at) model.createdAt = details.created_at;
  const decision = reviewDecision(details);
  if (decision) model.reviewState = decision;
  return model;
}

/** The overall review state the host's header shows, derived from blockers. */
function reviewDecision(details: DetailsResponse): string | undefined {
  const blockers = details.merge?.blockers ?? [];
  if (blockers.includes("changes_requested")) return "CHANGES_REQUESTED";
  if ((details.merge?.approvals ?? 0) > 0) return "APPROVED";
  return undefined;
}

const BLOCKER_TEXT: Record<string, string> = {
  conflicts: "This branch has conflicts that must be resolved.",
  checks_failing: "Required checks are failing.",
  checks_pending: "Required checks are still running.",
  approvals_required: "More approvals are required.",
  changes_requested: "Changes were requested.",
  not_open: "",
};

/**
 * Why a merge is not possible, in one sentence, or null. It stays quiet unless
 * the backend named a reason: mergeability is computed asynchronously and can
 * read false for a moment after a push, so an unknown flag must not flash "not
 * mergeable".
 */
export function mergeNotice(details: DetailsResponse): string | null {
  if (details.state === "draft") return "This pull request is a draft (its title starts with WIP:).";
  const reasons = (details.merge?.blockers ?? [])
    .map((code) => BLOCKER_TEXT[code] ?? "")
    .filter((line) => line !== "");
  if (reasons.length === 0) return null;
  const approvals = details.merge?.required_approvals;
  return reasons
    .map((line) =>
      line.startsWith("More approvals") && typeof approvals === "number"
        ? `${approvals} approval${approvals === 1 ? "" : "s"} required (${details.merge?.approvals ?? 0} so far).`
        : line,
    )
    .join(" ");
}

export type MergeChoice = {
  primary: string;
  others: string[];
  /** True when the repository reported no styles and the backend chooses. */
  unreported: boolean;
};

const STYLE_LABELS: Record<string, string> = {
  merge: "Create a merge commit",
  squash: "Squash and merge",
  rebase: "Rebase and merge",
  "rebase-merge": "Rebase, then merge commit",
  "fast-forward-only": "Fast-forward only",
};

export function styleLabel(style: string): string {
  return STYLE_LABELS[style] ?? style;
}

/**
 * The primary style and the alternatives. The backend already orders styles
 * squash, merge, rebase, then the rest; the repository's own default wins when
 * it is allowed. With no styles reported the button still renders and the
 * backend picks, so a failed lookup never locks anyone out.
 */
export function mergeChoice(details: DetailsResponse): MergeChoice {
  const styles = details.merge?.styles ?? [];
  if (styles.length === 0) return { primary: "", others: [], unreported: true };
  const preferred = details.merge?.default_style ?? "";
  const primary = styles.includes(preferred) ? preferred : styles[0]!;
  return { primary, others: styles.filter((style) => style !== primary), unreported: false };
}

/** Whether to offer a merge at all: only an open pull request. */
export function mergeOffered(details: DetailsResponse): boolean {
  return details.state === "open" || details.state === "draft";
}

/** A known blocker disables the button; an unknown state does not. */
export function mergeDisabled(details: DetailsResponse): boolean {
  if (details.state === "draft") return true;
  if (details.merge?.can_merge === false) return true;
  return (details.merge?.blockers ?? []).length > 0;
}

/** A message from any failure the action bridge can throw. */
export function failureMessage(cause: unknown): string {
  if (cause instanceof Error && cause.message.trim()) return cause.message.trim();
  if (typeof cause === "string" && cause.trim()) return cause.trim();
  return "The request failed.";
}
