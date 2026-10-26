import type { PluginHostApi } from "@kandev/plugin-sdk";
import { text } from "./review-model";

/**
 * The workspace a panel works in. The host routes one into most panels, but
 * the same component can be mounted without one; then it falls back to the
 * active workspace and follows it. A blank routed id counts as none.
 */
export function useWorkspaceId(host: PluginHostApi, routedWorkspaceId: string | undefined): string {
  const [activeWorkspaceId, setActiveWorkspaceId] = host.React.useState<string | undefined>(() =>
    host.context.getActiveWorkspaceId(),
  );
  host.React.useEffect(() => host.context.subscribeActiveWorkspace(setActiveWorkspaceId), []);
  return text(routedWorkspaceId) || text(activeWorkspaceId);
}

/**
 * State kept per workspace. The setter writes to the workspace current when it
 * was made, so a panel that follows the active workspace never shows, or
 * saves, one workspace's draft in another, and a request that settles after a
 * switch lands in the workspace that made it.
 */
export function useWorkspaceState<T>(
  host: PluginHostApi,
  workspaceId: string,
  initial: T,
): [T, (value: T) => void] {
  const [byWorkspace, setByWorkspace] = host.React.useState<ReadonlyMap<string, T>>(() => new Map());
  const set = host.React.useCallback(
    (value: T) => setByWorkspace((previous) => new Map(previous).set(workspaceId, value)),
    [workspaceId],
  );
  return [byWorkspace.has(workspaceId) ? (byWorkspace.get(workspaceId) as T) : initial, set];
}
