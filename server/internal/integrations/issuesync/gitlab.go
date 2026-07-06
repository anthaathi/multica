package issuesync

// GitLab issue-sync provider.
//
// NOTE: this provider is NOT registered in router.go by this file (editing
// router.go/main.go centrally avoids file conflicts across parallel work).
// After the GitLab secretbox is constructed, wire it up with:
//
//	h.IssueSync.Providers[issuesync.ProviderGitLab] =
//	    issuesync.NewGitLabProvider(h.Queries, h.GitLabBox)
//
// Credentials: the OAuth access token is encrypted at rest in
// gitlab_connection.access_token_encrypted. The provider decrypts it via the
// injected *secretbox.Box (issuesync may import internal/util/secretbox; it
// may NOT import handler, which would create a cycle). The instance base URL
// comes from the connection row (gitlab_base_url), falling back to the same
// GITLAB_URL / GITLAB_INTERNAL_URL precedence handler.gitlabInternalURL uses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// gitlabDefaultBaseURL mirrors handler.gitlabInternalURL: GITLAB_INTERNAL_URL
// (split-horizon / in-cluster server-to-server dial) overrides GITLAB_URL for
// API calls. Returns "" when neither env is set.
func gitlabDefaultBaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_INTERNAL_URL")), "/"); v != "" {
		return v
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_URL")), "/")
}

// GitLabRef is the external_ref JSONB shape for provider="gitlab" sources.
// external_key is path_with_namespace lowercased.
type GitLabRef struct {
	ProjectID         int    `json:"project_id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

// GitLabContainerKey normalizes a project path into the external_key format.
func GitLabContainerKey(pathWithNamespace string) string {
	return strings.ToLower(strings.TrimSpace(pathWithNamespace))
}

func gitlabRefFromSource(src db.IssueSyncSource) (GitLabRef, error) {
	var ref GitLabRef
	if err := json.Unmarshal(src.ExternalRef, &ref); err != nil {
		return ref, fmt.Errorf("bad gitlab external_ref: %w", err)
	}
	if ref.ProjectID == 0 || strings.TrimSpace(ref.PathWithNamespace) == "" {
		return ref, errors.New("gitlab external_ref missing project_id/path_with_namespace")
	}
	return ref, nil
}

// GitLabProvider talks to the GitLab REST API (v4) using the OAuth access
// token stored on the GitLab connection row.
type GitLabProvider struct {
	Queries *db.Queries
	Box     *secretbox.Box
	Client  *http.Client

	// tokenForConn resolves the access token + instance base URL for a
	// connection. Defaults to reading + decrypting the connection row; tests
	// override it to point at an httptest server without a database.
	tokenForConn func(ctx context.Context, connectionID string) (token, baseURL string, err error)
}

// NewGitLabProvider constructs a provider bound to the given queries + box.
// The HTTP client defaults to a 30s timeout, mirroring the GitHub provider.
func NewGitLabProvider(q *db.Queries, box *secretbox.Box) *GitLabProvider {
	p := &GitLabProvider{
		Queries: q,
		Box:     box,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
	p.tokenForConn = p.defaultTokenForConn
	return p
}

func (p *GitLabProvider) Name() string { return ProviderGitLab }

// defaultTokenForConn loads the connection, decrypts the access token, and
// resolves the instance base URL (connection row → env fallback).
func (p *GitLabProvider) defaultTokenForConn(ctx context.Context, connectionID string) (string, string, error) {
	connUUID, err := util.ParseUUID(connectionID)
	if err != nil {
		return "", "", fmt.Errorf("bad connection id: %w", err)
	}
	conn, err := p.Queries.GetGitLabConnectionByID(ctx, connUUID)
	if err != nil {
		return "", "", fmt.Errorf("load gitlab connection: %w", err)
	}
	if p.Box == nil {
		return "", "", errors.New("issuesync: gitlab secretbox not configured")
	}
	plain, err := p.Box.Open(conn.AccessTokenEncrypted)
	if err != nil {
		return "", "", fmt.Errorf("decrypt gitlab access token: %w", err)
	}
	baseURL := strings.TrimSpace(conn.GitlabBaseUrl)
	if baseURL == "" {
		baseURL = gitlabDefaultBaseURL()
	}
	return string(plain), baseURL, nil
}

// doJSON performs one authenticated v4 REST call and decodes into out (nil out
// discards the body). Non-2xx surfaces as an error with a body excerpt so
// outbox retries carry a diagnosable message.
func (p *GitLabProvider) doJSON(ctx context.Context, connectionID, method, path string, body any, out any) error {
	token, baseURL, err := p.tokenForConn(ctx, connectionID)
	if err != nil {
		return err
	}
	if baseURL == "" {
		return errors.New("issuesync: gitlab base URL not configured")
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("gitlab %s %s: status %d: %s", method, path, resp.StatusCode, string(excerpt))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── Payload converters (shared by REST + webhooks) ──────────────────────────

// glUser is the subset of GitLab's user object the sync engine consumes.
type glUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"`
}

