package handler

import (
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Config ──────────────────────────────────────────────────────────────────

// gitlabInstanceURL is the base URL of the GitLab instance this deployment
// integrates with (self-hosted or gitlab.com), e.g. https://gitlab.example.com.
// Stored without a trailing slash. Mutable so tests can point OAuth + API calls
// at an httptest server.
func gitlabInstanceURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_URL")), "/")
}

// gitlabInternalURL is the base URL used for server-to-server calls against the
// GitLab instance (OAuth token exchange at /oauth/token, /api/v4/* reads, and
// project webhook registration). When GITLAB_INTERNAL_URL is set, it overrides
// GITLAB_URL for these calls only — useful when the public GITLAB_URL is not
// resolvable from inside the cluster (e.g. it only exists in split-horizon /
// VPN DNS), so the backend dials an in-cluster Service address instead.
// GITLAB_URL remains the browser-facing identity (OAuth authorize URL, stored
// connection host used for repo host-matching). Falls back to GITLAB_URL.
func gitlabInternalURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_INTERNAL_URL")), "/"); v != "" {
		return v
	}
	return gitlabInstanceURL()
}

func gitlabOAuthClientID() string { return strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_ID")) }

func gitlabOAuthClientSecret() string {
	return strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_SECRET"))
}

// isGitLabConfigured reports whether the deployment can run the OAuth connect
// flow AND encrypt tokens at rest. All four must hold: the instance URL, the
// OAuth client id/secret, and a non-nil secretbox (MULTICA_GITLAB_SECRET_KEY).
// The Connect button reads this single flag so the frontend never offers a flow
// the backend would reject.
func (h *Handler) isGitLabConfigured() bool {
	return gitlabInstanceURL() != "" &&
		gitlabOAuthClientID() != "" &&
		gitlabOAuthClientSecret() != "" &&
		h.GitLabBox != nil
}

// gitlabPublicAPIURL returns the deployment's public API origin used to build
// the OAuth redirect URI and the webhook ingress URL. Mirrors the daemon setup
// derivation (MULTICA_PUBLIC_URL, falling back to the frontend origin).
func gitlabPublicAPIURL() string {
	if v := normalizePublicURL(os.Getenv("MULTICA_PUBLIC_URL")); v != "" {
		return v
	}
	return normalizePublicURL(os.Getenv("FRONTEND_ORIGIN"))
}

func gitlabOAuthRedirectURI() string {
	base := gitlabPublicAPIURL()
	if base == "" {
		return ""
	}
	return base + "/api/gitlab/oauth/callback"
}

func gitlabWebhookURL() string {
	base := gitlabPublicAPIURL()
	if base == "" {
		return ""
	}
	return base + "/api/webhooks/gitlab"
}

// ── Response shapes ─────────────────────────────────────────────────────────

// GitLabConnectionResponse is the JSON shape returned by the connection list
// endpoint. webhook_url + webhook_secret are the manual-configuration fallback
// for projects the OAuth token cannot administer; both are admin-only (the
// list handler strips them for non-managing members, matching how the GitHub
// installation list strips the numeric installation_id).
type GitLabConnectionResponse struct {
	ID              string  `json:"id"`
	WorkspaceID     string  `json:"workspace_id"`
	GitLabBaseURL   string  `json:"gitlab_base_url"`
	GitLabUsername  string  `json:"gitlab_username"`
	GitLabAvatarURL *string `json:"gitlab_avatar_url"`
	WebhookURL      *string `json:"webhook_url,omitempty"`
	WebhookSecret   *string `json:"webhook_secret,omitempty"`
	CreatedAt       string  `json:"created_at"`
}

type GitLabConnectResponse struct {
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

func gitlabConnectionToResponse(c db.GitlabConnection) GitLabConnectionResponse {
	return GitLabConnectionResponse{
		ID:              uuidToString(c.ID),
		WorkspaceID:     uuidToString(c.WorkspaceID),
		GitLabBaseURL:   c.GitlabBaseUrl,
		GitLabUsername:  c.GitlabUsername,
		GitLabAvatarURL: textToPtr(c.GitlabAvatarUrl),
		CreatedAt:       timestampToString(c.CreatedAt),
	}
}

// gitlabConnectionToBroadcast is the realtime payload: it never carries the
// webhook secret (realtime fans out to every workspace subscriber regardless of
// role). The frontend uses the event only to invalidate the connections query.
func gitlabConnectionToBroadcast(c db.GitlabConnection) GitLabConnectionResponse {
	return gitlabConnectionToResponse(c)
}

// gitlabMRRowToResponse maps a GitLab merge request onto the shared
// GitHubPullRequestResponse shape so the issue-detail PR list can render GitHub
// PRs and GitLab MRs from one array. GitLab vocabulary is mapped onto the PR
// fields (namespace/path → owner/name, iid → number, merge_status → clean/dirty,
// pipeline aggregate → checks). Provider="gitlab" lets the frontend show the
// right glyph.
func gitlabMRRowToResponse(m db.ListMergeRequestsByIssueRow) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		Provider:         "gitlab",
		ID:               uuidToString(m.ID),
		WorkspaceID:      uuidToString(m.WorkspaceID),
		RepoOwner:        m.NamespacePath,
		RepoName:         m.ProjectPath,
		Number:           m.MrIid,
		Title:            m.Title,
		State:            m.State,
		HtmlURL:          m.WebUrl,
		Branch:           textToPtr(m.SourceBranch),
		AuthorLogin:      textToPtr(m.AuthorUsername),
		AuthorAvatarURL:  textToPtr(m.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(m.MergedAt),
		ClosedAt:         timestampToPtr(m.ClosedAt),
		PRCreatedAt:      timestampToString(m.MrCreatedAt),
		PRUpdatedAt:      timestampToString(m.MrUpdatedAt),
		MergeableState:   gitlabMergeableState(m.MergeStatus),
		ChecksConclusion: aggregateChecksConclusion(m.ChecksFailed, m.ChecksPassed, m.ChecksPending, m.ChecksTotal),
		ChecksPassed:     m.ChecksPassed,
		ChecksFailed:     m.ChecksFailed,
		ChecksPending:    m.ChecksPending,
		Additions:        m.Additions,
		Deletions:        m.Deletions,
		ChangedFiles:     m.ChangedFiles,
	}
}

// gitlabMergeableState maps GitLab's merge_status onto the clean/dirty vocabulary
// the frontend understands (it only surfaces those two). Everything else
// round-trips and renders as unknown.
func gitlabMergeableState(s pgtype.Text) *string {
	if !s.Valid || s.String == "" {
		return nil
	}
	var v string
	switch s.String {
	case "can_be_merged":
		v = "clean"
	case "cannot_be_merged":
		v = "dirty"
	default:
		v = s.String
	}
	return &v
}

// ── State token ─────────────────────────────────────────────────────────────

// signGitLabState binds a workspace ID to the OAuth flow so the callback can
// recover the workspace without trusting query params alone. Same construction
// as github.go's signState but keyed on the OAuth client secret (the value an
// operator has already configured for GitLab). Format:
// "<workspaceID>.<nonce>.<sigHex>".
func signGitLabState(workspaceID string) (string, error) {
	secret := gitlabOAuthClientSecret()
	if secret == "" {
		return "", errors.New("gitlab integration is not configured")
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

func verifyGitLabState(token string) (string, bool) {
	secret := gitlabOAuthClientSecret()
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

// ── Connect / callback ──────────────────────────────────────────────────────

// GitLabConnect (GET /api/workspaces/{id}/gitlab/connect) returns the GitLab
// OAuth authorize URL the browser should open. The state token binds the
// resulting callback to this workspace.
func (h *Handler) GitLabConnect(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	if _, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id"); !ok {
		return
	}
	if !h.isGitLabConfigured() || gitlabOAuthRedirectURI() == "" {
		writeJSON(w, http.StatusOK, GitLabConnectResponse{Configured: false})
		return
	}
	state, err := signGitLabState(workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to sign state")
		return
	}
	authorizeURL := fmt.Sprintf(
		"%s/oauth/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		gitlabInstanceURL(),
		url.QueryEscape(gitlabOAuthClientID()),
		url.QueryEscape(gitlabOAuthRedirectURI()),
		url.QueryEscape("api"),
		url.QueryEscape(state),
	)
	writeJSON(w, http.StatusOK, GitLabConnectResponse{URL: authorizeURL, Configured: true})
}

type gitlabTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	CreatedAt    int64  `json:"created_at"`
}

type gitlabUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// GitLabOAuthCallback (GET /api/gitlab/oauth/callback) handles the redirect
// GitLab sends after the user authorizes the OAuth app. We exchange the code
// for tokens, fetch the connecting user's identity, encrypt and persist the
// connection, best-effort register project webhooks, then bounce the browser
// back to Settings → GitLab.
func (h *Handler) GitLabOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	settingsURL := strings.TrimRight(frontend, "/") + "/settings?tab=gitlab"

	if !h.isGitLabConfigured() {
		http.Redirect(w, r, settingsURL+"&gitlab_error=not_configured", http.StatusFound)
		return
	}
	if code == "" || state == "" {
		http.Redirect(w, r, settingsURL+"&gitlab_error=missing_params", http.StatusFound)
		return
	}
	workspaceID, ok := verifyGitLabState(state)
	if !ok {
		http.Redirect(w, r, settingsURL+"&gitlab_error=invalid_state", http.StatusFound)
		return
	}
	wsUUID, err := parseStrictUUID(workspaceID)
	if err != nil {
		http.Redirect(w, r, settingsURL+"&gitlab_error=bad_workspace", http.StatusFound)
		return
	}

	token, err := h.exchangeGitLabCode(r.Context(), code)
	if err != nil {
		slog.Error("gitlab: token exchange failed", "err", err)
		http.Redirect(w, r, settingsURL+"&gitlab_error=token_exchange_failed", http.StatusFound)
		return
	}
	user, err := h.fetchGitLabUser(r.Context(), token.AccessToken)
	if err != nil {
		slog.Error("gitlab: fetch user failed", "err", err)
		http.Redirect(w, r, settingsURL+"&gitlab_error=identity_failed", http.StatusFound)
		return
	}

	accessSealed, err := h.GitLabBox.Seal([]byte(token.AccessToken))
	if err != nil {
		http.Redirect(w, r, settingsURL+"&gitlab_error=persist_failed", http.StatusFound)
		return
	}
	var refreshSealed []byte
	if token.RefreshToken != "" {
		if refreshSealed, err = h.GitLabBox.Seal([]byte(token.RefreshToken)); err != nil {
			http.Redirect(w, r, settingsURL+"&gitlab_error=persist_failed", http.StatusFound)
			return
		}
	}
	webhookSecret, err := generateGitLabWebhookSecret()
	if err != nil {
		http.Redirect(w, r, settingsURL+"&gitlab_error=persist_failed", http.StatusFound)
		return
	}
	secretSealed, err := h.GitLabBox.Seal([]byte(webhookSecret))
	if err != nil {
		http.Redirect(w, r, settingsURL+"&gitlab_error=persist_failed", http.StatusFound)
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

	conn, err := h.Queries.CreateGitLabConnection(r.Context(), db.CreateGitLabConnectionParams{
		WorkspaceID:            wsUUID,
		GitlabBaseUrl:          gitlabInstanceURL(),
		GitlabUserID:           user.ID,
		GitlabUsername:         user.Username,
		GitlabAvatarUrl:        strToTextOrNull(user.AvatarURL),
		AccessTokenEncrypted:   accessSealed,
		RefreshTokenEncrypted:  refreshSealed,
		TokenExpiresAt:         expiresAt,
		WebhookSecretEncrypted: secretSealed,
		ConnectedByID:          connectedBy,
	})
	if err != nil {
		slog.Error("gitlab: persist connection failed", "err", err)
		http.Redirect(w, r, settingsURL+"&gitlab_error=persist_failed", http.StatusFound)
		return
	}

	// Best-effort: register webhooks on the workspace's GitLab repos so MR /
	// pipeline events start flowing without manual configuration. Failures are
	// logged; the settings UI surfaces the webhook URL + secret as a fallback.
	h.registerGitLabWebhooks(r.Context(), conn, token.AccessToken, webhookSecret)

	h.publish(protocol.EventGitLabConnectionCreated, workspaceID, "system", "", map[string]any{
		"connection": gitlabConnectionToBroadcast(conn),
	})
	http.Redirect(w, r, settingsURL+"&gitlab_connected=1", http.StatusFound)
}

func (h *Handler) exchangeGitLabCode(ctx context.Context, code string) (gitlabTokenResponse, error) {
	form := url.Values{
		"client_id":     {gitlabOAuthClientID()},
		"client_secret": {gitlabOAuthClientSecret()},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {gitlabOAuthRedirectURI()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gitlabInternalURL()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return gitlabTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return gitlabTokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return gitlabTokenResponse{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	var out gitlabTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return gitlabTokenResponse{}, err
	}
	if out.AccessToken == "" {
		return gitlabTokenResponse{}, errors.New("token response missing access_token")
	}
	return out, nil
}

func (h *Handler) fetchGitLabUser(ctx context.Context, accessToken string) (gitlabUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitlabInternalURL()+"/api/v4/user", nil)
	if err != nil {
		return gitlabUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return gitlabUser{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return gitlabUser{}, fmt.Errorf("user endpoint returned %d", resp.StatusCode)
	}
	var out gitlabUser
	if err := json.Unmarshal(body, &out); err != nil {
		return gitlabUser{}, err
	}
	if out.ID == 0 || out.Username == "" {
		return gitlabUser{}, errors.New("user response missing id/username")
	}
	return out, nil
}

func generateGitLabWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ── Listing / disconnect ────────────────────────────────────────────────────

// ListGitLabConnections returns the workspace's GitLab connections to any
// member. Connect/disconnect stay admin-only at the router level; the response
// carries `can_manage` and strips the webhook_url + webhook_secret for
// non-managing members (they are the manual-configuration handle, mirroring how
// the GitHub list strips installation_id).
func (h *Handler) ListGitLabConnections(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	member, _ := middleware.MemberFromContext(r.Context())
	canManage := roleAllowed(member.Role, "owner", "admin")

	rows, err := h.Queries.ListGitLabConnectionsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connections")
		return
	}
	whURL := gitlabWebhookURL()
	out := make([]GitLabConnectionResponse, 0, len(rows))
	for _, row := range rows {
		resp := gitlabConnectionToResponse(row)
		if canManage {
			if whURL != "" {
				u := whURL
				resp.WebhookURL = &u
			}
			if secret := h.decryptGitLabWebhookSecret(row); secret != "" {
				resp.WebhookSecret = &secret
			}
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connections": out,
		"configured":  h.isGitLabConfigured(),
		"can_manage":  canManage,
	})
}

func (h *Handler) DeleteGitLabConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	id := chi.URLParam(r, "connectionId")
	idUUID, ok := parseUUIDOrBadRequest(w, id, "connection id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteGitLabConnection(r.Context(), db.DeleteGitLabConnectionParams{
		ID:          idUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove connection")
		return
	}
	h.publish(protocol.EventGitLabConnectionDeleted, workspaceID, "system", "", map[string]any{
		"id": id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// decryptGitLabWebhookSecret returns the plaintext webhook secret for a
// connection, or "" if the box is unset or decryption fails.
func (h *Handler) decryptGitLabWebhookSecret(c db.GitlabConnection) string {
	if h.GitLabBox == nil || len(c.WebhookSecretEncrypted) == 0 {
		return ""
	}
	plain, err := h.GitLabBox.Open(c.WebhookSecretEncrypted)
	if err != nil {
		return ""
	}
	return string(plain)
}

// ── Webhook registration ────────────────────────────────────────────────────

// registerGitLabWebhooks best-effort creates a project webhook on each GitLab
// repo in the workspace's repos registry whose host matches the connection.
// Failures are logged and non-fatal — the settings UI surfaces the webhook URL +
// secret for manual setup on projects the token cannot administer.
func (h *Handler) registerGitLabWebhooks(ctx context.Context, conn db.GitlabConnection, accessToken, webhookSecret string) {
	whURL := gitlabWebhookURL()
	if whURL == "" {
		slog.Warn("gitlab: webhook URL not derivable (set MULTICA_PUBLIC_URL); skipping auto-registration")
		return
	}
	ws, err := h.Queries.GetWorkspace(ctx, conn.WorkspaceID)
	if err != nil || len(ws.Repos) == 0 {
		return
	}
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(ws.Repos, &repos); err != nil {
		return
	}
	host := hostFromURL(conn.GitlabBaseUrl)
	for _, rp := range repos {
		identity := repoIdentityFromURL(rp.URL) // host/owner/name (lowercased)
		if identity == "" {
			continue
		}
		if !strings.HasPrefix(identity, strings.ToLower(host)+"/") {
			continue
		}
		projectPath := strings.TrimPrefix(identity, strings.ToLower(host)+"/")
	if err := h.createGitLabProjectHook(ctx, gitlabInternalURL(), accessToken, projectPath, whURL, webhookSecret); err != nil {
			slog.Warn("gitlab: register project webhook failed", "err", err, "project", projectPath)
		}
	}
}

func (h *Handler) createGitLabProjectHook(ctx context.Context, baseURL, accessToken, projectPath, whURL, secret string) error {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/hooks", strings.TrimRight(baseURL, "/"), url.PathEscape(projectPath))
	form := url.Values{
		"url":                     {whURL},
		"token":                   {secret},
		"merge_requests_events":   {"true"},
		"pipeline_events":         {"true"},
		"issues_events":           {"true"},
		"note_events":             {"true"},
		"enable_ssl_verification": {"true"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hooks endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// ── Webhook ingress ─────────────────────────────────────────────────────────

// HandleGitLabWebhook (POST /api/webhooks/gitlab) is the destination for every
// event from a connected GitLab project. GitLab authenticates deliveries with a
// per-hook plaintext secret in the X-Gitlab-Token header (not an HMAC, unlike
// GitHub's X-Hub-Signature-256), so we match the token against every connection
// for the delivering instance and route to that connection's workspace.
func (h *Handler) HandleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if h.GitLabBox == nil {
		writeError(w, http.StatusServiceUnavailable, "gitlab webhooks not configured")
		return
	}
	token := r.Header.Get("X-Gitlab-Token")
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing token")
		return
	}

	// The project's web_url gives us the instance base URL to scope the
	// connection lookup; the token then selects the exact connection (and thus
	// the target workspace).
	var envelope struct {
		ObjectKind string `json:"object_kind"`
		Project    struct {
			WebURL string `json:"web_url"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "bad payload")
		return
	}
	baseURL := baseURLFromWebURL(envelope.Project.WebURL)
	if baseURL == "" {
		writeError(w, http.StatusBadRequest, "missing project url")
		return
	}
	conn, ok := h.matchGitLabConnection(r.Context(), baseURL, token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	switch envelope.ObjectKind {
	case "merge_request":
		h.handleMergeRequestEvent(r.Context(), conn, body)
	case "pipeline":
		h.handlePipelineEvent(r.Context(), conn, body)
	case "issue":
		h.handleGitLabIssueEvent(r.Context(), conn, body)
	case "note":
		h.handleGitLabNoteEvent(r.Context(), conn, body)
	default:
		// Acknowledge unmodeled events so GitLab doesn't mark the hook failing.
	}
	w.WriteHeader(http.StatusAccepted)
}

// matchGitLabConnection finds the connection whose decrypted webhook secret
// equals the delivered token, among all connections for the delivering GitLab
// instance. Comparison is constant-time.
func (h *Handler) matchGitLabConnection(ctx context.Context, baseURL, token string) (db.GitlabConnection, bool) {
	conns, err := h.Queries.ListGitLabConnectionsByProject(ctx, baseURL)
	if err != nil {
		slog.Warn("gitlab: lookup connections failed", "err", err)
		return db.GitlabConnection{}, false
	}
	for _, c := range conns {
		secret := h.decryptGitLabWebhookSecret(c)
		if secret == "" {
			continue
		}
		if hmac.Equal([]byte(secret), []byte(token)) {
			return c, true
		}
	}
	return db.GitlabConnection{}, false
}

// workspaceAutoLinkMRsEnabled reports whether the workspace allows the GitLab
// webhook to create issue ↔ MR link rows. Mirrors workspaceAutoLinkPRsEnabled
// but reads the GitLab settings keys: defaults to true, and short-circuits to
// false when the master `gitlab_enabled` switch is explicitly off.
func (h *Handler) workspaceAutoLinkMRsEnabled(ctx context.Context, workspaceID pgtype.UUID) bool {
	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil || len(ws.Settings) == 0 {
		return true
	}
	var s struct {
		GitLabEnabled            *bool `json:"gitlab_enabled"`
		GitLabAutoLinkMRsEnabled *bool `json:"gitlab_auto_link_mrs_enabled"`
	}
	if err := json.Unmarshal(ws.Settings, &s); err != nil {
		return true
	}
	if s.GitLabEnabled != nil && !*s.GitLabEnabled {
		return false
	}
	if s.GitLabAutoLinkMRsEnabled == nil {
		return true
	}
	return *s.GitLabAutoLinkMRsEnabled
}

// ── Merge request event ─────────────────────────────────────────────────────

type glMergeRequestPayload struct {
	ObjectKind string `json:"object_kind"`
	User       struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	ObjectAttributes struct {
		IID          int32  `json:"iid"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		State        string `json:"state"`
		Action       string `json:"action"`
		URL          string `json:"url"`
		SourceBranch string `json:"source_branch"`
		MergeStatus  string `json:"merge_status"`
		Draft        bool   `json:"draft"`
		WIP          bool   `json:"work_in_progress"`
		Oldrev       string `json:"oldrev"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
		MergedAt     string `json:"merged_at"`
		ClosedAt     string `json:"closed_at"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

func (h *Handler) handleMergeRequestEvent(ctx context.Context, conn db.GitlabConnection, body []byte) {
	var p glMergeRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad merge_request payload", "err", err)
		return
	}
	if p.Project.ID == 0 || p.ObjectAttributes.IID == 0 {
		return
	}
	wsID := conn.WorkspaceID
	namespace, project := splitPathWithNamespace(p.Project.PathWithNamespace, p.Project.Name)

	state := deriveMRState(p.ObjectAttributes.State, p.ObjectAttributes.Draft || p.ObjectAttributes.WIP)
	mergeStatus, clearMergeStatus := deriveMRMergeStatus(p.ObjectAttributes.Action, p.ObjectAttributes.MergeStatus, p.ObjectAttributes.Oldrev != "")

	mr, err := h.Queries.UpsertGitLabMergeRequest(ctx, db.UpsertGitLabMergeRequestParams{
		WorkspaceID:      wsID,
		ConnectionID:     conn.ID,
		ProjectID:        p.Project.ID,
		NamespacePath:    namespace,
		ProjectPath:      project,
		MrIid:            p.ObjectAttributes.IID,
		Title:            p.ObjectAttributes.Title,
		State:            state,
		WebUrl:           coalesce(p.ObjectAttributes.URL, p.Project.WebURL),
		SourceBranch:     strToTextOrNull(p.ObjectAttributes.SourceBranch),
		AuthorUsername:   strToTextOrNull(p.User.Username),
		AuthorAvatarUrl:  strToTextOrNull(p.User.AvatarURL),
		MergedAt:         parseGitLabTime(p.ObjectAttributes.MergedAt),
		ClosedAt:         parseGitLabTime(p.ObjectAttributes.ClosedAt),
		MrCreatedAt:      parseGitLabTimeRequired(p.ObjectAttributes.CreatedAt),
		MrUpdatedAt:      parseGitLabTimeRequired(p.ObjectAttributes.UpdatedAt),
		HeadSha:          p.ObjectAttributes.LastCommit.ID,
		MergeStatus:      mergeStatus,
		ClearMergeStatus: pgtype.Bool{Bool: clearMergeStatus, Valid: true},
		Additions:        0,
		Deletions:        0,
		ChangedFiles:     0,
	})
	if err != nil {
		slog.Warn("gitlab: upsert mr failed", "err", err)
		return
	}

	// Drain any pipeline events that arrived before this MR row was mirrored.
	h.replayPendingPipelinesForMR(ctx, mr)

	workspaceID := uuidToString(wsID)
	resp := gitlabMRRowToResponse(gitlabMRToListRow(mr))

	linkedIssueIDs := h.autoLinkGitLabMR(ctx, wsID, workspaceID, mr, p, state)

	h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
		"pull_request":     resp,
		"linked_issue_ids": linkedIssueIDs,
	})
}

// autoLinkGitLabMR scans the MR title/description/branch for issue identifiers,
// links them, and re-evaluates the auto-advance gate. Reuses the provider-
// agnostic identifier helpers from github.go. Returns the linked issue ids.
func (h *Handler) autoLinkGitLabMR(ctx context.Context, wsID pgtype.UUID, workspaceID string, mr db.GitlabMergeRequest, p glMergeRequestPayload, state string) []string {
	linkedIssueIDs := make([]string, 0)
	if !h.workspaceAutoLinkMRsEnabled(ctx, wsID) {
		return linkedIssueIDs
	}
	idents := extractIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.Description, p.ObjectAttributes.SourceBranch)
	closingIdents := map[string]struct{}{}
	for _, c := range extractClosingIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.Description) {
		closingIdents[c] = struct{}{}
	}
	qualifyingIdents := map[string]struct{}{}
	for _, id := range extractIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.SourceBranch) {
		qualifyingIdents[id] = struct{}{}
	}
	for c := range closingIdents {
		qualifyingIdents[c] = struct{}{}
	}
	preserveCloseIntent := p.ObjectAttributes.Action != "close" && p.ObjectAttributes.Action != "merge" && (state == "merged" || state == "closed")
	prefix := h.getIssuePrefix(ctx, wsID)
	reevalIssues := make([]db.Issue, 0, len(idents))
	for _, id := range idents {
		issue, ok := h.lookupIssueByIdentifier(ctx, wsID, prefix, id)
		if !ok {
			continue
		}
		_, declared := closingIdents[id]
		closeIntent := declared && !preserveCloseIntent
		_, qualifies := qualifyingIdents[id]
		referenceOnly := !qualifies
		if err := h.Queries.LinkIssueToMergeRequest(ctx, db.LinkIssueToMergeRequestParams{
			IssueID:             issue.ID,
			MergeRequestID:      mr.ID,
			CloseIntent:         closeIntent,
			ReferenceOnly:       referenceOnly,
			PreserveCloseIntent: preserveCloseIntent,
			LinkedByType:        strToText("system"),
			LinkedByID:          pgtype.UUID{},
		}); err != nil {
			slog.Warn("gitlab: link failed", "err", err)
			continue
		}
		linkedIssueIDs = append(linkedIssueIDs, uuidToString(issue.ID))
		reevalIssues = append(reevalIssues, issue)
	}

	if state == "merged" || state == "closed" {
		for _, issue := range reevalIssues {
			if issue.Status == "done" || issue.Status == "cancelled" {
				continue
			}
			counts, err := h.Queries.GetIssueMergeRequestCloseAggregate(ctx, issue.ID)
			if err != nil {
				slog.Warn("gitlab: count linked mr states failed", "err", err, "issue_id", uuidToString(issue.ID))
				continue
			}
			if counts.OpenCount == 0 && counts.MergedWithCloseIntentCount > 0 {
				h.advanceIssueToDone(ctx, issue, workspaceID)
			}
		}
	}
	return linkedIssueIDs
}

// ── Pipeline event ──────────────────────────────────────────────────────────

type glPipelinePayload struct {
	ObjectAttributes struct {
		ID         int64  `json:"id"`
		SHA        string `json:"sha"`
		Status     string `json:"status"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at"`
	} `json:"object_attributes"`
	MergeRequest struct {
		IID int32 `json:"iid"`
	} `json:"merge_request"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
}

// handlePipelineEvent records CI status for the MR the pipeline references. Like
// GitHub check suites, a pipeline that references an MR not yet mirrored is
// stashed and replayed when the merge_request webhook upserts the MR row.
func (h *Handler) handlePipelineEvent(ctx context.Context, conn db.GitlabConnection, body []byte) {
	var p glPipelinePayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad pipeline payload", "err", err)
		return
	}
	if p.Project.ID == 0 || p.ObjectAttributes.ID == 0 {
		return
	}
	if p.MergeRequest.IID == 0 {
		// Branch pipeline with no associated MR — nothing to attribute.
		slog.Info("gitlab: pipeline has no associated MR", "pipeline_id", p.ObjectAttributes.ID)
		return
	}
	wsID := conn.WorkspaceID
	updatedAt := parseGitLabTimeRequired(coalesce(p.ObjectAttributes.FinishedAt, p.ObjectAttributes.CreatedAt))

	mr, err := h.Queries.GetGitLabMergeRequest(ctx, db.GetGitLabMergeRequestParams{
		WorkspaceID: wsID,
		ProjectID:   p.Project.ID,
		MrIid:       p.MergeRequest.IID,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("gitlab: lookup mr for pipeline failed", "err", err)
			return
		}
		// Out-of-order: pipeline arrived before the MR was mirrored. Stash it.
		if err := h.Queries.UpsertPendingPipeline(ctx, db.UpsertPendingPipelineParams{
			WorkspaceID:       wsID,
			ConnectionID:      conn.ID,
			ProjectID:         p.Project.ID,
			MrIid:             p.MergeRequest.IID,
			PipelineID:        p.ObjectAttributes.ID,
			HeadSha:           p.ObjectAttributes.SHA,
			Status:            p.ObjectAttributes.Status,
			PipelineUpdatedAt: updatedAt,
		}); err != nil {
			slog.Warn("gitlab: stash pending pipeline failed", "err", err, "pipeline_id", p.ObjectAttributes.ID)
		}
		return
	}

	if err := h.Queries.UpsertGitLabPipeline(ctx, db.UpsertGitLabPipelineParams{
		MrID:       mr.ID,
		PipelineID: p.ObjectAttributes.ID,
		HeadSha:    p.ObjectAttributes.SHA,
		Status:     p.ObjectAttributes.Status,
		UpdatedAt:  updatedAt,
	}); err != nil {
		slog.Warn("gitlab: upsert pipeline failed", "err", err, "pipeline_id", p.ObjectAttributes.ID)
		return
	}

	issues, _ := h.Queries.ListIssueIDsForMergeRequest(ctx, mr.ID)
	linked := make([]string, 0, len(issues))
	for _, id := range issues {
		linked = append(linked, uuidToString(id))
	}
	h.publish(protocol.EventPullRequestUpdated, uuidToString(wsID), "system", "", map[string]any{
		"linked_issue_ids": linked,
	})
}

// replayPendingPipelinesForMR drains the pipeline stash for one MR and re-applies
// each event through the normal upsert path. Safe to call on every MR upsert.
func (h *Handler) replayPendingPipelinesForMR(ctx context.Context, mr db.GitlabMergeRequest) {
	pending, err := h.Queries.DrainPendingPipelinesForMR(ctx, db.DrainPendingPipelinesForMRParams{
		WorkspaceID: mr.WorkspaceID,
		ProjectID:   mr.ProjectID,
		MrIid:       mr.MrIid,
	})
	if err != nil {
		slog.Warn("gitlab: drain pending pipelines failed", "err", err, "mr_id", uuidToString(mr.ID))
		return
	}
	for _, row := range pending {
		if err := h.Queries.UpsertGitLabPipeline(ctx, db.UpsertGitLabPipelineParams{
			MrID:       mr.ID,
			PipelineID: row.PipelineID,
			HeadSha:    row.HeadSha,
			Status:     row.Status,
			UpdatedAt:  row.PipelineUpdatedAt,
		}); err != nil {
			slog.Warn("gitlab: replay pending pipeline failed", "err", err, "mr_id", uuidToString(mr.ID), "pipeline_id", row.PipelineID)
		}
	}
}

// ── Derivation helpers ──────────────────────────────────────────────────────

func deriveMRState(state string, draft bool) string {
	switch state {
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	case "locked", "opened":
		if draft {
			return "draft"
		}
		return "open"
	default:
		if draft {
			return "draft"
		}
		return "open"
	}
}

// deriveMRMergeStatus mirrors derivePRMergeableState: state-changing actions
// (open/reopen or a new push carrying oldrev) blank the prior merge verdict
// because GitLab recomputes it asynchronously; a concrete status is written;
// an empty status on a metadata event preserves the existing column.
func deriveMRMergeStatus(action, mergeStatus string, pushed bool) (pgtype.Text, bool) {
	if action == "open" || action == "reopen" || pushed {
		return pgtype.Text{}, true
	}
	if mergeStatus == "" {
		return pgtype.Text{}, false
	}
	return pgtype.Text{String: mergeStatus, Valid: true}, false
}

// parseGitLabTime parses the timestamp formats GitLab emits: webhook payloads
// use "2006-01-02 15:04:05 MST" / "… -0700", the API uses RFC3339. Returns an
// invalid timestamp on empty/unparseable input.
func parseGitLabTime(s string) pgtype.Timestamptz {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Timestamptz{}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	return pgtype.Timestamptz{}
}

func parseGitLabTimeRequired(s string) pgtype.Timestamptz {
	t := parseGitLabTime(s)
	if !t.Valid {
		return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	return t
}

// splitPathWithNamespace splits "group/subgroup/project" into the namespace
// ("group/subgroup") and project path ("project"). Falls back to projectName
// when the path is missing.
func splitPathWithNamespace(pathWithNamespace, projectName string) (namespace, project string) {
	pathWithNamespace = strings.Trim(strings.TrimSpace(pathWithNamespace), "/")
	if pathWithNamespace == "" {
		return "", projectName
	}
	idx := strings.LastIndex(pathWithNamespace, "/")
	if idx < 0 {
		return "", pathWithNamespace
	}
	return pathWithNamespace[:idx], pathWithNamespace[idx+1:]
}

// baseURLFromWebURL returns "scheme://host" from a GitLab project web_url,
// or "" if it can't be parsed. This is the instance base URL used to scope the
// connection lookup on webhook delivery.
func baseURLFromWebURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// hostFromURL returns the lowercased host of a base URL (no scheme), used to
// match repos-registry entries against a connection's instance.
func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(raw)
}

func strToTextOrNull(s string) pgtype.Text {
	return ptrToText(strPtrOrNil(s))
}

// gitlabMRToListRow adapts a bare GitlabMergeRequest (from an upsert) into the
// ListMergeRequestsByIssueRow shape used by gitlabMRRowToResponse. A bare row
// has no aggregated pipeline counts — broadcasts of a single MR default to 0 and
// the frontend re-queries the list for fresh counts.
func gitlabMRToListRow(m db.GitlabMergeRequest) db.ListMergeRequestsByIssueRow {
	return db.ListMergeRequestsByIssueRow{
		ID:              m.ID,
		WorkspaceID:     m.WorkspaceID,
		ConnectionID:    m.ConnectionID,
		ProjectID:       m.ProjectID,
		NamespacePath:   m.NamespacePath,
		ProjectPath:     m.ProjectPath,
		MrIid:           m.MrIid,
		Title:           m.Title,
		State:           m.State,
		WebUrl:          m.WebUrl,
		SourceBranch:    m.SourceBranch,
		AuthorUsername:  m.AuthorUsername,
		AuthorAvatarUrl: m.AuthorAvatarUrl,
		MergedAt:        m.MergedAt,
		ClosedAt:        m.ClosedAt,
		MrCreatedAt:     m.MrCreatedAt,
		MrUpdatedAt:     m.MrUpdatedAt,
		HeadSha:         m.HeadSha,
		MergeStatus:     m.MergeStatus,
		Additions:       m.Additions,
		Deletions:       m.Deletions,
		ChangedFiles:    m.ChangedFiles,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
		ChecksTotal:     0,
		ChecksPassed:    0,
		ChecksFailed:    0,
		ChecksPending:   0,
	}
}
