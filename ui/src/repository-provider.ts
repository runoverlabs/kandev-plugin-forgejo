import type {
  PluginHostApi,
  PluginIcon,
  RepositoryInspection,
  RepositoryProviderRegistration,
} from "@kandev/plugin-sdk";
import { record, text } from "./review-model";

/** The part of the recipe options the repository provider reads. */
export type RepositoryProviderOptions = {
  providerId: string;
  label: string;
  icon?: PluginIcon;
  supportsDraft?: boolean;
  matchesURL?(url: string): boolean;
};

type RepositoryPageResponse = {
  repositories?: unknown[];
  next_cursor?: unknown;
};

type RepositoryInspectionResponse = { repository?: unknown };
type BranchListResponse = { branches?: unknown[] };
type CreateChangeRequestResponse = {
  url?: unknown;
  output?: unknown;
  linked?: unknown;
  association_error?: unknown;
};

function normalizeRepository(providerId: string, value: unknown): RepositoryInspection | null {
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
    ...(providerScope ? { providerScope } : {}),
    ownerOrProject,
    repositoryId,
    repositoryName,
    cloneUrl,
    ...(defaultBranch ? { defaultBranch } : {}),
  };
}

function credentialFreeRepository(repository: RepositoryInspection) {
  return {
    provider_id: repository.providerId,
    provider_host: repository.providerHost,
    provider_scope: repository.providerScope ?? "",
    provider_repository_id: repository.repositoryId,
    owner_or_project: repository.ownerOrProject,
    name: repository.repositoryName,
    clone_url: repository.cloneUrl,
    default_branch: repository.defaultBranch ?? "",
  };
}

/**
 * The repository picker and pull request creation the recipe registers. A
 * created pull request is linked by the backend, so `refreshAfterMutation`
 * brings the review snapshots up to date before the host is told.
 */
export function createRepositoryProvider(
  host: PluginHostApi,
  options: RepositoryProviderOptions,
  refreshAfterMutation: (workspaceId: string, taskId: string, signal: AbortSignal) => Promise<void>,
): RepositoryProviderRegistration {
  return {
    id: options.providerId,
    label: options.label,
    ...(options.icon ? { icon: options.icon } : {}),
    ...(options.matchesURL ? { matchesURL: options.matchesURL } : {}),
    ...(options.supportsDraft === undefined ? {} : { supportsDraft: options.supportsDraft }),

    async listRepositories({ workspaceId, query = "", cursor = "", limit = 100, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<RepositoryPageResponse>(
        "repositories.list",
        { workspaceId, body: { query, cursor, limit } },
        { signal },
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
        ...(nextCursor ? { nextCursor } : {}),
      };
    },

    async listBranches({ workspaceId, repository, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<BranchListResponse>(
        "repositories.branches",
        { workspaceId, body: { repository: credentialFreeRepository(repository) } },
        { signal },
      );
      signal.throwIfAborted();
      return (response.branches ?? []).flatMap((branch) => {
        const name = text(record(branch).name);
        return name ? [{ name }] : [];
      });
    },

    async inspectURL({ workspaceId, url, signal }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<RepositoryInspectionResponse>(
        "repositories.inspect",
        { workspaceId, body: { url } },
        { signal },
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
      signal,
    }) {
      signal.throwIfAborted();
      const response = await host.api.invokeAction<CreateChangeRequestResponse>(
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
            draft,
          },
        },
        { signal },
      );
      const url = text(response.url);
      if (!url) throw new Error("source-control recipe: create response did not include a URL");
      await refreshAfterMutation(workspaceId, taskId, signal);
      const output = text(response.output);
      const associationError = text(response.association_error);
      return {
        url,
        provider: options.providerId,
        ...(output ? { output } : {}),
        ...(typeof response.linked === "boolean" ? { linked: response.linked } : {}),
        ...(associationError ? { associationError } : {}),
      };
    },
  };
}
