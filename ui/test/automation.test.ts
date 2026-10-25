import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  type AutomationSettings,
  type AutomationView,
  createAutomationSection,
  createAutomationSettings,
  outcomeLine,
} from "../src/automation";

const VIEW: AutomationView = {
  number: 7,
  seeded: true,
  auto_fix: false,
  auto_merge: false,
  on_review_requested: false,
  on_merged: false,
  on_closed: false,
  fork: false,
  rounds: 0,
  max_rounds: 10,
  exhausted: false,
  attempt: null,
  ignored: 0,
  trust: "everyone",
  integration_enabled: true,
};

const SETTINGS: AutomationSettings = {
  defaults: { auto_fix: false, auto_merge: false, on_review_requested: false, on_merged: true, on_closed: false },
  trust: "everyone",
  template: "",
  default_template: "Default prompt {{pr.feedback}}",
  max_rounds: 10,
};

type Node = { type: unknown; props: any; children: unknown[] };

function sameDeps(a: unknown[] = [], b: unknown[] = []): boolean {
  return a.length === b.length && a.every((value, index) => Object.is(value, b[index]));
}

/** A React-shaped host whose hooks keep state across renders. */
function makeHost(handlers: Record<string, (input: any) => unknown>) {
  const state: unknown[] = [];
  const memo: unknown[] = [];
  const effects: (() => void | (() => void))[] = [];
  let cursor = 0;
  const calls: { key: string; input: any }[] = [];
  const modals: any[] = [];
  const host: any = {
    jsx: (type: unknown, props: unknown, ...children: unknown[]): Node => ({ type, props, children }),
    React: {
      useState(initial: unknown) {
        const i = cursor++;
        if (!(i in state)) state[i] = typeof initial === "function" ? (initial as any)() : initial;
        return [state[i], (v: unknown) => (state[i] = typeof v === "function" ? (v as any)(state[i]) : v)];
      },
      useEffect(fn: () => void | (() => void), deps: unknown[]) {
        const i = cursor++;
        const previous = memo[i] as { deps: unknown[] } | undefined;
        if (!previous || !sameDeps(previous.deps, deps)) {
          memo[i] = { deps };
          effects.push(fn);
        }
      },
      useCallback(fn: unknown, deps: unknown[]) {
        const i = cursor++;
        const previous = memo[i] as { fn: unknown; deps: unknown[] } | undefined;
        if (previous && sameDeps(previous.deps, deps)) return previous.fn;
        memo[i] = { fn, deps };
        return fn;
      },
    },
    ui: new Proxy({}, { get: (_t, name) => String(name) }),
    toast: { success: vi.fn(), error: vi.fn() },
    openModal: vi.fn((options: any) => {
      modals.push(options);
      return { close: vi.fn() };
    }),
    context: { getActiveWorkspaceId: () => "ws-1", subscribeActiveWorkspace: () => () => {} },
    api: {
      invokeAction: vi.fn(async (key: string, input: any) => {
        calls.push({ key, input });
        const handler = handlers[key];
        if (!handler) throw new Error(`unexpected action ${key}`);
        return handler(input);
      }),
    },
  };
  const settle = async () => {
    const pending = effects.splice(0);
    for (const fn of pending) fn();
    for (let i = 0; i < 8; i++) await Promise.resolve();
  };
  const begin = () => {
    cursor = 0;
  };
  return { host, settle, begin, calls, modals };
}

function find(node: any, predicate: (n: any) => boolean): any {
  if (!node || typeof node !== "object") return null;
  if (predicate(node)) return node;
  const kids = [...(node.children ?? []), ...(node.props?.children ? [node.props.children] : [])].flat(5);
  for (const child of kids) {
    const hit = find(child, predicate);
    if (hit) return hit;
  }
  return null;
}

function text(node: any): string {
  if (node === null || node === undefined || node === false) return "";
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(text).join(" ");
  return [...(node.children ?? [])].map(text).join(" ");
}

const byTestId = (tree: any, id: string) => find(tree, (n) => n.props?.["data-testid"] === id);

function section(view: Partial<AutomationView>, extra: Record<string, (input: any) => unknown> = {}) {
  const current = { ...VIEW, ...view };
  const t = makeHost({
    "automation.get": () => current,
    "automation.set": (input) => Object.assign(current, input.body),
    "automation.retry": () => current,
    ...extra,
  });
  const Section = createAutomationSection(t.host);
  const render = () => {
    t.begin();
    return Section({ workspaceId: "ws-1", taskId: "task-1", number: 7 }) as Node;
  };
  return { ...t, render };
}

