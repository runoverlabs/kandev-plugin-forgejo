// ui/src/review-model.ts
function record(value) {
  return value !== null && typeof value === "object" ? value : {};
}
function text(value) {
  return typeof value === "string" ? value.trim() : "";
}
function finiteNumber(value) {
  return typeof value === "number" && Number.isFinite(value) ? value : void 0;
}
function nonNegativeInteger(value) {
  const number = finiteNumber(value);
  return number !== void 0 && Number.isInteger(number) && number >= 0 ? number : void 0;
}
function positiveInteger(value) {
  const number = nonNegativeInteger(value);
  return number !== void 0 && number > 0 ? number : void 0;
}
function normalizeTaskStatus(value) {
  const source = record(value);
  const number = positiveInteger(source.number);
  const state = text(source.state);
  const pipelineState = text(source.pipeline_state);
  if (number === void 0 || !["open", "merged", "closed", "draft"].includes(
    state
  ) || !["success", "failure", "pending", "neutral"].includes(
    pipelineState
  )) {
    return void 0;
  }
  const checks = Array.isArray(source.checks) ? source.checks.flatMap((value2) => {
    const check = record(value2);
    const id = text(check.id);
    const label = text(check.label);
    const checkState = text(check.state);
    if (!id || !label || !["success", "failure", "pending", "neutral"].includes(
      checkState
    )) {
      return [];
    }
    const detail = text(check.detail);
    const url = text(check.url);
    return [{
      id,
      label,
      state: checkState,
      ...detail ? { detail } : {},
      ...url ? { url } : {}
    }];
  }) : [];
  const reviewSource = record(source.review);
  const reviewState2 = text(reviewSource.state);
  const approved = nonNegativeInteger(reviewSource.approved);
  const required = nonNegativeInteger(reviewSource.required);
  const requested = nonNegativeInteger(reviewSource.requested);
  const review = approved !== void 0 && ["approved", "changes_requested", "pending"].includes(
    reviewState2
  ) ? {
    state: reviewState2,
    approved,
    ...required === void 0 ? {} : { required },
    ...requested === void 0 ? {} : { requested }
  } : void 0;
  const unresolvedComments = nonNegativeInteger(source.unresolved_comments);
  const updatedAt = finiteNumber(source.updated_at);
  return {
    number,
    state,
    pipelineState,
    checks,
    ...review ? { review } : {},
    ...unresolvedComments === void 0 ? {} : { unresolvedComments },
    ...updatedAt === void 0 ? {} : { updatedAt }
  };
}
function normalizeReview(providerId, value) {
  const source = record(value);
  const reviewKey = text(source.review_key);
  const title = text(source.title);
  const url = text(source.url);
  const connectionScope = text(source.connection_scope);
  const repositoryId = text(source.repository_id);
  const changeRequestNumber = positiveInteger(source.change_request_number);
  if (!reviewKey || !title || !url || !connectionScope || !repositoryId || changeRequestNumber === void 0) {
    return null;
  }
  const state = text(source.state);
  const taskStatus = normalizeTaskStatus(source.task_status);
  return {
    providerId,
    reviewKey,
    title,
    url,
    connectionScope,
    repositoryId,
    changeRequestNumber,
    state,
    ...taskStatus ? { taskStatus } : {}
  };
}
function normalizeAssociation(providerId, value) {
  const source = record(value);
  const taskId = text(source.task_id);
  const reviewKey = text(source.review_key);
  const connectionScope = text(source.connection_scope);
  const repositoryId = text(source.repository_id);
  const changeRequestNumber = positiveInteger(source.change_request_number);
  if (!taskId || !reviewKey || !connectionScope || !repositoryId || changeRequestNumber === void 0) {
    return null;
  }
  return {
    providerId,
    taskId,
    reviewKey,
    connectionScope,
    repositoryId,
    changeRequestNumber
  };
}

