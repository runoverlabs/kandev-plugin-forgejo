import { beforeEach, describe, expect, it, vi } from "vitest";
import { type DetailsResponse } from "../src/panel-model";
import { createReviewPanel } from "../src/review-panel";

const DETAILS: DetailsResponse = {
  provider_id: "forgejo",
  review_key: "scope/5#7",
  number: 7,
  title: "Add x",
  url: "https://forge/o/r/pulls/7",
  state: "open",
  author: "alice",
  head_sha: "abc1234",
  merge: { styles: ["squash", "merge"], default_style: "squash", delete_branch_default: false, blockers: [] },
  actor: "kandev",
  reviews: [],
  checks: [],
  comments: [],
  requested_reviewers: [],
};

const PROPS = {
  presentation: "desktop" as const,
  workspaceId: "ws-1",
  taskId: "task-1",
  reviewKey: "scope/5#7",
  connectionScope: "scope",
  repositoryId: "5",
  changeRequestNumber: 7,
};

function sameDeps(a: unknown[], b: unknown[]): boolean {
  return a.length === b.length && a.every((value, index) => Object.is(value, b[index]));
}

type Node = { type: unknown; props: any; children: unknown[] };

/** A React-shaped host: hooks keep state across renders so a re-render sees it. */
function mount(handlers: Record<string, (body: any) => unknown> = {}) {
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
        if (!(i in state)) state[i] = initial;
        return [state[i], (v: unknown) => (state[i] = typeof v === "function" ? (v as any)(state[i]) : v)];
      },
      // Memoised on shallow-equal deps, like React: an effect re-runs only when
      // its dependencies change, which is what keeps a re-render from reloading.
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
    api: {
      invokeAction: vi.fn(async (key: string, input: any) => {
        calls.push({ key, input });
        const handler = handlers[key];
        if (!handler) throw new Error(`unexpected action ${key}`);
        return handler(input.body);
      }),
    },
  };
  const afterMutation = vi.fn(async () => {});
  const Panel = createReviewPanel(host, { fallback: () => null, afterMutation });
  const render = (): Node => {
    cursor = 0;
    return Panel(PROPS) as Node;
  };
  const settle = async () => {
    const pending = effects.splice(0);
    for (const fn of pending) fn();
    for (let i = 0; i < 6; i++) await Promise.resolve();
  };
  return { host, render, settle, calls, modals, afterMutation };
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

const mergeButton = (tree: Node) => find(tree.props.headerActions, (n) => n.props?.["data-testid"] === "forgejo-merge-button");

