package issuesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// jiraAPIBaseHost is the Atlassian REST origin, mutable so tests can point the
// provider at an httptest server (mirrors GitHubAPIBase). The per-cloud base is
// "<host>/ex/jira/<cloudId>/rest/api/3".
var jiraAPIBaseHost = "https://api.atlassian.com"

// jiraTokenEndpoint / jiraAccessibleResourcesURL are the OAuth/token + identity
// discovery endpoints. Mutable for tests.
var (
	jiraTokenEndpoint           = "https://auth.atlassian.com/oauth/token"
	jiraAccessibleResourcesURL  = "https://api.atlassian.com/oauth/token/accessible-resources"
	jiraMeURL                   = "https://api.atlassian.com/me"
)

func jiraAPIBase(cloudID string) string {
	return strings.TrimRight(jiraAPIBaseHost, "/") + "/ex/jira/" + cloudID + "/rest/api/3"
}

// JiraRef is the external_ref JSONB shape for provider="jira" sources.
// external_key is the project key uppercased.
type JiraRef struct {
	ProjectID string `json:"project_id"`
	Key       string `json:"key"`
}

// JiraContainerKey normalizes a Jira project key into the external_key format
// (uppercased, trimmed).
func JiraContainerKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// JiraProvider talks to the Jira Cloud REST API (v3) using an OAuth 3LO
// access token, refreshing it (and persisting the rotated refresh token) when
// it nears expiry. The connection row holds the cloudId, encrypted tokens, and
// the at-rest webhook secret.
type JiraProvider struct {
	Queries *db.Queries
	Box     *secretbox.Box
	Client  *http.Client
}