// ui/src/repository-provider.ts
function normalizeRepository(providerId, value) {
  const source = record(value);
  const providerHost = text(source.provider_host);
  const ownerOrProject = text(source.owner_or_project);
  const repositoryId = text(source.repository_id);
  const repositoryName = text(source.name);
  const cloneUrl = text(source.clone_url);
  if (!providerHost || !ownerOrProject || !repositoryId || !repositoryName || !cloneUrl) return null;
  const providerScope = text(source.provider_scope);
  const defaultBranch = text(source.default_branch);
  return {
    providerId,
    providerHost,
    ...providerScope ? { providerScope } : {},
    ownerOrProject,
    repositoryId,
    repositoryName,
    cloneUrl,
    ...defaultBranch ? { defaultBranch } : {}
  };
}
function credentialFreeRepository(repository) {
  return {
    provider_id: repository.providerId,
    provider_host: repository.providerHost,
    provider_scope: repository.providerScope ?? "",
    provider_repository_id: repository.repositoryId,
    owner_or_project: repository.ownerOrProject,
    name: repository.repositoryName,
    clone_url: repository.cloneUrl,
    default_branch: repository.defaultBranch ?? ""
  };
}
function createRepositoryProvider(host, options, refreshAfterMutation) {
  return {
    id: options.providerId,
    label: options.label,
    ...options.icon ? { icon: options.icon } : {},
    ...options.matchesURL ? { matchesURL: options.matchesURL } : {},
    ...options.supportsDraft === void 0 ? {} : { supportsDraft: options.supportsDraft },
    async listRepositories({ workspaceId, query = "", cursor = "", limit = 100, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction(
        "repositories.list",
        { workspaceId, body: { query, cursor, limit } },
        { signal }
      );
      signal.throwIfAborted();
      const nextCursor = text(response.next_cursor);
      return {
        repositories: (response.repositories ?? []).flatMap((repository) => {
          const normalized = normalizeRepository(options.providerId, repository);
          return normalized ? [normalized] : [];
        }),
        // Omit the key entirely rather than setting it to undefined: the host
        // types it as an optional string under exactOptionalPropertyTypes.
        ...nextCursor ? { nextCursor } : {}
      };
    },
    async listBranches({ workspaceId, repository, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction(
        "repositories.branches",
        { workspaceId, body: { repository: credentialFreeRepository(repository) } },
        { signal }
      );
      signal.throwIfAborted();
      return (response.branches ?? []).flatMap((branch) => {
        const name = text(record(branch).name);
        return name ? [{ name }] : [];
      });
    },
    async inspectURL({ workspaceId, url, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction(
        "repositories.inspect",
        { workspaceId, body: { url } },
        { signal }
      );
      signal.throwIfAborted();
      return normalizeRepository(options.providerId, response.repository);
    },
    async createChangeRequest({
      workspaceId,
      taskId,
      sessionId,
      repositoryId,
      title,
      body,
      baseBranch,
      draft,
      signal
    }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction(
        "change_requests.create",
        {
          workspaceId,
          taskId,
          sessionId,
          repositoryId,
          body: {
            title,
            description: body,
            destination: baseBranch ?? "",
            draft
          }
        },
        { signal }
      );
      const url = text(response.url);
      if (!url) throw new Error("source-control recipe: create response did not include a URL");
      await refreshAfterMutation(workspaceId, taskId, signal);
      const output = text(response.output);
      const associationError = text(response.association_error);
      return {
        url,
        provider: options.providerId,
        ...output ? { output } : {},
        ...typeof response.linked === "boolean" ? { linked: response.linked } : {},
        ...associationError ? { associationError } : {}
      };
    }
  };
}

// ui/src/automation-model.ts
var SWITCHES = [
  { key: "auto_fix", label: "Auto-fix: prompt the agent when CI fails, the branch conflicts, or reviewers ask for changes" },
  { key: "auto_merge", label: "Auto-merge when CI passes, the branch merges cleanly and reviews allow it" },
  { key: "on_review_requested", label: "Tell the agent when your review is requested" },
  { key: "on_merged", label: "Tell the agent when the pull request is merged" },
  { key: "on_closed", label: "Tell the agent when the pull request is closed without merging" }
];
var TRUST_LEVELS = [
  {
    value: "everyone",
    label: "Everyone",
    hint: "Every review and comment reaches the agent, as with Kandev's GitHub integration. Anyone who can comment on the pull request puts text in front of the agent."
  },
  {
    value: "official",
    label: "Official reviewers",
    hint: "Only reviews that count towards approvals (reviewers with write access), and comments from people who left one."
  },
  {
    value: "none",
    label: "No review text",
    hint: "Auto-fix acts on CI failures and conflicts only."
  }
];
var FORK_WARNING = "This pull request comes from a fork. Its code and comments are controlled by the fork's author, and the agent will act on them with your executor's environment. Turn this on only if you trust that author.";
function outcomeLine(view) {
  if (!view.last_outcome) return null;
  const outcome = view.last_outcome.replace(/_/g, " ");
  return view.last_summary ? `Last outcome: ${outcome} \u2014 ${view.last_summary}` : `Last outcome: ${outcome}`;
}
function statusLines(view) {
  const lines = [];
  if (!view.integration_enabled) {
    lines.push("Paused: the Forgejo integration is off for this workspace.");
  }
  if (!view.seeded) {
    lines.push("Automation has not looked at this pull request yet: the switches show the workspace defaults it will take.");
  }
  if (view.status) lines.push(view.status);
  if (view.auto_fix) {
    lines.push(`Auto-fix rounds: ${view.rounds} of ${view.max_rounds}.`);
  }
  const outcome = outcomeLine(view);
  if (outcome) lines.push(outcome);
  if (view.ignored > 0) {
    lines.push(
      `${view.ignored} review ${view.ignored === 1 ? "comment was" : "comments were"} left out by the workspace's feedback setting (${view.trust}).`
    );
  }
  return lines;
}
function needsForkConfirmation(view, key, next) {
  return next && Boolean(view?.fork) && (key === "auto_fix" || key === "auto_merge");
}
function trustLevel(settings) {
  return TRUST_LEVELS.find((level) => level.value === settings.trust) ?? TRUST_LEVELS[0];
}
function templateDraft(settings, edited) {
  return edited ?? (settings.template || settings.default_template);
}

// ui/src/panel-model.ts
function person(name) {
  return { name: (name ?? "").trim() || "unknown" };
}
function reviewState(state) {
  const upper = (state ?? "").toUpperCase();
  return upper === "COMMENT" ? "COMMENTED" : upper || "COMMENTED";
}
function toDetailModel(details) {
  const draft = details.state === "draft";
  const model = {
    providerId: details.provider_id ?? "forgejo",
    reviewKey: details.review_key ?? "",
    number: details.number ?? 0,
    title: details.title ?? "",
    url: details.url ?? "",
    state: draft ? "open" : details.state ?? "open",
    author: person(details.author),
    ...branchesAndLines(details),
    reviews: detailReviews(details),
    requestedReviewers: (details.requested_reviewers ?? []).map((name) => person(name)),
    checks: detailChecks(details),
    comments: detailComments(details)
  };
  addReportedFields(model, details);
  return model;
}
function addReportedFields(model, details) {
  if (details.state === "draft") model.draft = true;
  if (details.description) model.description = details.description;
  if (details.created_at) model.createdAt = details.created_at;
  const decision = reviewDecision(details);
  if (decision) model.reviewState = decision;
}
function branchesAndLines(details) {
  return {
    sourceBranch: details.source_branch ?? "",
    targetBranch: details.target_branch ?? "",
    additions: details.additions ?? 0,
    deletions: details.deletions ?? 0
  };
}
function detailReviews(details) {
  return (details.reviews ?? []).map((review, index) => ({
    id: String(review.id ?? index),
    author: person(review.author),
    state: reviewState(review.state),
    ...review.body ? { body: review.body } : {},
    ...review.submitted_at ? { createdAt: review.submitted_at } : {}
  }));
}
function detailChecks(details) {
  return (details.checks ?? []).map((check, index) => ({
    id: check.id ?? String(index),
    name: check.label ?? "check",
    state: check.state ?? "neutral",
    ...check.detail ? { output: check.detail } : {},
    ...check.url ? { url: check.url } : {}
  }));
}
function detailComments(details) {
  return (details.comments ?? []).map((comment, index) => ({
    id: String(comment.id ?? index),
    author: person(comment.author),
    body: comment.body ?? "",
    ...comment.created_at ? { createdAt: comment.created_at } : {}
  }));
}
function reviewDecision(details) {
  const blockers = details.merge?.blockers ?? [];
  if (blockers.includes("changes_requested")) return "CHANGES_REQUESTED";
  if ((details.merge?.approvals ?? 0) > 0) return "APPROVED";
  return void 0;
}
var BLOCKER_TEXT = {
  conflicts: "This branch has conflicts that must be resolved.",
  checks_failing: "Required checks are failing.",
  checks_pending: "Required checks are still running.",
  approvals_required: "More approvals are required.",
  changes_requested: "Changes were requested.",
  not_open: ""
};
function mergeNotice(details) {
  if (details.state === "draft") return "This pull request is a draft (its title starts with WIP:).";
  const reasons = (details.merge?.blockers ?? []).map((code) => BLOCKER_TEXT[code] ?? "").filter((line) => line !== "");
  if (reasons.length === 0) return null;
  const approvals = details.merge?.required_approvals;
  return reasons.map(
    (line) => line.startsWith("More approvals") && typeof approvals === "number" ? `${approvals} approval${approvals === 1 ? "" : "s"} required (${details.merge?.approvals ?? 0} so far).` : line
  ).join(" ");
}
var STYLE_LABELS = {
  merge: "Create a merge commit",
  squash: "Squash and merge",
  rebase: "Rebase and merge",
  "rebase-merge": "Rebase, then merge commit",
  "fast-forward-only": "Fast-forward only"
};
function styleLabel(style) {
  return STYLE_LABELS[style] ?? style;
}
function mergeChoice(details) {
  const styles = details.merge?.styles ?? [];
  if (styles.length === 0) return { primary: "", others: [], unreported: true };
  const preferred = details.merge?.default_style ?? "";
  const primary = styles.includes(preferred) ? preferred : styles[0];
  return { primary, others: styles.filter((style) => style !== primary), unreported: false };
}
function mergeOffered(details) {
  return details.state === "open" || details.state === "draft";
}
function mergeDisabled(details) {
  if (details.state === "draft") return true;
  if (details.merge?.can_merge === false) return true;
  return (details.merge?.blockers ?? []).length > 0;
}
function failureMessage(cause) {
  if (cause instanceof Error && cause.message.trim()) return cause.message.trim();
  if (typeof cause === "string" && cause.trim()) return cause.trim();
  return "The request failed.";
}
var EVENT_LABELS = {
  approve: "Approve",
  request_changes: "Request changes",
  comment: "Comment"
};
function reviewRequest(event, body, inline) {
  const comments = [];
  for (const draft of inline) {
    const line = Number.parseInt(draft.line, 10);
    if (!draft.path.trim() || !draft.body.trim() || !Number.isInteger(line) || line <= 0) {
      return { problem: "Each inline comment needs a file path, a line number and some text." };
    }
    comments.push({ path: draft.path.trim(), line, body: draft.body.trim() });
  }
  if (event !== "approve" && !body.trim() && comments.length === 0) {
    return { problem: "Write something, or choose Approve." };
  }
  return { request: { event, body: body.trim(), comments } };
}

// ui/src/workspace.ts
function useWorkspaceId(host, routedWorkspaceId) {
  const [activeWorkspaceId, setActiveWorkspaceId] = host.React.useState(
    () => host.context.getActiveWorkspaceId()
  );
  host.React.useEffect(() => host.context.subscribeActiveWorkspace(setActiveWorkspaceId), []);
  return text(routedWorkspaceId) || text(activeWorkspaceId);
}
function useWorkspaceState(host, workspaceId, initial) {
  const [byWorkspace, setByWorkspace] = host.React.useState(() => /* @__PURE__ */ new Map());
  const set = host.React.useCallback(
    (value) => setByWorkspace((previous) => new Map(previous).set(workspaceId, value)),
    [workspaceId]
  );
  return [byWorkspace.has(workspaceId) ? byWorkspace.get(workspaceId) : initial, set];
}

// ui/src/automation.ts
function createAutomationSection(host) {
  return function AutomationSection(props) {
    const section = useAutomationSection(host, props);
    const current = section.view;
    if (!current) {
      return section.error ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, section.error) : null;
    }
    return renderSection(host, section, current);
  };
}
function useAutomationSection(host, props) {
  const [view, setView] = host.React.useState(null);
  const [error, setError] = host.React.useState(null);
  const [busy, setBusy] = host.React.useState(false);
  const call = (key, body = {}) => host.api.invokeAction(key, {
    workspaceId: props.workspaceId,
    taskId: props.taskId,
    body: { number: props.number, ...body }
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
  const write = async (key, body) => {
    if (busy) return;
    setBusy(true);
    try {
      setView(await call(key, body));
      setError(null);
    } catch (cause) {
      host.toast.error(failureMessage(cause));
      void load();
    } finally {
      setBusy(false);
    }
  };
  const toggle = (key, next) => {
    if (needsForkConfirmation(view, key, next)) {
      confirmForkSwitch(host, () => void write("automation.set", { [key]: true, confirm_fork: true }));
      return;
    }
    void write("automation.set", { [key]: next });
  };
  return { view, error, busy, write, toggle };
}
function confirmForkSwitch(host, turnOn) {
  let modal = null;
  modal = host.openModal({
    title: "Pull request from a fork",
    description: FORK_WARNING,
    size: "sm",
    content: () => host.jsx(
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
          }
        },
        "Turn on"
      )
    )
  });
}
function renderSection(host, section, current) {
  const { busy, error } = section;
  return host.jsx(
    "div",
    { className: "forgejo-automation", "data-testid": "forgejo-automation" },
    host.jsx("h4", { className: "forgejo-automation__heading" }, "Automation"),
    ...SWITCHES.map(
      ({ key, label }) => host.jsx(
        "label",
        { key, className: "forgejo-automation__toggle" },
        host.jsx(host.ui.Switch, {
          checked: current[key],
          disabled: busy,
          "aria-label": label,
          "data-testid": `forgejo-automation-${key}`,
          onCheckedChange: (next) => section.toggle(key, next === true)
        }),
        host.jsx("span", null, label)
      )
    ),
    ...statusLines(current).map(
      (line, index) => host.jsx("p", { key: `line-${index}`, className: "forgejo-automation__status", role: "status" }, line)
    ),
    current.error || error ? host.jsx("p", { className: "forgejo-automation__error", role: "alert" }, current.error || error) : null,
    current.merge_failure && current.auto_merge ? renderRecoveryButton(host, section, "merge") : null,
    current.exhausted ? renderRecoveryButton(host, section, "rounds") : null
  );
}
var RECOVERY = {
  merge: { testId: "forgejo-automation-retry-merge", label: "Retry merge" },
  rounds: { testId: "forgejo-automation-reset-rounds", label: "Reset rounds" }
};
function renderRecoveryButton(host, section, what) {
  const { testId, label } = RECOVERY[what];
  return host.jsx(
    host.ui.Button,
    {
      type: "button",
      size: "sm",
      variant: "outline",
      disabled: section.busy,
      "data-testid": testId,
      onClick: () => void section.write("automation.retry", { what })
    },
    label
  );
}
function createAutomationSettings(host) {
  return function AutomationSettingsPanel(props = {}) {
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
function useAutomationSettings(host, routedWorkspaceId) {
  const workspaceId = useWorkspaceId(host, routedWorkspaceId);
  const [settings, setSettings] = useWorkspaceState(host, workspaceId, null);
  const [template, setTemplate] = useWorkspaceState(host, workspaceId, null);
  const [error, setError] = useWorkspaceState(host, workspaceId, null);
  const [busy, setBusy] = useWorkspaceState(host, workspaceId, false);
  host.React.useEffect(() => {
    if (!workspaceId) return;
    let cancelled = false;
    host.api.invokeAction("automation.settings", { workspaceId }).then((loaded) => {
      if (cancelled) return;
      setSettings(loaded);
      setError(null);
    }).catch((cause) => {
      if (!cancelled) setError(failureMessage(cause));
    });
    return () => {
      cancelled = true;
    };
  }, [workspaceId]);
  const save = async (patch, done) => {
    if (busy || !workspaceId) return false;
    setBusy(true);
    try {
      const saved = await host.api.invokeAction("automation.settings_set", {
        workspaceId,
        body: patch
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
    save
  };
}
function renderSettings(host, panel, current) {
  const { busy, save } = panel;
  return host.jsx(
    "div",
    { className: "forgejo-automation", "data-testid": "forgejo-automation-settings" },
    host.jsx(
      "p",
      { className: "forgejo-automation__status" },
      "Defaults for pull requests linked from now on. Each pull request's own switches are in its review panel. Auto-fix and auto-merge are never turned on by default for a pull request from a fork."
    ),
    ...SWITCHES.map(
      ({ key, label }) => host.jsx(
        "label",
        { key, className: "forgejo-automation__toggle" },
        host.jsx(host.ui.Switch, {
          checked: current.defaults[key],
          disabled: busy,
          "aria-label": `Default: ${label}`,
          "data-testid": `forgejo-automation-default-${key}`,
          onCheckedChange: (next) => void save({ defaults: { ...current.defaults, [key]: next === true } })
        }),
        host.jsx("span", null, label)
      )
    ),
    ...renderTrustPicker(host, panel, current),
    ...renderTemplateEditor(host, panel, current)
  );
}
function renderTrustPicker(host, panel, current) {
  return [
    host.jsx(host.ui.Label, null, "Review feedback the agent sees"),
    host.jsx(
      host.ui.Select,
      { value: current.trust, onValueChange: (next) => void panel.save({ trust: next }) },
      host.jsx(host.ui.SelectTrigger, { "data-testid": "forgejo-automation-trust" }, host.jsx(host.ui.SelectValue, null)),
      host.jsx(
        host.ui.SelectContent,
        null,
        ...TRUST_LEVELS.map((level) => host.jsx(host.ui.SelectItem, { key: level.value, value: level.value }, level.label))
      )
    ),
    host.jsx("p", { className: "forgejo-automation__status" }, trustLevel(current).hint)
  ];
}
function renderTemplateEditor(host, panel, current) {
  const { busy, save, template, setTemplate } = panel;
  const draft = templateDraft(current, template);
  return [
    host.jsx(host.ui.Label, null, "Auto-fix prompt"),
    host.jsx(host.ui.Textarea, {
      value: draft,
      rows: 10,
      disabled: busy,
      "aria-label": "Auto-fix prompt",
      onChange: (event) => setTemplate(event.target.value)
    }),
    host.jsx(
      "p",
      { className: "forgejo-automation__status" },
      "Placeholders: {{pr.feedback}}, {{pr.url}}, {{pr.number}}, {{pr.title}}, {{pr.repository}}, {{pr.head_branch}}, {{pr.base_branch}}, {{attempt.round}}. The outcome instructions are always appended."
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
          onClick: () => void save({ template: draft === current.default_template ? "" : draft }, "Auto-fix prompt saved").then((saved) => {
            if (saved) setTemplate(null);
          })
        },
        "Save prompt"
      ),
      host.jsx(
        host.ui.Button,
        {
          type: "button",
          size: "sm",
          variant: "outline",
          disabled: busy || !current.template,
          onClick: () => void save({ template: "" }, "Auto-fix prompt reset").then((saved) => {
            if (saved) setTemplate(null);
          })
        },
        "Reset to default"
      )
    )
  ];
}

// ui/src/review-dialog.ts
function createReviewDialog(host) {
  return function ReviewDialog(props) {
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
        onChange: (changed) => dialog.setBody(changed.target.value)
      }),
      dialog.inline.map((draft, index) => renderInlineDraft(host, dialog, draft, index)),
      dialog.inline.length < 20 ? host.jsx(
        host.ui.Button,
        {
          type: "button",
          size: "sm",
          variant: "secondary",
          disabled: busy,
          onClick: () => dialog.setInline((current) => [...current, { path: "", line: "", body: "" }])
        },
        "Add inline comment"
      ) : null,
      problem ? host.jsx("p", { role: "alert", className: "forgejo-review-dialog__error" }, problem) : null,
      host.jsx(
        "div",
        { className: "forgejo-review-dialog__footer" },
        host.jsx(host.ui.Button, { type: "button", variant: "ghost", disabled: busy, onClick: props.onClose }, "Cancel"),
        host.jsx(
          host.ui.Button,
          { type: "button", disabled: busy, onClick: () => void dialog.submit() },
          busy ? "Submitting\u2026" : "Submit review"
        )
      )
    );
  };
}
function useReviewDialog(host, props) {
  const [event, setEvent] = host.React.useState("comment");
  const [body, setBody] = host.React.useState("");
  const [inline, setInline] = host.React.useState([]);
  const [busy, setBusy] = host.React.useState(false);
  const [problem, setProblem] = host.React.useState("");
  const submit = async () => {
    const checked = reviewRequest(event, body, inline);
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
  const updateInline = (index, patch) => setInline(
    (current) => current.map((draft, i) => i === index ? { ...draft, ...patch } : draft)
  );
  return {
    event,
    setEvent,
    body,
    setBody,
    inline,
    setInline,
    busy,
    problem,
    submit,
    updateInline
  };
}
function renderEventButtons(host, dialog) {
  return host.jsx(
    "div",
    { className: "forgejo-review-dialog__events", role: "group", "aria-label": "Review type" },
    Object.keys(EVENT_LABELS).map(
      (key) => host.jsx(
        host.ui.Button,
        {
          key,
          type: "button",
          size: "sm",
          variant: dialog.event === key ? "default" : "outline",
          "aria-pressed": dialog.event === key,
          disabled: dialog.busy,
          onClick: () => dialog.setEvent(key)
        },
        EVENT_LABELS[key]
      )
    )
  );
}
function renderInlineDraft(host, dialog, draft, index) {
  const { updateInline } = dialog;
  return host.jsx(
    "div",
    { key: index, className: "forgejo-review-dialog__inline" },
    host.jsx(host.ui.Input, {
      value: draft.path,
      placeholder: "path/to/file",
      "aria-label": "File path",
      onChange: (changed) => updateInline(index, { path: changed.target.value })
    }),
    host.jsx(host.ui.Input, {
      value: draft.line,
      placeholder: "line",
      inputMode: "numeric",
      "aria-label": "Line number",
      onChange: (changed) => updateInline(index, { line: changed.target.value })
    }),
    host.jsx(host.ui.Textarea, {
      value: draft.body,
      placeholder: "Comment on this line",
      "aria-label": "Inline comment",
      rows: 2,
      onChange: (changed) => updateInline(index, { body: changed.target.value })
    }),
    host.jsx(
      host.ui.Button,
      {
        type: "button",
        size: "sm",
        variant: "ghost",
        onClick: () => dialog.setInline((current) => current.filter((_, i) => i !== index))
      },
      "Remove"
    )
  );
}

// ui/src/review-panel.ts
function createReviewPanel(host, options) {
  const ReviewDialog = createReviewDialog(host);
  const AutomationSection = createAutomationSection(host);
  return function ReviewPanel(props) {
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
      onAction: async (request) => {
        if (request.actionId !== "comment") return;
        const text2 = (request.body ?? "").trim();
        if (!text2) return;
        await writes.run("comment", () => writes.act("change_requests.comment", { body: text2 }), "Comment posted");
      },
      headerActions: renderHeaderActions(host, state, writes),
      notice: renderNotice(host, AutomationSection, state)
    });
  };
}
function useReviewPanelState(host, props) {
  const [details, setDetails] = host.React.useState(null);
  const [loading, setLoading] = host.React.useState(true);
  const [error, setError] = host.React.useState(null);
  const [busy, setBusy] = host.React.useState(null);
  const [deleteBranch, setDeleteBranch] = host.React.useState(null);
  const [refreshKey, setRefreshKey] = host.React.useState(0);
  const number = Number(props.changeRequestNumber);
  const load = host.React.useCallback(
    async (signal) => {
      setLoading(true);
      setRefreshKey((key) => key + 1);
      try {
        const response = await host.api.invokeAction(
          "change_requests.details",
          { workspaceId: props.workspaceId, taskId: props.taskId, body: { number } },
          signal ? { signal } : void 0
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
    [props.workspaceId, props.taskId, number]
  );
  host.React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);
  return {
    props,
    number,
    details,
    loading,
    error,
    busy,
    setBusy,
    deleteBranch,
    setDeleteBranch,
    refreshKey,
    load
  };
}
function wantsDeleteBranch(state) {
  return state.deleteBranch ?? state.details?.merge?.delete_branch_default ?? false;
}
function panelWrites(host, options, ReviewDialog, state) {
  const { props, number, busy, setBusy, load } = state;
  const current = state.details;
  const run = async (id, write, success) => {
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
  const act = (key, body) => host.api.invokeAction(key, { workspaceId: props.workspaceId, taskId: props.taskId, body: { number, ...body } });
  const merge = (style) => {
    if (!current) return;
    void run(
      "merge",
      () => act("change_requests.merge", {
        head_sha: current.head_sha ?? "",
        ...style ? { style } : {},
        delete_branch: wantsDeleteBranch(state)
      }),
      "Pull request merged"
    );
  };
  const openReview = () => {
    let modal = null;
    modal = host.openModal({
      title: "Submit review",
      ...current?.actor ? { description: `Posted as ${current.actor}, the account Kandev uses for Forgejo.` } : {},
      size: "md",
      content: () => host.jsx(ReviewDialog, {
        onClose: () => modal?.close(),
        onSubmit: async (request) => {
          await act("change_requests.review", {
            event: request.event,
            body: request.body,
            comments: request.comments,
            head_sha: current?.head_sha ?? ""
          });
          host.toast.success("Review submitted");
          await load();
          void options.afterMutation(props.workspaceId, props.taskId);
        }
      })
    });
  };
  return { run, act, merge, openReview };
}
function renderHeaderActions(host, state, writes) {
  const current = state.details;
  if (!current || !mergeOffered(current)) return void 0;
  const controls = {
    current,
    busy: state.busy,
    choice: mergeChoice(current),
    wantsDelete: wantsDeleteBranch(state),
    setDeleteBranch: state.setDeleteBranch,
    writes
  };
  const { busy } = controls;
  return host.jsx(
    "div",
    { className: "forgejo-merge" },
    host.jsx(
      host.ui.Button,
      { type: "button", size: "sm", variant: "outline", disabled: busy !== null, onClick: writes.openReview },
      "Review"
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
        onCheckedChange: (next) => controls.setDeleteBranch(next === true)
      }),
      host.jsx("span", null, "Delete branch")
    ),
    current.actor ? host.jsx("span", { className: "forgejo-merge__actor" }, `as ${current.actor}`) : null
  );
}
function renderMergeButton(host, controls) {
  const { current, busy, choice, writes } = controls;
  return host.jsx(
    host.ui.Button,
    {
      type: "button",
      size: "sm",
      "data-testid": "forgejo-merge-button",
      disabled: busy !== null || mergeDisabled(current),
      onClick: () => writes.merge(choice.primary)
    },
    busy === "merge" ? "Merging\u2026" : !choice.unreported ? styleLabel(choice.primary) : "Merge"
  );
}
function renderMergeMenu(host, controls) {
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
          disabled: busy !== null || mergeDisabled(current)
        },
        "\u25BE"
      )
    ),
    host.jsx(
      host.ui.DropdownMenuContent,
      { align: "end" },
      choice.others.map(
        (style) => host.jsx(host.ui.DropdownMenuItem, { key: style, onSelect: () => writes.merge(style) }, styleLabel(style))
      )
    )
  );
}
function renderNotice(host, AutomationSection, state) {
  const current = state.details;
  const noticeText = current ? mergeNotice(current) : null;
  const mergeNoticeNode = noticeText ? host.jsx("p", { className: "forgejo-merge-notice", role: "status", "data-testid": "forgejo-merge-notice" }, noticeText) : null;
  const automation = current && Number.isFinite(state.number) ? host.jsx(AutomationSection, {
    workspaceId: state.props.workspaceId,
    taskId: state.props.taskId,
    number: state.number,
    refreshKey: state.refreshKey
  }) : null;
  return mergeNoticeNode || automation ? host.jsx("div", { className: "forgejo-panel-notice" }, mergeNoticeNode, automation) : void 0;
}
function commentActions(current, busy) {
  return current ? [
    {
      id: "comment",
      label: "Comment",
      pendingLabel: "Posting\u2026",
      placement: "comment",
      input: "text",
      busy: busy === "comment",
      disabled: busy !== null && busy !== "comment"
    }
  ] : void 0;
}

