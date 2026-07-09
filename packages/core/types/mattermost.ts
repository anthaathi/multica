/** A Mattermost bot installation bound to a single Multica agent.
 *
 * Wire shape mirrors `MattermostInstallationResponse` in
 * `server/internal/handler/mattermost.go`. New fields the backend adds in the
 * future MUST default to optional so older desktop builds keep parsing the
 * response — see CLAUDE.md → API Compatibility. */
export interface MattermostInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  /** The normalized Mattermost server URL this bot lives on. */
  server_url: string;
  /** The installed bot's Mattermost user id. */
  bot_user_id: string;
  /** The installed bot's Mattermost username (the @-mention handle). */
  bot_username: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
}

export interface ListMattermostInstallationsResponse {
  installations: MattermostInstallation[];
  /** Whether the deployment has the at-rest secret key configured. When false
   * the connect entry points are hidden and the panel renders an "ask the
   * operator to enable Mattermost" state. */
  configured: boolean;
  /** Whether the install path is available (true whenever Mattermost is
   * configured, i.e. the at-rest key is set — a bring-your-own-bot install
   * needs no hosted OAuth credentials). Kept as a separate flag for
   * forward/backward compat; optional so an older desktop build that predates
   * it treats it as off. */
  install_supported?: boolean;
}

/** Request body for a bring-your-own-bot (BYO) install: the server URL and the
 * bot access token the admin pastes from the bot account they created in the
 * Mattermost System Console. The backend validates the token live against the
 * server before persisting, then returns the created MattermostInstallation. */
export interface RegisterMattermostBYORequest {
  server_url: string;
  bot_token: string;
}

/** Post-redemption echo: the Mattermost user id the token carried is now bound
 * to the logged-in Multica user in this workspace/installation. */
export interface RedeemMattermostBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  mattermost_user_id: string;
}
