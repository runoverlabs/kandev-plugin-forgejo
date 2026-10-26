/** What a watch files tasks for. A record from before review watches has no kind. */
export type WatchKind = "issue" | "review";

/** One watch as the backend serializes it. */
export type Watch = {
  id: string;
  name: string;
  workflow_id: string;
  workflow_step_id: string;
  agent_profile_id?: string;
  executor_profile_id?: string;
  prompt?: string;
  start_agent?: boolean;
  repository_id?: string;
  base_branch?: string;
  repos?: { owner: string; name: string }[];
  labels?: string[];
  state?: string;
  query?: string;
  enabled?: boolean;
  poll_interval_seconds?: number;
  max_inflight_tasks?: number;
  dedup_scope?: string;
  kind?: WatchKind;
  review_scope?: string;
  include_drafts?: boolean;
  cleanup_policy?: string;
  fork_workflow_step_id?: string;
  last_cleanup_note?: string;
  last_polled_at?: string;
  last_error?: string;
  last_error_at?: string;
};

export type WorkflowOption = {
  id: string;
  name: string;
  steps: { id: string; name: string; is_start_step?: boolean; auto_starts_agent?: boolean }[];
};

export type WatchOptions = {
  workflows?: WorkflowOption[];
  agent_profiles?: { id: string; name: string }[];
  executor_profiles?: { id: string; name: string }[];
  default_interval?: number;
  min_interval?: number;
  default_max_inflight?: number;
  min_review_interval?: number;
  default_review_prompt?: string;
  archive_granted?: boolean;
};

export type RunResult = {
  matched?: number;
  created?: number;
  duplicates?: number;
  throttled?: number;
  drafts?: number;
  skipped_forks?: number;
  archived?: number;
  completed?: number;
  cleanup_note?: string;
  errors?: string[];
};

/** The editable shape of the form, all strings so inputs stay controlled. */
export type Draft = {
  id: string;
  name: string;
  repos: string;
  labels: string;
  state: string;
  query: string;
  workflowId: string;
  workflowStepId: string;
  agentProfileId: string;
  executorProfileId: string;
  prompt: string;
  pollIntervalSeconds: string;
  maxInflightTasks: string;
  dedupScope: string;
  startAgent: boolean;
  kind: WatchKind;
  reviewScope: string;
  includeDrafts: boolean;
  cleanupPolicy: string;
  forkStepId: string;
};

export const NONE = "__none__";

/** The column a new card lands in by default: the start step, else the first. */
export function startStep(workflow: WorkflowOption | undefined): WorkflowOption["steps"][number] | undefined {
  return workflow?.steps?.find((entry) => entry.is_start_step) ?? workflow?.steps?.[0];
}

export function emptyDraft(options: WatchOptions, kind: WatchKind): Draft {
  const workflow = options.workflows?.[0];
  const step = startStep(workflow);
  return {
    id: "",
    name: "",
    repos: "",
    labels: "",
    state: "open",
    query: "",
    workflowId: workflow?.id ?? "",
    workflowStepId: step?.id ?? "",
    agentProfileId: "",
    executorProfileId: "",
    prompt: "",
    pollIntervalSeconds: String(options.default_interval ?? 300),
    maxInflightTasks: String(options.default_max_inflight ?? 5),
    dedupScope: "watch",
    startAgent: false,
    kind,
    reviewScope: "user_and_teams",
    includeDrafts: false,
    cleanupPolicy: "never",
    forkStepId: "",
  };
}

export function toDraft(watch: Watch, options: WatchOptions, kind: WatchKind): Draft {
  return {
    ...emptyDraft(options, kind),
    id: watch.id,
    ...filterFields(watch),
    ...taskFields(watch, options),
    ...reviewFields(watch),
  };
}

function filterFields(watch: Watch): Pick<Draft, "name" | "repos" | "labels" | "state" | "query"> {
  return {
    name: watch.name ?? "",
    repos: repoList(watch),
    labels: (watch.labels ?? []).join(", "),
    state: watch.state || "open",
    query: watch.query ?? "",
  };
}

function taskFields(
  watch: Watch,
  options: WatchOptions,
): Pick<
  Draft,
  | "workflowId"
  | "workflowStepId"
  | "agentProfileId"
  | "executorProfileId"
  | "prompt"
  | "pollIntervalSeconds"
  | "maxInflightTasks"
  | "dedupScope"
  | "startAgent"
