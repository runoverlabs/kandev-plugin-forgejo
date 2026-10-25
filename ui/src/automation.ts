import type { Component, PluginHostApi } from "@kandev/plugin-sdk";
import { failureMessage } from "./panel-model";

/** The switches a pull request carries, and a workspace's defaults for them. */
export type Switches = {
  auto_fix: boolean;
  auto_merge: boolean;
  on_review_requested: boolean;
  on_merged: boolean;
  on_closed: boolean;
};

/** automation.get: one linked pull request's automation state. */
export type AutomationView = Switches & {
  number: number;
  seeded: boolean;
  fork: boolean;
  rounds: number;
  max_rounds: number;
  exhausted: boolean;
  attempt: { round: number; state: string; dispatch: string } | null;
  last_outcome?: string;
  last_summary?: string;
  last_outcome_at?: string;
  merge_failure?: string;
  merge_retry?: boolean;
  ignored: number;
  status?: string;
  error?: string;
  trust: string;
  integration_enabled: boolean;
};

/** automation.settings: the workspace's settings. */
export type AutomationSettings = {
  defaults: Switches;
  trust: string;
  template: string;
  default_template: string;
  max_rounds: number;
};

type SwitchKey = keyof Switches;

const SWITCHES: { key: SwitchKey; label: string }[] = [
  { key: "auto_fix", label: "Auto-fix: prompt the agent when CI fails, the branch conflicts, or reviewers ask for changes" },
  { key: "auto_merge", label: "Auto-merge when CI passes, the branch merges cleanly and reviews allow it" },
  { key: "on_review_requested", label: "Tell the agent when your review is requested" },
  { key: "on_merged", label: "Tell the agent when the pull request is merged" },
  { key: "on_closed", label: "Tell the agent when the pull request is closed without merging" },
];

export const TRUST_LEVELS: { value: string; label: string; hint: string }[] = [
  {
    value: "everyone",
    label: "Everyone",
    hint: "Every review and comment reaches the agent, as with Kandev's GitHub integration. Anyone who can comment on the pull request puts text in front of the agent.",
  },
  {
    value: "official",
    label: "Official reviewers",
    hint: "Only reviews that count towards approvals (reviewers with write access), and comments from people who left one.",
  },
  {
    value: "none",
    label: "No review text",
    hint: "Auto-fix acts on CI failures and conflicts only.",
  },
];

/** Fork pull requests carry code and comments their author controls. */
const FORK_WARNING =
  "This pull request comes from a fork. Its code and comments are controlled by the fork's author, and the agent will act on them with your executor's environment. Turn this on only if you trust that author.";

/** A one-line description of the agent's last report. */
export function outcomeLine(view: AutomationView): string | null {
  if (!view.last_outcome) return null;
  const outcome = view.last_outcome.replace(/_/g, " ");
  return view.last_summary ? `Last outcome: ${outcome} — ${view.last_summary}` : `Last outcome: ${outcome}`;
}

/**
 * The automation section of the review panel: the pull request's switches,
 * what automation is doing, and the two recovery buttons. Every call is
 * task-scoped; the number picks among the pull requests the task links.
 */