// ui/src/review-store.ts
function createSnapshotStore() {
  const snapshots = /* @__PURE__ */ new Map();
  const listeners = /* @__PURE__ */ new Map();
  const versions = /* @__PURE__ */ new Map();
  let epoch = 0;
  return {
    get: (key) => snapshots.get(key) ?? [],
    subscribe(key, listener) {
      const keyListeners = listeners.get(key) ?? /* @__PURE__ */ new Set();
      keyListeners.add(listener);
      listeners.set(key, keyListeners);
      return () => {
        keyListeners.delete(listener);
        if (keyListeners.size === 0) listeners.delete(key);
      };
    },
    beginRefresh(key) {
      const version = (versions.get(key) ?? 0) + 1;
      versions.set(key, version);
      return { epoch, version };
    },
    commit(key, token, values) {
      if (token.epoch !== epoch || versions.get(key) !== token.version) return false;
      snapshots.set(key, [...values]);
      listeners.get(key)?.forEach((listener) => listener());
      return true;
    },
    clear() {
      epoch += 1;
      versions.clear();
      snapshots.clear();
      listeners.forEach((keyListeners) => keyListeners.forEach((listener) => listener()));
      listeners.clear();
    }
  };
}

// ui/src/source-control.ts
function registerSourceControlRecipe(registry, host, options) {
  const recipe = {
    host,
    options,
    reviewStore: createSnapshotStore(),
    associationStore: createSnapshotStore(),
    overlays: /* @__PURE__ */ new Set()
  };
  const refreshers = createRefreshers(recipe);
  registry.registerRepositoryProvider(createRepositoryProvider(host, options, refreshers.refreshAfterMutation));
  registerLinkAction(registry, recipe, refreshers);
  registry.registerReviewProvider(reviewProvider(recipe, refreshers));
  return {
    destroy() {
      recipe.overlays.forEach((overlay) => overlay.close());
      recipe.overlays.clear();
      recipe.reviewStore.clear();
      recipe.associationStore.clear();
    }
  };
}
function createRefreshers(recipe) {
  const { host, options, reviewStore, associationStore } = recipe;
  async function refreshReviews(taskId, signal, workspaceId) {
    const token = reviewStore.beginRefresh(taskId);
    signal.throwIfAborted();
    const response = await host.api.invokeAction(
      "change_requests.get",
      { ...workspaceId ? { workspaceId } : {}, taskId },
      { signal }
    );
    signal.throwIfAborted();
    reviewStore.commit(
      taskId,
      token,
      (response.reviews ?? []).flatMap((review) => {
        const normalized = normalizeReview(options.providerId, review);
        return normalized ? [normalized] : [];
      })
    );
  }
  async function refreshAssociations(workspaceId, signal) {
    const token = associationStore.beginRefresh(workspaceId);
    signal.throwIfAborted();
    const response = await host.api.invokeAction(
      "change_requests.associations",
      { workspaceId },
      { signal }
    );
    signal.throwIfAborted();
    associationStore.commit(
      workspaceId,
      token,
      (response.associations ?? []).flatMap((association) => {
        const normalized = normalizeAssociation(options.providerId, association);
        return normalized ? [normalized] : [];
      })
    );
  }
  async function refreshAfterMutation(workspaceId, taskId, signal) {
    try {
      await Promise.all([
        refreshReviews(taskId, signal, workspaceId),
        refreshAssociations(workspaceId, signal)
      ]);
    } catch {
    }
  }
  return { refreshReviews, refreshAssociations, refreshAfterMutation };
}
function registerLinkAction(registry, recipe, refreshers) {
  const { host, options, overlays } = recipe;
  registry.registerTaskAction({
    id: `${options.providerId}-link-change-request`,
    label: `${options.label} ${options.changeRequestNoun}`,
    ...options.icon ? { icon: options.icon } : {},
    placement: "link",
    singleTaskOnly: true,
    async run(context) {
      const dialog = host.openTaskLinkDialog({
        title: `Link ${options.label} ${options.changeRequestNoun}`,
        description: `Enter a ${options.label} ${options.changeRequestNoun} URL or canonical reference.`,
        inputLabel: options.changeRequestNoun,
        emptyError: `Enter a valid ${options.label} ${options.changeRequestNoun} reference.`,
        failureMessage: `Failed to link ${options.label} ${options.changeRequestNoun}.`,
        successMessage: `${options.label} ${options.changeRequestNoun} linked`,
        inputTestId: `${options.providerId}-review-reference`,
        errorTestId: `${options.providerId}-review-reference-error`,
        submitTestId: `${options.providerId}-review-reference-submit`,
        async onSubmit(reference, signal) {
          signal.throwIfAborted();
          const parsed = options.parseReference(reference);
          if (!parsed) {
            throw new Error(`Enter a valid ${options.label} ${options.changeRequestNoun} reference.`);
          }
          await host.api.invokeAction(
            "change_requests.link",
            {
              workspaceId: context.workspaceId,
              taskId: context.taskId,
              body: { reference: parsed }
            },
            { signal }
          );
          await refreshers.refreshAfterMutation(context.workspaceId, context.taskId, signal);
        }
      });
      overlays.add(dialog);
    }
  });
}
function snapshotFallback(recipe, props) {
  const review = recipe.reviewStore.get(props.taskId).find(
    (candidate) => candidate.reviewKey === props.reviewKey && candidate.connectionScope === props.connectionScope && candidate.repositoryId === props.repositoryId && String(candidate.changeRequestNumber) === String(props.changeRequestNumber)
  );
  return review ? recipe.options.toChangeRequestDetail(review) : null;
}
function reviewProvider(recipe, refreshers) {
  const { host, options, reviewStore, associationStore } = recipe;
  const reviewPanel = createReviewPanel(host, {
    fallback: (props) => snapshotFallback(recipe, props),
    afterMutation: (workspaceId, taskId) => refreshers.refreshAfterMutation(workspaceId, taskId, new AbortController().signal)
  });
  return {
    id: options.providerId,
    label: options.label,
    ...options.icon ? { icon: options.icon } : {},
    changeRequestNoun: options.changeRequestNoun,
    order: options.order ?? 100,
    getSnapshot: (taskId) => reviewStore.get(taskId),
    subscribe: (taskId, listener) => reviewStore.subscribe(taskId, listener),
    refresh: (taskId, signal) => refreshers.refreshReviews(taskId, signal),
    getAssociationSnapshot: (workspaceId) => associationStore.get(workspaceId),
    subscribeAssociations: (workspaceId, listener) => associationStore.subscribe(workspaceId, listener),
    refreshAssociations: refreshers.refreshAssociations,
    async unlink({
      workspaceId,
      taskId,
      connectionScope,
      repositoryId,
      changeRequestNumber,
      signal
    }) {
      const number = typeof changeRequestNumber === "string" && /^\d+$/.test(changeRequestNumber) ? positiveInteger(Number(changeRequestNumber)) : positiveInteger(changeRequestNumber);
      if (!connectionScope.trim() || !repositoryId.trim() || number === void 0) {
        throw new Error("source-control recipe: cannot unlink an incomplete review identity");
      }
      signal.throwIfAborted();
      await host.api.invokeAction(
        "change_requests.unlink",
        {
          workspaceId,
          taskId,
          body: {
            connection_scope: connectionScope,
            repository_id: repositoryId,
            number
          }
        },
        { signal }
      );
    },
    // The panel is stateful, so its component type is created once here. A new
    // type per render would remount it every time and lose the loaded details.
    ReviewPanel: (props) => host.jsx(reviewPanel, props)
  };
}