// NewJiraProvider wires the provider with the DB handle (for token persistence
// on refresh) and the Jira secretbox (for token decryption).
func NewJiraProvider(q *db.Queries, box *secretbox.Box) *JiraProvider {
	return &JiraProvider{
		Queries: q,
		Box:     box,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *JiraProvider) Name() string { return ProviderJira }

func (p *JiraProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func jiraRefFromSource(src db.IssueSyncSource) (JiraRef, error) {
	var ref JiraRef
	if err := json.Unmarshal(src.ExternalRef, &ref); err != nil {
		return ref, fmt.Errorf("bad jira external_ref: %w", err)
	}
	if ref.Key == "" || ref.ProjectID == "" {
		return ref, errors.New("jira external_ref missing project_id/key")
	}
	return ref, nil
}

// ── Token lifecycle ─────────────────────────────────────────────────────────

// jiraOAuthClientID/Secret read the same OAuth app credentials the handler's
// connect flow uses (env-owned, mirroring how the GitHub provider reads
// GITHUB_APP_ID directly).
func jiraOAuthClientID() string     { return strings.TrimSpace(os.Getenv("JIRA_OAUTH_CLIENT_ID")) }
func jiraOAuthClientSecret() string { return strings.TrimSpace(os.Getenv("JIRA_OAUTH_CLIENT_SECRET")) }

// JiraTokenResponse is the shape of both the authorization-code and the
// refresh-token responses from auth.atlassian.com/oauth/token. RefreshToken is
// ALWAYS present on a refresh — Jira Cloud rotates refresh tokens and
// invalidates the previous one, so the caller must persist the new value.
type JiraTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// PostJiraToken posts a grant payload to the Jira OAuth token endpoint. Shared
// by the handler's authorization-code exchange and the provider's refresh so
// the token mechanics live in one place.
func PostJiraToken(ctx context.Context, payload map[string]any) (JiraTokenResponse, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return JiraTokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, jiraTokenEndpoint, bytes.NewReader(raw))
	if err != nil {
		return JiraTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return JiraTokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return JiraTokenResponse{}, fmt.Errorf("jira token endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	var out JiraTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return JiraTokenResponse{}, err
	}
	if out.AccessToken == "" {
		return JiraTokenResponse{}, errors.New("jira token response missing access_token")
	}
	return out, nil
}

// loadConnection resolves the connection row for a source/connection id.
func (p *JiraProvider) loadConnection(ctx context.Context, connectionID string) (db.JiraConnection, error) {
	id, err := util.ParseUUID(connectionID)
	if err != nil {
		return db.JiraConnection{}, fmt.Errorf("jira: invalid connection id %q: %w", connectionID, err)
	}
	conn, err := p.Queries.GetJiraConnectionByID(ctx, id)
	if err != nil {
		return db.JiraConnection{}, fmt.Errorf("jira: load connection %s: %w", connectionID, err)
	}
	return conn, nil
}

// authToken returns a usable access token, refreshing (and persisting the
// ROTATED refresh token) when the cached one is within the skew window of
// expiry. When refresh fails the cached token is tried anyway — it may still be
// valid for the few seconds of skew.
func (p *JiraProvider) authToken(ctx context.Context, conn db.JiraConnection) (string, error) {
	if p.Box == nil {
		return "", errors.New("jira: secret box not configured")
	}
	if jiraTokenNeedsRefresh(conn.TokenExpiresAt) {
		if tok, err := p.refreshAndStore(ctx, conn); err == nil {
			return tok, nil
		} else {
			slog.Warn("issuesync: jira token refresh failed; trying cached token",
				"error", err, "connection_id", util.UUIDToString(conn.ID))
		}
	}
	access, err := p.Box.Open(conn.AccessTokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("jira: decrypt access token: %w", err)
	}
	return string(access), nil
}

func jiraTokenNeedsRefresh(exp pgtype.Timestamptz) bool {
	if !exp.Valid {
		return false
	}
	return time.Now().After(exp.Time.Add(-60 * time.Second))
}

// refreshAndStore exchanges the refresh token for a fresh pair and writes both
// back to the connection. CRITICAL: Jira Cloud rotates refresh tokens — the
// response's refresh_token REPLACES the one we sent; if it is not persisted the
// next refresh (and thus all future API calls) fails with an invalid_grant.
func (p *JiraProvider) refreshAndStore(ctx context.Context, conn db.JiraConnection) (string, error) {
	if p.Box == nil {
		return "", errors.New("jira: secret box not configured")
	}
	if len(conn.RefreshTokenEncrypted) == 0 {
		return "", errors.New("jira: no refresh token stored")
	}
	refresh, err := p.Box.Open(conn.RefreshTokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("jira: decrypt refresh token: %w", err)
	}
	tok, err := PostJiraToken(ctx, map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     jiraOAuthClientID(),
		"client_secret": jiraOAuthClientSecret(),
		"refresh_token": string(refresh),
	})
	if err != nil {
		return "", err
	}
	accessSealed, err := p.Box.Seal([]byte(tok.AccessToken))
	if err != nil {
		return "", err
	}
	refreshSealed, err := p.Box.Seal([]byte(tok.RefreshToken))
	if err != nil {
		return "", err
	}
	var expires pgtype.Timestamptz
	if tok.ExpiresIn > 0 {
		expires = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second), Valid: true}
	}
	// Rotating refresh token: persist the NEW refresh_token (tok.RefreshToken),
	// not the one we sent. UpdateJiraConnectionTokens writes both atomically.
	if err := p.Queries.UpdateJiraConnectionTokens(ctx, db.UpdateJiraConnectionTokensParams{
		ID:                    conn.ID,
		AccessTokenEncrypted:  accessSealed,
		RefreshTokenEncrypted: refreshSealed,
		TokenExpiresAt:        expires,
	}); err != nil {
		return "", fmt.Errorf("jira: persist refreshed tokens: %w", err)
	}
	return tok.AccessToken, nil
}

// do performs one authenticated REST call against the connection's Jira cloud
// and decodes the response into out (nil out discards the body). Non-2xx
// surfaces as an error with a body excerpt so outbox retries carry a
// diagnosable message.
func (p *JiraProvider) do(ctx context.Context, conn db.JiraConnection, method, path string, body any, out any) error {
	token, err := p.authToken(ctx, conn)
	if err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, jiraAPIBase(conn.CloudID)+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("jira %s %s: status %d: %s", method, path, resp.StatusCode, string(excerpt))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── Payload converters (shared by REST + webhook) ───────────────────────────

type jiraStatus struct {
	Name          string `json:"name"`
	StatusCategory struct {
		Key string `json:"key"`
	} `json:"statusCategory"`
}

type jiraUser struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	AvatarURL    string `json:"avatarUrl"`
}