export function createAutomationSection(host: PluginHostApi): Component<{
  workspaceId: string;
  taskId: string;
  number: number;
  refreshKey?: number;
}> {
  return function AutomationSection(props) {
    const [view, setView] = host.React.useState(null as AutomationView | null);
    const [error, setError] = host.React.useState(null as string | null);
    const [busy, setBusy] = host.React.useState(false);

    const call = (key: string, body: Record<string, unknown> = {}) =>
      host.api.invokeAction<AutomationView>(key, {
        workspaceId: props.workspaceId,
        taskId: props.taskId,
        body: { number: props.number, ...body },
      });

    const load = host.React.useCallback(async () => {
      try {
        setView(await call("automation.get"));
        setError(null);
      } catch (cause) {
        setError(failureMessage(cause));
      }
    }, [props.workspaceId, props.taskId, props.number]);

    host.React.useEffect(() => {
      void load();
    }, [load, props.refreshKey]);

    const write = async (key: string, body: Record<string, unknown>) => {
      if (busy) return;
      setBusy(true);
      try {
        setView(await call(key, body));
        setError(null);
      } catch (cause) {
        host.toast.error(failureMessage(cause));
        // The refusal may come from what the view did not know yet (that the
        // pull request is a fork); the next toggle decides on a fresh one.
        void load();
      } finally {
        setBusy(false);
      }
    };

    const toggle = (key: SwitchKey, next: boolean) => {
      const current = view as AutomationView | null;
      if (next && current?.fork && (key === "auto_fix" || key === "auto_merge")) {
        let modal: { close(): void } | null = null;
        modal = host.openModal({
          title: "Pull request from a fork",
          description: FORK_WARNING,
          size: "sm",
          content: () =>
            host.jsx(
              "div",
              { className: "forgejo-automation__confirm" },
              host.jsx(host.ui.Button, { type: "button", variant: "outline", onClick: () => modal?.close() }, "Cancel"),
              host.jsx(
                host.ui.Button,
                {
                  type: "button",
                  "data-testid": "forgejo-automation-fork-confirm",
                  onClick: () => {
                    modal?.close();
                    void write("automation.set", { [key]: true, confirm_fork: true });
                  },
                },
                "Turn on",
              ),
            ),
        });
        return;
      }
      void write("automation.set", { [key]: next });
    };

    const current = view as AutomationView | null;
    if (!current) {
      return error
        ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, error)
        : null;
    }

    const lines: unknown[] = [];
    if (!current.integration_enabled) {
      lines.push("Paused: the Forgejo integration is off for this workspace.");
    }
    if (current.status) lines.push(current.status);
    if (current.auto_fix) {
      lines.push(`Auto-fix rounds: ${current.rounds} of ${current.max_rounds}.`);
    }
    const outcome = outcomeLine(current);
    if (outcome) lines.push(outcome);
    if (current.ignored > 0) {
      lines.push(
        `${current.ignored} review ${current.ignored === 1 ? "comment was" : "comments were"} left out by the workspace's feedback setting (${current.trust}).`,
      );
    }

    return host.jsx(
      "div",
      { className: "forgejo-automation", "data-testid": "forgejo-automation" },
      host.jsx("h4", { className: "forgejo-automation__heading" }, "Automation"),
      ...SWITCHES.map(({ key, label }) =>
        host.jsx(
          "label",
          { key, className: "forgejo-automation__toggle" },
          host.jsx(host.ui.Switch, {
            checked: current[key],
            disabled: busy,
            "aria-label": label,
            "data-testid": `forgejo-automation-${key}`,
            onCheckedChange: (next: boolean) => toggle(key, next === true),
          }),
          host.jsx("span", null, label),
        ),
      ),
      ...lines.map((line, index) =>
        host.jsx("p", { key: `line-${index}`, className: "forgejo-automation__status", role: "status" }, line),
      ),
      current.error || error
        ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, current.error || error)
        : null,
      current.merge_failure && current.auto_merge
        ? host.jsx(
            host.ui.Button,
            {
              type: "button",
              size: "sm",
              variant: "outline",
              disabled: busy,
              "data-testid": "forgejo-automation-retry-merge",
              onClick: () => void write("automation.retry", { what: "merge" }),
            },
            "Retry merge",
          )
        : null,
      current.exhausted
        ? host.jsx(
            host.ui.Button,
            {
              type: "button",
              size: "sm",
              variant: "outline",
              disabled: busy,
              "data-testid": "forgejo-automation-reset-rounds",
              onClick: () => void write("automation.retry", { what: "rounds" }),
            },
            "Reset rounds",
          )
        : null,
    );
  };
}

/**
 * The workspace's automation settings, in the integrations card: defaults for
 * newly linked pull requests, whose review text reaches the agent, and the
 * auto-fix prompt.
 */
