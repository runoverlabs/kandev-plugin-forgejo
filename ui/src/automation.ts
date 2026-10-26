import type { Component, PluginHostApi } from "@kandev/plugin-sdk";
import {
  FORK_WARNING,
  SWITCHES,
  TRUST_LEVELS,
  needsForkConfirmation,
  statusLines,
  templateDraft,
  trustLevel,
  type AutomationSettings,
  type AutomationView,
  type SwitchKey,
} from "./automation-model";
import { failureMessage } from "./panel-model";
import { useWorkspaceId, useWorkspaceState } from "./workspace";

export {
  TRUST_LEVELS,
  outcomeLine,
  type AutomationSettings,
  type AutomationView,
  type Switches,
} from "./automation-model";

type SectionProps = {
  workspaceId: string;
  taskId: string;
  number: number;
  refreshKey?: number;
};

type Section = {
  view: AutomationView | null;
  error: string | null;
  busy: boolean;
  write: (key: string, body: Record<string, unknown>) => Promise<void>;
  toggle: (key: SwitchKey, next: boolean) => void;
};

/**
 * The automation section of the review panel: the pull request's switches,
 * what automation is doing, and the two recovery buttons. Every call is
 * task-scoped; the number picks among the pull requests the task links.
 */
export function createAutomationSection(host: PluginHostApi): Component<SectionProps> {
  return function AutomationSection(props) {
    const section = useAutomationSection(host, props);
    const current = section.view;
    if (!current) {
      return section.error
        ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, section.error)
        : null;
    }
    return renderSection(host, section, current);
  };
}

function useAutomationSection(host: PluginHostApi, props: SectionProps): Section {
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
    if (needsForkConfirmation(view as AutomationView | null, key, next)) {
      confirmForkSwitch(host, () => void write("automation.set", { [key]: true, confirm_fork: true }));
      return;
    }
    void write("automation.set", { [key]: next });
  };

  return { view: view as AutomationView | null, error: error as string | null, busy, write, toggle };
}

function confirmForkSwitch(host: PluginHostApi, turnOn: () => void): void {
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
              turnOn();
            },
          },
          "Turn on",
        ),
      ),
  });
}

function renderSection(host: PluginHostApi, section: Section, current: AutomationView) {
  const { busy, error } = section;
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
          onCheckedChange: (next: boolean) => section.toggle(key, next === true),
        }),
        host.jsx("span", null, label),
      ),
    ),
    ...statusLines(current).map((line, index) =>
      host.jsx("p", { key: `line-${index}`, className: "forgejo-automation__status", role: "status" }, line),
    ),
    current.error || error
      ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, current.error || error)
      : null,
    current.merge_failure && current.auto_merge
      ? renderRecoveryButton(host, section, "merge")
      : null,
    current.exhausted
      ? renderRecoveryButton(host, section, "rounds")
      : null,
  );
}

const RECOVERY: Record<"merge" | "rounds", { testId: string; label: string }> = {
  merge: { testId: "forgejo-automation-retry-merge", label: "Retry merge" },
  rounds: { testId: "forgejo-automation-reset-rounds", label: "Reset rounds" },
};

function renderRecoveryButton(host: PluginHostApi, section: Section, what: keyof typeof RECOVERY) {
  const { testId, label } = RECOVERY[what];
  return host.jsx(
    host.ui.Button,
    {
      type: "button",
      size: "sm",
      variant: "outline",
      disabled: section.busy,
      "data-testid": testId,
      onClick: () => void section.write("automation.retry", { what }),
    },
    label,
  );
}

type SettingsPanel = {
  workspaceId: string;
  settings: AutomationSettings | null;
  template: string | null;
  setTemplate: (template: string | null) => void;
  error: string | null;
  busy: boolean;
  save: (patch: Record<string, unknown>, done?: string) => Promise<boolean>;
};

/**
 * The workspace's automation settings, in the integrations card: defaults for
 * newly linked pull requests, whose review text reaches the agent, and the
 * auto-fix prompt.
 */
