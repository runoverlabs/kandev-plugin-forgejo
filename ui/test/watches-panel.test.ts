import { beforeEach, describe, expect, it, vi } from "vitest";
import { createWatchesPanel } from "../src/watches-panel";

// Minimal React-shaped host stub: hooks run eagerly so a single render()
// exercises the effect body the way the host would.
function makeHost(overrides: Record<string, any> = {}) {
  const effects: (() => void | (() => void))[] = [];
  const state: any[] = [];
  let cursor = 0;
  const host: any = {
    jsx: (type: unknown, props: unknown, ...children: unknown[]) => ({ type, props, children }),
    React: {
      useState(initial: unknown) {
        const i = cursor++;
        if (!(i in state)) state[i] = typeof initial === "function" ? (initial as any)() : initial;
        return [
          state[i],
          (v: unknown) => {
            state[i] = typeof v === "function" ? (v as any)(state[i]) : v;
          },
        ];
      },
      useEffect(fn: () => void | (() => void)) {
        effects.push(fn);
      },
      useCallback: (fn: unknown) => fn,
    },
    ui: {
      Button: "button",
      Switch: "switch",
      Input: "input",
      Textarea: "textarea",
      Label: "label",
      Select: "select",
      SelectTrigger: "select-trigger",
      SelectValue: "select-value",
      SelectContent: "select-content",
      SelectItem: "select-item",
    },
    api: {
      invokeAction: vi.fn().mockImplementation(async (key: string) => {
        if (key === "watches.list") {
          return {
            watches: [
              {
                id: "watch-1",
                name: "Bugs",
                workflow_id: "wf-1",
                workflow_step_id: "step-inbox",
                repos: [{ owner: "acme", name: "app" }],
                labels: ["bug"],
                enabled: true,
              },
            ],
          };
        }
        if (key === "watches.options") {
          return {
            workflows: [
              { id: "wf-1", name: "Autopilot", steps: [{ id: "step-inbox", name: "Inbox", is_start_step: true }] },
            ],
            agent_profiles: [{ id: "agent-1", name: "Builder" }],
            default_interval: 300,
            min_interval: 30,
            default_max_inflight: 5,
          };
        }
        return {};
      }),
    },
    context: {
      getActiveWorkspaceId: () => undefined,
      subscribeActiveWorkspace: () => () => {},
    },
    ...overrides,
  };
  return {
    host,
    render(props: { workspaceId?: string } = {}) {
      cursor = 0;
      effects.length = 0;
      const tree = createWatchesPanel(host)(props);
      for (const fn of [...effects]) fn();
      return tree;
    },
  };
}

// Lets the panel's load promises settle. The panel awaits a Promise.all and
// then several state writes, so one microtask turn is not enough.
async function flush() {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

// Walks a rendered tree collecting every string child, so assertions can look
// for operator-facing copy without depending on the element structure.
function textOf(node: any): string {
  if (node === null || node === undefined || node === false) return "";
  if (typeof node === "string") return node;
  if (Array.isArray(node)) return node.map(textOf).join(" ");
  if (typeof node === "object" && "children" in node) return textOf(node.children);
  return "";
}

describe("watches panel", () => {
  beforeEach(() => vi.clearAllMocks());

  // Every watch action is scope "workspace". Invoking one without the selector
  // is rejected by the host envelope before it reaches the plugin process.
  it("scopes every request to the routed workspace", async () => {
    const { host, render } = makeHost();
    render({ workspaceId: "workspace-1" });
    await Promise.resolve();

    expect(host.api.invokeAction).toHaveBeenCalledTimes(2);
    for (const [, selector] of host.api.invokeAction.mock.calls) {
      expect(selector).toEqual({ workspaceId: "workspace-1" });
    }
    const keys = host.api.invokeAction.mock.calls.map(([key]: [string]) => key);
    expect(keys).toContain("watches.list");
    expect(keys).toContain("watches.options");
  });

  it("falls back to the active workspace when no prop is routed", async () => {
    const { host, render } = makeHost({
      context: {
        getActiveWorkspaceId: () => "workspace-active",
        subscribeActiveWorkspace: () => () => {},
      },
    });
    render();
    await Promise.resolve();

    const [, selector] = host.api.invokeAction.mock.calls[0]!;
    expect(selector).toEqual({ workspaceId: "workspace-active" });
  });

  // With no workspace there is nothing to scope a read to, so the panel says so
  // rather than issuing a call the host will reject.
  it("asks for a workspace instead of calling without one", async () => {
    const { host, render } = makeHost();
    const tree = render();
    await Promise.resolve();

    expect(host.api.invokeAction).not.toHaveBeenCalled();
    expect(textOf(tree)).toContain("Open a workspace");
  });

  it("renders a watch with its repositories and labels", async () => {
    const { render } = makeHost();
    render({ workspaceId: "workspace-1" });
    await flush();
    const tree = render({ workspaceId: "workspace-1" });

    const text = textOf(tree);
    expect(text).toContain("Bugs");
    expect(text).toContain("acme/app");
    expect(text).toContain("bug");
  });

  // A backend validation message names exactly what the operator must change,
  // so it must survive rather than being replaced by a generic transport error.
  it("surfaces a backend validation message verbatim", async () => {
    const { render } = makeHost({
      api: {
        invokeAction: vi
          .fn()
          .mockRejectedValue(new Error("rpc error: kandev-plugin-forgejo: watches: at least one repository is required")),
      },
    });
    render({ workspaceId: "workspace-1" });
    await flush();
    const tree = render({ workspaceId: "workspace-1" });

    expect(textOf(tree)).toContain("at least one repository is required");
  });

  it("replaces an opaque transport failure with something actionable", async () => {
    const { render } = makeHost({
      api: { invokeAction: vi.fn().mockRejectedValue(new Error("network down")) },
    });
    render({ workspaceId: "workspace-1" });
    await flush();
    const tree = render({ workspaceId: "workspace-1" });

    const text = textOf(tree);
    expect(text).toContain("Couldn't reach the Forgejo plugin");
    expect(text).not.toContain("network down");
  });
});
