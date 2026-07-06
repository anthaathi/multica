import { z } from "zod";

// Issue-sync frontend contract for the bidirectional sync engine
// (server/internal/integrations/issuesync). These types + zod schemas mirror
// the backend response shapes in server/internal/handler/{issue_sync,jira}.go.
//
// All schemas are intentionally LENIENT (see api/schemas.ts for the rationale):
//   - Provider/backfill enums are `z.string()`, not `z.enum([...])`, so an
//     unknown server-side value renders as a generic fallback instead of
//     crashing a `safeParse`.
//   - Every object schema ends with `.loose()` so unknown fields pass through.
//   - The API client parses every endpoint response through `parseWithFallback`
//     with the EMPTY_* constants below so a drifted contract degrades to an
//     empty list, never a white-screen.

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Sync-source provider. Server-side the values are "github" | "gitlab" | "jira". */
export type SyncProvider = "github" | "gitlab" | "jira";

/** Backfill worker state on a sync source. Server-driven; UI treats unknown
 *  values as "idle" via a default-bearing switch. */
export type BackfillStatus = "idle" | "running" | "completed" | "failed";

/** A sync source attaches a remote container (repo/project) to a project for
 *  bidirectional issue sync. Mirrors IssueSyncSourceResponse in the backend. */
export interface IssueSyncSource {
  id: string;
  project_id: string;
  workspace_id: string;
  provider: SyncProvider;
  /** Polymorphic connection id — github_installation, gitlab_connection, or
   *  jira_connection row. */
  connection_id: string;
  /** Provider-specific attachment payload. Shape varies by provider
   *  (github: {owner,name,repo_id}; gitlab: {project_id,path_with_namespace};
   *  jira: {project_id,key}). */
  external_ref: Record<string, unknown>;
  /** Canonical webhook-routing key (owner/name lowercased; path lowercased;
   *  jira key uppercased). */
  external_key: string;
  /** Local status → external status mapping. */
  status_mapping: Record<string, string>;
  sync_enabled: boolean;
  /** When true, new issues created in this project are pushed to the remote. */
  push_default: boolean;
  backfill_status: string;
  backfill_cursor: string | null;
  created_at: string;
  updated_at: string;
}

export interface ListSyncSourcesResponse {
  sources: IssueSyncSource[];
  total: number;
}

/** A remote container is one attachable repo/project reachable through a
 *  workspace connection — the picker data for the attach flow. Mirrors
 *  issuesync.Container. */
export interface RemoteContainer {
  key: string;
  name: string;
  url: string;
  /** Provider-specific payload stored verbatim as external_ref when attaching. */
  ref: Record<string, unknown>;
}

export interface ListRemoteContainersResponse {
  containers: RemoteContainer[];
  /** The connection_id the server actually used (defaults to the workspace's
   *  first connection of the provider when the caller omits connection_id). */
  connection_id: string;
}

/** A Jira connection (OAuth result). Mirrors JiraConnectionResponse. */
export interface JiraConnection {
  id: string;
  workspace_id: string;
  cloud_id: string;
  site_url: string;
  account_id: string;
  account_email: string;
  account_avatar: string | null;
  connected_by: string;
  created_at: string;
}

export interface ListJiraConnectionsResponse {
  connections: JiraConnection[];
  /** Whether the deployment has the Jira secretbox key configured. When
   *  false the Connect button is hidden / disabled. */
  configured: boolean;
  /** Whether the caller can connect / disconnect. Non-admin members get false. */
  can_manage?: boolean;
}

/** Connect-flow response from GET /api/workspaces/{id}/jira/connect. */
export interface JiraConnectResponse {
  /** The Jira OAuth authorize URL the browser should open. Empty when
   *  `configured` is false. */
  url?: string;
  configured: boolean;
}

// Create/Update payloads. external_ref is sent as the provider-specific object
// the picker returns from RemoteContainer.ref.

