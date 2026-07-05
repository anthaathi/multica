import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  connections: (wsId: string) => [...gitlabKeys.all(wsId), "connections"] as const,
};

export const gitlabConnectionsOptions = (wsId: string) =>
  queryOptions({
    queryKey: gitlabKeys.connections(wsId),
    queryFn: () => api.listGitLabConnections(wsId),
    enabled: !!wsId,
  });