func glUserToExternal(u *glUser) *ExternalUser {
	if u == nil {
		return nil
	}
	name := u.Name
	if name == "" {
		name = u.Username
	}
	return &ExternalUser{
		AccountID:   strconv.FormatInt(u.ID, 10),
		Login:       u.Username,
		DisplayName: name,
		Email:       u.Email,
		AvatarURL:   u.AvatarURL,
	}
}

// glIssue is the subset of GitLab's issue payload the sync engine consumes.
// The same struct decodes both REST responses (fields at top level, url in
// web_url) and webhook object_attributes (url in "url"). Shared by the REST
// path and the webhook converter below.
type glIssue struct {
	ID          int64  `json:"id"`
	IID         int    `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Labels      []struct {
		Title string `json:"title"`
	} `json:"labels"`
	Assignees []glUser `json:"assignees"`
	Author    *glUser  `json:"author"`
	WebURL    string   `json:"web_url"`
	URL       string   `json:"url"` // webhooks use "url" inside object_attributes
	UpdatedAt time.Time `json:"updated_at"`
}

func glIssueToExternal(gl glIssue) ExternalIssue {
	ext := ExternalIssue{
		ID:          strconv.FormatInt(gl.ID, 10),
		Key:         "#" + strconv.Itoa(gl.IID),
		Title:       gl.Title,
		Description: gl.Description,
		State:       gl.State,
		Author:      glUserToExternal(gl.Author),
		WebURL:      gl.WebURL,
		UpdatedAt:   gl.UpdatedAt,
	}
	if ext.WebURL == "" {
		ext.WebURL = gl.URL
	}
	// GitLab issues have at most one assignee via the API; the payload exposes
	// an array, so take the first.
	if len(gl.Assignees) > 0 {
		ext.Assignee = glUserToExternal(&gl.Assignees[0])
	}
	for _, l := range gl.Labels {
		ext.Labels = append(ext.Labels, l.Title)
	}
	return ext
}

// GitLabIssueToExternal converts a GitLab issue payload (REST response or full
// webhook body) into the neutral shape. Webhook bodies carry the issue under
// object_attributes; REST responses are top-level. ok=false when the payload
// is missing an id.
func GitLabIssueToExternal(raw json.RawMessage) (ExternalIssue, bool) {
	// Webhook: issue nested under object_attributes; labels/assignees may live
	// at the envelope top level or inside object_attributes.
	var env struct {
		ObjectAttributes json.RawMessage `json:"object_attributes"`
		Labels           []struct {
			Title string `json:"title"`
		} `json:"labels"`
		Assignees []glUser `json:"assignees"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.ObjectAttributes) > 0 {
		var gl glIssue
		if err := json.Unmarshal(env.ObjectAttributes, &gl); err != nil || gl.ID == 0 {
			return ExternalIssue{}, false
		}
		if len(gl.Labels) == 0 && len(env.Labels) > 0 {
			gl.Labels = env.Labels
		}
		if len(gl.Assignees) == 0 && len(env.Assignees) > 0 {
			gl.Assignees = env.Assignees
		}
		return glIssueToExternal(gl), true
	}
	// REST: top-level issue object.
	var gl glIssue
	if err := json.Unmarshal(raw, &gl); err != nil || gl.ID == 0 {
		return ExternalIssue{}, false
	}
	return glIssueToExternal(gl), true
}

