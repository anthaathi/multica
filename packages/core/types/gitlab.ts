export interface GitLabConnection {
  id: string;
  workspace_id: string;
  gitlab_base_url: string;
  gitlab_username: string;
  gitlab_avatar_url: string | null;
  /** Webhook ingress URL for manual configuration. Admin-only (omitted for
   * non-managing members and on realtime broadcasts). */
  webhook_url?: string;
  /** Per-connection webhook secret for manual configuration. Admin-only. */
  webhook_secret?: string;
  created_at: string;
}

export interface ListGitLabConnectionsResponse {
  connections: GitLabConnection[];
  /** Whether the deployment has GitLab OAuth credentials + at-rest key
   * configured. When false, the Connect button is hidden / disabled. */
  configured: boolean;
  /** Whether the caller can connect / disconnect. Non-managing members get
   * `false` and connections without the webhook secret. Older backends omit
   * the field; treat absence as `false` for read-only safety. */
  can_manage?: boolean;
}

export interface GitLabConnectResponse {
  /** The GitLab OAuth authorize URL the browser should open. Empty when
   * `configured` is false. */
  url?: string;
  configured: boolean;
}
