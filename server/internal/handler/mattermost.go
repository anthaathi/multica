package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/mattermost"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// MattermostInstallationResponse is the wire shape for a Mattermost
// installation row. The encrypted bot token in config is INTENTIONALLY absent
// — it is server-internal (only the outbound sender decrypts it). WS lease
// columns are runtime state, not API surface, so they are omitted too.
type MattermostInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	ServerURL       string `json:"server_url"`
	BotUserID       string `json:"bot_user_id"`
	BotUsername     string `json:"bot_username"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func mattermostInstallationToResponse(row db.ChannelInstallation) MattermostInstallationResponse {
	info := mattermost.DecodePublicConfig(row.Config)
	return MattermostInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		ServerURL:       info.ServerURL,
		BotUserID:       info.BotUserID,
		BotUsername:     info.BotUsername,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ListMattermostInstallations (GET /api/workspaces/{id}/mattermost/installations)
// is member-visible so the Integrations tab renders for non-admins. Response
// flags mirror Slack/Lark:
//   - configured: at-rest encryption key is set (MattermostInstall != nil).
//   - install_supported: true whenever configured, since a BYO install needs
//     only the at-rest key (no hosted OAuth creds).
func (h *Handler) ListMattermostInstallations(w http.ResponseWriter, r *http.Request) {
	if h.MattermostInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations":     []MattermostInstallationResponse{},
			"configured":        false,
			"install_supported": false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.MattermostInstall.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mattermost installations")
		return
	}
	out := make([]MattermostInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, mattermostInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":     out,
		"configured":        true,
		"install_supported": true,
	})
}

// RegisterMattermostBYORequest is the body for a bring-your-own-bot install:
// the Mattermost server URL and the bot access token the user pasted.
type RegisterMattermostBYORequest struct {
	ServerURL string `json:"server_url"`
	BotToken  string `json:"bot_token"`
}

// RegisterMattermostBYO (POST /api/workspaces/{id}/mattermost/install/byo?agent_id=…)
// installs a user-supplied ("bring your own") Mattermost bot for an agent, so
// several agents can each have their own bot identity on the SAME Mattermost
// server. Admin-only at the router. Mirrors RegisterSlackBYO.
func (h *Handler) RegisterMattermostBYO(w http.ResponseWriter, r *http.Request) {
	if h.MattermostInstall == nil {
		writeError(w, http.StatusServiceUnavailable, "mattermost integration not enabled")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	agentIDStr := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentIDStr == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, agentIDStr, "agent_id")
	if !ok {
		return
	}
	// Ownership pre-check at the boundary so a wrong agent_id is a clear 404.
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var body RegisterMattermostBYORequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	row, err := h.MattermostInstall.RegisterBYO(r.Context(), mattermost.RegisterBYOParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InitiatorID: initiatorUUID,
		ServerURL:   body.ServerURL,
		BotToken:    body.BotToken,
	})
	if err != nil {
		switch {
		case errors.Is(err, mattermost.ErrInvalidServerURL), errors.Is(err, mattermost.ErrInvalidBotToken):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, mattermost.ErrServerBotOwnedByAnotherWorkspace):
			writeError(w, http.StatusConflict, "this Mattermost bot is already connected to a different Multica workspace")
		default:
			// The dominant non-sentinel failure here is the server being
			// unreachable (a user error in the URL), so guide the user rather than
			// surfacing an opaque 500.
			writeError(w, http.StatusBadRequest, "could not verify the Mattermost connection — check the server URL is reachable and the bot access token is valid")
		}
		return
	}
	// Broadcast so every open client (Settings, Agent Integrations, other tabs)
	// invalidates its installations query and shows the new bot — matching the
	// revoke event and Slack's install semantics.
	h.publish(protocol.EventMattermostInstallationCreated, uuidToString(row.WorkspaceID), "user", userID, map[string]any{
		"id": uuidToString(row.ID),
	})
	writeJSON(w, http.StatusOK, mattermostInstallationToResponse(row))
}

// RevokeMattermostInstallation (DELETE /api/workspaces/{id}/mattermost/installations/{installationId})
// flips status to 'revoked'. Admin-only at the router. The row is preserved
// for audit; a re-install (re-pasting the bot's token) flips status back to
// 'active'.
func (h *Handler) RevokeMattermostInstallation(w http.ResponseWriter, r *http.Request) {
	if h.MattermostInstall == nil {
		writeError(w, http.StatusServiceUnavailable, "mattermost integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	// Workspace-scoped lookup so one workspace cannot revoke another's
	// installation by guessing the UUID.
	if _, err := h.MattermostInstall.GetInWorkspace(r.Context(), instUUID, wsUUID); err != nil {
		if errors.Is(err, mattermost.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "mattermost installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.MattermostInstall.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventMattermostInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// RedeemMattermostBindingTokenRequest carries the raw token the user clicked
// through from the bot's "link your account" prompt.
type RedeemMattermostBindingTokenRequest struct {
	Token string `json:"token"`
}

// RedeemMattermostBindingTokenResponse echoes the bound
// workspace/installation/user so the frontend can confirm without a second
// fetch.
type RedeemMattermostBindingTokenResponse struct {
	WorkspaceID      string `json:"workspace_id"`
	InstallationID   string `json:"installation_id"`
	MattermostUserID string `json:"mattermost_user_id"`
}

// RedeemMattermostBindingToken (POST /api/mattermost/binding/redeem) binds the
// Mattermost user id carried by the token to the logged-in Multica user. The
// redeemer's identity comes from the session, not the token, so a stolen token
// cannot bind a Mattermost id to an attacker's account. Failure modes map to
// distinct status codes:
//   - 410 Gone:      token unknown / consumed / expired
//   - 409 Conflict:  this Mattermost id is already bound to a different user
//   - 403 Forbidden: redeemer is not a workspace member
func (h *Handler) RedeemMattermostBindingToken(w http.ResponseWriter, r *http.Request) {
	if h.MattermostBindingTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "mattermost integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RedeemMattermostBindingTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}

	redeemed, err := h.MattermostBindingTokens.RedeemAndBind(r.Context(), req.Token, userUUID)
	if err != nil {
		switch {
		case errors.Is(err, mattermost.ErrBindingTokenInvalid):
			writeError(w, http.StatusGone, "binding token invalid or expired")
		case errors.Is(err, mattermost.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this Mattermost account is already bound to a different Multica user")
		case errors.Is(err, mattermost.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "binding refused (are you a workspace member?)")
		default:
			writeError(w, http.StatusInternalServerError, "failed to redeem token")
		}
		return
	}
	writeJSON(w, http.StatusOK, RedeemMattermostBindingTokenResponse{
		WorkspaceID:      uuidToString(redeemed.WorkspaceID),
		InstallationID:   uuidToString(redeemed.InstallationID),
		MattermostUserID: redeemed.MattermostUserID,
	})
}