export interface CreateSyncSourceInput {
  provider: SyncProvider;
  connection_id: string;
  external_ref: Record<string, unknown>;
  status_mapping?: Record<string, string>;
  push_default?: boolean;
  /** Defaults to true server-side when omitted. */
  sync_enabled?: boolean;
}

export interface UpdateSyncSourceInput {
  status_mapping?: Record<string, string>;
  push_default?: boolean;
  sync_enabled?: boolean;
}

/** A linked external issue rendered on the issue detail header. The backend
 *  issue response does not yet include sync_links; IssueSyncBadges accepts
 *  these optionally and renders nothing when absent. */
export interface IssueSyncLink {
  provider: SyncProvider;
  /** Human-readable id on the remote (e.g. "owner/repo#42", "PROJ-123"). */
  external_key: string;
  web_url?: string;
}

// ---------------------------------------------------------------------------
// Zod schemas (lenient)
// ---------------------------------------------------------------------------

const externalRefSchema = z.record(z.string(), z.unknown()).default({});

export const IssueSyncSourceSchema = z.object({
  id: z.string(),
  project_id: z.string(),
  workspace_id: z.string(),
  // Lenient: unknown provider values still parse so the UI default-cases them.
  provider: z.string(),
  connection_id: z.string(),
  external_ref: externalRefSchema,
  external_key: z.string(),
  status_mapping: z.record(z.string(), z.string()).default({}),
  sync_enabled: z.boolean(),
  push_default: z.boolean(),
  backfill_status: z.string(),
  backfill_cursor: z.string().nullable(),
  created_at: z.string(),
  updated_at: z.string(),
}).loose();

export const ListSyncSourcesResponseSchema = z.object({
  sources: z.array(IssueSyncSourceSchema).default([]),
  total: z.number().default(0),
}).loose();

export const RemoteContainerSchema = z.object({
  key: z.string(),
  name: z.string(),
  url: z.string(),
  ref: externalRefSchema,
}).loose();

export const ListRemoteContainersResponseSchema = z.object({
  containers: z.array(RemoteContainerSchema).default([]),
  connection_id: z.string().default(""),
}).loose();

export const JiraConnectionSchema = z.object({
  id: z.string(),
  workspace_id: z.string(),
  cloud_id: z.string().default(""),
  site_url: z.string().default(""),
  account_id: z.string().default(""),
  account_email: z.string().default(""),
  account_avatar: z.string().nullable(),
  connected_by: z.string().default(""),
  created_at: z.string(),
}).loose();

export const ListJiraConnectionsResponseSchema = z.object({
  connections: z.array(JiraConnectionSchema).default([]),
  configured: z.boolean().default(false),
  can_manage: z.boolean().optional(),
}).loose();

export const JiraConnectResponseSchema = z.object({
  url: z.string().optional(),
  configured: z.boolean().default(false),
}).loose();

// ---------------------------------------------------------------------------
// Fallback constants (used by parseWithFallback when the contract drifts)
// ---------------------------------------------------------------------------

export const EMPTY_SYNC_SOURCE: IssueSyncSource = {
  id: "",
  project_id: "",
  workspace_id: "",
  provider: "github",
  connection_id: "",
  external_ref: {},
  external_key: "",
  status_mapping: {},
  sync_enabled: false,
  push_default: false,
  backfill_status: "idle",
  backfill_cursor: null,
  created_at: "",
  updated_at: "",
};

export const EMPTY_LIST_SYNC_SOURCES_RESPONSE: ListSyncSourcesResponse = {
  sources: [],
  total: 0,
};

export const EMPTY_LIST_REMOTE_CONTAINERS_RESPONSE: ListRemoteContainersResponse = {
  containers: [],
  connection_id: "",
};

export const EMPTY_LIST_JIRA_CONNECTIONS_RESPONSE: ListJiraConnectionsResponse = {
  connections: [],
  configured: false,
  can_manage: false,
};

export const EMPTY_JIRA_CONNECT_RESPONSE: JiraConnectResponse = {
  url: "",
  configured: false,
};
