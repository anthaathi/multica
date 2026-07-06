package issuesync

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
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GitHubAPIBase is the base URL for GitHub's REST API. Mutable so tests can
// point the provider at an httptest server (mirrors handler.githubAPIBase).
var GitHubAPIBase = "https://api.github.com"

// GitHubRef is the external_ref JSONB shape for provider="github" sources.
// external_key is "owner/name" lowercased.
type GitHubRef struct {
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	RepoID int64  `json:"repo_id,omitempty"`
}

// GitHubContainerKey normalizes a repo full name into the external_key format.
func GitHubContainerKey(owner, name string) string {
	return strings.ToLower(owner + "/" + name)
}

// GitHubProvider talks to the GitHub REST API using GitHub App installation
// tokens. The App identity comes from GITHUB_APP_ID / GITHUB_APP_PRIVATE_KEY
// (the same envs the install flow uses); per-installation tokens are minted
// on demand and cached until shortly before expiry.
type GitHubProvider struct {
	Queries *db.Queries
	Client  *http.Client

	mu     sync.Mutex
	tokens map[string]githubToken // key: connection UUID string
}

type githubToken struct {
	value   string
	expires time.Time
}

func NewGitHubProvider(q *db.Queries) *GitHubProvider {
	return &GitHubProvider{
		Queries: q,
		Client:  &http.Client{Timeout: 30 * time.Second},
		tokens:  make(map[string]githubToken),
	}
}

func (p *GitHubProvider) Name() string { return ProviderGitHub }

