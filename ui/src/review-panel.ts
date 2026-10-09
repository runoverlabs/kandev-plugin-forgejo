import type { Component, PluginHostApi } from "@kandev/plugin-sdk";
import type { ChangeRequestDetailModel } from "./detail";
import {
  type DetailsResponse,
  failureMessage,
  mergeChoice,
  mergeDisabled,
  mergeNotice,
  mergeOffered,
  styleLabel,
  toDetailModel,
} from "./panel-model";

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

type ReviewEvent = "approve" | "request_changes" | "comment";

const EVENT_LABELS: Record<ReviewEvent, string> = {
  approve: "Approve",
  request_changes: "Request changes",
  comment: "Comment",
};

type InlineDraft = { path: string; line: string; body: string };

/**
 * The dialog the host's detail component has no equivalent for: pick a review
 * event, write a body, optionally attach inline comments. It owns its own draft
 * state and hands a finished request to `onSubmit`.
 */
function createReviewDialog(host: PluginHostApi) {
  return function ReviewDialog(props: {
    onSubmit(request: {
      event: ReviewEvent;
      body: string;
      comments: { path: string; line: number; body: string }[];
    }): Promise<void>;
    onClose(): void;
  }) {
    const [event, setEvent] = host.React.useState("comment" as ReviewEvent);
    const [body, setBody] = host.React.useState("");
    const [inline, setInline] = host.React.useState([] as InlineDraft[]);
    const [busy, setBusy] = host.React.useState(false);
    const [problem, setProblem] = host.React.useState("");

    const submit = async () => {
      const comments: { path: string; line: number; body: string }[] = [];
      for (const draft of inline as InlineDraft[]) {
        const line = Number.parseInt(draft.line, 10);
        if (!draft.path.trim() || !draft.body.trim() || !Number.isInteger(line) || line <= 0) {
          setProblem("Each inline comment needs a file path, a line number and some text.");
          return;
        }
        comments.push({ path: draft.path.trim(), line, body: draft.body.trim() });
      }
      if (event !== "approve" && !(body as string).trim() && comments.length === 0) {
        setProblem("Write something, or choose Approve.");
        return;
      }
      setProblem("");
      setBusy(true);
      try {
        await props.onSubmit({ event: event as ReviewEvent, body: (body as string).trim(), comments });
        props.onClose();
      } catch (cause) {
        setProblem(failureMessage(cause));
        setBusy(false);
      }
    };

    const updateInline = (index: number, patch: Partial<InlineDraft>) =>
      setInline((current: InlineDraft[]) =>
        current.map((draft, i) => (i === index ? { ...draft, ...patch } : draft)),
      );

    return host.jsx(
      "div",
      { className: "forgejo-review-dialog" },
      host.jsx(
        "div",
        { className: "forgejo-review-dialog__events", role: "group", "aria-label": "Review type" },
        (Object.keys(EVENT_LABELS) as ReviewEvent[]).map((key) =>
          host.jsx(
            host.ui.Button,
            {
              key,
              type: "button",
              size: "sm",
              variant: event === key ? "default" : "outline",
              "aria-pressed": event === key,
              disabled: busy,
              onClick: () => setEvent(key),
            },
            EVENT_LABELS[key],
          ),
        ),
      ),
      host.jsx(host.ui.Textarea, {
        value: body,
        placeholder: "Leave a comment",
        "aria-label": "Review body",
        rows: 5,
        disabled: busy,
        onChange: (changed: { target: { value: string } }) => setBody(changed.target.value),
      }),
      (inline as InlineDraft[]).map((draft, index) =>
        host.jsx(
          "div",
          { key: index, className: "forgejo-review-dialog__inline" },
          host.jsx(host.ui.Input, {
            value: draft.path,
            placeholder: "path/to/file",
            "aria-label": "File path",
            onChange: (changed: { target: { value: string } }) => updateInline(index, { path: changed.target.value }),
          }),
          host.jsx(host.ui.Input, {
            value: draft.line,
            placeholder: "line",
            inputMode: "numeric",
            "aria-label": "Line number",
            onChange: (changed: { target: { value: string } }) => updateInline(index, { line: changed.target.value }),
          }),
          host.jsx(host.ui.Textarea, {
            value: draft.body,
            placeholder: "Comment on this line",
            "aria-label": "Inline comment",
            rows: 2,
            onChange: (changed: { target: { value: string } }) => updateInline(index, { body: changed.target.value }),
          }),
          host.jsx(
            host.ui.Button,
            {
              type: "button",
              size: "sm",
              variant: "ghost",
              onClick: () => setInline((current: InlineDraft[]) => current.filter((_, i) => i !== index)),
            },
            "Remove",
          ),
        ),
      ),
      (inline as InlineDraft[]).length < 20
        ? host.jsx(
            host.ui.Button,
            {
              type: "button",
              size: "sm",
              variant: "secondary",
              disabled: busy,
              onClick: () => setInline((current: InlineDraft[]) => [...current, { path: "", line: "", body: "" }]),
            },
            "Add inline comment",
          )
        : null,
      problem ? host.jsx("p", { role: "alert", className: "forgejo-review-dialog__error" }, problem) : null,
      host.jsx(
        "div",
        { className: "forgejo-review-dialog__footer" },
        host.jsx(host.ui.Button, { type: "button", variant: "ghost", disabled: busy, onClick: props.onClose }, "Cancel"),
        host.jsx(
          host.ui.Button,
          { type: "button", disabled: busy, onClick: () => void submit() },
          busy ? "Submitting…" : "Submit review",
        ),
      ),
    );
  };
}

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

  return function ReviewPanel(props: PanelProps) {
    const [details, setDetails] = host.React.useState(null as DetailsResponse | null);
    const [loading, setLoading] = host.React.useState(true);
    const [error, setError] = host.React.useState(null as string | null);
    const [busy, setBusy] = host.React.useState(null as string | null);
    const [deleteBranch, setDeleteBranch] = host.React.useState(null as boolean | null);

    const number = Number(props.changeRequestNumber);

    const load = host.React.useCallback(
      async (signal?: AbortSignal) => {
        setLoading(true);
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

    const current = details as DetailsResponse | null;
    const model = current ? toDetailModel(current) : options.fallback(props);
    const choice = current ? mergeChoice(current) : null;
    const wantsDelete = deleteBranch ?? current?.merge?.delete_branch_default ?? false;

    const merge = (style: string) => {
      if (!current) return;
      void run(
        "merge",
        () =>
          act("change_requests.merge", {
            head_sha: current.head_sha ?? "",
            ...(style ? { style } : {}),
            delete_branch: wantsDelete,
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
            onSubmit: async (request: {
              event: ReviewEvent;
              body: string;
              comments: { path: string; line: number; body: string }[];
            }) => {
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

    const headerActions =
      current && mergeOffered(current)
        ? host.jsx(
            "div",
            { className: "forgejo-merge" },
            host.jsx(
              host.ui.Button,
              { type: "button", size: "sm", variant: "outline", disabled: busy !== null, onClick: openReview },
              "Review",
            ),
            host.jsx(
              host.ui.Button,
              {
                type: "button",
                size: "sm",
                "data-testid": "forgejo-merge-button",
                disabled: busy !== null || mergeDisabled(current),
                onClick: () => merge(choice?.primary ?? ""),
              },
              busy === "merge" ? "Merging…" : choice && !choice.unreported ? styleLabel(choice.primary) : "Merge",
            ),
            choice && choice.others.length > 0
              ? host.jsx(
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
                      host.jsx(host.ui.DropdownMenuItem, { key: style, onSelect: () => merge(style) }, styleLabel(style)),
                    ),
                  ),
                )
              : null,
            host.jsx(
              "label",
              { className: "forgejo-merge__delete" },
              host.jsx(host.ui.Checkbox, {
                checked: wantsDelete,
                disabled: busy !== null,
                "aria-label": "Delete branch after merge",
                onCheckedChange: (next: boolean) => setDeleteBranch(next === true),
              }),
              host.jsx("span", null, "Delete branch"),
            ),
            current.actor
              ? host.jsx("span", { className: "forgejo-merge__actor" }, `as ${current.actor}`)
              : null,
          )
        : undefined;

    const noticeText = current ? mergeNotice(current) : null;
    const notice = noticeText
      ? host.jsx("p", { className: "forgejo-merge-notice", role: "status", "data-testid": "forgejo-merge-notice" }, noticeText)
      : undefined;

    const actions = current
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

    return host.jsx(host.ui.ChangeRequestDetail, {
      detail: model,
      presentation: props.presentation,
      loading: loading && model === null,
      contentLoading: loading && model !== null,
      error: model === null ? error : null,
      onRefresh: () => void load(),
      onRetry: () => void load(),
      actions,
      busyActionId: busy,
      onAction: async (request: { actionId: string; body?: string }) => {
        if (request.actionId !== "comment") return;
        const text = (request.body ?? "").trim();
        if (!text) return;
        await run("comment", () => act("change_requests.comment", { body: text }), "Comment posted");
      },
      headerActions,
      notice,
    });
  };
}
