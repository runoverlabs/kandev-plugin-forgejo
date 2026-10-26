import type { PluginHostApi } from "@kandev/plugin-sdk";
import { NONE, minPollInterval, startStep, type Draft, type WatchOptions } from "./watch-model";

/** What the watch form reads from the panel that shows it. */
export type WatchFormContext = {
  host: PluginHostApi;
  review: boolean;
  noun: string;
  options: WatchOptions;
  busy: string | null;
  setDraft: (draft: Draft | null) => void;
  save: (draft: Draft) => Promise<void>;
};

type Update = (patch: Partial<Draft>) => void;

type SelectSpec = {
  value: string;
  onChange: (next: string) => void;
  items: { id: string; name: string }[];
  placeholder: string;
  allowNone?: boolean;
};

type InputChange = { target: { value: string } };

function field(host: PluginHostApi, label: string, control: unknown, hint?: string) {
  return host.jsx(
    "div",
    { className: "forgejo-watch-field" },
    host.jsx(host.ui.Label, null, label),
    control,
    hint ? host.jsx("p", { className: "forgejo-watch-field__hint" }, hint) : null,
  );
}

function select(host: PluginHostApi, { value, onChange, items, placeholder, allowNone = false }: SelectSpec) {
  return host.jsx(
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
}

/** The create and edit form for one watch. */
export function renderWatchForm(ctx: WatchFormContext, current: Draft) {
  const { host } = ctx;
  const update: Update = (patch) => ctx.setDraft({ ...current, ...patch });
  return host.jsx(
    "div",
    { className: "forgejo-watch-form" },
    host.jsx("h4", null, current.id ? "Edit watch" : "New watch"),
    ...renderMatchFields(ctx, current, update),
    ...renderTaskFields(ctx, current, update),
    ...renderScheduleFields(ctx, current, update),
    ...renderStartAgent(ctx, current, update),
    renderFormActions(ctx, current),
  );
}

/** Which issues or pull requests the watch picks up. */
function renderMatchFields(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host, review } = ctx;
  return [
    field(
      host,
      "Name",
      host.jsx(host.ui.Input, {
        value: current.name,
        placeholder: review ? "My reviews" : "Bug reports",
        onChange: (event: InputChange) => update({ name: event.target.value }),
      }),
    ),
    field(
      host,
      review ? "Repositories (optional)" : "Repositories",
      host.jsx(host.ui.Input, {
        value: current.repos,
        placeholder: "owner/name, owner/other",
        onChange: (event: InputChange) => update({ repos: event.target.value }),
      }),
      review
        ? "Owner/name pairs, separated by commas. Leave empty to follow every repository this token can see."
        : "One or more owner/name pairs, separated by commas.",
    ),
    field(
      host,
      "Labels",
      host.jsx(host.ui.Input, {
        value: current.labels,
        placeholder: "bug, needs-triage",
        onChange: (event: InputChange) => update({ labels: event.target.value }),
      }),
      `${review ? "A pull request" : "An issue"} must carry every label listed here. Leave empty to match any.`,
    ),
    ...(review ? renderReviewMatchFields(ctx, current, update) : [renderIssueState(ctx, current, update), null, null]),
    field(
      host,
      "Search",
      host.jsx(host.ui.Input, {
        value: current.query,
        placeholder: "crash",
        onChange: (event: InputChange) => update({ query: event.target.value }),
      }),
      review
        ? "Optional free text matched against the title and body, by the instance's search."
        : "Optional free text matched against the title and body. Served by the instance's issue indexer, so a brand-new issue may take a moment to match — and nothing matches if indexing is turned off.",
    ),
  ];
}

function renderIssueState(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host } = ctx;
  return field(
    host,
    "Issue state",
    select(host, {
      value: current.state,
      onChange: (next) => update({ state: next }),
      items: [
        { id: "open", name: "Open" },
        { id: "closed", name: "Closed" },
        { id: "all", name: "All" },
      ],
      placeholder: "Open",
    }),
  );
}

/** The issue-state slot is empty for a review watch; these follow it. */
function renderReviewMatchFields(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host } = ctx;
  return [
    null,
    field(
      host,
      "Whose requests",
      select(host, {
        value: current.reviewScope,
        onChange: (next) => update({ reviewScope: next }),
        items: [
          { id: "user_and_teams", name: "Me and my teams" },
          { id: "user", name: "Only me" },
        ],
        placeholder: "Me and my teams",
      }),
      "Pull requests where this token's account was asked to review, directly or through a team. A pull request drops out once the account answers.",
    ),
    host.jsx(
      "label",
      { className: "forgejo-watch-form__toggle" },
      host.jsx(host.ui.Switch, {
        checked: current.includeDrafts,
        "aria-label": "Also file draft pull requests",
        onCheckedChange: (next: boolean) => update({ includeDrafts: next }),
      }),
      host.jsx("span", null, "Include draft pull requests"),
    ),
  ];
}

