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
