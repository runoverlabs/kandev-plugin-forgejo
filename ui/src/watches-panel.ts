import type { Component, PluginHostApi } from "@kandev/plugin-sdk";
import { renderWatchForm, type WatchFormContext } from "./watch-form";
import {
  draftToBody,
  emptyDraft,
  formatWhen,
  summarize,
  summarizeCleanup,
  toDraft,
  watchQueryLine,
  type Draft,
  type RunResult,
  type Watch,
  type WatchKind,
  type WatchOptions,
} from "./watch-model";
import { useWorkspaceId } from "./workspace";

export type { Watch, WatchKind } from "./watch-model";

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

type Call = <T>(key: string, body?: unknown, signal?: AbortSignal) => Promise<T>;

/** The panel's state and the workspace it is scoped to. */
type WatchList = {
  workspaceId: string;
  watches: Watch[];
  options: WatchOptions;
  draft: Draft | null;
  setDraft: (draft: Draft | null) => void;
  loading: boolean;
  busy: string | null;
  error: string | null;
  notice: string | null;
  call: Call;
  act: (id: string, run: () => Promise<string | null>) => Promise<void>;
};

type WatchActions = {
  save: (draft: Draft) => Promise<void>;
  runNow: (watch: Watch) => Promise<void>;
  toggle: (watch: Watch, enabled: boolean) => Promise<void>;
  remove: (watch: Watch) => Promise<void>;
  reset: (watch: Watch) => Promise<void>;
  cleanUp: (watch: Watch) => Promise<void>;
};

/** What the row and form renderers read. */
type WatchesView = WatchFormContext & {
  kind: WatchKind;
  actions: WatchActions;
};

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
    const list = useWatchList(host, kind, props.workspaceId);
    const actions = useWatchActions(host, list, { review, noun });

    if (!list.workspaceId) {
      return host.jsx(
        "p",
        { className: "forgejo-watches__empty" },
        `Open a workspace to manage Forgejo ${noun} watches.`,
      );
    }
    const view: WatchesView = {
      host,
      kind,
      review,
      noun,
      options: list.options,
      busy: list.busy,
      setDraft: list.setDraft,
      save: actions.save,
      actions,
    };
    return renderWatchesPanel(view, list);
  };
}

function useWatchList(host: PluginHostApi, kind: WatchKind, routedWorkspaceId: string | undefined): WatchList {
  const [watches, setWatches] = host.React.useState<Watch[]>([]);
  const [options, setOptions] = host.React.useState<WatchOptions>({});
  const [draft, setDraft] = host.React.useState<Draft | null>(null);
  const [loading, setLoading] = host.React.useState(false);
  const [busy, setBusy] = host.React.useState<string | null>(null);
  const [error, setError] = host.React.useState<string | null>(null);
  const [notice, setNotice] = host.React.useState<string | null>(null);

  const workspaceId = useWorkspaceId(host, routedWorkspaceId);

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
          call<WatchOptions>("watches.options", undefined, signal),
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

  return { workspaceId, watches, options, draft, setDraft, loading, busy, error, notice, call, act };
}

function useWatchActions(
  host: PluginHostApi,
  { act, call, setDraft }: WatchList,
  { review, noun }: { review: boolean; noun: string },
): WatchActions {
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

  return { save, runNow, toggle, remove, reset, cleanUp };
}

function renderWatchesPanel(view: WatchesView, list: WatchList) {
  const { host, review } = view;
  const { watches, loading, error, notice, draft } = list;
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
    ...watches.map((watch) => renderWatchRow(view, watch)),
    draft
      ? renderWatchForm(view, draft)
      : host.jsx(
          host.ui.Button,
          {
            type: "button",
            variant: "secondary",
            size: "sm",
            onClick: () => view.setDraft(emptyDraft(view.options, view.kind)),
          },
          "Add watch",
        ),
  );
}

function renderWatchRow(view: WatchesView, watch: Watch) {
  const { host, busy, actions } = view;
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
        onCheckedChange: (next: boolean) => void actions.toggle(watch, next),
      }),
    ),
    host.jsx("p", { className: "forgejo-watch__query" }, watchQueryLine(watch, view.review)),
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
    renderRowActions(view, watch),
  );
}

function renderRowActions(view: WatchesView, watch: Watch) {
  const { host, busy, actions } = view;
  const working = busy === watch.id;
  const ghost = (label: string, onClick: () => void) =>
    host.jsx(host.ui.Button, { type: "button", variant: "ghost", size: "sm", disabled: working, onClick }, label);
  return host.jsx(
    "div",
    { className: "forgejo-watch__actions" },
    host.jsx(
      host.ui.Button,
      {
        type: "button",
        variant: "secondary",
        size: "sm",
        disabled: working || watch.enabled === false,
        onClick: () => void actions.runNow(watch),
      },
      working ? "Working…" : "Run now",
    ),
    ghost("Edit", () => view.setDraft(toDraft(watch, view.options, view.kind))),
    view.review && watch.cleanup_policy === "when_closed"
      ? ghost("Clean up now", () => void actions.cleanUp(watch))
      : null,
    ghost("Forget history", () => void actions.reset(watch)),
    ghost("Delete", () => void actions.remove(watch)),
  );
}
