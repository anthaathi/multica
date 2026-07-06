// ── Routes (wire centrally in cmd/server/router.go) ──────────────────────────
//
// To avoid file conflicts with sibling agents the Jira routes are NOT added to
// router.go in this change. The orchestrator wires them centrally. The exact
// lines to add:
//
//	// Public (no Multica auth — browser redirect + Jira delivery):
//	r.Get ("/api/jira/oauth/callback",             h.JiraOAuthCallback)
//	r.Post("/api/webhooks/jira/{connectionId}",     h.HandleJiraWebhook)
//
//	// Inside the existing admin r.Group (RequireWorkspaceRoleFromURL owner/admin),
//	// alongside r.Get("/gitlab/connect") / r.Delete("/gitlab/connections/{connectionId}"):
//	r.Get   ("/jira/connect",                        h.JiraConnect)
//	r.Delete("/jira/connections/{connectionId}",     h.DeleteJiraConnection)
//
//	// After the Jira box + sync engine are constructed (router.go ~line 563):
//	h.IssueSync.Providers[issuesync.ProviderJira] = issuesync.NewJiraProvider(queries, h.JiraBox)
//
// The member-visible list route r.Get("/jira/connections", h.ListJiraConnections)
// is already registered in router.go.

package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/integrations/issuesync"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// JiraConnectionResponse is the JSON shape for a Jira connection. Tokens and
// webhook secrets are never serialized — only the public identity fields.
type JiraConnectionResponse struct {
	ID            string  `json:"id"`
	WorkspaceID   string  `json:"workspace_id"`
	CloudID       string  `json:"cloud_id"`
	SiteURL       string  `json:"site_url"`
	AccountID     string  `json:"account_id"`
	AccountEmail  string  `json:"account_email"`
	AccountAvatar *string `json:"account_avatar"`
	ConnectedBy   string  `json:"connected_by"`
	CreatedAt     string  `json:"created_at"`
}

func jiraConnectionToResponse(c db.JiraConnection) JiraConnectionResponse {
	return JiraConnectionResponse{
		ID:            uuidToString(c.ID),
		WorkspaceID:   uuidToString(c.WorkspaceID),
		CloudID:       c.CloudID,
		SiteURL:       c.SiteUrl,
		AccountID:     c.AccountID,
		AccountEmail:  pgTextString(c.AccountEmail),
		AccountAvatar: textToPtr(c.AccountAvatarUrl),
		ConnectedBy:   uuidToString(c.ConnectedByID),
		CreatedAt:     timestampToString(c.CreatedAt),
	}
}

// pgTextString returns the underlying string of a pgtype.Text, or "" when the
// text is NULL/invalid. The Jira response uses a plain (non-pointer) email
// field, unlike the pointer avatar.
func pgTextString(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// isJiraConfigured reports whether the Jira secretbox key is set. The OAuth
// connect flow and token decryption both depend on it; when unset the handlers
// report "not configured" instead of attempting plaintext storage.
func (h *Handler) isJiraConfigured() bool {
	return strings.TrimSpace(os.Getenv("MULTICA_JIRA_SECRET_KEY")) != "" && h.JiraBox != nil
}

// ListJiraConnections returns the workspace's Jira connections for the settings
// UI. Mirrors ListGitLabConnections: member-visible, secrets stripped.
func (h *Handler) ListJiraConnections(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	member, _ := middleware.MemberFromContext(r.Context())
	canManage := roleAllowed(member.Role, "owner", "admin")

	rows, err := h.Queries.ListJiraConnectionsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connections")
		return
	}
	out := make([]JiraConnectionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, jiraConnectionToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connections": out,
		"configured":  h.isJiraConfigured(),
		"can_manage":  canManage,
	})
}