// ui/src/forgejo-icon.ts
function createForgejoIcon(host) {
  return function ForgejoIcon({ className } = {}) {
    return host.jsx(
      "svg",
      {
        className,
        viewBox: "0 0 24 24",
        width: "1em",
        height: "1em",
        fill: "none",
        stroke: "currentColor",
        strokeWidth: 2,
        strokeLinecap: "round",
        strokeLinejoin: "round",
        "aria-hidden": "true",
        focusable: "false"
      },
      host.jsx("circle", { cx: 6, cy: 5, r: 2.5 }),
      host.jsx("circle", { cx: 18, cy: 5, r: 2.5 }),
      host.jsx("circle", { cx: 6, cy: 19, r: 2.5 }),
      host.jsx("path", { d: "M6 7.5v9" }),
      host.jsx("path", { d: "M18 7.5v2a4 4 0 0 1-4 4h-4" })
    );
  };
}

// ui/src/connection-model.ts
function connectionView(workspaceId, status) {
  const configured = status?.configured === true;
  const connected = status?.connected === true;
  return {
    configured,
    connected,
    enabled: status?.enabled !== false,
    // Off unless the backend says it is on: the opposite default to `enabled`.
    agentMerge: status?.agent_merge === true,
    detail: connectionDetail(workspaceId, status, configured, connected)
  };
}
function connectionDetail(workspaceId, status, configured, connected) {
  if (!workspaceId) return "Open a workspace to check the Forgejo connection.";
  if (!connected) return text(status?.message) || (configured ? "Not verified yet." : "Not configured.");
  return connectedLine(status);
}
function connectedLine(status) {
  const version = text(status?.instance_version);
  const flavor = text(status?.flavor);
  return `Connected to ${text(status?.instance_url)} as ${text(status?.account)}` + (version ? ` (${flavor || "instance"} ${version})` : "");
}