type jiraProject struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// jiraIssueFields holds the subset of fields the sync engine consumes. Updated
// is left as a string and parsed with parseJiraTime (Jira emits several
// ISO-8601 variants).
type jiraIssueFields struct {
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description"`
	Status      *jiraStatus     `json:"status"`
	Labels      []string        `json:"labels"`
	Assignee    *jiraUser       `json:"assignee"`
	Reporter    *jiraUser       `json:"reporter"`
	Creator     *jiraUser       `json:"creator"`
	Project     *jiraProject    `json:"project"`
	Parent      *jiraIssueRef   `json:"parent"`
	Updated     string          `json:"updated"`
}

// jiraIssueRef is the minimal nested-issue reference Jira embeds for parent
// links in subtask fields.
type jiraIssueRef struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type jiraIssue struct {
	ID     string          `json:"id"`
	Key    string          `json:"key"`
	Fields json.RawMessage `json:"fields"`
}

// jiraUserToExternal maps a Jira user onto the neutral identity. Returns nil
// for absent users or users without an accountId (Jira's stable id).
func jiraUserToExternal(u *jiraUser) *ExternalUser {
	if u == nil || u.AccountID == "" {
		return nil
	}
	login := u.EmailAddress
	if login == "" {
		login = u.AccountID
	}
	return &ExternalUser{
		AccountID:   u.AccountID,
		Login:       login,
		DisplayName: u.DisplayName,
		Email:       u.EmailAddress,
		AvatarURL:   u.AvatarURL,
	}
}

// parseJiraTime parses the ISO-8601 variants Jira emits across REST + webhook
// payloads, returning the zero time when nothing matches.
func parseJiraTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05.000Z0700",
		"2006-01-02T15:04:05-0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// JiraIssueToExternal converts a Jira issue payload (REST search result or
// webhook issue block) into the neutral shape. ok=false when the payload lacks
// an id. State carries the status CATEGORY key ("new"/"indeterminate"/"done"),
// which is what mapping.go's MapInboundStatus keys on — concrete status names
// vary per workflow, but categories are stable.
func JiraIssueToExternal(raw json.RawMessage) (ExternalIssue, bool) {
	var j jiraIssue
	if err := json.Unmarshal(raw, &j); err != nil || j.ID == "" {
		return ExternalIssue{}, false
	}
	var f jiraIssueFields
	if len(j.Fields) > 0 {
		_ = json.Unmarshal(j.Fields, &f)
	}
	author := f.Reporter
	if author == nil {
		author = f.Creator
	}
	ext := ExternalIssue{
		ID:          j.ID,
		Key:         j.Key,
		Title:       f.Summary,
		Description: ADFToMarkdown(f.Description),
		State:       strings.ToLower(f.Status.StatusCategory.Key),
		Labels:      f.Labels,
		Assignee:    jiraUserToExternal(f.Assignee),
		Author:      jiraUserToExternal(author),
		UpdatedAt:   parseJiraTime(f.Updated),
	}
	if f.Parent != nil && f.Parent.ID != "" {
		ext.ParentExternalID = f.Parent.ID
	}
	return ext, true
}

type jiraComment struct {
	ID           string          `json:"id"`
	Body         json.RawMessage `json:"body"`
	Author       *jiraUser       `json:"author"`
	UpdateAuthor *jiraUser       `json:"updateAuthor"`
	Updated      string          `json:"updated"`
}

// JiraCommentToExternal converts a Jira comment payload (REST or webhook).
// ok=false when the payload lacks an id.
func JiraCommentToExternal(raw json.RawMessage) (ExternalComment, bool) {
	var c jiraComment
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" {
		return ExternalComment{}, false
	}
	author := c.UpdateAuthor
	if author == nil {
		author = c.Author
	}
	return ExternalComment{
		ID:        c.ID,
		Body:      ADFToMarkdown(c.Body),
		Author:    jiraUserToExternal(author),
		UpdatedAt: parseJiraTime(c.Updated),
	}, true
}

// ── Provider interface ──────────────────────────────────────────────────────

