package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/issuesync"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Responses ───────────────────────────────────────────────────────────────

// IssueSyncSourceResponse is the JSON shape for a sync-source row.
type IssueSyncSourceResponse struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"project_id"`
	WorkspaceID    string          `json:"workspace_id"`
	Provider       string          `json:"provider"`
	ConnectionID   string          `json:"connection_id"`
	ExternalRef    json.RawMessage `json:"external_ref"`
	ExternalKey    string          `json:"external_key"`
	StatusMapping  json.RawMessage `json:"status_mapping"`
	SyncEnabled    bool            `json:"sync_enabled"`
	PushDefault    bool            `json:"push_default"`
	BackfillStatus string          `json:"backfill_status"`
	BackfillCursor *string         `json:"backfill_cursor"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

func issueSyncSourceToResponse(s db.IssueSyncSource) IssueSyncSourceResponse {
	ref := json.RawMessage(s.ExternalRef)
	if len(ref) == 0 {
		ref = json.RawMessage("{}")
	}
	mapping := json.RawMessage(s.StatusMapping)
	if len(mapping) == 0 {
		mapping = json.RawMessage("{}")
	}
	return IssueSyncSourceResponse{
		ID:             uuidToString(s.ID),
		ProjectID:      uuidToString(s.ProjectID),
		WorkspaceID:    uuidToString(s.WorkspaceID),
		Provider:       s.Provider,
		ConnectionID:   uuidToString(s.ConnectionID),
		ExternalRef:    ref,
		ExternalKey:    s.ExternalKey,
		StatusMapping:  mapping,
		SyncEnabled:    s.SyncEnabled,
		PushDefault:    s.PushDefault,
		BackfillStatus: s.BackfillStatus,
		BackfillCursor: textToPtr(s.BackfillCursor),
		CreatedAt:      timestampToString(s.CreatedAt),
		UpdatedAt:      timestampToString(s.UpdatedAt),
	}
}

// ── Provider connection validation ──────────────────────────────────────────

// validateSyncConnection confirms the connection belongs to the workspace for
// the given provider. GitHub uses github_installation; GitLab/Jira use their
// own connection tables. Returns nil when valid.
func (h *Handler) validateSyncConnection(ctx context.Context, wsUUID pgtype.UUID, provider string, connectionID pgtype.UUID) error {
	if !connectionID.Valid {
		return errors.New("connection_id is required")
	}
	switch provider {
	case issuesync.ProviderGitHub:
		row, err := h.Queries.GetGitHubInstallationByID(ctx, connectionID)
		if err != nil {
			return err
		}
		if row.WorkspaceID != wsUUID {
			return errors.New("connection belongs to a different workspace")
		}
		return nil
	case issuesync.ProviderGitLab:
		row, err := h.Queries.GetGitLabConnectionByID(ctx, connectionID)
		if err != nil {
			return err
		}
		if row.WorkspaceID != wsUUID {
			return errors.New("connection belongs to a different workspace")
		}
		return nil
	case issuesync.ProviderJira:
		row, err := h.Queries.GetJiraConnectionByID(ctx, connectionID)
		if err != nil {
			return err
		}
		if row.WorkspaceID != wsUUID {
			return errors.New("connection belongs to a different workspace")
		}
		return nil
	default:
		return fmt.Errorf("unknown provider %q", provider)
	}
}