// glNote is the subset of a GitLab note (issue comment) the engine consumes.
type glNote struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	Author    *glUser   `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
}

func glNoteToExternal(n glNote) ExternalComment {
	return ExternalComment{
		ID:        strconv.FormatInt(n.ID, 10),
		Body:      n.Body,
		Author:    glUserToExternal(n.Author),
		WebURL:    n.URL,
		UpdatedAt: n.UpdatedAt,
	}
}

// GitLabCommentToExternal converts a GitLab note payload (REST response or full
// note-hook webhook body) into the neutral shape. In webhook deliveries the
// note is nested under object_attributes and the author is the top-level user
// (not embedded in the note); REST responses embed the author directly.
func GitLabCommentToExternal(raw json.RawMessage) (ExternalComment, bool) {
	var env struct {
		User             *glUser `json:"user"`
		ObjectAttributes glNote  `json:"object_attributes"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.ObjectAttributes.ID != 0 {
		c := glNoteToExternal(env.ObjectAttributes)
		if env.User != nil {
			c.Author = glUserToExternal(env.User)
		}
		return c, true
	}
	var n glNote
	if err := json.Unmarshal(raw, &n); err != nil || n.ID == 0 {
		return ExternalComment{}, false
	}
	return glNoteToExternal(n), true
}

// ── Provider interface ──────────────────────────────────────────────────────

func (p *GitLabProvider) ListContainers(ctx context.Context, connectionID string) ([]Container, error) {
	var containers []Container
	page := 1
	for {
		var projects []struct {
			ID                int64  `json:"id"`
			Name              string `json:"name"`
			PathWithNamespace string `json:"path_with_namespace"`
			WebURL            string `json:"web_url"`
		}
		path := fmt.Sprintf("/api/v4/projects?membership=true&per_page=100&page=%d", page)
		if err := p.doJSON(ctx, connectionID, http.MethodGet, path, nil, &projects); err != nil {
			return nil, err
		}
		for _, pr := range projects {
			containers = append(containers, Container{
				Key:  GitLabContainerKey(pr.PathWithNamespace),
				Name: pr.PathWithNamespace,
				URL:  pr.WebURL,
				Ref: map[string]any{
					"project_id":          pr.ID,
					"path_with_namespace": pr.PathWithNamespace,
				},
			})
		}
		if len(projects) < 100 {
			return containers, nil
		}
		page++
	}
}

func (p *GitLabProvider) ListIssues(ctx context.Context, src db.IssueSyncSource, cursor string) ([]ExternalIssue, string, error) {
	ref, err := gitlabRefFromSource(src)
	if err != nil {
		return nil, "", err
	}
	page := 1
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n > 1 {
			page = n
		}
	}
	var raw []json.RawMessage
	path := fmt.Sprintf("/api/v4/projects/%d/issues?state=opened&per_page=100&page=%d", ref.ProjectID, page)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodGet, path, nil, &raw); err != nil {
		return nil, "", err
	}
	var issues []ExternalIssue
	for _, r := range raw {
		if ext, ok := GitLabIssueToExternal(r); ok {
			issues = append(issues, ext)
		}
	}
	next := ""
	if len(raw) == 100 {
		next = strconv.Itoa(page + 1)
	}
	return issues, next, nil
}

// gitlabIssueBody builds the POST/PUT body. GitLab has no separate status
// transition — state rides the same endpoint ("opened"/"closed").
func gitlabIssueBody(out OutboundIssue) map[string]any {
	body := map[string]any{
		"title":       out.Title,
		"description": out.Description,
	}
	if out.State != "" {
		body["state_event"] = gitlabStateEvent(out.State)
	}
	labels := out.Labels
	if labels == nil {
		labels = []string{}
	}
	body["labels"] = labels
	// GitLab assigns by numeric user id — exactly what external_identity stores
	// as external_account_id (no login swap needed, unlike GitHub).
	if out.AssigneeAccountID != "" {
		if id, err := strconv.ParseInt(out.AssigneeAccountID, 10, 64); err == nil {
			body["assignee_ids"] = []int64{id}
		}
	}
	return body
}

// gitlabStateEvent maps the REST "opened"/"closed" vocabulary to GitLab's
// state_event parameter ("reopen"/"close"). "opened" is the create default
// and needs no event on update.
func gitlabStateEvent(state string) string {
	switch state {
	case "closed":
		return "close"
	case "opened":
		return "reopen"
	default:
		return ""
	}
}

