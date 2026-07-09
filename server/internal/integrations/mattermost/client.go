package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// This file is the minimal hand-rolled Mattermost REST client. The official
// github.com/mattermost/mattermost/server/public module drags a very large
// dependency tree for what amounts to a handful of endpoints, so — like the
// Lark adapter's http_client.go — we speak the small API surface directly.
// Every call authenticates with the installation's bot token (Bearer).

// mmUser is the subset of a Mattermost user object the adapter reads
// (GET /api/v4/users/me, POST /api/v4/users/ids).
type mmUser struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	IsBot     bool   `json:"is_bot"`
}

// mmPost is the subset of a Mattermost post object the adapter reads. Props
// stays raw: only the bot/webhook markers are ever inspected.
type mmPost struct {
	ID        string          `json:"id"`
	CreateAt  int64           `json:"create_at"` // ms since epoch
	UserID    string          `json:"user_id"`
	ChannelID string          `json:"channel_id"`
	RootID    string          `json:"root_id"`
	Message   string          `json:"message"`
	Type      string          `json:"type"` // "" for a normal user post; "system_*" otherwise
	Props     json.RawMessage `json:"props,omitempty"`
}

// mmPostList is the {order, posts} envelope returned by the channel-posts and
// thread endpoints. order is NEWEST-first post ids; posts maps id -> post.
type mmPostList struct {
	Order []string          `json:"order"`
	Posts map[string]mmPost `json:"posts"`
}

// mmAPIError is Mattermost's standard error body, used to surface a readable
// message instead of a bare status code.
type mmAPIError struct {
	Message    string `json:"message"`
	StatusCode int    `json:"status_code"`
}

// restAPI is the client surface the rest of the adapter consumes; tests inject
// a fake, production uses *restClient over an httptest-overridable base URL.
type restAPI interface {
	GetMe(ctx context.Context) (mmUser, error)
	CreatePost(ctx context.Context, channelID, rootID, message string) (mmPost, error)
	GetPostsForChannel(ctx context.Context, channelID string, perPage int, beforePostID string) (mmPostList, error)
	GetPostThread(ctx context.Context, postID string) (mmPostList, error)
	AddReaction(ctx context.Context, userID, postID, emojiName string) error
	RemoveReaction(ctx context.Context, postID, emojiName string) error
	GetUsersByIDs(ctx context.Context, ids []string) ([]mmUser, error)
}

// restClient speaks the Mattermost REST API v4 for ONE server + token pair.
type restClient struct {
	baseURL    string // normalized server URL, no trailing slash
	token      string
	httpClient *http.Client
}

var _ restAPI = (*restClient)(nil)

// newRestClient builds a client for one server/token. A nil httpClient uses
// http.DefaultClient (valid TLS certs required; self-signed servers are out of
// scope for v1).
func newRestClient(serverURL, token string, httpClient *http.Client) *restClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &restClient{baseURL: serverURL, token: token, httpClient: httpClient}
}

// GetMe validates the token and returns the bot's own identity
// (GET /api/v4/users/me). It is the install-time live check AND the source of
// the bot user id / username stored in the installation config.
func (c *restClient) GetMe(ctx context.Context) (mmUser, error) {
	var u mmUser
	err := c.do(ctx, http.MethodGet, "/api/v4/users/me", nil, &u)
	return u, err
}

// CreatePost posts one message (POST /api/v4/posts), threading under rootID
// when set.
func (c *restClient) CreatePost(ctx context.Context, channelID, rootID, message string) (mmPost, error) {
	body := map[string]string{"channel_id": channelID, "message": message}
	if rootID != "" {
		body["root_id"] = rootID
	}
	var p mmPost
	err := c.do(ctx, http.MethodPost, "/api/v4/posts", body, &p)
	return p, err
}

// GetPostsForChannel reads a page of a channel's posts, newest-first
// (GET /api/v4/channels/{id}/posts). beforePostID pages to strictly older
// posts.
func (c *restClient) GetPostsForChannel(ctx context.Context, channelID string, perPage int, beforePostID string) (mmPostList, error) {
	q := url.Values{}
	if perPage > 0 {
		q.Set("per_page", strconv.Itoa(perPage))
	}
	if beforePostID != "" {
		q.Set("before", beforePostID)
	}
	path := "/api/v4/channels/" + url.PathEscape(channelID) + "/posts"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var list mmPostList
	err := c.do(ctx, http.MethodGet, path, nil, &list)
	return list, err
}

// GetPostThread reads one thread's posts (GET /api/v4/posts/{id}/thread).
func (c *restClient) GetPostThread(ctx context.Context, postID string) (mmPostList, error) {
	var list mmPostList
	err := c.do(ctx, http.MethodGet, "/api/v4/posts/"+url.PathEscape(postID)+"/thread", nil, &list)
	return list, err
}

// AddReaction stamps an emoji reaction onto a post (POST /api/v4/reactions).
func (c *restClient) AddReaction(ctx context.Context, userID, postID, emojiName string) error {
	body := map[string]string{"user_id": userID, "post_id": postID, "emoji_name": emojiName}
	return c.do(ctx, http.MethodPost, "/api/v4/reactions", body, nil)
}

// RemoveReaction removes the token owner's own reaction from a post
// (DELETE /api/v4/users/me/posts/{id}/reactions/{emoji}).
func (c *restClient) RemoveReaction(ctx context.Context, postID, emojiName string) error {
	path := "/api/v4/users/me/posts/" + url.PathEscape(postID) + "/reactions/" + url.PathEscape(emojiName)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// GetUsersByIDs batch-resolves users for display names
// (POST /api/v4/users/ids).
func (c *restClient) GetUsersByIDs(ctx context.Context, ids []string) ([]mmUser, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var users []mmUser
	err := c.do(ctx, http.MethodPost, "/api/v4/users/ids", ids, &users)
	return users, err
}

// do runs one authenticated JSON round trip. A non-2xx response is surfaced as
// an *apiError carrying the status code and Mattermost's message field.
func (c *restClient) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mattermost: encode request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("mattermost: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mattermost: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Cap the read: error bodies are tiny and success bodies are bounded pages.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("mattermost: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &apiError{StatusCode: resp.StatusCode}
		var parsed mmAPIError
		if json.Unmarshal(data, &parsed) == nil && parsed.Message != "" {
			apiErr.Message = parsed.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("mattermost: decode response: %w", err)
	}
	return nil
}

// apiError is a non-2xx Mattermost API response. Callers branch on StatusCode
// (401 -> invalid token at install time).
type apiError struct {
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("mattermost: API error (status %d)", e.StatusCode)
	}
	return fmt.Sprintf("mattermost: %s (status %d)", e.Message, e.StatusCode)
}

// displayName picks the friendliest available name for a Mattermost user.
func displayName(u mmUser) string {
	switch {
	case u.Nickname != "":
		return u.Nickname
	case u.FirstName != "" || u.LastName != "":
		if u.FirstName != "" && u.LastName != "" {
			return u.FirstName + " " + u.LastName
		}
		return u.FirstName + u.LastName
	default:
		return u.Username
	}
}
