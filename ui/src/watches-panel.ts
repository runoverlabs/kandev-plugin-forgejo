import type { Component, PluginHostApi } from "@kandev/plugin-sdk";

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

type WorkflowOption = {
  id: string;
  name: string;
  steps: { id: string; name: string; is_start_step?: boolean; auto_starts_agent?: boolean }[];
};

type Options = {
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

type RunResult = {
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
type Draft = {
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

const NONE = "__none__";

function emptyDraft(options: Options, kind: WatchKind): Draft {
  const workflow = options.workflows?.[0];
  const step = workflow?.steps?.find((entry) => entry.is_start_step) ?? workflow?.steps?.[0];
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

function toDraft(watch: Watch, options: Options, kind: WatchKind): Draft {
  return {
    ...emptyDraft(options, kind),
    id: watch.id,
    name: watch.name ?? "",
    repos: (watch.repos ?? []).map((repo) => `${repo.owner}/${repo.name}`).join(", "),
    labels: (watch.labels ?? []).join(", "),
    state: watch.state || "open",
    query: watch.query ?? "",
    workflowId: watch.workflow_id ?? "",
    workflowStepId: watch.workflow_step_id ?? "",
    agentProfileId: watch.agent_profile_id ?? "",
    executorProfileId: watch.executor_profile_id ?? "",
    prompt: watch.prompt ?? "",
    pollIntervalSeconds: String(watch.poll_interval_seconds ?? options.default_interval ?? 300),
    maxInflightTasks: String(watch.max_inflight_tasks ?? options.default_max_inflight ?? 5),
    dedupScope: watch.dedup_scope || "watch",
    startAgent: watch.start_agent === true,
    reviewScope: watch.review_scope || "user_and_teams",
    includeDrafts: watch.include_drafts === true,
    cleanupPolicy: watch.cleanup_policy || "never",
    forkStepId: watch.fork_workflow_step_id ?? "",
  };
}

/** Splits a comma- or newline-separated operator list into trimmed entries. */
function splitList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter((entry) => entry.length > 0);
}

function draftToBody(draft: Draft): Record<string, unknown> {
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

/**
 * Turns a failure from the action bridge into something an operator can act on.
 *
 * Backend validation messages are deliberately preserved: unlike a transport
 * error, "at least one repository is required" names exactly what the operator
 * must change. Everything else is replaced, because host envelope errors name
 * the wrong actor and read as though the operator typed something wrong.
 */
function operatorMessage(cause: unknown): string {
  if (typeof console !== "undefined") {
    console.error("[kandev-plugin-forgejo] watch request failed", cause);
  }
  const raw = cause instanceof Error ? cause.message : String(cause ?? "");
  const marker = "kandev-plugin-forgejo: ";
  const index = raw.indexOf(marker);
  if (index >= 0) {
    const message = raw.slice(index + marker.length).trim();
    if (message) return message.charAt(0).toUpperCase() + message.slice(1);
  }
  const watchMarker = "watches: ";
  const watchIndex = raw.indexOf(watchMarker);
  if (watchIndex >= 0) {
    const message = raw.slice(watchIndex + watchMarker.length).trim();
    if (message) return message.charAt(0).toUpperCase() + message.slice(1);
  }
  return "Couldn't reach the Forgejo plugin. Check the Kandev server logs for details.";
}

function summarize(result: RunResult): string {
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
function summarizeCleanup(result: RunResult): string {
  const archived = result.archived ?? 0;
  const completed = result.completed ?? 0;
  if (archived + completed === 0) return "Nothing to clean up: no tracked pull request has finished.";
  return `${archived} archived, ${completed} completed.`;
}

function formatWhen(value?: string): string {
  if (!value) return "never";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

/**
 * Issue watches for the workspace integrations screen.
 *
 * Kandev's native issue-watch surfaces are compiled in per provider, so a
 * plugin has no contract to render into them; this panel is the whole UI for
 * the feature. Everything it shows is workspace-scoped, matching the backend:
 * the host verifies the workspace before the action runs.
 */
export function createWatchesPanel(
  host: PluginHostApi,
  { kind = "issue" }: { kind?: WatchKind } = {},
): Component<{ workspaceId?: string }> {
  const review = kind === "review";
  const noun = review ? "pull request" : "issue";
  return function ForgejoWatchesPanel(props: { workspaceId?: string } = {}) {
    const [watches, setWatches] = host.React.useState<Watch[]>([]);
    const [options, setOptions] = host.React.useState<Options>({});
    const [draft, setDraft] = host.React.useState<Draft | null>(null);
    const [loading, setLoading] = host.React.useState(false);
    const [busy, setBusy] = host.React.useState<string | null>(null);
    const [error, setError] = host.React.useState<string | null>(null);
    const [notice, setNotice] = host.React.useState<string | null>(null);

    const [activeWorkspaceId, setActiveWorkspaceId] = host.React.useState<string | undefined>(() =>
      host.context.getActiveWorkspaceId(),
    );
    host.React.useEffect(() => host.context.subscribeActiveWorkspace(setActiveWorkspaceId), []);
    const workspaceId = (props.workspaceId ?? activeWorkspaceId ?? "").trim();

    const call = host.React.useCallback(
      async <T,>(key: string, body?: unknown, signal?: AbortSignal): Promise<T> =>
        host.api.invokeAction<T>(
          key,
          { workspaceId, ...(body ? { body } : {}) },
          signal ? { signal } : undefined,
        ),
      [workspaceId],
    );

    const load = host.React.useCallback(
      async (signal: AbortSignal) => {
        if (!workspaceId) {
          setWatches([]);
          return;
        }
        setLoading(true);
        setError(null);
        try {
          const [list, config] = await Promise.all([
            call<{ watches?: Watch[] }>("watches.list", { kind }, signal),
            call<Options>("watches.options", undefined, signal),
          ]);
          if (signal.aborted) return;
          setWatches(list?.watches ?? []);
          setOptions(config ?? {});
        } catch (cause) {
          if (!signal.aborted) setError(operatorMessage(cause));
        } finally {
          if (!signal.aborted) setLoading(false);
        }
      },
      [workspaceId, call],
    );

    host.React.useEffect(() => {
      const controller = new AbortController();
      void load(controller.signal);
      return () => controller.abort();
    }, [load]);

    const refresh = host.React.useCallback(() => {
      const controller = new AbortController();
      void load(controller.signal);
    }, [load]);

    const act = host.React.useCallback(
      async (id: string, run: () => Promise<string | null>) => {
        setBusy(id);
        setError(null);
        setNotice(null);
        try {
          const message = await run();
          if (message) setNotice(message);
          refresh();
        } catch (cause) {
          setError(operatorMessage(cause));
        } finally {
          setBusy(null);
        }
      },
      [refresh],
    );

    const save = host.React.useCallback(
      (current: Draft) =>
        act(current.id || "new", async () => {
          await call(current.id ? "watches.update" : "watches.create", draftToBody(current));
          setDraft(null);
          return current.id ? "Watch updated." : "Watch created.";
        }),
      [act, call],
    );

    const runNow = host.React.useCallback(
      (watch: Watch) =>
        act(watch.id, async () => {
          const response = await call<{ result?: RunResult }>("watches.run", { id: watch.id });
          return summarize(response?.result ?? {});
        }),
      [act, call],
    );

    const toggle = host.React.useCallback(
      (watch: Watch, enabled: boolean) =>
        act(watch.id, async () => {
          await call("watches.update", { id: watch.id, enabled });
          return null;
        }),
      [act, call],
    );

    const remove = host.React.useCallback(
      (watch: Watch) =>
        act(watch.id, async () => {
          await call("watches.delete", { id: watch.id });
          return "Watch deleted.";
        }),
      [act, call],
    );

    const reset = host.React.useCallback(
      (watch: Watch) =>
        act(watch.id, async () => {
          const response = await call<{ forgotten?: number }>("watches.reset", { id: watch.id });
          return `Forgot ${response?.forgotten ?? 0} tracked ${noun}(s); the next run will file the ones still ${
            review ? "awaiting your review" : "matching"
          } again.`;
        }),
      [act, call],
    );

    const cleanUp = host.React.useCallback(
      (watch: Watch) =>
        act(watch.id, async () => {
          const response = await call<{ result?: RunResult }>("watches.cleanup", { id: watch.id });
          return summarizeCleanup(response?.result ?? {});
        }),
      [act, call],
    );

    if (!workspaceId) {
      return host.jsx(
        "p",
        { className: "forgejo-watches__empty" },
        `Open a workspace to manage Forgejo ${noun} watches.`,
      );
    }

    const field = (label: string, control: unknown, hint?: string) =>
      host.jsx(
        "div",
        { className: "forgejo-watch-field" },
        host.jsx(host.ui.Label, null, label),
        control,
        hint ? host.jsx("p", { className: "forgejo-watch-field__hint" }, hint) : null,
      );

    const select = (
      value: string,
      onChange: (next: string) => void,
      items: { id: string; name: string }[],
      placeholder: string,
      allowNone = false,
    ) =>
      host.jsx(
        host.ui.Select,
        { value: value || (allowNone ? NONE : ""), onValueChange: (next: string) => onChange(next === NONE ? "" : next) },
        host.jsx(host.ui.SelectTrigger, null, host.jsx(host.ui.SelectValue, { placeholder })),
        host.jsx(
          host.ui.SelectContent,
          null,
          allowNone ? host.jsx(host.ui.SelectItem, { value: NONE }, "None") : null,
          ...items.map((item) => host.jsx(host.ui.SelectItem, { key: item.id, value: item.id }, item.name)),
        ),
      );

    const renderForm = (current: Draft) => {
      const workflows = options.workflows ?? [];
      const steps = workflows.find((entry) => entry.id === current.workflowId)?.steps ?? [];
      const update = (patch: Partial<Draft>) => setDraft({ ...current, ...patch });

      return host.jsx(
        "div",
        { className: "forgejo-watch-form" },
        host.jsx("h4", null, current.id ? "Edit watch" : "New watch"),

        field(
          "Name",
          host.jsx(host.ui.Input, {
            value: current.name,
            placeholder: review ? "My reviews" : "Bug reports",
            onChange: (event: { target: { value: string } }) => update({ name: event.target.value }),
          }),
        ),
        field(
          review ? "Repositories (optional)" : "Repositories",
          host.jsx(host.ui.Input, {
            value: current.repos,
            placeholder: "owner/name, owner/other",
            onChange: (event: { target: { value: string } }) => update({ repos: event.target.value }),
          }),
          review
            ? "Owner/name pairs, separated by commas. Leave empty to follow every repository this token can see."
            : "One or more owner/name pairs, separated by commas.",
        ),
        field(
          "Labels",
          host.jsx(host.ui.Input, {
            value: current.labels,
            placeholder: "bug, needs-triage",
            onChange: (event: { target: { value: string } }) => update({ labels: event.target.value }),
          }),
          `${review ? "A pull request" : "An issue"} must carry every label listed here. Leave empty to match any.`,
        ),
        review
          ? null
          : field(
              "Issue state",
              select(
                current.state,
                (next) => update({ state: next }),
                [
                  { id: "open", name: "Open" },
                  { id: "closed", name: "Closed" },
                  { id: "all", name: "All" },
                ],
                "Open",
              ),
            ),
        review
          ? field(
              "Whose requests",
              select(
                current.reviewScope,
                (next) => update({ reviewScope: next }),
                [
                  { id: "user_and_teams", name: "Me and my teams" },
                  { id: "user", name: "Only me" },
                ],
                "Me and my teams",
              ),
              "Pull requests where this token's account was asked to review, directly or through a team. A pull request drops out once the account answers.",
            )
          : null,
        review
          ? host.jsx(
              "label",
              { className: "forgejo-watch-form__toggle" },
              host.jsx(host.ui.Switch, {
                checked: current.includeDrafts,
                "aria-label": "Also file draft pull requests",
                onCheckedChange: (next: boolean) => update({ includeDrafts: next }),
              }),
              host.jsx("span", null, "Include draft pull requests"),
            )
          : null,
        field(
          "Search",
          host.jsx(host.ui.Input, {
            value: current.query,
            placeholder: "crash",
            onChange: (event: { target: { value: string } }) => update({ query: event.target.value }),
          }),
          review
            ? "Optional free text matched against the title and body, by the instance's search."
            : "Optional free text matched against the title and body. Served by the instance's issue indexer, so a brand-new issue may take a moment to match — and nothing matches if indexing is turned off.",
        ),

        field(
          "Workflow",
          select(
            current.workflowId,
            (next) => {
              const workflow = workflows.find((entry) => entry.id === next);
              const step = workflow?.steps?.find((entry) => entry.is_start_step) ?? workflow?.steps?.[0];
              update({ workflowId: next, workflowStepId: step?.id ?? "" });
            },
            workflows.map((entry) => ({ id: entry.id, name: entry.name })),
            "Choose a workflow",
          ),
        ),
        field(
          "Column",
          select(
            current.workflowStepId,
            (next) => update({ workflowStepId: next }),
            steps.map((entry) => ({ id: entry.id, name: entry.name })),
            "Choose a column",
          ),
          `Where a card lands when a ${noun} matches.`,
        ),
        review
          ? field(
              "Column for fork pull requests",
              select(
                current.forkStepId,
                (next) => update({ forkStepId: next }),
                steps.filter((entry) => !entry.auto_starts_agent).map((entry) => ({ id: entry.id, name: entry.name })),
                "Same column",
                true,
              ),
              "A pull request from a fork is code its author controls, so its task never starts an agent and never checks the fork out. Pick a column that does not start agents on entry; with none, fork pull requests are left out if the column above starts agents.",
            )
          : null,
        field(
          "Agent profile",
          select(
            current.agentProfileId,
            (next) => update({ agentProfileId: next }),
            options.agent_profiles ?? [],
            "Workspace default",
            true,
          ),
        ),
        field(
          "Executor profile",
          select(
            current.executorProfileId,
            (next) => update({ executorProfileId: next }),
            options.executor_profiles ?? [],
            "Workspace default",
            true,
          ),
        ),
        field(
          "Prompt",
          host.jsx(host.ui.Textarea, {
            value: current.prompt,
            rows: 3,
            placeholder: review
              ? (options.default_review_prompt ?? "Review this pull request.")
              : "Investigate this issue and propose a fix.",
            onChange: (event: { target: { value: string } }) => update({ prompt: event.target.value }),
          }),
          review
            ? "The task description, and what the agent is asked. Placeholders: {{pr.number}} {{pr.title}} {{pr.link}} {{pr.author}} {{pr.repo}} {{pr.branch}} {{pr.base_branch}}. Empty uses the default shown."
            : "Sent to the agent when the task starts.",
        ),

        field(
          "Poll interval (seconds)",
          host.jsx(host.ui.Input, {
            type: "number",
            min: (review ? options.min_review_interval : options.min_interval) ?? (review ? 60 : 30),
            value: current.pollIntervalSeconds,
            onChange: (event: { target: { value: string } }) =>
              update({ pollIntervalSeconds: event.target.value }),
          }),
          `At least ${(review ? options.min_review_interval : options.min_interval) ?? (review ? 60 : 30)} seconds.`,
        ),
        field(
          "Open task limit",
          host.jsx(host.ui.Input, {
            type: "number",
            min: 1,
            value: current.maxInflightTasks,
            onChange: (event: { target: { value: string } }) =>
              update({ maxInflightTasks: event.target.value }),
          }),
          `How many of this watch's tasks may be open at once. Matched ${noun}s above the limit wait for a later run.`,
        ),
        review
          ? field(
              "When a pull request is merged or closed",
              select(
                current.cleanupPolicy,
                (next) => update({ cleanupPolicy: next }),
                [
                  { id: "never", name: "Leave the task alone" },
                  { id: "when_closed", name: "Archive the task" },
                ],
                "Leave the task alone",
              ),
              "Tasks are never deleted. Without the archive grant they are marked complete instead.",
            )
          : field(
              "Duplicate handling",
              select(
                current.dedupScope,
                (next) => update({ dedupScope: next }),
                [
                  { id: "watch", name: "One task per watch" },
                  { id: "workspace", name: "One task per issue" },
                ],
                "One task per watch",
              ),
              "“One task per issue” stops a second watch filing the same issue again.",
            ),
        review && current.cleanupPolicy === "when_closed" && options.archive_granted === false
          ? host.jsx(
              "p",
              { className: "forgejo-watch-form__hint", role: "note" },
              "Archiving needs the “Host v2 tasks” grant from your Kandev operator. Until then, finished tasks are completed rather than archived.",
            )
          : null,
        host.jsx(
          "label",
          { className: "forgejo-watch-form__toggle" },
          host.jsx(host.ui.Switch, {
            checked: current.startAgent,
            "aria-label": "Start an agent as soon as the task is created",
            onCheckedChange: (next: boolean) => update({ startAgent: next }),
          }),
          host.jsx("span", null, "Start an agent immediately"),
        ),
        current.startAgent && !current.agentProfileId
          ? host.jsx(
              "p",
              { className: "forgejo-watch-form__hint", role: "note" },
              "Choose an agent profile: an unattended watch should not depend on whichever profile the workspace defaults to.",
            )
          : null,

        host.jsx(
          "div",
          { className: "forgejo-watch-form__actions" },
          host.jsx(
            host.ui.Button,
            {
              type: "button",
              size: "sm",
              disabled: busy !== null,
              onClick: () => void save(current),
            },
            current.id ? "Save watch" : "Create watch",
          ),
          host.jsx(
            host.ui.Button,
            { type: "button", variant: "ghost", size: "sm", onClick: () => setDraft(null) },
            "Cancel",
          ),
        ),
      );
    };

    const renderRow = (watch: Watch) => {
      const repos = (watch.repos ?? []).map((repo) => `${repo.owner}/${repo.name}`).join(", ");
      const labels = (watch.labels ?? []).join(", ");
      return host.jsx(
        "div",
        { className: "forgejo-watch", key: watch.id, "data-enabled": watch.enabled !== false },
        host.jsx(
          "div",
          { className: "forgejo-watch__head" },
          host.jsx("strong", null, watch.name || "Untitled watch"),
          host.jsx(host.ui.Switch, {
            checked: watch.enabled !== false,
            disabled: busy === watch.id,
            "aria-label": `Enable the ${watch.name || "untitled"} watch`,
            onCheckedChange: (next: boolean) => void toggle(watch, next),
          }),
        ),
        host.jsx(
          "p",
          { className: "forgejo-watch__query" },
          review
            ? [
                watch.review_scope === "user" ? "Requested of me" : "Requested of me or my teams",
                repos ? `in ${repos}` : "in every repository",
                labels,
              ]
                .filter(Boolean)
                .join(" — ")
            : repos + (labels ? ` — ${labels}` : ""),
        ),
        host.jsx(
          "p",
          { className: "forgejo-watch__meta" },
          `Last checked ${formatWhen(watch.last_polled_at)}`,
        ),
        watch.last_error
          ? host.jsx("p", { className: "forgejo-watch__error", role: "alert" }, watch.last_error)
          : null,
        watch.last_cleanup_note
          ? host.jsx("p", { className: "forgejo-watch__meta", role: "note" }, watch.last_cleanup_note)
          : null,
        host.jsx(
          "div",
          { className: "forgejo-watch__actions" },
          host.jsx(
            host.ui.Button,
            {
              type: "button",
              variant: "secondary",
              size: "sm",
              disabled: busy === watch.id || watch.enabled === false,
              onClick: () => void runNow(watch),
            },
            busy === watch.id ? "Working…" : "Run now",
          ),
          host.jsx(
            host.ui.Button,
            {
              type: "button",
              variant: "ghost",
              size: "sm",
              disabled: busy === watch.id,
              onClick: () => setDraft(toDraft(watch, options, kind)),
            },
            "Edit",
          ),
          review && watch.cleanup_policy === "when_closed"
            ? host.jsx(
                host.ui.Button,
                {
                  type: "button",
                  variant: "ghost",
                  size: "sm",
                  disabled: busy === watch.id,
                  onClick: () => void cleanUp(watch),
                },
                "Clean up now",
              )
            : null,
          host.jsx(
            host.ui.Button,
            {
              type: "button",
              variant: "ghost",
              size: "sm",
              disabled: busy === watch.id,
              onClick: () => void reset(watch),
            },
            "Forget history",
          ),
          host.jsx(
            host.ui.Button,
            {
              type: "button",
              variant: "ghost",
              size: "sm",
              disabled: busy === watch.id,
              onClick: () => void remove(watch),
            },
            "Delete",
          ),
        ),
      );
    };

    return host.jsx(
      "div",
      { className: "forgejo-watches" },
      host.jsx(
        "p",
        { className: "forgejo-watches__intro" },
        review
          ? "File a task for every pull request that asks for your review. Each watch searches the instance for open requests and files a card for each new one."
          : "Turn Forgejo issues into tasks. Each watch polls the repositories you name and files a card for every new issue that matches.",
      ),
      error ? host.jsx("p", { className: "forgejo-watches__error", role: "alert" }, error) : null,
      notice ? host.jsx("p", { className: "forgejo-watches__notice", role: "status" }, notice) : null,
      loading && watches.length === 0
        ? host.jsx("p", { className: "forgejo-watches__empty" }, "Loading watches…")
        : null,
      !loading && watches.length === 0
        ? host.jsx(
            "p",
            { className: "forgejo-watches__empty" },
            review ? "No review watches yet." : "No watches yet.",
          )
        : null,
      ...watches.map(renderRow),
      draft
        ? renderForm(draft)
        : host.jsx(
            host.ui.Button,
            {
              type: "button",
              variant: "secondary",
              size: "sm",
              onClick: () => setDraft(emptyDraft(options, kind)),
            },
            "Add watch",
          ),
    );
  };
}
