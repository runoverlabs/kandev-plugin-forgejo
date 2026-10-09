import { beforeEach, describe, expect, it, vi } from "vitest";
import { createConnectionPanel } from "../src/connection-panel";

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
        return [state[i], (v: unknown) => { state[i] = typeof v === "function" ? (v as any)(state[i]) : v; }];
      },
      useEffect(fn: () => void | (() => void)) { effects.push(fn); },
      useCallback: (fn: unknown) => fn,
    },
    ui: { Button: "button", Switch: "switch" },
    api: {
      invokeAction: vi
        .fn()
        .mockResolvedValue({ configured: true, connected: true, enabled: true, account: "kandev" }),
    },
    context: {
      getActiveWorkspaceId: () => undefined,
      subscribeActiveWorkspace: () => () => {},
    },
    setIntegrationEnabled: vi.fn(),
    ...overrides,
  };
  return {
    host,
    render(props: { workspaceId?: string } = {}) {
      cursor = 0;
      effects.length = 0;
      const tree = createConnectionPanel(host)(props);
      // Run effects the way React would after commit.
      for (const fn of [...effects]) fn();
      return tree;
    },
  };
}

describe("connection panel", () => {
  beforeEach(() => vi.clearAllMocks());

  // Regression: every manifest action is scope "workspace". Invoking one
  // without the selector is rejected by the host envelope before it ever
  // reaches the plugin process, so the panel renders a default "not
  // configured" state and the backend looks broken when it is fine.
  it("passes workspaceId from its props to the action", async () => {
    const { host, render } = makeHost();
    render({ workspaceId: "workspace-1" });
    await Promise.resolve();

    expect(host.api.invokeAction).toHaveBeenCalledTimes(1);
    const [key, selector] = host.api.invokeAction.mock.calls[0]!;
    expect(key).toBe("connection.get");
    expect(selector).toEqual({ workspaceId: "workspace-1" });
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

    expect(host.api.invokeAction.mock.calls[0]![1]).toEqual({ workspaceId: "workspace-active" });
  });

  // Without a workspace there is no legal call to make; the panel must not
  // fire one just to have the host reject it.
  it("makes no request when no workspace can be resolved", async () => {
    const { host, render } = makeHost();
    render();
    await Promise.resolve();
    expect(host.api.invokeAction).not.toHaveBeenCalled();
  });

  it("publishes the enabled state for the badge", async () => {
    const { host, render } = makeHost();
    render({ workspaceId: "workspace-1" });
    await Promise.resolve();
    await Promise.resolve();
    expect(host.setIntegrationEnabled).toHaveBeenCalledWith("forgejo", "workspace-1", true);
  });

  // Kandev renders no enable control for a plugin integration; the plugin has
  // to provide one itself, which is why the Forgejo card had no toggle while
  // native integrations did.
  it("renders its own enable toggle", () => {
    const { render } = makeHost();
    const rendered = JSON.stringify(render({ workspaceId: "workspace-1" }));
    expect(rendered).toContain("switch");
    expect(rendered).toContain("Enable Forgejo for this workspace");
  });

  it("persists a toggle change and republishes the badge", async () => {
    const { host, render } = makeHost();
    const tree: any = render({ workspaceId: "workspace-1" });
    await Promise.resolve();

    const toggle = JSON.parse(JSON.stringify(tree)); // structure only
    expect(JSON.stringify(toggle)).toContain("switch");

    // Drive the handler the way the Switch would.
    const findSwitch = (node: any): any => {
      if (!node || typeof node !== "object") return null;
      if (node.type === "switch") return node;
      for (const child of node.children ?? []) {
        const hit = findSwitch(child);
        if (hit) return hit;
      }
      return null;
    };
    const node = findSwitch(tree);
    expect(node).toBeTruthy();
    await node.props.onCheckedChange(false);
    await Promise.resolve();

    expect(host.api.invokeAction).toHaveBeenCalledWith(
      "connection.set_enabled",
      { workspaceId: "workspace-1", body: { enabled: false } },
      expect.anything(),
    );
    expect(host.setIntegrationEnabled).toHaveBeenLastCalledWith("forgejo", "workspace-1", false);
  });

  // A failed write must not leave the UI claiming state the backend rejected.
  it("reverts the toggle when persisting fails", async () => {
    const invokeAction = vi
      .fn()
      .mockResolvedValueOnce({ configured: true, connected: true, enabled: true })
      .mockRejectedValueOnce(new Error("boom"));
    const { host, render } = makeHost({ api: { invokeAction } });
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    const tree: any = render({ workspaceId: "workspace-1" });
    await Promise.resolve();
    const findSwitch = (node: any): any => {
      if (!node || typeof node !== "object") return null;
      if (node.type === "switch") return node;
      for (const child of node.children ?? []) {
        const hit = findSwitch(child);
        if (hit) return hit;
      }
      return null;
    };
    await findSwitch(tree).props.onCheckedChange(false);
    await Promise.resolve();

    expect(host.setIntegrationEnabled).toHaveBeenLastCalledWith("forgejo", "workspace-1", true);
    errorSpy.mockRestore();
  });

  // Regression: host envelope errors named the wrong actor and read as though
  // the operator had mistyped something.
  it("never renders a raw host error string", async () => {
    const { render } = makeHost({
      api: {
        invokeAction: vi
          .fn()
          .mockRejectedValue(new Error("workspace action requires only workspaceId")),
      },
    });
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    render({ workspaceId: "workspace-1" });
    await Promise.resolve();
    await Promise.resolve();
    const rendered = JSON.stringify(render({ workspaceId: "workspace-1" }));

    expect(rendered).not.toContain("requires only workspaceId");
    expect(rendered).toContain("Check the Kandev server logs");
    // The raw cause stays diagnosable.
    expect(errorSpy).toHaveBeenCalled();
    errorSpy.mockRestore();
  });

  it("states that the connection is shared across workspaces", () => {
    const { render } = makeHost();
    expect(JSON.stringify(render({ workspaceId: "workspace-1" })))
      .toContain("shared by every workspace");
  });

  describe("agent merge switch", () => {
    const switches = (node: any, found: any[] = []): any[] => {
      if (!node || typeof node !== "object") return found;
      if (node.type === "switch") found.push(node);
      for (const child of node.children ?? []) switches(child, found);
      return found;
    };
    const agentSwitch = (tree: any) =>
      switches(tree).find((node) => String(node.props["aria-label"]).includes("agents"));

    // The opposite default to the enable switch: nothing is allowed until the
    // backend says so.
    it("is off unless the backend reports it on", async () => {
      const { render } = makeHost();
      const tree: any = render({ workspaceId: "workspace-1" });
      await Promise.resolve();
      expect(agentSwitch(tree).props.checked).toBe(false);
      expect(JSON.stringify(tree)).toContain("Agents may not merge");
    });

    it("persists the choice through its own action", async () => {
      const { host, render } = makeHost();
      const tree: any = render({ workspaceId: "workspace-1" });
      await Promise.resolve();
      await agentSwitch(tree).props.onCheckedChange(true);
      expect(host.api.invokeAction).toHaveBeenCalledWith(
        "connection.set_agent_merge",
        { workspaceId: "workspace-1", body: { enabled: true } },
        expect.anything(),
      );
    });

    it("is disabled without a workspace", () => {
      const { render } = makeHost();
      const tree: any = render({});
      expect(agentSwitch(tree).props.disabled).toBe(true);
    });
  });
});