export function createAutomationSettings(host: PluginHostApi): Component<{ workspaceId?: string }> {
  return function AutomationSettingsPanel(props: { workspaceId?: string } = {}) {
    const [settings, setSettings] = host.React.useState(null as AutomationSettings | null);
    const [template, setTemplate] = host.React.useState(null as string | null);
    const [error, setError] = host.React.useState(null as string | null);
    const [busy, setBusy] = host.React.useState(false);
    const [activeWorkspaceId, setActiveWorkspaceId] = host.React.useState<string | undefined>(() =>
      host.context.getActiveWorkspaceId(),
    );
    host.React.useEffect(() => host.context.subscribeActiveWorkspace(setActiveWorkspaceId), []);
    const workspaceId = (props.workspaceId ?? activeWorkspaceId ?? "").trim();

    host.React.useEffect(() => {
      if (!workspaceId) return;
      let cancelled = false;
      host.api
        .invokeAction<AutomationSettings>("automation.settings", { workspaceId })
        .then((loaded) => {
          if (cancelled) return;
          setSettings(loaded);
          setTemplate(null);
          setError(null);
        })
        .catch((cause) => {
          if (!cancelled) setError(failureMessage(cause));
        });
      return () => {
        cancelled = true;
      };
    }, [workspaceId]);

    const save = async (patch: Record<string, unknown>, done?: string) => {
      if (busy || !workspaceId) return;
      setBusy(true);
      try {
        const saved = await host.api.invokeAction<AutomationSettings>("automation.settings_set", {
          workspaceId,
          body: patch,
        });
        setSettings(saved);
        setError(null);
        if (done) host.toast.success(done);
      } catch (cause) {
        host.toast.error(failureMessage(cause));
      } finally {
        setBusy(false);
      }
    };

    if (!workspaceId) {
      return host.jsx("p", { className: "forgejo-automation__status" }, "Pick a workspace to configure automation.");
    }
    const current = settings as AutomationSettings | null;
    if (!current) {
      return error ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, error) : null;
    }
    const trust = TRUST_LEVELS.find((level) => level.value === current.trust) ?? TRUST_LEVELS[0]!;
    const draft = (template as string | null) ?? (current.template || current.default_template);

    return host.jsx(
      "div",
      { className: "forgejo-automation", "data-testid": "forgejo-automation-settings" },
      host.jsx(
        "p",
        { className: "forgejo-automation__status" },
        "Defaults for pull requests linked from now on. Each pull request's own switches are in its review panel. Auto-fix and auto-merge are never turned on by default for a pull request from a fork.",
      ),
      ...SWITCHES.map(({ key, label }) =>
        host.jsx(
          "label",
          { key, className: "forgejo-automation__toggle" },
          host.jsx(host.ui.Switch, {
            checked: current.defaults[key],
            disabled: busy,
            "aria-label": `Default: ${label}`,
            "data-testid": `forgejo-automation-default-${key}`,
            onCheckedChange: (next: boolean) => void save({ defaults: { ...current.defaults, [key]: next === true } }),
          }),
          host.jsx("span", null, label),
        ),
      ),
      host.jsx(host.ui.Label, null, "Review feedback the agent sees"),
      host.jsx(
        host.ui.Select,
        { value: current.trust, onValueChange: (next: string) => void save({ trust: next }) },
        host.jsx(host.ui.SelectTrigger, { "data-testid": "forgejo-automation-trust" }, host.jsx(host.ui.SelectValue, null)),
        host.jsx(
          host.ui.SelectContent,
          null,
          ...TRUST_LEVELS.map((level) => host.jsx(host.ui.SelectItem, { key: level.value, value: level.value }, level.label)),
        ),
      ),
      host.jsx("p", { className: "forgejo-automation__status" }, trust.hint),
      host.jsx(host.ui.Label, null, "Auto-fix prompt"),
      host.jsx(host.ui.Textarea, {
        value: draft,
        rows: 10,
        disabled: busy,
        "aria-label": "Auto-fix prompt",
        onChange: (event: { target: { value: string } }) => setTemplate(event.target.value),
      }),
      host.jsx(
        "p",
        { className: "forgejo-automation__status" },
        "Placeholders: {{pr.feedback}}, {{pr.url}}, {{pr.number}}, {{pr.title}}, {{pr.repository}}, {{pr.head_branch}}, {{pr.base_branch}}, {{attempt.round}}. The outcome instructions are always appended.",
      ),
      host.jsx(
        "div",
        { className: "forgejo-automation__actions" },
        host.jsx(
          host.ui.Button,
          {
            type: "button",
            size: "sm",
            disabled: busy || template === null,
            "data-testid": "forgejo-automation-save-template",
            onClick: () =>
              void save({ template: draft === current.default_template ? "" : draft }, "Auto-fix prompt saved").then(() =>
                setTemplate(null),
              ),
          },
          "Save prompt",
        ),
        host.jsx(
          host.ui.Button,
          {
            type: "button",
            size: "sm",
            variant: "outline",
            disabled: busy || !current.template,
            onClick: () => void save({ template: "" }, "Auto-fix prompt reset").then(() => setTemplate(null)),
          },
          "Reset to default",
        ),
      ),
    );
  };
}