// ui/src/connection-panel.ts
function operatorMessage(cause) {
  if (typeof console !== "undefined") {
    console.error("[kandev-plugin-forgejo] connection status request failed", cause);
  }
  return "Couldn't reach the Forgejo plugin. Check the Kandev server logs for details.";
}
function createConnectionPanel(host) {
  return function ForgejoConnectionPanel(props = {}) {
    const connection = useConnection(host, props.workspaceId);
    return renderConnectionPanel(host, connection, connectionView(connection.workspaceId, connection.status));
  };
}
function useConnection(host, routedWorkspaceId) {
  const [status, setStatus] = host.React.useState(null);
  const [checking, setChecking] = host.React.useState(false);
  const [error, setError] = host.React.useState(null);
  const workspaceId = useWorkspaceId(host, routedWorkspaceId);
  const publishEnabled = host.React.useCallback(
    (scopeId, value) => {
      host.setIntegrationEnabled?.("forgejo", scopeId, value);
    },
    []
  );
  const scope = { host, workspaceId, setStatus, setError, publishEnabled };
  const setEnabled = useEnabledSwitch(scope);
  const setAgentMerge = useAgentMergeSwitch(scope);
  const load = useStatusLoad(scope, setChecking);
  host.React.useEffect(() => {
    const controller = new AbortController();
    void load(false, controller.signal);
    return () => controller.abort();
  }, [load]);
  return { workspaceId, status, checking, error, setEnabled, setAgentMerge, load };
}
function useEnabledSwitch(scope) {
  const { host, workspaceId, setStatus, setError, publishEnabled } = scope;
  return host.React.useCallback(
    async (next) => {
      if (!workspaceId) return;
      setStatus((current) => ({ ...current ?? {}, enabled: next }));
      publishEnabled(workspaceId, next);
      const controller = new AbortController();
      try {
        await host.api.invokeAction(
          "connection.set_enabled",
          { workspaceId, body: { enabled: next } },
          { signal: controller.signal }
        );
      } catch (cause) {
        setStatus((current) => ({ ...current ?? {}, enabled: !next }));
        publishEnabled(workspaceId, !next);
        setError(operatorMessage(cause));
      }
    },
    [workspaceId, publishEnabled]
  );
}
function useAgentMergeSwitch(scope) {
  const { host, workspaceId, setStatus, setError } = scope;
  return host.React.useCallback(
    async (next) => {
      if (!workspaceId) return;
      setStatus((current) => ({ ...current ?? {}, agent_merge: next }));
      const controller = new AbortController();
      try {
        await host.api.invokeAction(
          "connection.set_agent_merge",
          { workspaceId, body: { enabled: next } },
          { signal: controller.signal }
        );
      } catch (cause) {
        setStatus((current) => ({ ...current ?? {}, agent_merge: !next }));
        setError(operatorMessage(cause));
      }
    },
    [workspaceId]
  );
}
function useStatusLoad(scope, setChecking) {
  const { host, workspaceId, setStatus, setError, publishEnabled } = scope;
  return host.React.useCallback(
    async (probe, signal) => {
      if (!workspaceId) {
        setStatus(null);
        setError(null);
        setChecking(false);
        return;
      }
      setChecking(true);
      setError(null);
      try {
        const response = await host.api.invokeAction(
          probe ? "connection.test" : "connection.get",
          { workspaceId },
          { signal }
        );
        if (signal.aborted) return;
        setStatus(response);
        publishEnabled(workspaceId, response?.enabled !== false);
      } catch (cause) {
        if (!signal.aborted) setError(operatorMessage(cause));
      } finally {
        if (!signal.aborted) setChecking(false);
      }
    },
    [workspaceId]
  );
}
function renderConnectionPanel(host, connection, view) {
  const { workspaceId, error, checking } = connection;
  return host.jsx(
    "div",
    { className: "forgejo-connection" },
    host.jsx(
      "p",
      {
        className: "forgejo-connection__state",
        "data-state": view.connected ? "connected" : "disconnected"
      },
      view.detail
    ),
    // The connection is plugin-wide: one instance URL and token serve every
    // workspace. Say so here rather than implying a per-workspace setting.
    host.jsx(
      "p",
      { className: "forgejo-connection__scope" },
      "This connection is shared by every workspace. Edit it at Settings > Plugins > Forgejo."
    ),
    error ? host.jsx("p", { className: "forgejo-connection__error", role: "alert" }, error) : null,
    renderEnabledToggle(host, connection, view),
    renderAgentMergeToggle(host, connection, view),
    host.jsx(
      host.ui.Button,
      {
        type: "button",
        variant: "secondary",
        size: "sm",
        disabled: checking || !workspaceId,
        onClick: () => {
          const controller = new AbortController();
          void connection.load(true, controller.signal);
        }
      },
      checking ? "Checking\u2026" : "Test connection"
    )
  );
}
function renderEnabledToggle(host, connection, view) {
  return host.jsx(
    "label",
    { className: "forgejo-connection__toggle" },
    host.jsx(host.ui.Switch, {
      checked: view.enabled,
      disabled: !connection.workspaceId,
      "aria-label": "Enable Forgejo for this workspace",
      onCheckedChange: (next) => void connection.setEnabled(next)
    }),
    host.jsx("span", null, view.enabled ? "Enabled for this workspace" : "Disabled for this workspace")
  );
}
function renderAgentMergeToggle(host, connection, view) {
  return host.jsx(
    "label",
    { className: "forgejo-connection__toggle" },
    host.jsx(host.ui.Switch, {
      checked: view.agentMerge,
      disabled: !connection.workspaceId || !view.enabled,
      "aria-label": "Let agents merge pull requests in this workspace",
      onCheckedChange: (next) => void connection.setAgentMerge(next)
    }),
    host.jsx(
      "span",
      null,
      view.agentMerge ? "Agents may merge pull requests when checks pass" : "Agents may not merge pull requests (review and comment stay on)"
    )
  );
}

