import type { ReviewSummary, ReviewTaskAssociation, ReviewTaskStatus } from "@kandev/plugin-sdk";

/*
 * The source-control actions' replies, normalized into the host's review
 * types. A reply is untrusted shape: anything missing or malformed is dropped
 * rather than shown.
 */

export function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

export function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

export function finiteNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export function nonNegativeInteger(value: unknown): number | undefined {
  const number = finiteNumber(value);
  return number !== undefined && Number.isInteger(number) && number >= 0 ? number : undefined;
}

export function positiveInteger(value: unknown): number | undefined {
  const number = nonNegativeInteger(value);
  return number !== undefined && number > 0 ? number : undefined;
}

export function normalizeTaskStatus(value: unknown): ReviewTaskStatus | undefined {
  const source = record(value);
  const number = positiveInteger(source.number);
  const state = text(source.state);
  const pipelineState = text(source.pipeline_state);
  if (
    number === undefined ||
    !(["open", "merged", "closed", "draft"] as const).includes(
      state as "open" | "merged" | "closed" | "draft",
    ) ||
    !(["success", "failure", "pending", "neutral"] as const).includes(
      pipelineState as "success" | "failure" | "pending" | "neutral",
    )
  ) {
    return undefined;
  }

  const checks = Array.isArray(source.checks)
    ? source.checks.flatMap((value) => {
        const check = record(value);
        const id = text(check.id);
        const label = text(check.label);
        const checkState = text(check.state);
        if (
          !id ||
          !label ||
          !(["success", "failure", "pending", "neutral"] as const).includes(
            checkState as "success" | "failure" | "pending" | "neutral",
          )
        ) {
          return [];
        }
        const detail = text(check.detail);
        const url = text(check.url);
        return [{
          id,
          label,
          state: checkState as "success" | "failure" | "pending" | "neutral",
          ...(detail ? { detail } : {}),
          ...(url ? { url } : {}),
        }];
      })
    : [];

  const reviewSource = record(source.review);
  const reviewState = text(reviewSource.state);
  const approved = nonNegativeInteger(reviewSource.approved);
  const required = nonNegativeInteger(reviewSource.required);
  const requested = nonNegativeInteger(reviewSource.requested);
  const review =
    approved !== undefined &&
    (["approved", "changes_requested", "pending"] as const).includes(
      reviewState as "approved" | "changes_requested" | "pending",
    )
      ? {
          state: reviewState as "approved" | "changes_requested" | "pending",
          approved,
          ...(required === undefined ? {} : { required }),
          ...(requested === undefined ? {} : { requested }),
        }
      : undefined;
  const unresolvedComments = nonNegativeInteger(source.unresolved_comments);
  const updatedAt = finiteNumber(source.updated_at);

  return {
    number,
    state: state as "open" | "merged" | "closed" | "draft",
    pipelineState: pipelineState as "success" | "failure" | "pending" | "neutral",
    checks,
    ...(review ? { review } : {}),
    ...(unresolvedComments === undefined ? {} : { unresolvedComments }),
    ...(updatedAt === undefined ? {} : { updatedAt }),
  };
}

export function normalizeReview(providerId: string, value: unknown): ReviewSummary | null {
  const source = record(value);
  const reviewKey = text(source.review_key);
  const title = text(source.title);
  const url = text(source.url);
  const connectionScope = text(source.connection_scope);
  const repositoryId = text(source.repository_id);
  const changeRequestNumber = positiveInteger(source.change_request_number);
  if (
    !reviewKey ||
    !title ||
    !url ||
    !connectionScope ||
    !repositoryId ||
    changeRequestNumber === undefined
  ) {
    return null;
  }
  const state = text(source.state);
  const taskStatus = normalizeTaskStatus(source.task_status);
  return {
    providerId,
    reviewKey,
    title,
    url,
    connectionScope,
    repositoryId,
    changeRequestNumber,
    state,
    ...(taskStatus ? { taskStatus } : {}),
  };
}

export function normalizeAssociation(providerId: string, value: unknown): ReviewTaskAssociation | null {
  const source = record(value);
  const taskId = text(source.task_id);
  const reviewKey = text(source.review_key);
  const connectionScope = text(source.connection_scope);
  const repositoryId = text(source.repository_id);
  const changeRequestNumber = positiveInteger(source.change_request_number);
  if (
    !taskId ||
    !reviewKey ||
    !connectionScope ||
    !repositoryId ||
    changeRequestNumber === undefined
  ) {
    return null;
  }
  return {
    providerId,
    taskId,
    reviewKey,
    connectionScope,
    repositoryId,
    changeRequestNumber,
  };
}