func (p *GitLabProvider) CreateIssue(ctx context.Context, src db.IssueSyncSource, out OutboundIssue) (*ExternalIssue, error) {
	ref, err := gitlabRefFromSource(src)
	if err != nil {
		return nil, err
	}
	body := gitlabIssueBody(out)
	// POST rejects state_event — new issues start "opened"; a "closed" target
	// is applied with a follow-up PUT below.
	stateEvent, _ := body["state_event"].(string)
	delete(body, "state_event")
	var raw json.RawMessage
	path := fmt.Sprintf("/api/v4/projects/%d/issues", ref.ProjectID)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPost, path, body, &raw); err != nil {
		return nil, err
	}
	ext, ok := GitLabIssueToExternal(raw)
	if !ok {
		return nil, errors.New("gitlab: unexpected create response")
	}
	if stateEvent == "close" {
		patchPath := fmt.Sprintf("/api/v4/projects/%d/issues/%s", ref.ProjectID, numberFromKey(ext.Key))
		closeBody := map[string]any{"state_event": "close"}
		if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPut, patchPath, closeBody, &raw); err == nil {
			if closed, ok := GitLabIssueToExternal(raw); ok {
				ext = closed
			}
		}
	}
	return &ext, nil
}

// issueNumberForID recovers the project-scoped iid from the stored link key.
// The link's external_key is "#<iid>", written on link creation from both the
// webhook and REST paths. Reuses github.go's numberFromKey (same package).
func (p *GitLabProvider) issueNumberForID(ctx context.Context, src db.IssueSyncSource, externalID string) (string, error) {
	link, err := p.Queries.GetExternalIssueLinkByExternalID(ctx, db.GetExternalIssueLinkByExternalIDParams{
		SyncSourceID: src.ID,
		ExternalID:   externalID,
	})
	if err != nil {
		return "", fmt.Errorf("gitlab: no link for external id %s: %w", externalID, err)
	}
	number := numberFromKey(link.ExternalKey)
	if number == "" {
		return "", fmt.Errorf("gitlab: link %s has no issue iid", util.UUIDToString(link.ID))
	}
	return number, nil
}

func (p *GitLabProvider) UpdateIssue(ctx context.Context, src db.IssueSyncSource, externalID string, out OutboundIssue) (*ExternalIssue, error) {
	ref, err := gitlabRefFromSource(src)
	if err != nil {
		return nil, err
	}
	iid, err := p.issueNumberForID(ctx, src, externalID)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := fmt.Sprintf("/api/v4/projects/%d/issues/%s", ref.ProjectID, iid)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPut, path, gitlabIssueBody(out), &raw); err != nil {
		return nil, err
	}
	ext, ok := GitLabIssueToExternal(raw)
	if !ok {
		return nil, errors.New("gitlab: unexpected update response")
	}
	return &ext, nil
}

func (p *GitLabProvider) CreateComment(ctx context.Context, src db.IssueSyncSource, externalID, body string) (*ExternalComment, error) {
	ref, err := gitlabRefFromSource(src)
	if err != nil {
		return nil, err
	}
	iid, err := p.issueNumberForID(ctx, src, externalID)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := fmt.Sprintf("/api/v4/projects/%d/issues/%s/notes", ref.ProjectID, iid)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPost, path, map[string]any{"body": body}, &raw); err != nil {
		return nil, err
	}
	ext, ok := GitLabCommentToExternal(raw)
	if !ok {
		return nil, errors.New("gitlab: unexpected note response")
	}
	return &ext, nil
}

func (p *GitLabProvider) UpdateComment(ctx context.Context, src db.IssueSyncSource, externalID, commentID, body string) (*ExternalComment, error) {
	ref, err := gitlabRefFromSource(src)
	if err != nil {
		return nil, err
	}
	iid, err := p.issueNumberForID(ctx, src, externalID)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := fmt.Sprintf("/api/v4/projects/%d/issues/%s/notes/%s", ref.ProjectID, iid, commentID)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPut, path, map[string]any{"body": body}, &raw); err != nil {
		return nil, err
	}
	ext, ok := GitLabCommentToExternal(raw)
	if !ok {
		return nil, errors.New("gitlab: unexpected note response")
	}
	return &ext, nil
}