// ui/src/watch-model.ts
var NONE = "__none__";
function startStep(workflow) {
  return workflow?.steps?.find((entry) => entry.is_start_step) ?? workflow?.steps?.[0];
}
function emptyDraft(options, kind) {
  const workflow = options.workflows?.[0];
  const step = startStep(workflow);
  return {
    id: "",
    name: "",
    repos: "",
    labels: "",
    state: "open",
    query: "",
    workflowId: workflow?.id ?? "",
    workflowStepId: step?.id ?? "",
    agentProfileId: "",
    executorProfileId: "",
    prompt: "",
    pollIntervalSeconds: String(options.default_interval ?? 300),
    maxInflightTasks: String(options.default_max_inflight ?? 5),
    dedupScope: "watch",
    startAgent: false,
    kind,
    reviewScope: "user_and_teams",
    includeDrafts: false,
    cleanupPolicy: "never",
    forkStepId: ""
  };
}
function toDraft(watch, options, kind) {
  return {
    ...emptyDraft(options, kind),
    id: watch.id,
    ...filterFields(watch),
    ...taskFields(watch, options),
    ...reviewFields(watch)
  };
}
function filterFields(watch) {
  return {
    name: watch.name ?? "",
    repos: repoList(watch),
    labels: (watch.labels ?? []).join(", "),
    state: watch.state || "open",
    query: watch.query ?? ""
  };
}
function taskFields(watch, options) {
  return {
    workflowId: watch.workflow_id ?? "",
    workflowStepId: watch.workflow_step_id ?? "",
    agentProfileId: watch.agent_profile_id ?? "",
    executorProfileId: watch.executor_profile_id ?? "",
    prompt: watch.prompt ?? "",
    pollIntervalSeconds: String(watch.poll_interval_seconds ?? options.default_interval ?? 300),
    maxInflightTasks: String(watch.max_inflight_tasks ?? options.default_max_inflight ?? 5),
    dedupScope: watch.dedup_scope || "watch",
    startAgent: watch.start_agent === true
  };
}
function reviewFields(watch) {
  return {
    reviewScope: watch.review_scope || "user_and_teams",
    includeDrafts: watch.include_drafts === true,
    cleanupPolicy: watch.cleanup_policy || "never",
    forkStepId: watch.fork_workflow_step_id ?? ""
  };
}
function repoList(watch) {
  return (watch.repos ?? []).map((repo) => `${repo.owner}/${repo.name}`).join(", ");
}
function splitList(value) {
  return value.split(/[\n,]/).map((entry) => entry.trim()).filter((entry) => entry.length > 0);
}
function draftToBody(draft) {
  return {
    ...draft.id ? { id: draft.id } : {},
    name: draft.name.trim(),
    repos: splitList(draft.repos),
    labels: splitList(draft.labels),
    state: draft.state,
    query: draft.query.trim(),
    workflow_id: draft.workflowId,
    workflow_step_id: draft.workflowStepId,
    agent_profile_id: draft.agentProfileId,
    executor_profile_id: draft.executorProfileId,
    prompt: draft.prompt,
    start_agent: draft.startAgent,
    poll_interval_seconds: Number(draft.pollIntervalSeconds) || 0,
    max_inflight_tasks: Number(draft.maxInflightTasks) || 0,
    dedup_scope: draft.dedupScope,
    // The kind is fixed at creation; the backend rejects a change, and sending
    // it on update just restates it.
    kind: draft.kind,
    ...draft.kind === "review" ? {
      review_scope: draft.reviewScope,
      include_drafts: draft.includeDrafts,
      cleanup_policy: draft.cleanupPolicy,
      fork_workflow_step_id: draft.forkStepId
    } : {}
  };
}
function summarize(result) {
  const parts = [`${result.created ?? 0} created`];
  if (result.duplicates) parts.push(`${result.duplicates} already tracked`);
  if (result.throttled) parts.push(`${result.throttled} held back by the task limit`);
  if (result.drafts) parts.push(`${result.drafts} draft(s) left out`);
  if (result.skipped_forks) {
    parts.push(`${result.skipped_forks} from a fork left out (no safe column to put them in)`);
  }
  const retired = (result.archived ?? 0) + (result.completed ?? 0);
  if (retired) {
    parts.push(
      `${result.archived ?? 0} archived, ${result.completed ?? 0} completed because their pull request is finished`
    );
  }
  return `${result.matched ?? 0} matched \u2014 ${parts.join(", ")}.`;
}
function summarizeCleanup(result) {
  const archived = result.archived ?? 0;
  const completed = result.completed ?? 0;
  if (archived + completed === 0) return "Nothing to clean up: no tracked pull request has finished.";
  return `${archived} archived, ${completed} completed.`;
}
function formatWhen(value) {
  if (!value) return "never";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}
function watchQueryLine(watch, review) {
  const repos = repoList(watch);
  const labels = (watch.labels ?? []).join(", ");
  if (!review) return repos + (labels ? ` \u2014 ${labels}` : "");
  return [
    watch.review_scope === "user" ? "Requested of me" : "Requested of me or my teams",
    repos ? `in ${repos}` : "in every repository",
    labels
  ].filter(Boolean).join(" \u2014 ");
}
function minPollInterval(options, review) {
  return (review ? options.min_review_interval : options.min_interval) ?? (review ? 60 : 30);
}