// DeleteJiraConnection removes a Jira connection and cascades the delete to its
// sync sources (connection_id is provider-polymorphic with no FK, so the
// cascade is explicit).
func (h *Handler) DeleteJiraConnection(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	idUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "connectionId"), "connection id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteIssueSyncSourcesByConnection(r.Context(), db.DeleteIssueSyncSourcesByConnectionParams{
		Provider:     "jira",
		ConnectionID: idUUID,
	}); err != nil {
		slog.Warn("issue_sync: cascade delete sources for jira connection failed", "error", err)
	}
	if err := h.Queries.DeleteJiraConnection(r.Context(), db.DeleteJiraConnectionParams{
		ID:          idUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove connection")
		return
	}
	h.publish(protocol.EventJiraConnectionDeleted, chi.URLParam(r, "id"), "system", "", map[string]any{
		"id": chi.URLParam(r, "connectionId"),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ── Config helpers ──────────────────────────────────────────────────────────

func jiraOAuthClientID() string     { return strings.TrimSpace(os.Getenv("JIRA_OAUTH_CLIENT_ID")) }
func jiraOAuthClientSecret() string { return strings.TrimSpace(os.Getenv("JIRA_OAUTH_CLIENT_SECRET")) }

// jiraPublicAPIURL is the deployment's public API origin, used to build the
// OAuth redirect URI and the Jira webhook ingress URL. Same derivation as
// gitlabPublicAPIURL (MULTICA_PUBLIC_URL → FRONTEND_ORIGIN).
func jiraPublicAPIURL() string {
	if v := normalizePublicURL(os.Getenv("MULTICA_PUBLIC_URL")); v != "" {
		return v
	}
	return normalizePublicURL(os.Getenv("FRONTEND_ORIGIN"))
}

func jiraOAuthRedirectURI() string {
	base := jiraPublicAPIURL()
	if base == "" {
		return ""
	}
	return base + "/api/jira/oauth/callback"
}

// jiraWebhookURL builds the per-connection delivery URL. The webhook secret is
// carried as a ?secret= query param because Jira dynamic webhooks do not send
// a signing header; HandleJiraWebhook constant-time compares it to the
// connection's stored secret.
func jiraWebhookURL(connectionID pgtype.UUID, secret string) string {
	base := jiraPublicAPIURL()
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/api/webhooks/jira/%s?secret=%s", base, uuidToString(connectionID), url.QueryEscape(secret))
}

func jiraFrontendSettingsURL() string {
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	return strings.TrimRight(frontend, "/") + "/settings?tab=jira"
}

func generateJiraWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ── Signed OAuth state ──────────────────────────────────────────────────────

// signJiraState binds a workspace ID to the OAuth flow so the callback can
// recover the workspace without trusting query params alone. Same construction
// as signGitLabState, keyed on the Jira OAuth client secret. Format:
// "<workspaceID>.<nonce>.<sigHex>".
func signJiraState(workspaceID string) (string, error) {
	secret := jiraOAuthClientSecret()
	if secret == "" {
		return "", errors.New("jira integration is not configured")
	}
	nonceBytes := make([]byte, 12)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(nonceBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(workspaceID))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	sig := hex.EncodeToString(mac.Sum(nil))
	return workspaceID + "." + nonce + "." + sig, nil
}

func verifyJiraState(token string) (string, bool) {
	secret := jiraOAuthClientSecret()
	if secret == "" {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	workspaceID, nonce, sig := parts[0], parts[1], parts[2]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(workspaceID))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return "", false
	}
	return workspaceID, true
}

// ── Connect ─────────────────────────────────────────────────────────────────

// JiraConnectResponse is the JSON returned by JiraConnect: the authorize URL
// the browser should open, plus a configured flag the frontend gates the button on.
type JiraConnectResponse struct {
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

// JiraConnect (GET /api/workspaces/{id}/jira/connect) returns the Jira OAuth
// 3LO authorize URL the browser should open. The state token binds the
// resulting callback to this workspace.
func (h *Handler) JiraConnect(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	if _, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id"); !ok {
		return
	}
	if !h.isJiraConfigured() || jiraOAuthClientID() == "" || jiraOAuthRedirectURI() == "" {
		writeJSON(w, http.StatusOK, JiraConnectResponse{Configured: false})
		return
	}
	state, err := signJiraState(workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to sign state")
		return
	}
	// Jira Cloud OAuth 3LO authorize endpoint. scope grants user-read (identity),
	// jira-work read/write (issues/comments/webhooks), and offline_access (the
	// rotating refresh token).
	authorizeURL := fmt.Sprintf(
		"https://auth.atlassian.com/authorize?audience=api.atlassian.com&client_id=%s&scope=%s&redirect_uri=%s&state=%s&response_type=code&prompt=consent",
		url.QueryEscape(jiraOAuthClientID()),
		url.QueryEscape("read:jira-user read:jira-work write:jira-work offline_access"),
		url.QueryEscape(jiraOAuthRedirectURI()),
		url.QueryEscape(state),
	)
	writeJSON(w, http.StatusOK, JiraConnectResponse{URL: authorizeURL, Configured: true})
}

// ── OAuth callback ──────────────────────────────────────────────────────────

type jiraAccessibleResource struct {
	ID     string   `json:"id"`  // cloudId
	Name   string   `json:"name"`
	URL    string   `json:"url"` // site URL, e.g. https://example.atlassian.net
	Scopes []string `json:"scopes"`
}

type jiraMe struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Picture   string `json:"picture"`
}

// jiraConnectResponse is the realtime-safe broadcast payload (tokens + secret
// never leave the backend). Mirrors gitlabConnectionToBroadcast.
func jiraConnectionToBroadcast(c db.JiraConnection) JiraConnectionResponse {
	return jiraConnectionToResponse(c)
}

// JiraOAuthCallback (GET /api/jira/oauth/callback) handles the redirect Atlassian
// sends after the user authorizes the OAuth app. We exchange the code for
// tokens, discover the accessible cloud site(s), fetch the connecting user's
// identity, encrypt and persist the connection, best-effort register the Jira
// webhook, then bounce the browser back to Settings → Jira.
func (h *Handler) JiraOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	siteHint := strings.TrimSpace(q.Get("site"))
	settingsURL := jiraFrontendSettingsURL()

	if !h.isJiraConfigured() {
		http.Redirect(w, r, settingsURL+"&jira_error=not_configured", http.StatusFound)
		return
	}
	if code == "" || state == "" {
		http.Redirect(w, r, settingsURL+"&jira_error=missing_params", http.StatusFound)
		return
	}
	workspaceID, ok := verifyJiraState(state)
	if !ok {
		http.Redirect(w, r, settingsURL+"&jira_error=invalid_state", http.StatusFound)
		return
	}
	wsUUID, err := parseStrictUUID(workspaceID)
	if err != nil {
		http.Redirect(w, r, settingsURL+"&jira_error=bad_workspace", http.StatusFound)
		return
	}

	token, err := h.exchangeJiraCode(r.Context(), code)
	if err != nil {
		slog.Error("jira: token exchange failed", "err", err)
		http.Redirect(w, r, settingsURL+"&jira_error=token_exchange_failed", http.StatusFound)
		return
	}
	resources, err := h.fetchJiraAccessibleResources(r.Context(), token.AccessToken)
	if err != nil || len(resources) == 0 {
		slog.Error("jira: accessible-resources failed", "err", err)
		http.Redirect(w, r, settingsURL+"&jira_error=identity_failed", http.StatusFound)
		return
	}
	site := pickJiraSite(resources, siteHint)

	// Best-effort identity fetch — the connection is still usable without it.
	me, _ := h.fetchJiraMe(r.Context(), token.AccessToken)
	accountID := me.AccountID
	if accountID == "" {
		// No /me identity (scope revoked or endpoint degraded); leave a
		// placeholder so the column's NOT NULL holds. The webhook
		// echo-suppression path degrades to content-hash matching only.
		accountID = "unknown"
	}

	accessSealed, err := h.JiraBox.Seal([]byte(token.AccessToken))
	if err != nil {
		http.Redirect(w, r, settingsURL+"&jira_error=persist_failed", http.StatusFound)
		return
	}
	var refreshSealed []byte
	if token.RefreshToken != "" {
		if refreshSealed, err = h.JiraBox.Seal([]byte(token.RefreshToken)); err != nil {
			http.Redirect(w, r, settingsURL+"&jira_error=persist_failed", http.StatusFound)
			return
		}
	}
	webhookSecret, err := generateJiraWebhookSecret()
	if err != nil {
		http.Redirect(w, r, settingsURL+"&jira_error=persist_failed", http.StatusFound)
		return
	}
	secretSealed, err := h.JiraBox.Seal([]byte(webhookSecret))
	if err != nil {
		http.Redirect(w, r, settingsURL+"&jira_error=persist_failed", http.StatusFound)
		return
	}

	var expiresAt pgtype.Timestamptz
	if token.ExpiresIn > 0 {
		expiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second), Valid: true}
	}
	connectedBy := pgtype.UUID{}
	if userID := requestUserID(r); userID != "" {
		if u, err := parseStrictUUID(userID); err == nil {
			connectedBy = u
		}
	}

	conn, err := h.Queries.CreateJiraConnection(r.Context(), db.CreateJiraConnectionParams{
		WorkspaceID:            wsUUID,
		CloudID:                site.ID,
		SiteUrl:                site.URL,
		AccountID:              accountID,
		AccountEmail:           strToTextOrNull(me.Email),
		AccountAvatarUrl:       strToTextOrNull(me.Picture),
		AccessTokenEncrypted:   accessSealed,
		RefreshTokenEncrypted:  refreshSealed,
		TokenExpiresAt:         expiresAt,
		WebhookSecretEncrypted: secretSealed,
		ConnectedByID:          connectedBy,
	})
	if err != nil {
		slog.Error("jira: persist connection failed", "err", err)
		http.Redirect(w, r, settingsURL+"&jira_error=persist_failed", http.StatusFound)
		return
	}

	// Best-effort: register the dynamic webhook so issue/comment events start
	// flowing without manual configuration. Failures are logged; the connection
	// is still usable (a later refresh can retry, or the operator wires it by
	// hand using the per-connection secret).
	h.registerJiraWebhook(r.Context(), conn, token.AccessToken, webhookSecret)

	h.publish(protocol.EventJiraConnectionCreated, workspaceID, "system", "", map[string]any{
		"connection": jiraConnectionToBroadcast(conn),
	})
	http.Redirect(w, r, settingsURL+"&jira_connected=1", http.StatusFound)
}

