import type { Component, PluginHostApi } from "@kandev/plugin-sdk";
import { createAutomationSection } from "./automation";
import type { ChangeRequestDetailModel } from "./detail";
import {
  type DetailsResponse,
  type MergeChoice,
  type ReviewRequest,
  failureMessage,
  mergeChoice,
  mergeDisabled,
  mergeNotice,
  mergeOffered,
  styleLabel,
  toDetailModel,
} from "./panel-model";
import { createReviewDialog } from "./review-dialog";

export type PanelProps = {
  presentation: "desktop" | "mobile";
  workspaceId: string;
  taskId: string;
  reviewKey: string;
  connectionScope: string;
  repositoryId: string;
  changeRequestNumber: string | number;
};

export type ReviewPanelOptions = {
  /** The narrow snapshot's model, shown until the full details arrive. */
  fallback(props: PanelProps): ChangeRequestDetailModel | null;
  /** Refreshes the host's sidebar and Kanban glyphs after a mutation. */
  afterMutation(workspaceId: string, taskId: string): Promise<void>;
};

type PanelState = {
  props: PanelProps;
  number: number;
  details: DetailsResponse | null;
  loading: boolean;
  error: string | null;
  busy: string | null;
  setBusy: (busy: string | null) => void;
  deleteBranch: boolean | null;
  setDeleteBranch: (deleteBranch: boolean) => void;
  refreshKey: number;
  load: (signal?: AbortSignal) => Promise<void>;
};

type PanelWrites = {
  run: (id: string, write: () => Promise<unknown>, success: string) => Promise<void>;
  act: (key: string, body: Record<string, unknown>) => Promise<unknown>;
  merge: (style: string) => void;
  openReview: () => void;
};

/** What the header's merge controls work from. */
type MergeControls = {
  current: DetailsResponse;
  busy: string | null;
  choice: MergeChoice;
  wantsDelete: boolean;
  setDeleteBranch: (deleteBranch: boolean) => void;
  writes: PanelWrites;
};

/**
 * The Review panel: full pull request detail from `change_requests.details`,
 * with merge, review and comment wired to their own write actions.
 *
 * It paints the narrow snapshot immediately and swaps in the full model when
 * the details arrive, so opening the panel never starts blank. Every write is a
 * separate action keyed to the task; the pull request is never named by owner or
 * repository, only by its number among those the task already links.
 */
export function createReviewPanel(host: PluginHostApi, options: ReviewPanelOptions): Component<PanelProps> {
  const ReviewDialog = createReviewDialog(host);
  const AutomationSection = createAutomationSection(host);

  return function ReviewPanel(props: PanelProps) {
    const state = useReviewPanelState(host, props);
    const writes = panelWrites(host, options, ReviewDialog, state);
    const { details: current, loading, error, busy } = state;
    const model = current ? toDetailModel(current) : options.fallback(props);

    return host.jsx(host.ui.ChangeRequestDetail, {
      detail: model,
      presentation: props.presentation,
      loading: loading && model === null,
      contentLoading: loading && model !== null,
      error: model === null ? error : null,
      onRefresh: () => void state.load(),
      onRetry: () => void state.load(),
      actions: commentActions(current, busy),
      busyActionId: busy,
      onAction: async (request: { actionId: string; body?: string }) => {
        if (request.actionId !== "comment") return;
        const text = (request.body ?? "").trim();
        if (!text) return;
        await writes.run("comment", () => writes.act("change_requests.comment", { body: text }), "Comment posted");
      },
      headerActions: renderHeaderActions(host, state, writes),
      notice: renderNotice(host, AutomationSection, state),
    });
  };
}

function useReviewPanelState(host: PluginHostApi, props: PanelProps): PanelState {
  const [details, setDetails] = host.React.useState(null as DetailsResponse | null);
  const [loading, setLoading] = host.React.useState(true);
  const [error, setError] = host.React.useState(null as string | null);
  const [busy, setBusy] = host.React.useState(null as string | null);
  const [deleteBranch, setDeleteBranch] = host.React.useState(null as boolean | null);
  // Bumped on every reload, so the automation section refreshes with the
  // panel instead of polling on its own.
  const [refreshKey, setRefreshKey] = host.React.useState(0);

  const number = Number(props.changeRequestNumber);

  const load = host.React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setRefreshKey((key: number) => key + 1);
      try {
        const response = await host.api.invokeAction<DetailsResponse>(
          "change_requests.details",
          { workspaceId: props.workspaceId, taskId: props.taskId, body: { number } },
          signal ? { signal } : undefined,
        );
        if (signal?.aborted) return;
        setDetails(response);
        setError(null);
      } catch (cause) {
        if (signal?.aborted) return;
        setError(failureMessage(cause));
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [props.workspaceId, props.taskId, number],
  );

  host.React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  return {
    props,
    number,
    details: details as DetailsResponse | null,
    loading,
    error: error as string | null,
    busy: busy as string | null,
    setBusy,
    deleteBranch: deleteBranch as boolean | null,
    setDeleteBranch,
    refreshKey: refreshKey as number,
    load,
  };
}

function wantsDeleteBranch(state: PanelState): boolean {
  return state.deleteBranch ?? state.details?.merge?.delete_branch_default ?? false;
}