// ui/src/watch-form.ts
function field(host, label, control, hint) {
  return host.jsx(
    "div",
    { className: "forgejo-watch-field" },
    host.jsx(host.ui.Label, null, label),
    control,
    hint ? host.jsx("p", { className: "forgejo-watch-field__hint" }, hint) : null
  );
}
function select(host, { value, onChange, items, placeholder, allowNone = false }) {
  return host.jsx(
    host.ui.Select,
    { value: value || (allowNone ? NONE : ""), onValueChange: (next) => onChange(next === NONE ? "" : next) },
    host.jsx(host.ui.SelectTrigger, null, host.jsx(host.ui.SelectValue, { placeholder })),
    host.jsx(
      host.ui.SelectContent,
      null,
      allowNone ? host.jsx(host.ui.SelectItem, { value: NONE }, "None") : null,
      ...items.map((item) => host.jsx(host.ui.SelectItem, { key: item.id, value: item.id }, item.name))
    )
  );
}
function renderWatchForm(ctx, current) {
  const { host } = ctx;
  const update = (patch) => ctx.setDraft({ ...current, ...patch });
  return host.jsx(
    "div",
    { className: "forgejo-watch-form" },
    host.jsx("h4", null, current.id ? "Edit watch" : "New watch"),
    ...renderMatchFields(ctx, current, update),
    ...renderTaskFields(ctx, current, update),
    ...renderScheduleFields(ctx, current, update),
    ...renderStartAgent(ctx, current, update),
    renderFormActions(ctx, current)
  );
}
function renderMatchFields(ctx, current, update) {
  const { host, review } = ctx;
  return [
    field(
      host,
      "Name",
      host.jsx(host.ui.Input, {
        value: current.name,
        placeholder: review ? "My reviews" : "Bug reports",
        onChange: (event) => update({ name: event.target.value })
      })
    ),
    field(
      host,
      review ? "Repositories (optional)" : "Repositories",
      host.jsx(host.ui.Input, {
        value: current.repos,
        placeholder: "owner/name, owner/other",
        onChange: (event) => update({ repos: event.target.value })
      }),
      review ? "Owner/name pairs, separated by commas. Leave empty to follow every repository this token can see." : "One or more owner/name pairs, separated by commas."
    ),
    field(
      host,
      "Labels",
      host.jsx(host.ui.Input, {
        value: current.labels,
        placeholder: "bug, needs-triage",
        onChange: (event) => update({ labels: event.target.value })
      }),
      `${review ? "A pull request" : "An issue"} must carry every label listed here. Leave empty to match any.`
    ),
    ...review ? renderReviewMatchFields(ctx, current, update) : [renderIssueState(ctx, current, update), null, null],
    field(
      host,
      "Search",
      host.jsx(host.ui.Input, {
        value: current.query,
        placeholder: "crash",
        onChange: (event) => update({ query: event.target.value })
      }),
      review ? "Optional free text matched against the title and body, by the instance's search." : "Optional free text matched against the title and body. Served by the instance's issue indexer, so a brand-new issue may take a moment to match \u2014 and nothing matches if indexing is turned off."
    )
  ];
}
function renderIssueState(ctx, current, update) {
  const { host } = ctx;
  return field(
    host,
    "Issue state",
    select(host, {
      value: current.state,
      onChange: (next) => update({ state: next }),
      items: [
        { id: "open", name: "Open" },
        { id: "closed", name: "Closed" },
        { id: "all", name: "All" }
      ],
      placeholder: "Open"
    })
  );
}
function renderReviewMatchFields(ctx, current, update) {
  const { host } = ctx;
  return [
    null,
    field(
      host,
      "Whose requests",
      select(host, {
        value: current.reviewScope,
        onChange: (next) => update({ reviewScope: next }),
        items: [
          { id: "user_and_teams", name: "Me and my teams" },
          { id: "user", name: "Only me" }
        ],
        placeholder: "Me and my teams"
      }),
      "Pull requests where this token's account was asked to review, directly or through a team. A pull request drops out once the account answers."
    ),
    host.jsx(
      "label",
      { className: "forgejo-watch-form__toggle" },
      host.jsx(host.ui.Switch, {
        checked: current.includeDrafts,
        "aria-label": "Also file draft pull requests",
        onCheckedChange: (next) => update({ includeDrafts: next })
      }),
      host.jsx("span", null, "Include draft pull requests")
    )
  ];
}
function renderTaskFields(ctx, current, update) {
  const { host, review, options } = ctx;
  const workflows = options.workflows ?? [];
  const steps = workflows.find((entry) => entry.id === current.workflowId)?.steps ?? [];
  return [
    field(
      host,
      "Workflow",
      select(host, {
        value: current.workflowId,
        onChange: (next) => {
          const step = startStep(workflows.find((entry) => entry.id === next));
          update({ workflowId: next, workflowStepId: step?.id ?? "" });
        },
        items: workflows.map((entry) => ({ id: entry.id, name: entry.name })),
        placeholder: "Choose a workflow"
      })
    ),
    field(
      host,
      "Column",
      select(host, {
        value: current.workflowStepId,
        onChange: (next) => update({ workflowStepId: next }),
        items: steps.map((entry) => ({ id: entry.id, name: entry.name })),
        placeholder: "Choose a column"
      }),
      `Where a card lands when a ${ctx.noun} matches.`
    ),
    review ? field(
      host,
      "Column for fork pull requests",
      select(host, {
        value: current.forkStepId,
        onChange: (next) => update({ forkStepId: next }),
        items: steps.filter((entry) => !entry.auto_starts_agent).map((entry) => ({ id: entry.id, name: entry.name })),
        placeholder: "Same column",
        allowNone: true
      }),
      "A pull request from a fork is code its author controls, so its task never starts an agent and never checks the fork out. Pick a column that does not start agents on entry; with none, fork pull requests are left out if the column above starts agents."
    ) : null,
    ...renderProfileFields(ctx, current, update),
    field(
      host,
      "Prompt",
      host.jsx(host.ui.Textarea, {
        value: current.prompt,
        rows: 3,
        placeholder: review ? options.default_review_prompt ?? "Review this pull request." : "Investigate this issue and propose a fix.",
        onChange: (event) => update({ prompt: event.target.value })
      }),
      review ? "The task description, and what the agent is asked. Placeholders: {{pr.number}} {{pr.title}} {{pr.link}} {{pr.author}} {{pr.repo}} {{pr.branch}} {{pr.base_branch}}. Empty uses the default shown." : "Sent to the agent when the task starts."
    )
  ];
}
function renderProfileFields(ctx, current, update) {
  const { host, options } = ctx;
  return [
    field(
      host,
      "Agent profile",
      select(host, {
        value: current.agentProfileId,
        onChange: (next) => update({ agentProfileId: next }),
        items: options.agent_profiles ?? [],
        placeholder: "Workspace default",
        allowNone: true
      })
    ),
    field(
      host,
      "Executor profile",
      select(host, {
        value: current.executorProfileId,
        onChange: (next) => update({ executorProfileId: next }),
        items: options.executor_profiles ?? [],
        placeholder: "Workspace default",
        allowNone: true
      })
    )
  ];
}
function renderScheduleFields(ctx, current, update) {
  const { host, review, options } = ctx;
  const minimum = minPollInterval(options, review);
  return [
    field(
      host,
      "Poll interval (seconds)",
      host.jsx(host.ui.Input, {
        type: "number",
        min: minimum,
        value: current.pollIntervalSeconds,
        onChange: (event) => update({ pollIntervalSeconds: event.target.value })
      }),
      `At least ${minimum} seconds.`
    ),
    field(
      host,
      "Open task limit",
      host.jsx(host.ui.Input, {
        type: "number",
        min: 1,
        value: current.maxInflightTasks,
        onChange: (event) => update({ maxInflightTasks: event.target.value })
      }),
      `How many of this watch's tasks may be open at once. Matched ${ctx.noun}s above the limit wait for a later run.`
    ),
    review ? renderCleanupPolicy(ctx, current, update) : renderDedupScope(ctx, current, update),
    review && current.cleanupPolicy === "when_closed" && options.archive_granted === false ? host.jsx(
      "p",
      { className: "forgejo-watch-form__hint", role: "note" },
      "This Kandev has not approved archiving for the plugin, so finished tasks are completed rather than archived."
    ) : null
  ];
}
function renderCleanupPolicy(ctx, current, update) {
  const { host } = ctx;
  return field(
    host,
    "When a pull request is merged or closed",
    select(host, {
      value: current.cleanupPolicy,
      onChange: (next) => update({ cleanupPolicy: next }),
      items: [
        { id: "never", name: "Leave the task alone" },
        { id: "when_closed", name: "Archive the task" }
      ],
      placeholder: "Leave the task alone"
    }),
    "Tasks are never deleted. Where the host will not archive them, they are marked complete instead."
  );
}
function renderDedupScope(ctx, current, update) {
  const { host } = ctx;
  return field(
    host,
    "Duplicate handling",
    select(host, {
      value: current.dedupScope,
      onChange: (next) => update({ dedupScope: next }),
      items: [
        { id: "watch", name: "One task per watch" },
        { id: "workspace", name: "One task per issue" }
      ],
      placeholder: "One task per watch"
    }),
    "\u201COne task per issue\u201D stops a second watch filing the same issue again."
  );
}
function renderStartAgent(ctx, current, update) {
  const { host } = ctx;
  return [
    host.jsx(
      "label",
      { className: "forgejo-watch-form__toggle" },
      host.jsx(host.ui.Switch, {
        checked: current.startAgent,
        "aria-label": "Start an agent as soon as the task is created",
        onCheckedChange: (next) => update({ startAgent: next })
      }),
      host.jsx("span", null, "Start an agent immediately")
    ),
    current.startAgent && !current.agentProfileId ? host.jsx(
      "p",
      { className: "forgejo-watch-form__hint", role: "note" },
      "Choose an agent profile: an unattended watch should not depend on whichever profile the workspace defaults to."
    ) : null
  ];
}
function renderFormActions(ctx, current) {
  const { host } = ctx;
  return host.jsx(
    "div",
    { className: "forgejo-watch-form__actions" },
    host.jsx(
      host.ui.Button,
      {
        type: "button",
        size: "sm",
        disabled: ctx.busy !== null,
        onClick: () => void ctx.save(current)
      },
      current.id ? "Save watch" : "Create watch"
    ),
    host.jsx(
      host.ui.Button,
      { type: "button", variant: "ghost", size: "sm", onClick: () => ctx.setDraft(null) },
      "Cancel"
    )
  );
}