// ListContainers lists the Jira projects visible to the connection, for the
// attach picker. GET /rest/api/3/project returns a flat array.
func (p *JiraProvider) ListContainers(ctx context.Context, connectionID string) ([]Container, error) {
	conn, err := p.loadConnection(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	var projects []struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if err := p.do(ctx, conn, http.MethodGet, "/project", nil, &projects); err != nil {
		return nil, err
	}
	containers := make([]Container, 0, len(projects))
	for _, pr := range projects {
		key := JiraContainerKey(pr.Key)
		if key == "" || pr.ID == "" {
			continue
		}
		containers = append(containers, Container{
			Key:  key,
			Name: pr.Name,
			Ref: map[string]any{
				"project_id": pr.ID,
				"key":        key,
			},
		})
	}
	return containers, nil
}

// ListIssues pages through a project's non-terminal issues for backfill. The
// cursor is the next startAt offset (as a string); "" starts at 0 and is
// returned when the page is the last.
func (p *JiraProvider) ListIssues(ctx context.Context, src db.IssueSyncSource, cursor string) ([]ExternalIssue, string, error) {
	ref, err := jiraRefFromSource(src)
	if err != nil {
		return nil, "", err
	}
	conn, err := p.loadConnection(ctx, src.ConnectionID.String())
	if err != nil {
		return nil, "", err
	}
	// Jira deprecated GET /rest/api/3/search (returns 410, CHANGE-2046).
	// The replacement is POST /rest/api/3/search/jql with cursor-based
	// pagination via nextPageToken (an opaque string, not startAt/total).
	body := map[string]any{
		"jql":        fmt.Sprintf("project = %s AND statusCategory in (\"To Do\", \"In Progress\")", ref.Key),
		"fields":     []string{"*all"},
		"maxResults": 100,
	}
	if cursor != "" {
		body["nextPageToken"] = cursor
	}
	var out struct {
		Issues        []json.RawMessage `json:"issues"`
		NextPageToken string            `json:"nextPageToken"`
		IsLast        bool              `json:"isLast"`
	}
	if err := p.do(ctx, conn, http.MethodPost, "/search/jql", body, &out); err != nil {
		return nil, "", err
	}
	issues := make([]ExternalIssue, 0, len(out.Issues))
	browseBase := strings.TrimRight(conn.SiteUrl, "/") + "/browse/"
	for _, raw := range out.Issues {
		if ext, ok := JiraIssueToExternal(raw); ok {
			if ext.WebURL == "" && ext.Key != "" {
				ext.WebURL = browseBase + ext.Key
			}
			issues = append(issues, ext)
		}
	}
	next := ""
	if !out.IsLast && out.NextPageToken != "" {
		next = out.NextPageToken
	}
	return issues, next, nil
}

// CreateIssue creates a Jira issue and returns its identity. Jira does not
// accept a status on create; an initial state is applied via a best-effort
// transition afterwards (matching the category the outbound mapper picked).
func (p *JiraProvider) CreateIssue(ctx context.Context, src db.IssueSyncSource, out OutboundIssue) (*ExternalIssue, error) {
	ref, err := jiraRefFromSource(src)
	if err != nil {
		return nil, err
	}
	conn, err := p.loadConnection(ctx, src.ConnectionID.String())
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"summary":     out.Title,
		"description": json.RawMessage(MarkdownToADF(out.Description)),
		"project":     map[string]any{"key": ref.Key},
		"labels":      nonNilLabels(out.Labels),
	}
	if out.AssigneeAccountID != "" {
		fields["assignee"] = map[string]any{"accountId": out.AssigneeAccountID}
	}
	var raw json.RawMessage
	if err := p.do(ctx, conn, http.MethodPost, "/issue", map[string]any{"fields": fields}, &raw); err != nil {
		return nil, err
	}
	ext, ok := JiraIssueToExternal(raw)
	if !ok {
		// Create returns {id, key, self} with no fields block; build a minimal
		// identity so the link row can be recorded.
		var min struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		}
		_ = json.Unmarshal(raw, &min)
		ext = ExternalIssue{ID: min.ID, Key: min.Key, Title: out.Title, Description: out.Description}
	}
	if out.State != "" {
		_ = p.transitionIssue(ctx, conn, ext.ID, out.State)
	}
	return &ext, nil
}