describe("review panel", () => {
  beforeEach(() => vi.clearAllMocks());

  it("loads details for the task and the pull request number", async () => {
    const t = mount({ "change_requests.details": () => DETAILS });
    t.render();
    await t.settle();
    expect(t.calls[0]).toEqual({
      key: "change_requests.details",
      input: { workspaceId: "ws-1", taskId: "task-1", body: { number: 7 } },
    });
    const tree = t.render();
    expect(tree.props.detail).toMatchObject({ number: 7, author: { name: "alice" } });
    expect(tree.props.loading).toBe(false);
  });

  it("offers the repository's primary style as the merge button", async () => {
    const t = mount({ "change_requests.details": () => DETAILS });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(mergeButton(tree).children[0]).toBe("Squash and merge");
    expect(mergeButton(tree).props.disabled).toBe(false);
  });

  it("merges with the head the panel last saw, then reloads and refreshes the host", async () => {
    const t = mount({
      "change_requests.details": () => DETAILS,
      "change_requests.merge": () => ({ outcome: "merged" }),
    });
    t.render();
    await t.settle();
    mergeButton(t.render()).props.onClick();
    await t.settle();

    const merge = t.calls.find((c) => c.key === "change_requests.merge")!;
    expect(merge.input).toEqual({
      workspaceId: "ws-1",
      taskId: "task-1",
      body: { number: 7, head_sha: "abc1234", style: "squash", delete_branch: false },
    });
    expect(t.host.toast.success).toHaveBeenCalledWith("Pull request merged");
    expect(t.calls.filter((c) => c.key === "change_requests.details")).toHaveLength(2);
    expect(t.afterMutation).toHaveBeenCalledWith("ws-1", "task-1");
  });

  it("shows the failure and reloads instead of pretending it worked", async () => {
    const t = mount({
      "change_requests.details": () => DETAILS,
      "change_requests.merge": () => {
        throw new Error("The pull request changed since it was last read. Refresh and try again.");
      },
    });
    t.render();
    await t.settle();
    mergeButton(t.render()).props.onClick();
    await t.settle();
    expect(t.host.toast.error).toHaveBeenCalledWith(expect.stringContaining("changed since"));
    expect(t.host.toast.success).not.toHaveBeenCalled();
    expect(t.calls.filter((c) => c.key === "change_requests.details")).toHaveLength(2);
  });

  it("disables merge while it runs and refuses a second click", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const t = mount({
      "change_requests.details": () => DETAILS,
      "change_requests.merge": () => gate,
    });
    t.render();
    await t.settle();
    mergeButton(t.render()).props.onClick();
    await Promise.resolve();
    const during = mergeButton(t.render());
    expect(during.props.disabled).toBe(true);
    expect(during.children[0]).toBe("Merging…");
    during.props.onClick();
    release();
    await t.settle();
    expect(t.calls.filter((c) => c.key === "change_requests.merge")).toHaveLength(1);
  });

  it("explains a blocked merge and disables the button", async () => {
    const t = mount({
      "change_requests.details": () => ({ ...DETAILS, merge: { ...DETAILS.merge, blockers: ["conflicts"] } }),
    });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(mergeButton(tree).props.disabled).toBe(true);
    expect(tree.props.notice.children[0]).toContain("conflicts");
  });

  it("still renders a merge button when the repository reported no styles", async () => {
    const t = mount({ "change_requests.details": () => ({ ...DETAILS, merge: { styles: [], blockers: [] } }) });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(mergeButton(tree).children[0]).toBe("Merge");
    mergeButton(tree).props.onClick();
    await t.settle();
    // No style is sent, so the backend chooses.
    expect(t.calls.find((c) => c.key === "change_requests.merge")?.input.body).not.toHaveProperty("style");
  });

  it("renders no write controls for a merged pull request", async () => {
    const t = mount({ "change_requests.details": () => ({ ...DETAILS, state: "merged" }) });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(tree.props.headerActions).toBeUndefined();
  });

  // A pull request the task does not link answers 404; the panel must show the
  // error and offer no way to act on it.
  it("renders no write controls when the details cannot be loaded", async () => {
    const t = mount({
      "change_requests.details": () => {
        throw new Error("That pull request is not linked to this task.");
      },
    });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(tree.props.headerActions).toBeUndefined();
    expect(tree.props.actions).toBeUndefined();
    expect(tree.props.error).toContain("not linked");
  });

  it("posts a comment through its own action", async () => {
    const t = mount({
      "change_requests.details": () => DETAILS,
      "change_requests.comment": () => ({ ok: true }),
    });
    t.render();
    await t.settle();
    const tree = t.render();
    expect(tree.props.actions[0]).toMatchObject({ id: "comment", placement: "comment", input: "text" });
    await tree.props.onAction({ actionId: "comment", body: "  looks good  " });
    expect(t.calls.find((c) => c.key === "change_requests.comment")?.input.body).toEqual({
      number: 7,
      body: "looks good",
    });
    await tree.props.onAction({ actionId: "comment", body: "   " });
    expect(t.calls.filter((c) => c.key === "change_requests.comment")).toHaveLength(1);
  });

  it("sends the chosen branch-deletion preference with the merge", async () => {
    const t = mount({
      "change_requests.details": () => ({ ...DETAILS, merge: { ...DETAILS.merge, delete_branch_default: true } }),
      "change_requests.merge": () => ({}),
    });
    t.render();
    await t.settle();
    mergeButton(t.render()).props.onClick();
    await t.settle();
    expect(t.calls.find((c) => c.key === "change_requests.merge")?.input.body.delete_branch).toBe(true);
  });

  it("opens the review dialog and submits event, body and inline comments", async () => {
    const t = mount({
      "change_requests.details": () => DETAILS,
      "change_requests.review": () => ({ state: "APPROVED" }),
    });
    t.render();
    await t.settle();
    const review = find(t.render().props.headerActions, (n) => n.children?.[0] === "Review");
    review.props.onClick();
    expect(t.modals[0].title).toBe("Submit review");
    expect(t.modals[0].description).toContain("kandev");

    // Render the dialog content and drive it the way the buttons would.
    const content = t.modals[0].content() as Node;
    const dialog = content.type as (props: any) => Node;
    const onSubmit = content.props.onSubmit as (request: any) => Promise<void>;
    expect(typeof dialog).toBe("function");
    await onSubmit({ event: "request_changes", body: "fix this", comments: [{ path: "a.go", line: 4, body: "nit" }] });

    expect(t.calls.find((c) => c.key === "change_requests.review")?.input.body).toEqual({
      number: 7,
      event: "request_changes",
      body: "fix this",
      comments: [{ path: "a.go", line: 4, body: "nit" }],
      head_sha: "abc1234",
    });
    expect(t.host.toast.success).toHaveBeenCalledWith("Review submitted");
  });
});