describe("automation section", () => {
  beforeEach(() => vi.clearAllMocks());

  it("loads the task's pull request and renders the switches", async () => {
    const t = section({ auto_fix: true, rounds: 3, status: "Auto-fix round 3 is with the agent." });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(t.calls[0]).toEqual({
      key: "automation.get",
      input: { workspaceId: "ws-1", taskId: "task-1", body: { number: 7 } },
    });
    expect(byTestId(tree, "forgejo-automation-auto_fix").props.checked).toBe(true);
    expect(byTestId(tree, "forgejo-automation-auto_merge").props.checked).toBe(false);
    expect(text(tree)).toContain("Auto-fix round 3 is with the agent.");
    expect(text(tree)).toContain("Auto-fix rounds: 3 of 10.");
  });

  it("turns a switch on through the task-scoped action", async () => {
    const t = section({});
    t.render();
    await t.settle();
    byTestId(t.render(), "forgejo-automation-on_merged").props.onCheckedChange(true);
    await t.settle();
    expect(t.calls.at(-1)).toEqual({
      key: "automation.set",
      input: { workspaceId: "ws-1", taskId: "task-1", body: { number: 7, on_merged: true } },
    });
  });

  it("asks before turning on auto-fix for a fork, and not for a lifecycle prompt", async () => {
    const t = section({ fork: true });
    t.render();
    await t.settle();
    byTestId(t.render(), "forgejo-automation-auto_fix").props.onCheckedChange(true);
    expect(t.modals).toHaveLength(1);
    expect(t.modals[0].description).toContain("fork");
    expect(t.calls.some((call) => call.key === "automation.set")).toBe(false);

    const confirm = byTestId(t.modals[0].content(), "forgejo-automation-fork-confirm");
    confirm.props.onClick();
    await t.settle();
    expect(t.calls.at(-1)?.input.body).toEqual({ number: 7, auto_fix: true });

    byTestId(t.render(), "forgejo-automation-on_closed").props.onCheckedChange(true);
    expect(t.modals).toHaveLength(1);
  });

  it("offers retry and reset only when there is something to retry", async () => {
    const quiet = section({ auto_merge: true });
    quiet.render();
    await quiet.settle();
    const calm = quiet.render();
    expect(byTestId(calm, "forgejo-automation-retry-merge")).toBeNull();
    expect(byTestId(calm, "forgejo-automation-reset-rounds")).toBeNull();

    const t = section({ auto_merge: true, auto_fix: true, merge_failure: "Branch protection blocks this.", exhausted: true });
    t.render();
    await t.settle();
    const tree = t.render();
    byTestId(tree, "forgejo-automation-retry-merge").props.onClick();
    await t.settle();
    expect(t.calls.at(-1)?.input.body).toEqual({ number: 7, what: "merge" });
    byTestId(t.render(), "forgejo-automation-reset-rounds").props.onClick();
    await t.settle();
    expect(t.calls.at(-1)?.input.body).toEqual({ number: 7, what: "rounds" });
  });

  it("says when the integration is off, what was ignored, and the host's error", async () => {
    const t = section({
      integration_enabled: false,
      ignored: 2,
      trust: "official",
      error: "Kandev has not approved this plugin's capabilities since the last upgrade.",
    });
    t.render();
    await t.settle();
    const rendered = text(t.render());
    expect(rendered).toContain("Paused: the Forgejo integration is off");
    expect(rendered).toContain("2 review comments were left out by the workspace's feedback setting (official)");
    expect(rendered).toContain("has not approved");
  });

  it("reports a failed load instead of rendering switches", async () => {
    const t = section({}, {
      "automation.get": () => {
        throw new Error("That pull request is not linked to this task.");
      },
    });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(byTestId(tree, "forgejo-automation-auto_fix")).toBeNull();
    expect(text(tree)).toContain("not linked");
  });
});

describe("outcome line", () => {
  it("reads the last report", () => {
    expect(outcomeLine(VIEW)).toBeNull();
    expect(outcomeLine({ ...VIEW, last_outcome: "non_actionable" })).toBe("Last outcome: non actionable");
    expect(outcomeLine({ ...VIEW, last_outcome: "blocked", last_summary: "needs a secret" })).toBe(
      "Last outcome: blocked — needs a secret",
    );
  });
});

describe("automation settings", () => {
  beforeEach(() => vi.clearAllMocks());

  function settings(initial: Partial<AutomationSettings> = {}) {
    const current = { ...SETTINGS, ...initial };
    const t = makeHost({
      "automation.settings": () => current,
      "automation.settings_set": (input) => Object.assign(current, input.body),
    });
    const Panel = createAutomationSettings(t.host);
    const render = () => {
      t.begin();
      return Panel({ workspaceId: "ws-1" }) as Node;
    };
    return { ...t, render };
  }

  it("loads the workspace's settings and changes a default", async () => {
    const t = settings();
    t.render();
    await t.settle();
    const tree = t.render();
    expect(t.calls[0]).toEqual({ key: "automation.settings", input: { workspaceId: "ws-1" } });
    expect(byTestId(tree, "forgejo-automation-default-on_merged").props.checked).toBe(true);
    byTestId(tree, "forgejo-automation-default-auto_fix").props.onCheckedChange(true);
    await t.settle();
    expect(t.calls.at(-1)).toEqual({
      key: "automation.settings_set",
      input: { workspaceId: "ws-1", body: { defaults: { ...SETTINGS.defaults, auto_fix: true } } },
    });
  });

  it("explains the trust level and saves a new one", async () => {
    const t = settings();
    t.render();
    await t.settle();
    const tree = t.render();
    expect(text(tree)).toContain("Anyone who can comment on the pull request puts text in front of the agent.");
    const select = find(tree, (n) => n.type === "Select");
    select.props.onValueChange("official");
    await t.settle();
    expect(t.calls.at(-1)?.input.body).toEqual({ trust: "official" });
  });

  it("shows the default prompt, and stores an unchanged one as the default", async () => {
    const t = settings();
    t.render();
    await t.settle();
    let tree = t.render();
    const textarea = find(tree, (n) => n.type === "Textarea");
    expect(textarea.props.value).toBe(SETTINGS.default_template);
    expect(byTestId(tree, "forgejo-automation-save-template").props.disabled).toBe(true);

    textarea.props.onChange({ target: { value: "Custom {{pr.feedback}}" } });
    tree = t.render();
    byTestId(tree, "forgejo-automation-save-template").props.onClick();
    await t.settle();
    expect(t.calls.at(-1)?.input.body).toEqual({ template: "Custom {{pr.feedback}}" });

    textarea.props.onChange({ target: { value: SETTINGS.default_template } });
    byTestId(t.render(), "forgejo-automation-save-template").props.onClick();
    await t.settle();
    expect(t.calls.at(-1)?.input.body).toEqual({ template: "" });
  });
});