// pickJiraSite selects the cloud site to bind the connection to. When the
// operator passes ?site= (a URL prefix or cloudId hint), the first matching
// resource wins; otherwise the first accessible site.
func pickJiraSite(resources []jiraAccessibleResource, hint string) jiraAccessibleResource {
	if hint != "" {
		h := strings.ToLower(hint)
		for _, r := range resources {
			if strings.EqualFold(r.ID, hint) || strings.Contains(strings.ToLower(r.URL), h) {
				return r
			}
		}
	}
	return resources[0]
}

func (h *Handler) exchangeJiraCode(ctx context.Context, code string) (issuesync.JiraTokenResponse, error) {
	return issuesync.PostJiraToken(ctx, map[string]any{
		"grant_type":    "authorization_code",
		"client_id":     jiraOAuthClientID(),
		"client_secret": jiraOAuthClientSecret(),
		"code":          code,
		"redirect_uri":  jiraOAuthRedirectURI(),
	})
}

func (h *Handler) fetchJiraAccessibleResources(ctx context.Context, accessToken string) ([]jiraAccessibleResource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.atlassian.com/oauth/token/accessible-resources", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accessible-resources returned %d: %s", resp.StatusCode, string(body))
	}
	var out []jiraAccessibleResource
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *Handler) fetchJiraMe(ctx context.Context, accessToken string) (jiraMe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.atlassian.com/me", nil)
	if err != nil {
		return jiraMe{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return jiraMe{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return jiraMe{}, fmt.Errorf("me endpoint returned %d", resp.StatusCode)
	}
	var out jiraMe
	if err := json.Unmarshal(body, &out); err != nil {
		return jiraMe{}, err
	}
	return out, nil
}

// ── Webhook registration ────────────────────────────────────────────────────

// registerJiraWebhook best-effort registers the Jira dynamic webhook for the
// connection so issue/comment events start flowing without manual setup. The
// delivery URL embeds the per-connection secret as a query param (Jira dynamic
// webhooks send no signing header). Dynamic webhooks EXPIRE after 30 days, so a
// later refresh/re-registration is expected; failures are logged only.
func (h *Handler) registerJiraWebhook(ctx context.Context, conn db.JiraConnection, accessToken, webhookSecret string) {
	whURL := jiraWebhookURL(conn.ID, webhookSecret)
	if whURL == "" {
		slog.Warn("jira: webhook URL not derivable (set MULTICA_PUBLIC_URL); skipping auto-registration")
		return
	}
	body := map[string]any{
		"url": whURL,
		"webhooks": []map[string]any{
			{
				"events":     []string{"jira:issue_created", "jira:issue_updated", "comment_created", "comment_updated"},
				"jql_filter": "issueType != null",
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	endpoint := fmt.Sprintf("https://api.atlassian.com/ex/jira/%s/rest/api/3/webhook", conn.CloudID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("jira: register webhook failed", "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		slog.Warn("jira: register webhook returned non-2xx", "status", resp.StatusCode)
	}
}


// ── Webhook ingress ─────────────────────────────────────────────────────────

// decryptJiraWebhookSecret returns the plaintext webhook secret for a
// connection, or "" if the box is unset or decryption fails.
func (h *Handler) decryptJiraWebhookSecret(c db.JiraConnection) string {
	if h.JiraBox == nil || len(c.WebhookSecretEncrypted) == 0 {
		return ""
	}
	plain, err := h.JiraBox.Open(c.WebhookSecretEncrypted)
	if err != nil {
		return ""
	}
	return string(plain)
}

// HandleJiraWebhook (POST /api/webhooks/jira/{connectionId}) is the destination
// for Jira dynamic-webhook deliveries. Authentication is the per-connection
// secret carried in the ?secret= query param of the registered URL (constant-
// time compared to the stored secret). Issue events normalize via
// JiraIssueToExternal; comment events additionally normalize the comment block.
// The project key (uppercased) routes the event to the matching sync source(s).
func (h *Handler) HandleJiraWebhook(w http.ResponseWriter, r *http.Request) {
	if h.JiraBox == nil {
		writeError(w, http.StatusServiceUnavailable, "jira webhooks not configured")
		return
	}
	connUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "connectionId"), "connection id")
	if !ok {
		return
	}
	conn, err := h.Queries.GetJiraConnectionByID(r.Context(), connUUID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unknown connection")
		return
	}
	secret := h.decryptJiraWebhookSecret(conn)
	delivered := r.URL.Query().Get("secret")
	if secret == "" || !hmac.Equal([]byte(secret), []byte(delivered)) {
		writeError(w, http.StatusUnauthorized, "invalid webhook secret")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	var envelope struct {
		WebhookEvent string          `json:"webhookEvent"`
		Issue        json.RawMessage `json:"issue"`
		Comment      json.RawMessage `json:"comment"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "bad payload")
		return
	}

	// Non-issue deliveries are acknowledged so Jira does not mark the hook
	// failing, but carry nothing for the sync engine.
	if len(envelope.Issue) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ext, ok := issuesync.JiraIssueToExternal(envelope.Issue)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	projectKey := jiraProjectKeyFromPayload(envelope.Issue)
	if projectKey == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	ctx := r.Context()
	switch envelope.WebhookEvent {
	case "jira:issue_created", "jira:issue_updated":
		h.applyRemoteIssueWebhook(ctx, issuesync.ProviderJira, projectKey, issuesync.IssueEvent{
			Kind: "issue", Issue: ext,
		})
	case "comment_created", "comment_updated":
		if len(envelope.Comment) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		comment, ok := issuesync.JiraCommentToExternal(envelope.Comment)
		if !ok {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Echo suppression: drop comments authored by the connection's own
		// Jira identity (our outbound CreateComment/UpdateComment writes). The
		// content-hash check in the engine is the second layer.
		if comment.Author != nil && comment.Author.AccountID != "" && comment.Author.AccountID == conn.AccountID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.applyRemoteIssueWebhook(ctx, issuesync.ProviderJira, projectKey, issuesync.IssueEvent{
			Kind: "comment", Issue: ext, Comment: &comment,
		})
	default:
		// Acknowledge unmodeled events so Jira does not mark the hook failing.
	}
	w.WriteHeader(http.StatusAccepted)
}

// jiraProjectKeyFromPayload extracts the uppercased project key for webhook
// routing. Prefers fields.project.key; falls back to the issue key's project
// prefix (e.g. "PROJ" from "PROJ-42"). Matches the external_key stored on
// issue_sync_source (uppercased project key).
func jiraProjectKeyFromPayload(issueRaw json.RawMessage) string {
	var probe struct {
		Key    string `json:"key"`
		Fields struct {
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
		} `json:"fields"`
	}
	_ = json.Unmarshal(issueRaw, &probe)
	if k := strings.TrimSpace(probe.Fields.Project.Key); k != "" {
		return strings.ToUpper(k)
	}
	if idx := strings.Index(probe.Key, "-"); idx > 0 {
		return strings.ToUpper(probe.Key[:idx])
	}
	return ""
}
