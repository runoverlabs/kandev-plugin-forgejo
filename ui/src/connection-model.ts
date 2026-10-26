import { text } from "./review-model";

/** The `connection.get` / `connection.test` response; any field may be absent. */
export type ConnectionStatus = {
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

export type ConnectionView = {
  configured: boolean;
  connected: boolean;
  enabled: boolean;
  agentMerge: boolean;
  detail: string;
};

export function connectionView(workspaceId: string, status: ConnectionStatus | null): ConnectionView {
  const configured = status?.configured === true;
  const connected = status?.connected === true;
  return {
    configured,
    connected,
    enabled: status?.enabled !== false,
    // Off unless the backend says it is on: the opposite default to `enabled`.
    agentMerge: status?.agent_merge === true,
    detail: connectionDetail(workspaceId, status, configured, connected),
  };
}

function connectionDetail(
  workspaceId: string,
  status: ConnectionStatus | null,
  configured: boolean,
  connected: boolean,
): string {
  if (!workspaceId) return "Open a workspace to check the Forgejo connection.";
  if (!connected) return text(status?.message) || (configured ? "Not verified yet." : "Not configured.");
  return connectedLine(status);
}

function connectedLine(status: ConnectionStatus | null): string {
  const version = text(status?.instance_version);
  const flavor = text(status?.flavor);
  return (
    `Connected to ${text(status?.instance_url)} as ${text(status?.account)}` +
    (version ? ` (${flavor || "instance"} ${version})` : "")
  );
}
