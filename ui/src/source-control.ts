import type {
  PluginHostApi,
  PluginIcon,
  PluginRegistry,
  ReviewSummary,
  ReviewTaskAssociation,
} from "@kandev/plugin-sdk";
import type { ChangeRequestDetailModel } from "./detail";
import { createRepositoryProvider } from "./repository-provider";
import { createReviewPanel, type PanelProps } from "./review-panel";
import { normalizeAssociation, normalizeReview, positiveInteger } from "./review-model";
import { createSnapshotStore, type SnapshotStore } from "./review-store";

export type SourceControlRecipeOptions = {
  providerId: string;
  label: string;
  icon?: PluginIcon;
  changeRequestNoun: string;
  order?: number;
  supportsDraft?: boolean;
  matchesURL?(url: string): boolean;
  parseReference(reference: string): string | null;
  toChangeRequestDetail(review: ReviewSummary): unknown;
};

export type SourceControlRecipeLifecycle = {
  destroy(): void;
};

type ReviewListResponse = { reviews?: unknown[] };
type ReviewAssociationListResponse = { associations?: unknown[] };

/** What every part of a registered recipe shares. */
type Recipe = {
  host: PluginHostApi;
  options: SourceControlRecipeOptions;
  reviewStore: SnapshotStore<string, ReviewSummary>;
  associationStore: SnapshotStore<string, ReviewTaskAssociation>;
  overlays: Set<{ close(): void }>;
};

type ReviewProviderRegistration = Parameters<PluginRegistry["registerReviewProvider"]>[0];

type Refreshers = {
  refreshReviews(taskId: string, signal: AbortSignal, workspaceId?: string): Promise<void>;
  refreshAssociations(workspaceId: string, signal: AbortSignal): Promise<void>;
  refreshAfterMutation(workspaceId: string, taskId: string, signal: AbortSignal): Promise<void>;
};

export function registerSourceControlRecipe(
  registry: PluginRegistry,
  host: PluginHostApi,
  options: SourceControlRecipeOptions,
): SourceControlRecipeLifecycle {
  const recipe: Recipe = {
    host,
    options,
    reviewStore: createSnapshotStore<string, ReviewSummary>(),
    associationStore: createSnapshotStore<string, ReviewTaskAssociation>(),
    overlays: new Set<{ close(): void }>(),
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
    },
  };
}

function createRefreshers(recipe: Recipe): Refreshers {
  const { host, options, reviewStore, associationStore } = recipe;

  async function refreshReviews(taskId: string, signal: AbortSignal, workspaceId?: string) {
    const token = reviewStore.beginRefresh(taskId);
    signal.throwIfAborted();
    const response = await host.api.invokeAction<ReviewListResponse>(
      "change_requests.get",
      { ...(workspaceId ? { workspaceId } : {}), taskId },
      { signal },
    );
    signal.throwIfAborted();
    reviewStore.commit(
      taskId,
      token,
      (response.reviews ?? []).flatMap((review) => {
        const normalized = normalizeReview(options.providerId, review);
        return normalized ? [normalized] : [];
      }),
    );
  }

  async function refreshAssociations(workspaceId: string, signal: AbortSignal) {
    const token = associationStore.beginRefresh(workspaceId);
    signal.throwIfAborted();
    const response = await host.api.invokeAction<ReviewAssociationListResponse>(
      "change_requests.associations",
      { workspaceId },
      { signal },
    );
    signal.throwIfAborted();
    associationStore.commit(
      workspaceId,
      token,
      (response.associations ?? []).flatMap((association) => {
        const normalized = normalizeAssociation(options.providerId, association);
        return normalized ? [normalized] : [];
      }),
    );
  }

  async function refreshAfterMutation(workspaceId: string, taskId: string, signal: AbortSignal) {
    try {
      await Promise.all([
        refreshReviews(taskId, signal, workspaceId),
        refreshAssociations(workspaceId, signal),
      ]);
    } catch {
      // The mutation already succeeded. Its known outcome wins even when the
      // supplied signal cancels this optional refresh; the host can retry it.
    }
  }

  return { refreshReviews, refreshAssociations, refreshAfterMutation };
}

function registerLinkAction(registry: PluginRegistry, recipe: Recipe, refreshers: Refreshers): void {
  const { host, options, overlays } = recipe;
  registry.registerTaskAction({
    id: `${options.providerId}-link-change-request`,
    label: `${options.label} ${options.changeRequestNoun}`,
    ...(options.icon ? { icon: options.icon } : {}),
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
              body: { reference: parsed },
            },
            { signal },
          );
          await refreshers.refreshAfterMutation(context.workspaceId, context.taskId, signal);
        },
      });
      overlays.add(dialog);
    },
  });
}

/** The snapshot review a panel was opened for, until the full details arrive. */
function snapshotFallback(recipe: Recipe, props: PanelProps): ChangeRequestDetailModel | null {
  const review = recipe.reviewStore.get(props.taskId).find(
    (candidate) =>
      candidate.reviewKey === props.reviewKey &&
      candidate.connectionScope === props.connectionScope &&
      candidate.repositoryId === props.repositoryId &&
      String(candidate.changeRequestNumber) === String(props.changeRequestNumber),
  );
  return review ? (recipe.options.toChangeRequestDetail(review) as ChangeRequestDetailModel) : null;
}

function reviewProvider(recipe: Recipe, refreshers: Refreshers): ReviewProviderRegistration {
  const { host, options, reviewStore, associationStore } = recipe;
  const reviewPanel = createReviewPanel(host, {
    fallback: (props) => snapshotFallback(recipe, props),
    afterMutation: (workspaceId, taskId) =>
      refreshers.refreshAfterMutation(workspaceId, taskId, new AbortController().signal),
  });
  return {
    id: options.providerId,
    label: options.label,
    ...(options.icon ? { icon: options.icon } : {}),
    changeRequestNoun: options.changeRequestNoun,
    order: options.order ?? 100,
    getSnapshot: (taskId) => reviewStore.get(taskId),
    subscribe: (taskId, listener) => reviewStore.subscribe(taskId, listener),
    refresh: (taskId, signal) => refreshers.refreshReviews(taskId, signal),
    getAssociationSnapshot: (workspaceId) => associationStore.get(workspaceId),
    subscribeAssociations: (workspaceId, listener) =>
      associationStore.subscribe(workspaceId, listener),
    refreshAssociations: refreshers.refreshAssociations,
    async unlink({
      workspaceId,
      taskId,
      connectionScope,
      repositoryId,
      changeRequestNumber,
      signal,
    }) {
      const number =
        typeof changeRequestNumber === "string" && /^\d+$/.test(changeRequestNumber)
          ? positiveInteger(Number(changeRequestNumber))
          : positiveInteger(changeRequestNumber);
      if (!connectionScope.trim() || !repositoryId.trim() || number === undefined) {
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
            number,
          },
        },
        { signal },
      );
    },
    // The panel is stateful, so its component type is created once here. A new
    // type per render would remount it every time and lose the loaded details.
    ReviewPanel: (props) => host.jsx(reviewPanel, props),
  };
}