// ui/src/watches-panel.ts
function operatorMessage2(cause) {
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
function createWatchesPanel(host, { kind = "issue" } = {}) {
  const review = kind === "review";
  const noun = review ? "pull request" : "issue";
  return function ForgejoWatchesPanel(props = {}) {
    const list = useWatchList(host, kind, props.workspaceId);
    const actions = useWatchActions(host, list, { review, noun });
    if (!list.workspaceId) {
      return host.jsx(
        "p",
        { className: "forgejo-watches__empty" },
        `Open a workspace to manage Forgejo ${noun} watches.`
      );
    }
    const view = {
      host,
      kind,
      review,
      noun,
      options: list.options,
      busy: list.busy,
      setDraft: list.setDraft,
      save: actions.save,
      actions
    };
    return renderWatchesPanel(view, list);
  };
}
function useWatchList(host, kind, routedWorkspaceId) {
  const workspaceId = useWorkspaceId(host, routedWorkspaceId);
  const [watches, setWatches] = useWorkspaceState(host, workspaceId, []);
  const [options, setOptions] = useWorkspaceState(host, workspaceId, {});
  const [draft, setDraft] = useWorkspaceState(host, workspaceId, null);
  const [loading, setLoading] = useWorkspaceState(host, workspaceId, false);
  const [busy, setBusy] = useWorkspaceState(host, workspaceId, null);
  const [error, setError] = useWorkspaceState(host, workspaceId, null);
  const [notice, setNotice] = useWorkspaceState(host, workspaceId, null);
  const call = host.React.useCallback(
    async (key, body, signal) => host.api.invokeAction(
      key,
      { workspaceId, ...body ? { body } : {} },
      signal ? { signal } : void 0
    ),
    [workspaceId]
  );
  const load = host.React.useCallback(
    async (signal) => {
      if (!workspaceId) {
        setWatches([]);
        return;
      }
      setLoading(true);
      setError(null);
      try {
        const [list, config] = await Promise.all([
          call("watches.list", { kind }, signal),
          call("watches.options", void 0, signal)
        ]);
        if (signal.aborted) return;
        setWatches(list?.watches ?? []);
        setOptions(config ?? {});
      } catch (cause) {
        if (!signal.aborted) setError(operatorMessage2(cause));
      } finally {
        if (!signal.aborted) setLoading(false);
      }
    },
    [workspaceId, call]
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
    async (id, run) => {
      setBusy(id);
      setError(null);
      setNotice(null);
      try {
        const message = await run();
        if (message) setNotice(message);
        refresh();
      } catch (cause) {
        setError(operatorMessage2(cause));
      } finally {
        setBusy(null);
      }
    },
    [refresh]
  );
  return { workspaceId, watches, options, draft, setDraft, loading, busy, error, notice, call, act };
}
function useWatchActions(host, { act, call, setDraft }, { review, noun }) {
  const save = host.React.useCallback(
    (current) => act(current.id || "new", async () => {
      await call(current.id ? "watches.update" : "watches.create", draftToBody(current));
      setDraft(null);
      return current.id ? "Watch updated." : "Watch created.";
    }),
    [act, call]
  );
  const runNow = host.React.useCallback(
    (watch) => act(watch.id, async () => {
      const response = await call("watches.run", { id: watch.id });
      return summarize(response?.result ?? {});
    }),
    [act, call]
  );
  const toggle = host.React.useCallback(
    (watch, enabled) => act(watch.id, async () => {
      await call("watches.update", { id: watch.id, enabled });
      return null;
    }),
    [act, call]
  );
  const remove = host.React.useCallback(
    (watch) => act(watch.id, async () => {
      await call("watches.delete", { id: watch.id });
      return "Watch deleted.";
    }),
    [act, call]
  );
  const reset = host.React.useCallback(
    (watch) => act(watch.id, async () => {
      const response = await call("watches.reset", { id: watch.id });
      return `Forgot ${response?.forgotten ?? 0} tracked ${noun}(s); the next run will file the ones still ${review ? "awaiting your review" : "matching"} again.`;
    }),
    [act, call]
  );
  const cleanUp = host.React.useCallback(
    (watch) => act(watch.id, async () => {
      const response = await call("watches.cleanup", { id: watch.id });
      return summarizeCleanup(response?.result ?? {});
    }),
    [act, call]
  );
  return { save, runNow, toggle, remove, reset, cleanUp };
}
function renderWatchesPanel(view, list) {
  const { host, review } = view;
  const { watches, loading, error, notice, draft } = list;
  return host.jsx(
    "div",
    { className: "forgejo-watches" },
    host.jsx(
      "p",
      { className: "forgejo-watches__intro" },
      review ? "File a task for every pull request that asks for your review. Each watch searches the instance for open requests and files a card for each new one." : "Turn Forgejo issues into tasks. Each watch polls the repositories you name and files a card for every new issue that matches."
    ),
    error ? host.jsx("p", { className: "forgejo-watches__error", role: "alert" }, error) : null,
    notice ? host.jsx("p", { className: "forgejo-watches__notice", role: "status" }, notice) : null,
    loading && watches.length === 0 ? host.jsx("p", { className: "forgejo-watches__empty" }, "Loading watches\u2026") : null,
    !loading && watches.length === 0 ? host.jsx(
      "p",
      { className: "forgejo-watches__empty" },
      review ? "No review watches yet." : "No watches yet."
    ) : null,
    ...watches.map((watch) => renderWatchRow(view, watch)),
    draft ? renderWatchForm(view, draft) : host.jsx(
      host.ui.Button,
      {
        type: "button",
        variant: "secondary",
        size: "sm",
        onClick: () => view.setDraft(emptyDraft(view.options, view.kind))
      },
      "Add watch"
    )
  );
}
function renderWatchRow(view, watch) {
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
        onCheckedChange: (next) => void actions.toggle(watch, next)
      })
    ),
    host.jsx("p", { className: "forgejo-watch__query" }, watchQueryLine(watch, view.review)),
    host.jsx(
      "p",
      { className: "forgejo-watch__meta" },
      `Last checked ${formatWhen(watch.last_polled_at)}`
    ),
    watch.last_error ? host.jsx("p", { className: "forgejo-watch__error", role: "alert" }, watch.last_error) : null,
    watch.last_cleanup_note ? host.jsx("p", { className: "forgejo-watch__meta", role: "note" }, watch.last_cleanup_note) : null,
    renderRowActions(view, watch)
  );
}
function renderRowActions(view, watch) {
  const { host, busy, actions } = view;
  const working = busy === watch.id;
  const ghost = (label, onClick) => host.jsx(host.ui.Button, { type: "button", variant: "ghost", size: "sm", disabled: working, onClick }, label);
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
        onClick: () => void actions.runNow(watch)
      },
      working ? "Working\u2026" : "Run now"
    ),
    ghost("Edit", () => view.setDraft(toDraft(watch, view.options, view.kind))),
    view.review && watch.cleanup_policy === "when_closed" ? ghost("Clean up now", () => void actions.cleanUp(watch)) : null,
    ghost("Forget history", () => void actions.reset(watch)),
    ghost("Delete", () => void actions.remove(watch))
  );
}

// ui/src/detail.ts
function toChangeRequestDetail(review) {
  const status = review.taskStatus;
  const reported = status?.state ?? review.state;
  const draft = reported === "draft";
  const detail = {
    providerId: review.providerId,
    reviewKey: review.reviewKey,
    number: status?.number ?? review.changeRequestNumber,
    title: review.title,
    url: review.url,
    state: draft ? "open" : reported,
    author: { name: "unknown" },
    sourceBranch: "",
    targetBranch: "",
    additions: 0,
    deletions: 0,
    reviews: [],
    requestedReviewers: [],
    checks: (status?.checks ?? []).map((check) => ({
      id: check.id,
      name: check.label,
      state: check.state,
      ...check.detail ? { output: check.detail } : {},
      ...check.url ? { url: check.url } : {}
    })),
    comments: []
  };
  if (draft) {
    detail.draft = true;
  }
  addReviewStatus(detail, status);
  return detail;
}
function addReviewStatus(detail, status) {
  if (status?.review) {
    detail.reviewState = status.review.state.toUpperCase();
    if (status.review.requested !== void 0) {
      detail.pendingReviewCount = status.review.requested;
    }
  }
  if (status?.updatedAt !== void 0) {
    detail.lastSyncedAt = new Date(status.updatedAt).toISOString();
  }
}

// ui/src/references.ts
var OWNER_REPO_NUMBER = /^[^/\s]+\/[^#\s]+#[1-9]\d*$/;
var PULL_REQUEST_PATH = /\/([^/\s]+)\/([^/\s]+)\/pulls\/([1-9]\d*)(?:[/?#].*)?$/;
function parsePullRequestReference(reference) {
  const trimmed = reference.trim();
  if (!trimmed) return null;
  if (OWNER_REPO_NUMBER.test(trimmed)) return trimmed;
  if (!trimmed.includes("://")) return null;
  let url;
  try {
    url = new URL(trimmed);
  } catch {
    return null;
  }
  const match = PULL_REQUEST_PATH.exec(url.pathname);
  if (!match) return null;
  const [, owner, repository, number] = match;
  if (!owner || !repository || !number || Number(number) <= 0) return null;
  return trimmed;
}
function looksLikeRepositoryURL(url) {
  const trimmed = url.trim();
  if (!trimmed) return false;
  if (/^[\w.+-]+@[^:\s]+:[^\s]+$/.test(trimmed)) return true;
  try {
    const parsed = new URL(trimmed);
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}

// ui/src/bundle.ts
var PLUGIN_ID = "kandev-plugin-forgejo";
var PROVIDER_ID = "forgejo";
var sourceControl;
window.registerKandevPlugin(PLUGIN_ID, {
  initialize(registry, host) {
    const icon = createForgejoIcon(host);
    sourceControl = registerSourceControlRecipe(registry, host, {
      providerId: PROVIDER_ID,
      label: "Forgejo",
      icon,
      changeRequestNoun: "pull request",
      order: 40,
      supportsDraft: true,
      matchesURL: looksLikeRepositoryURL,
      parseReference: parsePullRequestReference,
      toChangeRequestDetail
    });
    if (typeof registry.registerIntegrationSettings === "function") {
      const ConnectionPanel = createConnectionPanel(host);
      const WatchesPanel = createWatchesPanel(host);
      const ReviewWatchesPanel = createWatchesPanel(host, { kind: "review" });
      const AutomationSettingsPanel = createAutomationSettings(host);
      const SettingsPanel = (props = {}) => host.jsx(
        "div",
        { className: "forgejo-settings" },
        host.jsx(ConnectionPanel, props),
        host.jsx("hr", { className: "forgejo-settings__rule" }),
        host.jsx("h3", { className: "forgejo-settings__heading" }, "Issue watches"),
        host.jsx(WatchesPanel, props),
        host.jsx("hr", { className: "forgejo-settings__rule" }),
        host.jsx("h3", { className: "forgejo-settings__heading" }, "Review watches"),
        host.jsx(ReviewWatchesPanel, props),
        host.jsx("hr", { className: "forgejo-settings__rule" }),
        host.jsx("h3", { className: "forgejo-settings__heading" }, "Pull request automation"),
        host.jsx(AutomationSettingsPanel, props)
      );
      registry.registerIntegrationSettings({
        id: PROVIDER_ID,
        label: "Forgejo",
        description: "Connect a Forgejo or Gitea instance for repositories, pull requests, reviews, issue and review watches, and pull request automation.",
        icon,
        Component: SettingsPanel
      });
    }
  },
  // initialize may run again in the same tab after a disable/enable cycle, so
  // destroy must leave no timers, listeners, or cached snapshots behind.
  destroy() {
    sourceControl?.destroy();
    sourceControl = void 0;
  }
});
