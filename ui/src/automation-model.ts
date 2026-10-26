/** The switches a pull request carries, and a workspace's defaults for them. */
export type Switches = {
  auto_fix: boolean;
  auto_merge: boolean;
  on_review_requested: boolean;
  on_merged: boolean;
  on_closed: boolean;
};

/** automation.get: one linked pull request's automation state. */
export type AutomationView = Switches & {
  number: number;
  seeded: boolean;
  fork: boolean;
  rounds: number;
  max_rounds: number;
  exhausted: boolean;
  attempt: { round: number; state: string; dispatch: string } | null;
  last_outcome?: string;
  last_summary?: string;
  last_outcome_at?: string;
  merge_failure?: string;
  merge_retry?: boolean;
  ignored: number;
  status?: string;
  error?: string;
  trust: string;
  integration_enabled: boolean;
};

/** automation.settings: the workspace's settings. */
export type AutomationSettings = {
  defaults: Switches;
  trust: string;
  template: string;
  default_template: string;
  max_rounds: number;
};

export type SwitchKey = keyof Switches;

export const SWITCHES: { key: SwitchKey; label: string }[] = [
  { key: "auto_fix", label: "Auto-fix: prompt the agent when CI fails, the branch conflicts, or reviewers ask for changes" },
  { key: "auto_merge", label: "Auto-merge when CI passes, the branch merges cleanly and reviews allow it" },
  { key: "on_review_requested", label: "Tell the agent when your review is requested" },
  { key: "on_merged", label: "Tell the agent when the pull request is merged" },
  { key: "on_closed", label: "Tell the agent when the pull request is closed without merging" },
];

export const TRUST_LEVELS: { value: string; label: string; hint: string }[] = [
  {
    value: "everyone",
    label: "Everyone",
    hint: "Every review and comment reaches the agent, as with Kandev's GitHub integration. Anyone who can comment on the pull request puts text in front of the agent.",
  },
  {
    value: "official",
    label: "Official reviewers",
    hint: "Only reviews that count towards approvals (reviewers with write access), and comments from people who left one.",
  },
  {
    value: "none",
    label: "No review text",
    hint: "Auto-fix acts on CI failures and conflicts only.",
  },
];

/** Fork pull requests carry code and comments their author controls. */
export const FORK_WARNING =
  "This pull request comes from a fork. Its code and comments are controlled by the fork's author, and the agent will act on them with your executor's environment. Turn this on only if you trust that author.";

/** A one-line description of the agent's last report. */
export function outcomeLine(view: AutomationView): string | null {
  if (!view.last_outcome) return null;
  const outcome = view.last_outcome.replace(/_/g, " ");
  return view.last_summary ? `Last outcome: ${outcome} — ${view.last_summary}` : `Last outcome: ${outcome}`;
}

/** What automation is doing on the pull request, one status line each. */
export function statusLines(view: AutomationView): string[] {
  const lines: string[] = [];
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
      `${view.ignored} review ${view.ignored === 1 ? "comment was" : "comments were"} left out by the workspace's feedback setting (${view.trust}).`,
    );
  }
  return lines;
}

/** Turning on a switch that acts on fork-controlled text asks first. */
export function needsForkConfirmation(view: AutomationView | null, key: SwitchKey, next: boolean): boolean {
  return next && Boolean(view?.fork) && (key === "auto_fix" || key === "auto_merge");
}

export function trustLevel(settings: AutomationSettings): { value: string; label: string; hint: string } {
  return TRUST_LEVELS.find((level) => level.value === settings.trust) ?? TRUST_LEVELS[0]!;
}

/** The prompt being edited, or the stored one, or the default. */
export function templateDraft(settings: AutomationSettings, edited: string | null): string {
  return edited ?? (settings.template || settings.default_template);
}