> {
  return {
    workflowId: watch.workflow_id ?? "",
    workflowStepId: watch.workflow_step_id ?? "",
    agentProfileId: watch.agent_profile_id ?? "",
    executorProfileId: watch.executor_profile_id ?? "",
    prompt: watch.prompt ?? "",
    pollIntervalSeconds: String(watch.poll_interval_seconds ?? options.default_interval ?? 300),
    maxInflightTasks: String(watch.max_inflight_tasks ?? options.default_max_inflight ?? 5),
    dedupScope: watch.dedup_scope || "watch",
    startAgent: watch.start_agent === true,
  };
}

function reviewFields(watch: Watch): Pick<Draft, "reviewScope" | "includeDrafts" | "cleanupPolicy" | "forkStepId"> {
  return {
    reviewScope: watch.review_scope || "user_and_teams",
    includeDrafts: watch.include_drafts === true,
    cleanupPolicy: watch.cleanup_policy || "never",
    forkStepId: watch.fork_workflow_step_id ?? "",
  };
}

/** The watch's repositories as the operator typed them. */
export function repoList(watch: Watch): string {
  return (watch.repos ?? []).map((repo) => `${repo.owner}/${repo.name}`).join(", ");
}

/** Splits a comma- or newline-separated operator list into trimmed entries. */
function splitList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter((entry) => entry.length > 0);
}

export function draftToBody(draft: Draft): Record<string, unknown> {
  return {
    ...(draft.id ? { id: draft.id } : {}),
    name: draft.name.trim(),
    repos: splitList(draft.repos),
    labels: splitList(draft.labels),
    state: draft.state,
    query: draft.query.trim(),
    workflow_id: draft.workflowId,
    workflow_step_id: draft.workflowStepId,
    agent_profile_id: draft.agentProfileId,
    executor_profile_id: draft.executorProfileId,
    prompt: draft.prompt,
    start_agent: draft.startAgent,
    poll_interval_seconds: Number(draft.pollIntervalSeconds) || 0,
    max_inflight_tasks: Number(draft.maxInflightTasks) || 0,
    dedup_scope: draft.dedupScope,
    // The kind is fixed at creation; the backend rejects a change, and sending
    // it on update just restates it.
    kind: draft.kind,
    ...(draft.kind === "review"
      ? {
          review_scope: draft.reviewScope,
          include_drafts: draft.includeDrafts,
          cleanup_policy: draft.cleanupPolicy,
          fork_workflow_step_id: draft.forkStepId,
        }
      : {}),
  };
}

export function summarize(result: RunResult): string {
  const parts = [`${result.created ?? 0} created`];
  if (result.duplicates) parts.push(`${result.duplicates} already tracked`);
  if (result.throttled) parts.push(`${result.throttled} held back by the task limit`);
  if (result.drafts) parts.push(`${result.drafts} draft(s) left out`);
  if (result.skipped_forks) {
    parts.push(`${result.skipped_forks} from a fork left out (no safe column to put them in)`);
  }
  const retired = (result.archived ?? 0) + (result.completed ?? 0);
  if (retired) {
    parts.push(
      `${result.archived ?? 0} archived, ${result.completed ?? 0} completed because their pull request is finished`,
    );
  }
  return `${result.matched ?? 0} matched — ${parts.join(", ")}.`;
}

/** The summary of a manual cleanup, which has no discovery numbers. */
export function summarizeCleanup(result: RunResult): string {
  const archived = result.archived ?? 0;
  const completed = result.completed ?? 0;
  if (archived + completed === 0) return "Nothing to clean up: no tracked pull request has finished.";
  return `${archived} archived, ${completed} completed.`;
}

export function formatWhen(value?: string): string {
  if (!value) return "never";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

/** What a watch row says it matches. */
export function watchQueryLine(watch: Watch, review: boolean): string {
  const repos = repoList(watch);
  const labels = (watch.labels ?? []).join(", ");
  if (!review) return repos + (labels ? ` — ${labels}` : "");
  return [
    watch.review_scope === "user" ? "Requested of me" : "Requested of me or my teams",
    repos ? `in ${repos}` : "in every repository",
    labels,
  ]
    .filter(Boolean)
    .join(" — ");
}

/** The shortest poll interval the backend accepts for this kind of watch. */
export function minPollInterval(options: WatchOptions, review: boolean): number {
  return (review ? options.min_review_interval : options.min_interval) ?? (review ? 60 : 30);
}
