import type { Component, PluginHostApi } from "@kandev/plugin-sdk";

type ConnectionStatus = {
  enabled?: unknown;
  agent_merge?: unknown;
  configured?: unknown;
  connected?: unknown;
  instance_url?: unknown;
  instance_version?: unknown;
  flavor?: unknown;
  account?: unknown;
  message?: unknown;
};

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

/**
 * Turns any failure from the action bridge into something an operator can act
 * on. Host envelope errors ("workspace action requires only workspaceId") and
 * provider transport errors are implementation detail: they name the wrong
 * actor and read as though the operator typed something wrong. The raw error
 * still goes to the console so it stays diagnosable.
 */
function operatorMessage(cause: unknown): string {
  if (typeof console !== "undefined") {
    console.error("[kandev-plugin-forgejo] connection status request failed", cause);
  }
  return "Couldn't reach the Forgejo plugin. Check the Kandev server logs for details.";
}

/**
 * Connection status for the workspace integrations screen.
 *
 * The credentials themselves live in the manifest's `config_schema` and are
 * edited at Settings > Plugins > Forgejo; this panel never renders or collects
 * a token. It reports what the backend derived.
 *
 * Kandev does not supply an enable/disable control for a plugin integration --
 * a plugin that wants one renders it here and publishes the result with
 * `host.setIntegrationEnabled` so the host's badge follows. The choice is
 * stored per workspace on the backend, which also honors it, so turning this
 * off genuinely withdraws the integration from the workspace instead of only
 * moving a badge.
 */