// signAppJWT mints the short-lived RS256 App JWT. Mirrors
// handler.signGitHubAppJWT (which is unexported in another package): iat
// back-dated 60s for clock skew, exp capped at 9 minutes.
func signAppJWT(now time.Time) (string, error) {
	appID := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	pemKey := strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY"))
	if appID == "" || pemKey == "" {
		return "", errors.New("issuesync: GITHUB_APP_ID / GITHUB_APP_PRIVATE_KEY not configured")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return "", fmt.Errorf("parse GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}

// installationToken returns a cached-or-fresh installation access token for
// the connection (a github_installation row UUID).
func (p *GitHubProvider) installationToken(ctx context.Context, connectionID string) (string, error) {
	p.mu.Lock()
	if tok, ok := p.tokens[connectionID]; ok && time.Now().Before(tok.expires.Add(-2*time.Minute)) {
		p.mu.Unlock()
		return tok.value, nil
	}
	p.mu.Unlock()

	connUUID, err := util.ParseUUID(connectionID)
	if err != nil {
		return "", fmt.Errorf("bad connection id: %w", err)
	}
	inst, err := p.Queries.GetGitHubInstallationByID(ctx, connUUID)
	if err != nil {
		return "", fmt.Errorf("load installation: %w", err)
	}
	appJWT, err := signAppJWT(time.Now())
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens",
		strings.TrimRight(GitHubAPIBase, "/"), inst.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+appJWT)
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("mint installation token: status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode installation token: %w", err)
	}
	p.mu.Lock()
	p.tokens[connectionID] = githubToken{value: out.Token, expires: out.ExpiresAt}
	p.mu.Unlock()
	return out.Token, nil
}

// doJSON performs one authenticated REST call and decodes the response into
// out (nil out discards the body). Non-2xx surfaces as an error with a body
// excerpt so outbox retries carry a diagnosable message.
func (p *GitHubProvider) doJSON(ctx context.Context, connectionID, method, path string, body any, out any) error {
	token, err := p.installationToken(ctx, connectionID)
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
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(GitHubAPIBase, "/")+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
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
		return fmt.Errorf("github %s %s: status %d: %s", method, path, resp.StatusCode, string(excerpt))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func githubRefFromSource(src db.IssueSyncSource) (GitHubRef, error) {
	var ref GitHubRef
	if err := json.Unmarshal(src.ExternalRef, &ref); err != nil {
		return ref, fmt.Errorf("bad github external_ref: %w", err)
	}
	if ref.Owner == "" || ref.Name == "" {
		return ref, errors.New("github external_ref missing owner/name")
	}
	return ref, nil
}

// ghIssue is the subset of GitHub's issue payload the sync engine consumes.
// Shared by REST responses and webhook payloads (handler/github.go reuses the
// converter below).
type ghIssue struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignee    *ghUser   `json:"assignee"`
	User        *ghUser   `json:"user"`
	HTMLURL     string    `json:"html_url"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct { // present when the "issue" is actually a PR
		URL string `json:"url"`
	} `json:"pull_request"`
}

type ghUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Type      string `json:"type"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"`
}

func ghUserToExternal(u *ghUser) *ExternalUser {
	if u == nil {
		return nil
	}
	return &ExternalUser{
		AccountID:   strconv.FormatInt(u.ID, 10),
		Login:       u.Login,
		DisplayName: u.Login,
		Email:       u.Email,
		AvatarURL:   u.AvatarURL,
	}
}

// GitHubIssueToExternal converts a GitHub issue payload (REST or webhook)
// into the neutral shape. ok=false when the payload is a pull request —
// GitHub delivers PRs through the issues API surface and those are owned by
// the existing PR-mirror integration, not issue sync.
func GitHubIssueToExternal(raw json.RawMessage) (ExternalIssue, bool) {
	var gh ghIssue
	if err := json.Unmarshal(raw, &gh); err != nil || gh.ID == 0 {
		return ExternalIssue{}, false
	}
	if gh.PullRequest != nil {
		return ExternalIssue{}, false
	}
	ext := ExternalIssue{
		ID:          strconv.FormatInt(gh.ID, 10),
		Key:         "#" + strconv.Itoa(gh.Number),
		Title:       gh.Title,
		Description: gh.Body,
		State:       gh.State,
		Assignee:    ghUserToExternal(gh.Assignee),
		Author:      ghUserToExternal(gh.User),
		WebURL:      gh.HTMLURL,
		UpdatedAt:   gh.UpdatedAt,
	}
	for _, l := range gh.Labels {
		ext.Labels = append(ext.Labels, l.Name)
	}
	return ext, true
}

type ghComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      *ghUser   `json:"user"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GitHubCommentToExternal converts a GitHub issue-comment payload.
func GitHubCommentToExternal(raw json.RawMessage) (ExternalComment, bool) {
	var gh ghComment
	if err := json.Unmarshal(raw, &gh); err != nil || gh.ID == 0 {
		return ExternalComment{}, false
	}
	return ExternalComment{
		ID:        strconv.FormatInt(gh.ID, 10),
		Body:      gh.Body,
		Author:    ghUserToExternal(gh.User),
		WebURL:    gh.HTMLURL,
		UpdatedAt: gh.UpdatedAt,
	}, true
}

func (p *GitHubProvider) ListContainers(ctx context.Context, connectionID string) ([]Container, error) {
	var containers []Container
	page := 1
	for {
		var out struct {
			Repositories []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				FullName string `json:"full_name"`
				HTMLURL  string `json:"html_url"`
				Owner    struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := p.doJSON(ctx, connectionID, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Repositories {
			containers = append(containers, Container{
				Key:  GitHubContainerKey(r.Owner.Login, r.Name),
				Name: r.FullName,
				URL:  r.HTMLURL,
				Ref: map[string]any{
					"owner":   r.Owner.Login,
					"name":    r.Name,
					"repo_id": r.ID,
				},
			})
		}
		if len(out.Repositories) < 100 {
			return containers, nil
		}
		page++
	}
}

func (p *GitHubProvider) ListIssues(ctx context.Context, src db.IssueSyncSource, cursor string) ([]ExternalIssue, string, error) {
	ref, err := githubRefFromSource(src)
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
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&per_page=100&page=%d", ref.Owner, ref.Name, page)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodGet, path, nil, &raw); err != nil {
		return nil, "", err
	}
	var issues []ExternalIssue
	for _, r := range raw {
		if ext, ok := GitHubIssueToExternal(r); ok {
			issues = append(issues, ext)
		}
	}
	next := ""
	if len(raw) == 100 {
		next = strconv.Itoa(page + 1)
	}
	return issues, next, nil
}

// githubIssueBody builds the PATCH/POST body. GitHub has no separate status
// transition call — state rides the same endpoint.
func githubIssueBody(out OutboundIssue) map[string]any {
	body := map[string]any{
		"title": out.Title,
		"body":  out.Description,
	}
	if out.State != "" {
		body["state"] = out.State
	}
	// Explicit empty array clears labels remotely; matches the reconcile
	// semantics of the inbound label sync.
	labels := out.Labels
	if labels == nil {
		labels = []string{}
	}
	body["labels"] = labels
	if out.AssigneeAccountID != "" {
		// GitHub assigns by login, not numeric id; the identity table stores
		// the login alongside the id and buildOutbound passes the account id.
		// Resolution happens in assigneeLogins.
		body["assignees"] = []string{out.AssigneeAccountID}
	} else {
		body["assignees"] = []string{}
	}
	return body
}

// resolveAssigneeLogin swaps the numeric account id for the login GitHub's
// assign API expects, via the identity cache.
func (p *GitHubProvider) resolveAssigneeLogin(ctx context.Context, src db.IssueSyncSource, out *OutboundIssue) {
	if out.AssigneeAccountID == "" {
		return
	}
	identity, err := p.Queries.GetExternalIdentity(ctx, db.GetExternalIdentityParams{
		WorkspaceID:       src.WorkspaceID,
		Provider:          ProviderGitHub,
		ExternalAccountID: out.AssigneeAccountID,
	})
	if err != nil || !identity.ExternalLogin.Valid || identity.ExternalLogin.String == "" {
		out.AssigneeAccountID = ""
		return
	}
	out.AssigneeAccountID = identity.ExternalLogin.String
}

func (p *GitHubProvider) CreateIssue(ctx context.Context, src db.IssueSyncSource, out OutboundIssue) (*ExternalIssue, error) {
	ref, err := githubRefFromSource(src)
	if err != nil {
		return nil, err
	}
	p.resolveAssigneeLogin(ctx, src, &out)
	body := githubIssueBody(out)
	// POST /issues rejects "state" — created issues are always open; a
	// closed state is applied with a follow-up PATCH below.
	state, _ := body["state"].(string)
	delete(body, "state")
	var raw json.RawMessage
	path := fmt.Sprintf("/repos/%s/%s/issues", ref.Owner, ref.Name)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPost, path, body, &raw); err != nil {
		return nil, err
	}
	ext, ok := GitHubIssueToExternal(raw)
	if !ok {
		return nil, errors.New("github: unexpected create response")
	}
	if state == "closed" {
		patchPath := fmt.Sprintf("/repos/%s/%s/issues/%s", ref.Owner, ref.Name, numberFromKey(ext.Key))
		if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPatch, patchPath, map[string]any{"state": "closed"}, &raw); err == nil {
			if closed, ok := GitHubIssueToExternal(raw); ok {
				ext = closed
			}
		}
	}
	return &ext, nil
}

// numberFromKey strips the "#" prefix from the human key ("#123" → "123").
// GitHub's REST paths address issues by number, while link identity uses the
// stable numeric id — the key carries the number across.
func numberFromKey(key string) string {
	return strings.TrimPrefix(key, "#")
}

func (p *GitHubProvider) UpdateIssue(ctx context.Context, src db.IssueSyncSource, externalID string, out OutboundIssue) (*ExternalIssue, error) {
	ref, err := githubRefFromSource(src)
	if err != nil {
		return nil, err
	}
	number, err := p.issueNumberForID(ctx, src, externalID)
	if err != nil {
		return nil, err
	}
	p.resolveAssigneeLogin(ctx, src, &out)
	var raw json.RawMessage
	path := fmt.Sprintf("/repos/%s/%s/issues/%s", ref.Owner, ref.Name, number)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPatch, path, githubIssueBody(out), &raw); err != nil {
		return nil, err
	}
	ext, ok := GitHubIssueToExternal(raw)
	if !ok {
		return nil, errors.New("github: unexpected update response")
	}
	return &ext, nil
}

// issueNumberForID recovers the REST issue number from the stored link key.
// The link's external_key is "#<number>", written on link creation from both
// the webhook and REST paths.
func (p *GitHubProvider) issueNumberForID(ctx context.Context, src db.IssueSyncSource, externalID string) (string, error) {
	link, err := p.Queries.GetExternalIssueLinkByExternalID(ctx, db.GetExternalIssueLinkByExternalIDParams{
		SyncSourceID: src.ID,
		ExternalID:   externalID,
	})
	if err != nil {
		return "", fmt.Errorf("github: no link for external id %s: %w", externalID, err)
	}
	number := numberFromKey(link.ExternalKey)
	if number == "" {
		return "", fmt.Errorf("github: link %s has no issue number", util.UUIDToString(link.ID))
	}
	return number, nil
}

func (p *GitHubProvider) CreateComment(ctx context.Context, src db.IssueSyncSource, externalID, body string) (*ExternalComment, error) {
	ref, err := githubRefFromSource(src)
	if err != nil {
		return nil, err
	}
	number, err := p.issueNumberForID(ctx, src, externalID)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := fmt.Sprintf("/repos/%s/%s/issues/%s/comments", ref.Owner, ref.Name, number)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPost, path, map[string]any{"body": body}, &raw); err != nil {
		return nil, err
	}
	ext, ok := GitHubCommentToExternal(raw)
	if !ok {
		return nil, errors.New("github: unexpected comment response")
	}
	return &ext, nil
}

func (p *GitHubProvider) UpdateComment(ctx context.Context, src db.IssueSyncSource, _ /* issue externalID */, commentID, body string) (*ExternalComment, error) {
	ref, err := githubRefFromSource(src)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%s", ref.Owner, ref.Name, commentID)
	if err := p.doJSON(ctx, src.ConnectionID.String(), http.MethodPatch, path, map[string]any{"body": body}, &raw); err != nil {
		return nil, err
	}
	ext, ok := GitHubCommentToExternal(raw)
	if !ok {
		return nil, errors.New("github: unexpected comment response")
	}
	return &ext, nil
}
