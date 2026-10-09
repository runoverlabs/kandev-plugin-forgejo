import "./host-contract";
import {
  registerSourceControlRecipe,
  type SourceControlRecipeLifecycle,
} from "./source-control";
import { createForgejoIcon } from "./forgejo-icon";
import { createConnectionPanel } from "./connection-panel";
import { createWatchesPanel } from "./watches-panel";
import { toChangeRequestDetail } from "./detail";
import { looksLikeRepositoryURL, parsePullRequestReference } from "./references";

// Must equal manifest.yaml's `id`.
const PLUGIN_ID = "kandev-plugin-forgejo";
// Must equal manifest repository_providers[0] and the backend ProviderID.
const PROVIDER_ID = "forgejo";

let sourceControl: SourceControlRecipeLifecycle | undefined;

window.registerKandevPlugin(PLUGIN_ID, {
  initialize(registry, host) {
    const icon = createForgejoIcon(host);

    // One call registers the repository provider, the task Link action, and
    // the review provider. Everything provider-specific is passed in here.
    sourceControl = registerSourceControlRecipe(registry, host, {
      providerId: PROVIDER_ID,
      label: "Forgejo",
      icon,
      changeRequestNoun: "pull request",
      order: 40,
      supportsDraft: true,
      matchesURL: looksLikeRepositoryURL,
      parseReference: parsePullRequestReference,
      toChangeRequestDetail,
    });

    // Kandev's native integration settings surface, shared with the built-in
    // code hosts. The credential fields themselves come from the manifest's
    // config_schema; this panel only reports reachability.
    //
    // Guarded: the source-control registrations above are the plugin's reason
    // to exist, and a host without this hook must not take them down with it.
    if (typeof registry.registerIntegrationSettings === "function") {
      const ConnectionPanel = createConnectionPanel(host);
      const WatchesPanel = createWatchesPanel(host);
      const ReviewWatchesPanel = createWatchesPanel(host, { kind: "review" });

      // One settings surface, two sections. Kandev's native issue-watch UI is
      // compiled in per provider, so a plugin has nowhere else to render this:
      // the watch list lives under the connection it depends on.
      const SettingsPanel = (props: { workspaceId?: string } = {}) =>
        host.jsx(
          "div",
          { className: "forgejo-settings" },
          host.jsx(ConnectionPanel, props),
          host.jsx("hr", { className: "forgejo-settings__rule" }),
          host.jsx("h3", { className: "forgejo-settings__heading" }, "Issue watches"),
          host.jsx(WatchesPanel, props),
          host.jsx("hr", { className: "forgejo-settings__rule" }),
          host.jsx("h3", { className: "forgejo-settings__heading" }, "Review watches"),
          host.jsx(ReviewWatchesPanel, props),
        );

      registry.registerIntegrationSettings({
        id: PROVIDER_ID,
        label: "Forgejo",
        description:
          "Connect a Forgejo or Gitea instance for repositories, pull requests, reviews, and issue and review watches.",
        icon,
        Component: SettingsPanel,
      });
    }
  },

  // initialize may run again in the same tab after a disable/enable cycle, so
  // destroy must leave no timers, listeners, or cached snapshots behind.
  destroy() {
    sourceControl?.destroy();
    sourceControl = undefined;
  },
});
