import type { PluginHostApi } from "@kandev/plugin-sdk";
import {
  EVENT_LABELS,
  failureMessage,
  reviewRequest,
  type InlineDraft,
  type ReviewEvent,
  type ReviewRequest,
} from "./panel-model";

export type ReviewDialogProps = {
  onSubmit(request: ReviewRequest): Promise<void>;
  onClose(): void;
};

type Dialog = {
  event: ReviewEvent;
  setEvent: (event: ReviewEvent) => void;
  body: string;
  setBody: (body: string) => void;
  inline: InlineDraft[];
  setInline: (update: (current: InlineDraft[]) => InlineDraft[]) => void;
  busy: boolean;
  problem: string;
  submit: () => Promise<void>;
  updateInline: (index: number, patch: Partial<InlineDraft>) => void;
};

/**
 * The dialog the host's detail component has no equivalent for: pick a review
 * event, write a body, optionally attach inline comments. It owns its own draft
 * state and hands a finished request to `onSubmit`.
 */
export function createReviewDialog(host: PluginHostApi) {
  return function ReviewDialog(props: ReviewDialogProps) {
    const dialog = useReviewDialog(host, props);
    const { busy, problem } = dialog;
    return host.jsx(
      "div",
      { className: "forgejo-review-dialog" },
      renderEventButtons(host, dialog),
      host.jsx(host.ui.Textarea, {
        value: dialog.body,
        placeholder: "Leave a comment",
        "aria-label": "Review body",
        rows: 5,
        disabled: busy,
        onChange: (changed: { target: { value: string } }) => dialog.setBody(changed.target.value),
      }),
      dialog.inline.map((draft, index) => renderInlineDraft(host, dialog, draft, index)),
      dialog.inline.length < 20
        ? host.jsx(
            host.ui.Button,
            {
              type: "button",
              size: "sm",
              variant: "secondary",
              disabled: busy,
              onClick: () => dialog.setInline((current: InlineDraft[]) => [...current, { path: "", line: "", body: "" }]),
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
          { type: "button", disabled: busy, onClick: () => void dialog.submit() },
          busy ? "Submitting…" : "Submit review",
        ),
      ),
    );
  };
}

function useReviewDialog(host: PluginHostApi, props: ReviewDialogProps): Dialog {
  const [event, setEvent] = host.React.useState("comment" as ReviewEvent);
  const [body, setBody] = host.React.useState("");
  const [inline, setInline] = host.React.useState([] as InlineDraft[]);
  const [busy, setBusy] = host.React.useState(false);
  const [problem, setProblem] = host.React.useState("");

  const submit = async () => {
    const checked = reviewRequest(event as ReviewEvent, body as string, inline as InlineDraft[]);
    if ("problem" in checked) {
      setProblem(checked.problem);
      return;
    }
    setProblem("");
    setBusy(true);
    try {
      await props.onSubmit(checked.request);
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

  return {
    event: event as ReviewEvent,
    setEvent,
    body: body as string,
    setBody,
    inline: inline as InlineDraft[],
    setInline,
    busy,
    problem: problem as string,
    submit,
    updateInline,
  };
}

function renderEventButtons(host: PluginHostApi, dialog: Dialog) {
  return host.jsx(
    "div",
    { className: "forgejo-review-dialog__events", role: "group", "aria-label": "Review type" },
    (Object.keys(EVENT_LABELS) as ReviewEvent[]).map((key) =>
      host.jsx(
        host.ui.Button,
        {
          key,
          type: "button",
          size: "sm",
          variant: dialog.event === key ? "default" : "outline",
          "aria-pressed": dialog.event === key,
          disabled: dialog.busy,
          onClick: () => dialog.setEvent(key),
        },
        EVENT_LABELS[key],
      ),
    ),
  );
}

function renderInlineDraft(host: PluginHostApi, dialog: Dialog, draft: InlineDraft, index: number) {
  const { updateInline } = dialog;
  return host.jsx(
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
        onClick: () => dialog.setInline((current: InlineDraft[]) => current.filter((_, i) => i !== index)),
      },
      "Remove",
    ),
  );
}
