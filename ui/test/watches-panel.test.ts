import { beforeEach, describe, expect, it, vi } from "vitest";
import { createWatchesPanel } from "../src/watches-panel";

// Minimal React-shaped host stub: hooks run eagerly so a single render()
// exercises the effect body the way the host would.
function makeHost(overrides: Record<string, any> = {}, watchList: any[] | null = null) {
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
        if (key === "watches.list" && watchList) return { watches: watchList };
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
              {
                id: "wf-1",
                name: "Autopilot",
                steps: [
                  { id: "step-inbox", name: "Inbox", is_start_step: true },
                  { id: "step-run", name: "Run", auto_starts_agent: true },
                ],
              },
            ],
            default_review_prompt: "Review Pull Request #{{pr.number}}",
            min_review_interval: 60,
            archive_granted: false,
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
    render(props: { workspaceId?: string } = {}, kind?: "issue" | "review") {
      cursor = 0;
      effects.length = 0;
      const tree = createWatchesPanel(host, kind ? { kind } : undefined)(props);
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
      expect(selector.workspaceId).toBe("workspace-1");
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
    expect(selector.workspaceId).toBe("workspace-active");
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

// --- review watches ---------------------------------------------------------

type Node = { type: unknown; props: any; children: unknown[] };

function findAll(node: any, predicate: (n: Node) => boolean, found: Node[] = []): Node[] {
  if (node === null || node === undefined || typeof node !== "object") return found;
  if (Array.isArray(node)) {
    for (const entry of node) findAll(entry, predicate, found);
    return found;
  }
  if ("type" in node && predicate(node as Node)) found.push(node as Node);
  if ("children" in node) findAll((node as Node).children, predicate, found);
  return found;
}

const button = (tree: unknown, label: string) =>
  findAll(tree, (n) => n.type === "button" && textOf(n).trim() === label)[0];

const reviewRow = {
  id: "rw-1",
  kind: "review",
  name: "My reviews",
  workflow_id: "wf-1",
  workflow_step_id: "step-inbox",
  review_scope: "user_and_teams",
  cleanup_policy: "when_closed",
  enabled: true,
};

async function openForm(kind: "issue" | "review", watchList: any[] = []) {
  const made = makeHost({}, watchList);
  made.render({ workspaceId: "workspace-1" }, kind);
  await flush();
  let tree = made.render({ workspaceId: "workspace-1" }, kind);
  button(tree, "Add watch")!.props.onClick();
  tree = made.render({ workspaceId: "workspace-1" }, kind);
  return { ...made, tree };
}

describe("review watches", () => {
  beforeEach(() => vi.clearAllMocks());

  it("lists only its own kind", async () => {
    const issue = makeHost();
    issue.render({ workspaceId: "workspace-1" }, "issue");
    const review = makeHost();
    review.render({ workspaceId: "workspace-1" }, "review");
    await flush();
    const listBody = (h: any) => h.host.api.invokeAction.mock.calls.find(([k]: [string]) => k === "watches.list")[1].body;
    expect(listBody(issue)).toEqual({ kind: "issue" });
    expect(listBody(review)).toEqual({ kind: "review" });
  });

  it("shows the review fields and hides the issue-only ones", async () => {
    const { tree } = await openForm("review");
    const text = textOf(tree);
    for (const label of [
      "Whose requests",
      "Column for fork pull requests",
      "When a pull request is merged or closed",
      "Repositories (optional)",
    ]) {
      expect(text).toContain(label);
    }
    expect(findAll(tree, (n) => n.props?.["aria-label"] === "Also file draft pull requests")).toHaveLength(1);
    expect(text).not.toContain("Issue state");
    expect(text).not.toContain("Duplicate handling");
  });

  it("leaves the issue form exactly as it was", async () => {
    const { tree } = await openForm("issue");
    const text = textOf(tree);
    expect(text).toContain("Issue state");
    expect(text).toContain("Duplicate handling");
    for (const label of ["Whose requests", "Column for fork pull requests", "merged or closed"]) {
      expect(text).not.toContain(label);
    }
  });

  it("offers only columns that do not start an agent for fork pull requests", async () => {
    const { tree } = await openForm("review");
    const field = findAll(tree, (n) => n.type === "div" && textOf(n).startsWith("Column for fork pull requests"))[0]!;
    const items = findAll(field, (n) => n.type === "select-item").map((n) => textOf(n).trim());
    expect(items).toContain("Inbox");
    expect(items).not.toContain("Run");
  });

  it("creates a review watch with its review fields and its kind", async () => {
    const { host, render, tree } = await openForm("review");
    findAll(tree, (n) => n.type === "input" && n.props.placeholder === "My reviews")[0]!.props.onChange({
      target: { value: "Reviews" },
    });
    const filled = render({ workspaceId: "workspace-1" }, "review");
    button(filled, "Create watch")!.props.onClick();
    await flush();

    const create = host.api.invokeAction.mock.calls.find(([key]: [string]) => key === "watches.create");
    expect(create).toBeTruthy();
    expect(create[1].workspaceId).toBe("workspace-1");
    expect(create[1].body).toMatchObject({
      kind: "review",
      name: "Reviews",
      review_scope: "user_and_teams",
      include_drafts: false,
      cleanup_policy: "never",
      fork_workflow_step_id: "",
      workflow_id: "wf-1",
      workflow_step_id: "step-inbox",
    });
  });

  it("does not send review fields from an issue watch", async () => {
    const { host, render, tree } = await openForm("issue");
    findAll(tree, (n) => n.type === "input" && n.props.placeholder === "Bug reports")[0]!.props.onChange({
      target: { value: "Bugs" },
    });
    findAll(tree, (n) => n.type === "input" && n.props.placeholder === "owner/name, owner/other")[0]!.props.onChange({
      target: { value: "acme/app" },
    });
    button(render({ workspaceId: "workspace-1" }, "issue"), "Create watch")!.props.onClick();
    await flush();
    const body = host.api.invokeAction.mock.calls.find(([key]: [string]) => key === "watches.create")[1].body;
    expect(body.kind).toBe("issue");
    for (const key of ["review_scope", "include_drafts", "cleanup_policy", "fork_workflow_step_id"]) {
      expect(body).not.toHaveProperty(key);
    }
  });

  it("explains the missing archive grant when cleanup is on", async () => {
    const { host, render, tree } = await openForm("review");
    // Choose "Archive the task": the first select with that item.
    const select = findAll(tree, (n) => n.type === "select" && textOf(n).includes("Archive the task"))[0]!;
    select.props.onValueChange("when_closed");
    expect(textOf(render({ workspaceId: "workspace-1" }, "review"))).toContain("Host v2 tasks");
    void host;
  });

  it("offers a manual cleanup only where a policy is set and reports the outcome", async () => {
    const made = makeHost({}, [reviewRow, { ...reviewRow, id: "rw-2", name: "Quiet", cleanup_policy: "never" }]);
    made.host.api.invokeAction.mockImplementation(async (key: string) => {
      if (key === "watches.list") return { watches: [reviewRow, { ...reviewRow, id: "rw-2", name: "Quiet", cleanup_policy: "never" }] };
      if (key === "watches.cleanup") return { result: { archived: 2, completed: 1 } };
      return {};
    });
    made.render({ workspaceId: "workspace-1" }, "review");
    await flush();
    const tree = made.render({ workspaceId: "workspace-1" }, "review");

    const buttons = findAll(tree, (n) => n.type === "button" && textOf(n).trim() === "Clean up now");
    expect(buttons).toHaveLength(1);
    buttons[0]!.props.onClick();
    await flush();
    expect(made.host.api.invokeAction).toHaveBeenCalledWith(
      "watches.cleanup",
      { workspaceId: "workspace-1", body: { id: "rw-1" } },
      undefined,
    );
    expect(textOf(made.render({ workspaceId: "workspace-1" }, "review"))).toContain("2 archived, 1 completed.");
  });

  it("says why tasks were completed instead of archived", async () => {
    const made = makeHost({}, [{ ...reviewRow, last_cleanup_note: "Tasks are completed, not archived: no grant." }]);
    made.render({ workspaceId: "workspace-1" }, "review");
    await flush();
    expect(textOf(made.render({ workspaceId: "workspace-1" }, "review"))).toContain("not archived: no grant.");
  });

  it("pausing sends only the id and the switch, so no field is erased", async () => {
    const made = makeHost({}, [reviewRow]);
    made.render({ workspaceId: "workspace-1" }, "review");
    await flush();
    const tree = made.render({ workspaceId: "workspace-1" }, "review");
    findAll(tree, (n) => n.type === "switch" && String(n.props["aria-label"]).startsWith("Enable the"))[0]!.props.onCheckedChange(false);
    await flush();
    expect(made.host.api.invokeAction).toHaveBeenCalledWith(
      "watches.update",
      { workspaceId: "workspace-1", body: { id: "rw-1", enabled: false } },
      undefined,
    );
  });

  it("summarizes drafts, forks and retirements from a run", async () => {
    const made = makeHost({}, [reviewRow]);
    made.host.api.invokeAction.mockImplementation(async (key: string) => {
      if (key === "watches.list") return { watches: [reviewRow] };
      if (key === "watches.run") {
        return { result: { matched: 5, created: 1, drafts: 2, skipped_forks: 1, archived: 1, completed: 0 } };
      }
      return {};
    });
    made.render({ workspaceId: "workspace-1" }, "review");
    await flush();
    button(made.render({ workspaceId: "workspace-1" }, "review"), "Run now")!.props.onClick();
    await flush();
    const text = textOf(made.render({ workspaceId: "workspace-1" }, "review"));
    expect(text).toContain("2 draft(s) left out");
    expect(text).toContain("1 from a fork left out");
    expect(text).toContain("1 archived, 0 completed");
  });
});
