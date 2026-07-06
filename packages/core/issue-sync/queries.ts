import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type {
  CreateSyncSourceInput,
  IssueSyncSource,
  ListSyncSourcesResponse,
  UpdateSyncSourceInput,
  JiraConnection,
  ListJiraConnectionsResponse,
  RemoteContainer,
  ListRemoteContainersResponse,
} from "../types";

// Workspace-scoped query keys. WS events (issue_sync_source:* /
// jira_connection:*) invalidate the wsId prefix so every project under the
// workspace stays fresh without per-project handlers (see
// use-realtime-sync.ts).

export const syncSourceKeys = {
  all: (wsId: string) => ["sync-sources", wsId] as const,
  list: (wsId: string, projectId: string) =>
    [...syncSourceKeys.all(wsId), projectId] as const,
};

export const jiraConnectionKeys = {
  all: (wsId: string) => ["jira-connections", wsId] as const,
  list: (wsId: string) => [...jiraConnectionKeys.all(wsId)] as const,
};

export const remoteContainerKeys = {
  all: (wsId: string) => ["remote-containers", wsId] as const,
  list: (
    wsId: string,
    provider: string,
    connectionId: string | undefined,
  ) => [...remoteContainerKeys.all(wsId), provider, connectionId ?? "default"] as const,
};

// ── Sync sources ────────────────────────────────────────────────────────────

export function syncSourcesOptions(wsId: string, projectId: string) {
  return queryOptions({
    queryKey: syncSourceKeys.list(wsId, projectId),
    queryFn: () => api.listSyncSources(projectId),
    select: (data) => data.sources,
    enabled: !!wsId && !!projectId,
  });
}

export function useCreateSyncSource(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateSyncSourceInput) =>
      api.createSyncSource(projectId, data),
    onSuccess: (created) => {
      qc.setQueryData<ListSyncSourcesResponse>(
        syncSourceKeys.list(wsId, projectId),
        (old) =>
          old && !old.sources.some((s) => s.id === created.id)
            ? {
                ...old,
                sources: [...old.sources, created],
                total: old.total + 1,
              }
            : old,
      );
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: syncSourceKeys.list(wsId, projectId) });
    },
  });
}

export function useUpdateSyncSource(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      sourceId,
      data,
    }: {
      sourceId: string;
      data: UpdateSyncSourceInput;
    }) => api.updateSyncSource(projectId, sourceId, data),
    onSuccess: (updated) => {
      qc.setQueryData<ListSyncSourcesResponse>(
        syncSourceKeys.list(wsId, projectId),
        (old) =>
          old
            ? {
                ...old,
                sources: old.sources.map((s) =>
                  s.id === updated.id ? updated : s,
                ),
              }
            : old,
      );
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: syncSourceKeys.list(wsId, projectId) });
    },
  });
}

export function useDeleteSyncSource(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (sourceId: string) => api.deleteSyncSource(projectId, sourceId),
    onMutate: async (sourceId) => {
      await qc.cancelQueries({
        queryKey: syncSourceKeys.list(wsId, projectId),
      });
      const prev = qc.getQueryData<ListSyncSourcesResponse>(
        syncSourceKeys.list(wsId, projectId),
      );
      qc.setQueryData<ListSyncSourcesResponse>(
        syncSourceKeys.list(wsId, projectId),
        (old) =>
          old
            ? {
                ...old,
                sources: old.sources.filter((s) => s.id !== sourceId),
                total: Math.max(0, old.total - 1),
              }
            : old,
      );
      return { prev };
    },
    onError: (_err, _id, ctx) => {
      if (ctx?.prev) {
        qc.setQueryData(syncSourceKeys.list(wsId, projectId), ctx.prev);
      }
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: syncSourceKeys.list(wsId, projectId) });
    },
  });
}

// ── Remote containers (attach picker data) ──────────────────────────────────

export function remoteContainersOptions(
  wsId: string,
  provider: string,
  connectionId?: string,
) {
  return queryOptions({
    queryKey: remoteContainerKeys.list(wsId, provider, connectionId),
    queryFn: () => api.listRemoteContainers(wsId, provider, connectionId),
    select: (data) => data.containers,
    // Require a provider; connectionId is optional (server defaults to the
    // workspace's first connection of the provider when omitted).
    enabled: !!wsId && !!provider,
  });
}

// ── Jira connections ────────────────────────────────────────────────────────

export function jiraConnectionsOptions(wsId: string) {
  return queryOptions({
    queryKey: jiraConnectionKeys.list(wsId),
    queryFn: () => api.listJiraConnections(wsId),
    enabled: !!wsId,
  });
}

// Re-export the types the UI consumes from this module so views import from a
// single entrypoint (matches the gitlab/github barrel shape).
export type {
  IssueSyncSource,
  CreateSyncSourceInput,
  UpdateSyncSourceInput,
  RemoteContainer,
  JiraConnection,
  ListJiraConnectionsResponse,
  ListRemoteContainersResponse,
};