function panelWrites(
  host: PluginHostApi,
  options: ReviewPanelOptions,
  ReviewDialog: ReturnType<typeof createReviewDialog>,
  state: PanelState,
): PanelWrites {
  const { props, number, busy, setBusy, load } = state;
  const current = state.details;

  // One write at a time. A failure is a toast and a reload, because the usual
  // cause (the head moved, someone else merged) is something the fresh state
  // explains.
  const run = async (id: string, write: () => Promise<unknown>, success: string) => {
    if (busy) return;
    setBusy(id);
    try {
      await write();
      host.toast.success(success);
    } catch (cause) {
      host.toast.error(failureMessage(cause));
    } finally {
      setBusy(null);
      await load();
      void options.afterMutation(props.workspaceId, props.taskId);
    }
  };

  const act = (key: string, body: Record<string, unknown>) =>
    host.api.invokeAction(key, { workspaceId: props.workspaceId, taskId: props.taskId, body: { number, ...body } });

  const merge = (style: string) => {
    if (!current) return;
    void run(
      "merge",
      () =>
        act("change_requests.merge", {
          head_sha: current.head_sha ?? "",
          ...(style ? { style } : {}),
          delete_branch: wantsDeleteBranch(state),
        }),
      "Pull request merged",
    );
  };

  const openReview = () => {
    let modal: { close(): void } | null = null;
    modal = host.openModal({
      title: "Submit review",
      ...(current?.actor
        ? { description: `Posted as ${current.actor}, the account Kandev uses for Forgejo.` }
        : {}),
      size: "md",
      content: () =>
        host.jsx(ReviewDialog, {
          onClose: () => modal?.close(),
          onSubmit: async (request: ReviewRequest) => {
            await act("change_requests.review", {
              event: request.event,
              body: request.body,
              comments: request.comments,
              head_sha: current?.head_sha ?? "",
            });
            host.toast.success("Review submitted");
            await load();
            void options.afterMutation(props.workspaceId, props.taskId);
          },
        }),
    });
  };

  return { run, act, merge, openReview };
}

function renderHeaderActions(host: PluginHostApi, state: PanelState, writes: PanelWrites) {
  const current = state.details;
  if (!current || !mergeOffered(current)) return undefined;
  const controls: MergeControls = {
    current,
    busy: state.busy,
    choice: mergeChoice(current),
    wantsDelete: wantsDeleteBranch(state),
    setDeleteBranch: state.setDeleteBranch,
    writes,
  };
  const { busy } = controls;
  return host.jsx(
    "div",
    { className: "forgejo-merge" },
    host.jsx(
      host.ui.Button,
      { type: "button", size: "sm", variant: "outline", disabled: busy !== null, onClick: writes.openReview },
      "Review",
    ),
    renderMergeButton(host, controls),
    controls.choice.others.length > 0 ? renderMergeMenu(host, controls) : null,
    host.jsx(
      "label",
      { className: "forgejo-merge__delete" },
      host.jsx(host.ui.Checkbox, {
        checked: controls.wantsDelete,
        disabled: busy !== null,
        "aria-label": "Delete branch after merge",
        onCheckedChange: (next: boolean) => controls.setDeleteBranch(next === true),
      }),
      host.jsx("span", null, "Delete branch"),
    ),
    current.actor
      ? host.jsx("span", { className: "forgejo-merge__actor" }, `as ${current.actor}`)
      : null,
  );
}

function renderMergeButton(host: PluginHostApi, controls: MergeControls) {
  const { current, busy, choice, writes } = controls;
  return host.jsx(
    host.ui.Button,
    {
      type: "button",
      size: "sm",
      "data-testid": "forgejo-merge-button",
      disabled: busy !== null || mergeDisabled(current),
      onClick: () => writes.merge(choice.primary),
    },
    busy === "merge" ? "Merging…" : !choice.unreported ? styleLabel(choice.primary) : "Merge",
  );
}

function renderMergeMenu(host: PluginHostApi, controls: MergeControls) {
  const { current, busy, choice, writes } = controls;
  return host.jsx(
    host.ui.DropdownMenu,
    null,
    host.jsx(
      host.ui.DropdownMenuTrigger,
      { asChild: true },
      host.jsx(
        host.ui.Button,
        {
          type: "button",
          size: "sm",
          variant: "outline",
          "aria-label": "More merge options",
          disabled: busy !== null || mergeDisabled(current),
        },
        "▾",
      ),
    ),
    host.jsx(
      host.ui.DropdownMenuContent,
      { align: "end" },
      choice.others.map((style) =>
        host.jsx(host.ui.DropdownMenuItem, { key: style, onSelect: () => writes.merge(style) }, styleLabel(style)),
      ),
    ),
  );
}

function renderNotice(
  host: PluginHostApi,
  AutomationSection: ReturnType<typeof createAutomationSection>,
  state: PanelState,
) {
  const current = state.details;
  const noticeText = current ? mergeNotice(current) : null;
  const mergeNoticeNode = noticeText
    ? host.jsx("p", { className: "forgejo-merge-notice", role: "status", "data-testid": "forgejo-merge-notice" }, noticeText)
    : null;
  // The host's detail component has no slot of its own for plugin controls;
  // its notice area takes any node, so automation lives there.
  const automation =
    current && Number.isFinite(state.number)
      ? host.jsx(AutomationSection, {
          workspaceId: state.props.workspaceId,
          taskId: state.props.taskId,
          number: state.number,
          refreshKey: state.refreshKey,
        })
      : null;
  return mergeNoticeNode || automation
    ? host.jsx("div", { className: "forgejo-panel-notice" }, mergeNoticeNode, automation)
    : undefined;
}

function commentActions(current: DetailsResponse | null, busy: string | null) {
  return current
    ? [
        {
          id: "comment",
          label: "Comment",
          pendingLabel: "Posting…",
          placement: "comment" as const,
          input: "text" as const,
          busy: busy === "comment",
          disabled: busy !== null && busy !== "comment",
        },
      ]
    : undefined;
}