export function createConnectionPanel(host: PluginHostApi): Component<{ workspaceId?: string }> {
  return function ForgejoConnectionPanel(props: { workspaceId?: string } = {}) {
    const [status, setStatus] = host.React.useState<ConnectionStatus | null>(null);
    const [checking, setChecking] = host.React.useState(false);
    const [error, setError] = host.React.useState<string | null>(null);

    // The host routes a workspace into this panel, but the same component can
    // be mounted without one; fall back to the active workspace and follow it.
    const [activeWorkspaceId, setActiveWorkspaceId] = host.React.useState<string | undefined>(
      () => host.context.getActiveWorkspaceId(),
    );
    host.React.useEffect(
      () => host.context.subscribeActiveWorkspace(setActiveWorkspaceId),
      [],
    );
    const workspaceId = text(props.workspaceId) || text(activeWorkspaceId) || "";

    const publishEnabled = host.React.useCallback(
      (scopeId: string, value: boolean) => {
        // Optional: a host predating this API must not break the panel.
        host.setIntegrationEnabled?.("forgejo", scopeId, value);
      },
      [],
    );

    const setEnabled = host.React.useCallback(
      async (next: boolean) => {
        if (!workspaceId) return;
        // Reflect the choice immediately; the badge and the panel should not
        // wait on a round trip.
        setStatus((current: ConnectionStatus | null) => ({ ...(current ?? {}), enabled: next }));
        publishEnabled(workspaceId, next);
        const controller = new AbortController();
        try {
          await host.api.invokeAction(
            "connection.set_enabled",
            { workspaceId, body: { enabled: next } },
            { signal: controller.signal },
          );
        } catch (cause) {
          // The write failed, so put the control back where it was rather
          // than leaving the UI claiming something the backend did not store.
          setStatus((current: ConnectionStatus | null) => ({ ...(current ?? {}), enabled: !next }));
          publishEnabled(workspaceId, !next);
          setError(operatorMessage(cause));
        }
      },
      [workspaceId, publishEnabled],
    );

    const setAgentMerge = host.React.useCallback(
      async (next: boolean) => {
        if (!workspaceId) return;
        setStatus((current: ConnectionStatus | null) => ({ ...(current ?? {}), agent_merge: next }));
        const controller = new AbortController();
        try {
          await host.api.invokeAction(
            "connection.set_agent_merge",
            { workspaceId, body: { enabled: next } },
            { signal: controller.signal },
          );
        } catch (cause) {
          // Same rule as the enable switch: never show a permission the backend
          // did not store, least of all this one.
          setStatus((current: ConnectionStatus | null) => ({ ...(current ?? {}), agent_merge: !next }));
          setError(operatorMessage(cause));
        }
      },
      [workspaceId],
    );

    const load = host.React.useCallback(
      async (probe: boolean, signal: AbortSignal) => {
        // Every action in the manifest is scope: "workspace". Calling one
        // without the selector is rejected by the host before it reaches the
        // plugin process, so there is nothing useful to request yet.
        if (!workspaceId) {
          setStatus(null);
          setError(null);
          setChecking(false);
          return;
        }
        setChecking(true);
        setError(null);
        try {
          const response = await host.api.invokeAction<ConnectionStatus>(
            probe ? "connection.test" : "connection.get",
            { workspaceId },
            { signal },
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
      [workspaceId],
    );

    // The host gives this panel no AbortSignal, so it owns a controller and
    // aborts it on cleanup.
    host.React.useEffect(() => {
      const controller = new AbortController();
      void load(false, controller.signal);
      return () => controller.abort();
    }, [load]);

    const configured = status?.configured === true;
    const connected = status?.connected === true;
    const enabled = status?.enabled !== false;
    // Off unless the backend says it is on: the opposite default to `enabled`.
    const agentMerge = status?.agent_merge === true;

    let detail: string;
    if (!workspaceId) {
      detail = "Open a workspace to check the Forgejo connection.";
    } else if (connected) {
      const version = text(status?.instance_version);
      const flavor = text(status?.flavor);
      detail =
        `Connected to ${text(status?.instance_url)} as ${text(status?.account)}` +
        (version ? ` (${flavor || "instance"} ${version})` : "");
    } else {
      detail =
        text(status?.message) ||
        (configured ? "Not verified yet." : "Not configured.");
    }

    return host.jsx(
      "div",
      { className: "forgejo-connection" },
      host.jsx(
        "p",
        {
          className: "forgejo-connection__state",
          "data-state": connected ? "connected" : "disconnected",
        },
        detail,
      ),
      // The connection is plugin-wide: one instance URL and token serve every
      // workspace. Say so here rather than implying a per-workspace setting.
      host.jsx(
        "p",
        { className: "forgejo-connection__scope" },
        "This connection is shared by every workspace. Edit it at Settings > Plugins > Forgejo.",
      ),
      error ? host.jsx("p", { className: "forgejo-connection__error", role: "alert" }, error) : null,
      host.jsx(
        "label",
        { className: "forgejo-connection__toggle" },
        host.jsx(host.ui.Switch, {
          checked: enabled,
          disabled: !workspaceId,
          "aria-label": "Enable Forgejo for this workspace",
          onCheckedChange: (next: boolean) => void setEnabled(next),
        }),
        host.jsx("span", null, enabled ? "Enabled for this workspace" : "Disabled for this workspace"),
      ),
      host.jsx(
        "label",
        { className: "forgejo-connection__toggle" },
        host.jsx(host.ui.Switch, {
          checked: agentMerge,
          disabled: !workspaceId || !enabled,
          "aria-label": "Let agents merge pull requests in this workspace",
          onCheckedChange: (next: boolean) => void setAgentMerge(next),
        }),
        host.jsx(
          "span",
          null,
          agentMerge
            ? "Agents may merge pull requests when checks pass"
            : "Agents may not merge pull requests (review and comment stay on)",
        ),
      ),
      host.jsx(
        host.ui.Button,
        {
          type: "button",
          variant: "secondary",
          size: "sm",
          disabled: checking || !workspaceId,
          onClick: () => {
            const controller = new AbortController();
            void load(true, controller.signal);
          },
        },
        checking ? "Checking…" : "Test connection",
      ),
    );
  };
}