// UpdateIssue patches summary/description/labels/assignee and applies a
// best-effort status transition. The REST update addresses the issue by id
// (issueIdOrKey), so externalID is used directly. PUT returns 204 (no body), so
// the returned ExternalIssue is built from the outbound content.
func (p *JiraProvider) UpdateIssue(ctx context.Context, src db.IssueSyncSource, externalID string, out OutboundIssue) (*ExternalIssue, error) {
	conn, err := p.loadConnection(ctx, src.ConnectionID.String())
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"summary":     out.Title,
		"description": json.RawMessage(MarkdownToADF(out.Description)),
		"labels":      nonNilLabels(out.Labels),
	}
	if out.AssigneeAccountID != "" {
		fields["assignee"] = map[string]any{"accountId": out.AssigneeAccountID}
	} else {
		// nil clears the assignee remotely.
		fields["assignee"] = nil
	}
	path := "/issue/" + externalID
	if err := p.do(ctx, conn, http.MethodPut, path, map[string]any{"fields": fields}, nil); err != nil {
		return nil, err
	}
	if out.State != "" {
		_ = p.transitionIssue(ctx, conn, externalID, out.State)
	}
	ext := ExternalIssue{ID: externalID, Title: out.Title, Description: out.Description, State: out.State, Labels: out.Labels}
	return &ext, nil
}

// CreateComment mirrors a local comment to the remote issue. The body is
// converted from markdown to ADF.
func (p *JiraProvider) CreateComment(ctx context.Context, src db.IssueSyncSource, externalID, body string) (*ExternalComment, error) {
	conn, err := p.loadConnection(ctx, src.ConnectionID.String())
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := "/issue/" + externalID + "/comment"
	if err := p.do(ctx, conn, http.MethodPost, path, map[string]any{"body": json.RawMessage(MarkdownToADF(body))}, &raw); err != nil {
		return nil, err
	}
	ext, ok := JiraCommentToExternal(raw)
	if !ok {
		return nil, errors.New("jira: unexpected comment response")
	}
	return &ext, nil
}

// UpdateComment propagates a local comment edit. externalID is the issue id;
// commentID is the Jira comment id.
func (p *JiraProvider) UpdateComment(ctx context.Context, src db.IssueSyncSource, externalID, commentID, body string) (*ExternalComment, error) {
	conn, err := p.loadConnection(ctx, src.ConnectionID.String())
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := "/issue/" + externalID + "/comment/" + commentID
	if err := p.do(ctx, conn, http.MethodPut, path, map[string]any{"body": json.RawMessage(MarkdownToADF(body))}, &raw); err != nil {
		return nil, err
	}
	ext, ok := JiraCommentToExternal(raw)
	if !ok {
		return nil, errors.New("jira: unexpected comment response")
	}
	return &ext, nil
}

// transitionIssue best-effort moves an issue into the requested status CATEGORY
// ("new"/"indeterminate"/"done"). Jira transitions are addressed by id, so the
// available transitions are fetched and the first whose target category matches
// is applied. Errors are non-fatal (the caller treats a missed transition as a
// soft failure).
func (p *JiraProvider) transitionIssue(ctx context.Context, conn db.JiraConnection, issueID, category string) error {
	if category == "" {
		return nil
	}
	path := "/issue/" + issueID + "/transitions"
	var out struct {
		Transitions []struct {
			ID string `json:"id"`
			To struct {
				StatusCategory struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := p.do(ctx, conn, http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	for _, t := range out.Transitions {
		if strings.EqualFold(t.To.StatusCategory.Key, category) {
			return p.do(ctx, conn, http.MethodPost, path, map[string]any{
				"transition": map[string]any{"id": t.ID},
			}, nil)
		}
	}
	return nil
}

// nonNilLabels ensures an explicit empty array clears labels remotely while a
// nil slice is serialized as an empty array too (Jira rejects a missing labels
// field on update in some flows).
func nonNilLabels(labels []string) []string {
	if labels == nil {
		return []string{}
	}
	return labels
}