// normalizeSyncExternalRef validates and normalizes the provider-specific
// external_ref payload, returning the canonical external_ref bytes and the
// external_key (the lookup key used for webhook routing).
func normalizeSyncExternalRef(provider string, ref json.RawMessage) (normalized []byte, key string, err error) {
	if len(ref) == 0 {
		return nil, "", errors.New("external_ref is required")
	}
	switch provider {
	case issuesync.ProviderGitHub:
		var gr issuesync.GitHubRef
		if err := json.Unmarshal(ref, &gr); err != nil {
			return nil, "", fmt.Errorf("invalid github external_ref: %w", err)
		}
		gr.Owner = strings.TrimSpace(gr.Owner)
		gr.Name = strings.TrimSpace(gr.Name)
		if gr.Owner == "" || gr.Name == "" {
			return nil, "", errors.New("github external_ref requires owner and name")
		}
		key = issuesync.GitHubContainerKey(gr.Owner, gr.Name)
		out, mErr := json.Marshal(gr)
		return out, key, mErr
	case issuesync.ProviderGitLab:
		var gr struct {
			ProjectID         int    `json:"project_id"`
			PathWithNamespace string `json:"path_with_namespace"`
		}
		if err := json.Unmarshal(ref, &gr); err != nil {
			return nil, "", fmt.Errorf("invalid gitlab external_ref: %w", err)
		}
		gr.PathWithNamespace = strings.TrimSpace(gr.PathWithNamespace)
		if gr.PathWithNamespace == "" || gr.ProjectID == 0 {
			return nil, "", errors.New("gitlab external_ref requires project_id and path_with_namespace")
		}
		key = strings.ToLower(gr.PathWithNamespace)
		out, mErr := json.Marshal(gr)
		return out, key, mErr
	case issuesync.ProviderJira:
		var jr struct {
			ProjectID string `json:"project_id"`
			Key       string `json:"key"`
		}
		if err := json.Unmarshal(ref, &jr); err != nil {
			return nil, "", fmt.Errorf("invalid jira external_ref: %w", err)
		}
		jr.Key = strings.ToUpper(strings.TrimSpace(jr.Key))
		jr.ProjectID = strings.TrimSpace(jr.ProjectID)
		if jr.Key == "" || jr.ProjectID == "" {
			return nil, "", errors.New("jira external_ref requires project_id and key")
		}
		key = jr.Key
		out, mErr := json.Marshal(jr)
		return out, key, mErr
	default:
		return nil, "", fmt.Errorf("unknown provider %q", provider)
	}
}

// ── CRUD ────────────────────────────────────────────────────────────────────