export function createAutomationSettings(host: PluginHostApi): Component<{ workspaceId?: string }> {
  return function AutomationSettingsPanel(props: { workspaceId?: string } = {}) {
    const panel = useAutomationSettings(host, props.workspaceId);
    if (!panel.workspaceId) {
      return host.jsx("p", { className: "forgejo-automation__status" }, "Pick a workspace to configure automation.");
    }
    const current = panel.settings;
    if (!current) {
      return panel.error ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, panel.error) : null;
    }
    return renderSettings(host, panel, current);
  };
}

function useAutomationSettings(host: PluginHostApi, routedWorkspaceId: string | undefined): SettingsPanel {
  const workspaceId = useWorkspaceId(host, routedWorkspaceId);
  const [settings, setSettings] = useWorkspaceState<AutomationSettings | null>(host, workspaceId, null);
  const [template, setTemplate] = useWorkspaceState<string | null>(host, workspaceId, null);
  const [error, setError] = useWorkspaceState<string | null>(host, workspaceId, null);
  const [busy, setBusy] = useWorkspaceState(host, workspaceId, false);

  host.React.useEffect(() => {
    if (!workspaceId) return;
    let cancelled = false;
    host.api
      .invokeAction<AutomationSettings>("automation.settings", { workspaceId })
      .then((loaded) => {
        if (cancelled) return;
        setSettings(loaded);
        setError(null);
      })
      .catch((cause) => {
        if (!cancelled) setError(failureMessage(cause));
      });
    return () => {
      cancelled = true;
    };
  }, [workspaceId]);

  // Resolves true once the settings are stored, so a caller keeps its draft
  // when they are not.
  const save = async (patch: Record<string, unknown>, done?: string): Promise<boolean> => {
    if (busy || !workspaceId) return false;
    setBusy(true);
    try {
      const saved = await host.api.invokeAction<AutomationSettings>("automation.settings_set", {
        workspaceId,
        body: patch,
      });
      setSettings(saved);
      setError(null);
      if (done) host.toast.success(done);
      return true;
    } catch (cause) {
      host.toast.error(failureMessage(cause));
      return false;
    } finally {
      setBusy(false);
    }
  };

  return {
    workspaceId,
    settings,
    template,
    setTemplate,
    error,
    busy,
    save,
  };
}

function renderSettings(host: PluginHostApi, panel: SettingsPanel, current: AutomationSettings) {
  const { busy, save } = panel;
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
    ...renderTrustPicker(host, panel, current),
    ...renderTemplateEditor(host, panel, current),
  );
}

function renderTrustPicker(host: PluginHostApi, panel: SettingsPanel, current: AutomationSettings) {
  return [
    host.jsx(host.ui.Label, null, "Review feedback the agent sees"),
    host.jsx(
      host.ui.Select,
      { value: current.trust, onValueChange: (next: string) => void panel.save({ trust: next }) },
      host.jsx(host.ui.SelectTrigger, { "data-testid": "forgejo-automation-trust" }, host.jsx(host.ui.SelectValue, null)),
      host.jsx(
        host.ui.SelectContent,
        null,
        ...TRUST_LEVELS.map((level) => host.jsx(host.ui.SelectItem, { key: level.value, value: level.value }, level.label)),
      ),
    ),
    host.jsx("p", { className: "forgejo-automation__status" }, trustLevel(current).hint),
  ];
}

function renderTemplateEditor(host: PluginHostApi, panel: SettingsPanel, current: AutomationSettings) {
  const { busy, save, template, setTemplate } = panel;
  const draft = templateDraft(current, template);
  return [
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
            void save({ template: draft === current.default_template ? "" : draft }, "Auto-fix prompt saved").then((saved) => {
              if (saved) setTemplate(null);
            }),
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
          onClick: () =>
            void save({ template: "" }, "Auto-fix prompt reset").then((saved) => {
              if (saved) setTemplate(null);
            }),
        },
        "Reset to default",
      ),
    ),
  ];
}