/** Where a matched card lands and what it asks of the agent. */
function renderTaskFields(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host, review, options } = ctx;
  const workflows = options.workflows ?? [];
  const steps = workflows.find((entry) => entry.id === current.workflowId)?.steps ?? [];
  return [
    field(
      host,
      "Workflow",
      select(host, {
        value: current.workflowId,
        onChange: (next) => {
          const step = startStep(workflows.find((entry) => entry.id === next));
          update({ workflowId: next, workflowStepId: step?.id ?? "" });
        },
        items: workflows.map((entry) => ({ id: entry.id, name: entry.name })),
        placeholder: "Choose a workflow",
      }),
    ),
    field(
      host,
      "Column",
      select(host, {
        value: current.workflowStepId,
        onChange: (next) => update({ workflowStepId: next }),
        items: steps.map((entry) => ({ id: entry.id, name: entry.name })),
        placeholder: "Choose a column",
      }),
      `Where a card lands when a ${ctx.noun} matches.`,
    ),
    review
      ? field(
          host,
          "Column for fork pull requests",
          select(host, {
            value: current.forkStepId,
            onChange: (next) => update({ forkStepId: next }),
            items: steps.filter((entry) => !entry.auto_starts_agent).map((entry) => ({ id: entry.id, name: entry.name })),
            placeholder: "Same column",
            allowNone: true,
          }),
          "A pull request from a fork is code its author controls, so its task never starts an agent and never checks the fork out. Pick a column that does not start agents on entry; with none, fork pull requests are left out if the column above starts agents.",
        )
      : null,
    ...renderProfileFields(ctx, current, update),
    field(
      host,
      "Prompt",
      host.jsx(host.ui.Textarea, {
        value: current.prompt,
        rows: 3,
        placeholder: review
          ? (options.default_review_prompt ?? "Review this pull request.")
          : "Investigate this issue and propose a fix.",
        onChange: (event: InputChange) => update({ prompt: event.target.value }),
      }),
      review
        ? "The task description, and what the agent is asked. Placeholders: {{pr.number}} {{pr.title}} {{pr.link}} {{pr.author}} {{pr.repo}} {{pr.branch}} {{pr.base_branch}}. Empty uses the default shown."
        : "Sent to the agent when the task starts.",
    ),
  ];
}

function renderProfileFields(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host, options } = ctx;
  return [
    field(
      host,
      "Agent profile",
      select(host, {
        value: current.agentProfileId,
        onChange: (next) => update({ agentProfileId: next }),
        items: options.agent_profiles ?? [],
        placeholder: "Workspace default",
        allowNone: true,
      }),
    ),
    field(
      host,
      "Executor profile",
      select(host, {
        value: current.executorProfileId,
        onChange: (next) => update({ executorProfileId: next }),
        items: options.executor_profiles ?? [],
        placeholder: "Workspace default",
        allowNone: true,
      }),
    ),
  ];
}

/** How often the watch runs, how much it may file, and what happens after. */
function renderScheduleFields(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host, review, options } = ctx;
  const minimum = minPollInterval(options, review);
  return [
    field(
      host,
      "Poll interval (seconds)",
      host.jsx(host.ui.Input, {
        type: "number",
        min: minimum,
        value: current.pollIntervalSeconds,
        onChange: (event: InputChange) => update({ pollIntervalSeconds: event.target.value }),
      }),
      `At least ${minimum} seconds.`,
    ),
    field(
      host,
      "Open task limit",
      host.jsx(host.ui.Input, {
        type: "number",
        min: 1,
        value: current.maxInflightTasks,
        onChange: (event: InputChange) => update({ maxInflightTasks: event.target.value }),
      }),
      `How many of this watch's tasks may be open at once. Matched ${ctx.noun}s above the limit wait for a later run.`,
    ),
    review ? renderCleanupPolicy(ctx, current, update) : renderDedupScope(ctx, current, update),
    review && current.cleanupPolicy === "when_closed" && options.archive_granted === false
      ? host.jsx(
          "p",
          { className: "forgejo-watch-form__hint", role: "note" },
          "This Kandev has not approved archiving for the plugin, so finished tasks are completed rather than archived.",
        )
      : null,
  ];
}

function renderCleanupPolicy(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host } = ctx;
  return field(
    host,
    "When a pull request is merged or closed",
    select(host, {
      value: current.cleanupPolicy,
      onChange: (next) => update({ cleanupPolicy: next }),
      items: [
        { id: "never", name: "Leave the task alone" },
        { id: "when_closed", name: "Archive the task" },
      ],
      placeholder: "Leave the task alone",
    }),
    "Tasks are never deleted. Where the host will not archive them, they are marked complete instead.",
  );
}

function renderDedupScope(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host } = ctx;
  return field(
    host,
    "Duplicate handling",
    select(host, {
      value: current.dedupScope,
      onChange: (next) => update({ dedupScope: next }),
      items: [
        { id: "watch", name: "One task per watch" },
        { id: "workspace", name: "One task per issue" },
      ],
      placeholder: "One task per watch",
    }),
    "“One task per issue” stops a second watch filing the same issue again.",
  );
}

function renderStartAgent(ctx: WatchFormContext, current: Draft, update: Update) {
  const { host } = ctx;
  return [
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
  ];
}

function renderFormActions(ctx: WatchFormContext, current: Draft) {
  const { host } = ctx;
  return host.jsx(
    "div",
    { className: "forgejo-watch-form__actions" },
    host.jsx(
      host.ui.Button,
      {
        type: "button",
        size: "sm",
        disabled: ctx.busy !== null,
        onClick: () => void ctx.save(current),
      },
      current.id ? "Save watch" : "Create watch",
    ),
    host.jsx(
      host.ui.Button,
      { type: "button", variant: "ghost", size: "sm", onClick: () => ctx.setDraft(null) },
      "Cancel",
    ),
  );
}