// ListSyncSources returns the sync sources attached to a project.
func (h *Handler) ListSyncSources(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	sources, err := h.Queries.ListIssueSyncSourcesByProject(r.Context(), project.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sync sources")
		return
	}
	resp := make([]IssueSyncSourceResponse, len(sources))
	for i, s := range sources {
		resp[i] = issueSyncSourceToResponse(s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": resp, "total": len(resp)})
}

// CreateSyncSourceRequest is the body for POST /api/projects/{id}/sync-sources.
type CreateSyncSourceRequest struct {
	Provider      string          `json:"provider"`
	ConnectionID  string          `json:"connection_id"`
	ExternalRef   json.RawMessage `json:"external_ref"`
	StatusMapping json.RawMessage `json:"status_mapping"`
	PushDefault   *bool           `json:"push_default"`
	// SyncEnabled defaults to true when omitted.
	SyncEnabled *bool `json:"sync_enabled"`
}

// CreateSyncSource attaches a remote container to a project for bidirectional
// sync, then kicks off a backfill of its open issues in a goroutine.
func (h *Handler) CreateSyncSource(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userIDStr, ok := requireUserID(w, r)
	if !ok {
		return
	}
	userID, err := util.ParseUUID(userIDStr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve user id")
		return
	}
	var req CreateSyncSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	if req.Provider != issuesync.ProviderGitHub &&
		req.Provider != issuesync.ProviderGitLab &&
		req.Provider != issuesync.ProviderJira {
		writeError(w, http.StatusBadRequest, "provider must be github, gitlab, or jira")
		return
	}
	connUUID, ok := parseUUIDOrBadRequest(w, req.ConnectionID, "connection_id")
	if !ok {
		return
	}
	if err := h.validateSyncConnection(r.Context(), project.WorkspaceID, req.Provider, connUUID); err != nil {
		writeError(w, http.StatusBadRequest, "connection not found in this workspace for the given provider")
		return
	}
	refBytes, externalKey, err := normalizeSyncExternalRef(req.Provider, req.ExternalRef)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mapping := req.StatusMapping
	if len(mapping) == 0 {
		mapping = json.RawMessage("{}")
	}
	pushDefault := false
	if req.PushDefault != nil {
		pushDefault = *req.PushDefault
	}
	syncEnabled := true
	if req.SyncEnabled != nil {
		syncEnabled = *req.SyncEnabled
	}

	// Enforce the single push_default-per-project invariant before insert.
	if pushDefault {
		if err := h.Queries.ClearIssueSyncSourcePushDefault(r.Context(), project.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to reset push default")
			return
		}
	}

	source, err := h.Queries.CreateIssueSyncSource(r.Context(), db.CreateIssueSyncSourceParams{
		WorkspaceID:   project.WorkspaceID,
		ProjectID:     project.ID,
		Provider:      req.Provider,
		ConnectionID:  connUUID,
		ExternalRef:   refBytes,
		ExternalKey:   externalKey,
		StatusMapping: mapping,
		PushDefault:   pushDefault,
		CreatedBy:     userID,
	})
	if err != nil {
		slog.Warn("issue_sync: create source failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create sync source")
		return
	}

	// Backfill is best-effort: the worker reads open remote issues and mirrors
	// them locally. Run async so the request returns immediately; failures land
	// in backfill_status on the source row.
	if syncEnabled && h.IssueSync != nil {
		go h.IssueSync.RunBackfill(context.Background(), source)
	}

	h.publish(protocol.EventIssueSyncSourceCreated, uuidToString(project.WorkspaceID), "member", userIDStr, map[string]any{
		"source_id":  uuidToString(source.ID),
		"project_id": uuidToString(project.ID),
	})
	writeJSON(w, http.StatusCreated, issueSyncSourceToResponse(source))
}

// UpdateSyncSourceRequest is the body for PUT sync-sources/{sourceId}.
type UpdateSyncSourceRequest struct {
	StatusMapping json.RawMessage `json:"status_mapping"`
	PushDefault   *bool           `json:"push_default"`
	SyncEnabled   *bool           `json:"sync_enabled"`
}

// UpdateSyncSource changes status mapping / push default / enabled flag.
func (h *Handler) UpdateSyncSource(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceId"), "source id")
	if !ok {
		return
	}
	var req UpdateSyncSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	existing, err := h.Queries.GetIssueSyncSourceInWorkspace(r.Context(), db.GetIssueSyncSourceInWorkspaceParams{
		ID:          sourceUUID,
		WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "sync source not found")
		return
	}
	if existing.ProjectID != project.ID {
		writeError(w, http.StatusNotFound, "sync source not found")
		return
	}

	params := db.UpdateIssueSyncSourceParams{ID: sourceUUID}
	set := false
	if req.StatusMapping != nil {
		params.StatusMapping = req.StatusMapping
		set = true
	}
	if req.SyncEnabled != nil {
		params.SyncEnabled = pgtype.Bool{Bool: *req.SyncEnabled, Valid: true}
		set = true
	}
	if req.PushDefault != nil {
		// Clear the existing default before promoting this source, preserving
		// the at-most-one-per-project partial unique constraint.
		if *req.PushDefault {
			if err := h.Queries.ClearIssueSyncSourcePushDefault(r.Context(), project.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to reset push default")
				return
			}
		}
		params.PushDefault = pgtype.Bool{Bool: *req.PushDefault, Valid: true}
		set = true
	}
	if !set {
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	source, err := h.Queries.UpdateIssueSyncSource(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update sync source")
		return
	}
	h.publish(protocol.EventIssueSyncSourceUpdated, uuidToString(project.WorkspaceID), "system", "", map[string]any{
		"source_id":  uuidToString(source.ID),
		"project_id": uuidToString(project.ID),
	})
	writeJSON(w, http.StatusOK, issueSyncSourceToResponse(source))
}

// DeleteSyncSource removes a sync source. Linked issues stay (they are real
// Multica issues); only the external_issue_link rows and the source are dropped.
func (h *Handler) DeleteSyncSource(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceId"), "source id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteIssueSyncSource(r.Context(), db.DeleteIssueSyncSourceParams{
		ID:          sourceUUID,
		WorkspaceID: project.WorkspaceID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete sync source")
		return
	}
	h.publish(protocol.EventIssueSyncSourceDeleted, uuidToString(project.WorkspaceID), "system", "", map[string]any{
		"source_id":  uuidToString(sourceUUID),
		"project_id": uuidToString(project.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote container listing (picker data) ──────────────────────────────────

// ListRemoteContainers lists the repos/projects reachable through a workspace
// connection, for the attach picker. Provider is required; connection_id is
// optional (defaults to the workspace's first connection of that provider).
func (h *Handler) ListRemoteContainers(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	provider := strings.TrimSpace(chi.URLParam(r, "provider"))
	if h.IssueSync == nil {
		writeError(w, http.StatusServiceUnavailable, "issue sync is not configured")
		return
	}
	p := h.IssueSync.Provider(provider)
	if p == nil {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	connectionID := strings.TrimSpace(r.URL.Query().Get("connection_id"))
	if connectionID == "" {
		// Default to the workspace's most recent connection of this provider.
		cid, err := h.defaultConnectionForProvider(r.Context(), wsUUID, provider)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"containers": []any{}, "connection_id": ""})
			return
		}
		connectionID = cid
	}
	if err := h.validateSyncConnection(r.Context(), wsUUID, provider, mustParseUUIDLoose(connectionID)); err != nil {
		writeError(w, http.StatusBadRequest, "connection not found in this workspace")
		return
	}
	containers, err := p.ListContainers(r.Context(), connectionID)
	if err != nil {
		slog.Warn("issue_sync: list containers failed", "provider", provider, "error", err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to list %s projects: %s", provider, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": containers, "connection_id": connectionID})
}

func (h *Handler) defaultConnectionForProvider(ctx context.Context, wsUUID pgtype.UUID, provider string) (string, error) {
	switch provider {
	case issuesync.ProviderGitHub:
		rows, err := h.Queries.ListGitHubInstallationsByWorkspace(ctx, wsUUID)
		if err != nil || len(rows) == 0 {
			return "", errors.New("no connection")
		}
		return uuidToString(rows[0].ID), nil
	case issuesync.ProviderGitLab:
		rows, err := h.Queries.ListGitLabConnectionsByWorkspace(ctx, wsUUID)
		if err != nil || len(rows) == 0 {
			return "", errors.New("no connection")
		}
		return uuidToString(rows[0].ID), nil
	case issuesync.ProviderJira:
		rows, err := h.Queries.ListJiraConnectionsByWorkspace(ctx, wsUUID)
		if err != nil || len(rows) == 0 {
			return "", errors.New("no connection")
		}
		return uuidToString(rows[0].ID), nil
	}
	return "", errors.New("no connection")
}

// mustParseUUIDLoose parses a UUID string into pgtype.UUID, returning the zero
// (invalid) UUID on failure. A bad value fails the subsequent
// validateSyncConnection lookup, so this never panics.
func mustParseUUIDLoose(s string) pgtype.UUID {
	u, err := parseUUIDLoose(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return u
}

// ── Engine payload injection helper ──────────────────────────────────────────

// BuildIssueResponseForSync builds an IssueResponse (with the workspace's issue
// prefix) for engine-driven writes. Injected into the engine as IssuePayload so
// the activity/notification listeners — which type-assert payload["issue"] to
// IssueResponse — keep working for sync-driven creates/updates.
func (h *Handler) BuildIssueResponseForSync(ctx context.Context, issue db.Issue) IssueResponse {
	return issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
}

// ── Inbound webhook normalization ───────────────────────────────────────────

// applyRemoteIssueWebhook routes a normalized inbound event to every sync
// source bound to the remote container (provider + external_key). Called by the
// provider webhook handlers after they normalize their payloads. Errors are
// logged, not returned to the webhook caller — a 202 still goes back so the
// provider does not retry the whole delivery on a transient DB error.
func (h *Handler) applyRemoteIssueWebhook(ctx context.Context, provider, externalKey string, evt issuesync.IssueEvent) {
	if h.IssueSync == nil || externalKey == "" {
		return
	}
	sources, err := h.Queries.ListIssueSyncSourcesByExternalKey(ctx, db.ListIssueSyncSourcesByExternalKeyParams{
		Provider:    provider,
		ExternalKey: externalKey,
	})
	if err != nil {
		slog.Warn("issue_sync: webhook source lookup failed", "provider", provider, "key", externalKey, "error", err)
		return
	}
	for _, src := range sources {
		if err := h.IssueSync.ApplyRemote(ctx, src, evt); err != nil {
			slog.Warn("issue_sync: apply remote event failed",
				"provider", provider, "key", externalKey,
				"source_id", util.UUIDToString(src.ID), "error", err)
		}
	}
}

// ── GitHub webhook issue/comment handlers ───────────────────────────────────

// ghIssueWebhookPayload is the shape GitHub delivers for `issues` and
// `issue_comment` events. The repository block carries the owner/name used to
// route the event to the matching sync source.
type ghIssueWebhookPayload struct {
	Action     string          `json:"action"`
	Issue      json.RawMessage `json:"issue"`
	Comment    json.RawMessage `json:"comment"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handleGitHubIssueEvent normalizes an `issues` webhook and applies it through
// the sync engine. The `deleted` action has no issue body to mirror and is
// ignored (issue deletion does not propagate — Multica issues are not deleted
// by a remote close).
func (h *Handler) handleGitHubIssueEvent(ctx context.Context, body []byte) {
	var p ghIssueWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("github: bad issues webhook payload", "error", err)
		return
	}
	switch p.Action {
	case "deleted", "transferred":
		return
	}
	ext, ok := issuesync.GitHubIssueToExternal(p.Issue)
	if !ok {
		return
	}
	key := githubRepoKeyFromPayload(p.Repository)
	if key == "" {
		key = repoKeyFromIssuePayload(p.Issue)
	}
	h.applyRemoteIssueWebhook(ctx, issuesync.ProviderGitHub, key, issuesync.IssueEvent{Kind: "issue", Issue: ext})
}

// handleGitHubIssueCommentEvent normalizes an `issue_comment` webhook.
func (h *Handler) handleGitHubIssueCommentEvent(ctx context.Context, body []byte) {
	var p ghIssueWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("github: bad issue_comment webhook payload", "error", err)
		return
	}
	if p.Action == "deleted" {
		return
	}
	ext, ok := issuesync.GitHubIssueToExternal(p.Issue)
	if !ok {
		return
	}
	comment, ok := issuesync.GitHubCommentToExternal(p.Comment)
	if !ok {
		return
	}
	// Echo-suppression layer 2a: drop events caused by the GitHub App itself
	// (the bot user that CreateComment/UpdateComment write as). The App's
	// numeric id is not available here without an API call; the content-hash
	// check in the engine catches the echo, so we only short-circuit the
	// common "bot authored" case via the login when present.
	if comment.Author != nil && isGitHubAppLogin(comment.Author.Login) {
		return
	}
	key := githubRepoKeyFromPayload(p.Repository)
	if key == "" {
		key = repoKeyFromIssuePayload(p.Issue)
	}
	h.applyRemoteIssueWebhook(ctx, issuesync.ProviderGitHub, key, issuesync.IssueEvent{
		Kind: "comment", Issue: ext, Comment: &comment,
	})
}

// githubRepoKeyFromPayload extracts the lowercased owner/name from the
// top-level repository block of a GitHub webhook delivery. Returns "" when
// the block is absent or malformed.
func githubRepoKeyFromPayload(repo *struct {
	FullName string `json:"full_name"`
}) string {
	if repo == nil {
		return ""
	}
	fn := strings.TrimSpace(repo.FullName)
	if fn == "" {
		return ""
	}
	parts := strings.SplitN(fn, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(fn)
}

// repoKeyFromIssuePayload is the fallback path: extract owner/name from the
// issue's repository_url / html_url when the top-level repository block is
// absent (older payloads or a trimmed delivery).
func repoKeyFromIssuePayload(raw json.RawMessage) string {
	var probe struct {
		RepositoryURL string `json:"repository_url"`
		HTMLURL       string `json:"html_url"`
	}
	_ = json.Unmarshal(raw, &probe)
	for _, u := range []string{probe.RepositoryURL, probe.HTMLURL} {
		if key := githubRepoKeyFromURL(u); key != "" {
			return key
		}
	}
	return ""
}

// githubRepoKeyFromURL extracts "owner/name" (lowercased) from a GitHub URL.
func githubRepoKeyFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// https://api.github.com/repos/owner/name  OR  https://github.com/owner/name
	for _, marker := range []string{"/repos/", "github.com/"} {
		if idx := strings.Index(raw, marker); idx >= 0 {
			rest := strings.TrimRight(raw[idx+len(marker):], "/")
			rest = strings.TrimSuffix(rest, ".git")
			// owner/name may have a trailing path component (/issues/123);
			// keep only the first two segments.
			parts := strings.SplitN(rest, "/", 3)
			if len(parts) >= 2 {
				return strings.ToLower(parts[0] + "/" + parts[1])
			}
		}
	}
	return ""
}

// isGitHubAppLogin returns true for the bot-login shape GitHub uses for Apps
// (the app slug suffixed with "[bot]"), so comment echoes from our own
// CreateComment writes are dropped before they reach the engine.
func isGitHubAppLogin(login string) bool {
	return strings.HasSuffix(login, "[bot]")
}

// ── GitLab webhook issue/comment handlers ───────────────────────────────────

// handleGitLabIssueEvent normalizes a GitLab `issue` hook and applies it
// through the sync engine. The external_key is the lowercased
// path_with_namespace of the delivering project, matching what
// normalizeSyncExternalRef stores. Events whose actor is the connection's own
// gitlab_user_id are dropped (echo suppression layer 2b) — our own pushes
// write as that user.
func (h *Handler) handleGitLabIssueEvent(ctx context.Context, conn db.GitlabConnection, body []byte) {
	var p struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ObjectAttributes json.RawMessage `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad issue webhook payload", "error", err)
		return
	}
	if p.User.ID != 0 && p.User.ID == conn.GitlabUserID {
		return // echo: we authored this change
	}
	ext, ok := issuesync.GitLabIssueToExternal(body)
	if !ok {
		return
	}
	key := issuesync.GitLabContainerKey(p.Project.PathWithNamespace)
	if key == "" {
		return
	}
	h.applyRemoteIssueWebhook(ctx, issuesync.ProviderGitLab, key, issuesync.IssueEvent{Kind: "issue", Issue: ext})
}

// handleGitLabNoteEvent normalizes a GitLab `note` hook. Only notes on issues
// are mirrored (noteable_type == "Issue"); MR/snippet comments are owned by
// other integrations. The comment and its parent issue are extracted from the
// envelope and routed through the engine the same way GitHub comments are.
func (h *Handler) handleGitLabNoteEvent(ctx context.Context, conn db.GitlabConnection, body []byte) {
	var p struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ObjectAttributes struct {
			NoteableType string `json:"noteable_type"`
		} `json:"object_attributes"`
		Issue        json.RawMessage `json:"issue"`
		MergeRequest json.RawMessage `json:"merge_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad note webhook payload", "error", err)
		return
	}
	if p.User.ID != 0 && p.User.ID == conn.GitlabUserID {
		return // echo: we authored this note
	}
	if p.ObjectAttributes.NoteableType != "Issue" {
		return // skip MR / snippet / commit notes
	}
	comment, ok := issuesync.GitLabCommentToExternal(body)
	if !ok {
		return
	}
	issue, ok := issuesync.GitLabIssueToExternal(p.Issue)
	if !ok {
		return
	}
	key := issuesync.GitLabContainerKey(p.Project.PathWithNamespace)
	if key == "" {
		return
	}
	h.applyRemoteIssueWebhook(ctx, issuesync.ProviderGitLab, key, issuesync.IssueEvent{
		Kind: "comment", Issue: issue, Comment: &comment,
	})
}

// Jira connection handlers (ListJiraConnections, DeleteJiraConnection,
// JiraConnectionResponse, jiraConnectionToResponse, isJiraConfigured) live in
// jira.go alongside the OAuth 3LO flow — they need the JiraConnection model
// and secretbox helpers that belong to the Jira integration (M3).
