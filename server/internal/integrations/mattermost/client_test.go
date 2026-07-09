package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// mmTestRoute is one canned response keyed by "METHOD /path" (path excludes
// the query string). A zero status means 200.
type mmTestRoute struct {
	status int
	body   string
}

// mmRecordedRequest captures one inbound request to the test server, for
// assertion of method, path, query, body and auth header.
type mmRecordedRequest struct {
	method      string
	path        string
	query       url.Values
	body        string
	authHeader  string
	contentType string
}

// mmTestRequests records every request the test server received.
type mmTestRequests struct {
	mu       sync.Mutex
	requests []mmRecordedRequest
}

func (r *mmTestRequests) last() mmRecordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return mmRecordedRequest{}
	}
	return r.requests[len(r.requests)-1]
}

func (r *mmTestRequests) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// mmTestServer spins up an httptest.Server whose handler dispatches on
// "METHOD /path" (path excludes the query string) to a canned status+body.
// Every incoming request is recorded so tests can assert on payload, headers
// and query params. Unmapped routes reply 404 with a Mattermost-shaped error
// body. The server is closed automatically via t.Cleanup.
func mmTestServer(t *testing.T, routes map[string]mmTestRoute) (*httptest.Server, *mmTestRequests) {
	t.Helper()
	rec := &mmTestRequests{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, mmRecordedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			query:       r.URL.Query(),
			body:        string(body),
			authHeader:  r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
		})
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no route registered","status_code":404}`))
			return
		}
		if route.status != 0 {
			w.WriteHeader(route.status)
		}
		_, _ = w.Write([]byte(route.body))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestGetMe_OK(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"GET /api/v4/users/me": {
			status: 200,
			body:   `{"id":"abc","username":"bot","first_name":"B","last_name":"Ot"}`,
		},
	})
	c := newRestClient(srv.URL, "tok", nil)

	u, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe err = %v", err)
	}
	if u.ID != "abc" || u.Username != "bot" || u.FirstName != "B" || u.LastName != "Ot" {
		t.Errorf("GetMe = %+v, want {id:abc username:bot first_name:B last_name:Ot}", u)
	}
	if got := rec.last().authHeader; got != "Bearer tok" {
		t.Errorf("Authorization header = %q, want \"Bearer tok\"", got)
	}
	if rec.last().method != http.MethodGet {
		t.Errorf("method = %q, want GET", rec.last().method)
	}
	// displayName prefers first+last when both are present.
	if got := displayName(u); got != "B Ot" {
		t.Errorf("displayName(GetMe) = %q, want \"B Ot\"", got)
	}
}

func TestGetMe_UnauthorizedReturnsAPIError(t *testing.T) {
	srv, _ := mmTestServer(t, map[string]mmTestRoute{
		"GET /api/v4/users/me": {
			status: 401,
			body:   `{"message":"Invalid or expired token","status_code":401}`,
		},
	})
	c := newRestClient(srv.URL, "bad-token", nil)

	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe err = nil, want non-nil for 401")
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *apiError", err, err)
	}
	if ae.StatusCode != 401 {
		t.Errorf("apiError.StatusCode = %d, want 401", ae.StatusCode)
	}
	if !strings.Contains(ae.Message, "Invalid") {
		t.Errorf("apiError.Message = %q, want it to carry the server message", ae.Message)
	}
}

func TestCreatePost_OK(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"POST /api/v4/posts": {
			status: 201,
			body:   `{"id":"p9","channel_id":"CH1","message":"hi","root_id":"r1"}`,
		},
	})
	c := newRestClient(srv.URL, "tok", nil)

	p, err := c.CreatePost(context.Background(), "CH1", "r1", "hi")
	if err != nil {
		t.Fatalf("CreatePost err = %v", err)
	}
	if p.ID != "p9" || p.ChannelID != "CH1" || p.Message != "hi" || p.RootID != "r1" {
		t.Errorf("CreatePost = %+v, want id=p9 channel=CH1 message=hi root=r1", p)
	}
	// Verify the request body sent.
	var got map[string]string
	if err := json.Unmarshal([]byte(rec.last().body), &got); err != nil {
		t.Fatalf("request body is not a JSON object: %v (body=%q)", err, rec.last().body)
	}
	if got["channel_id"] != "CH1" || got["message"] != "hi" || got["root_id"] != "r1" {
		t.Errorf("request body = %s, want channel_id=CH1 message=hi root_id=r1", rec.last().body)
	}
	if rec.last().authHeader != "Bearer tok" {
		t.Errorf("Authorization = %q, want \"Bearer tok\"", rec.last().authHeader)
	}
}

func TestCreatePost_EmptyRootOmitsField(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"POST /api/v4/posts": {status: 201, body: `{"id":"p1"}`},
	})
	c := newRestClient(srv.URL, "tok", nil)

	_, err := c.CreatePost(context.Background(), "CH1", "", "hi")
	if err != nil {
		t.Fatalf("CreatePost err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(rec.last().body), &got); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if _, present := got["root_id"]; present {
		t.Errorf("root_id present in body %s, want omitted when rootID is empty", rec.last().body)
	}
	if got["channel_id"] != "CH1" || got["message"] != "hi" {
		t.Errorf("request body = %s, want channel_id=CH1 message=hi", rec.last().body)
	}
}

func TestGetPostsForChannel_OrderAndPostsAndBefore(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"GET /api/v4/channels/CH1/posts": {
			status: 200,
			body:   `{"order":["p2","p1"],"posts":{"p1":{"id":"p1","message":"old"},"p2":{"id":"p2","message":"new"}}}`,
		},
	})
	c := newRestClient(srv.URL, "tok", nil)

	list, err := c.GetPostsForChannel(context.Background(), "CH1", 50, "p1")
	if err != nil {
		t.Fatalf("GetPostsForChannel err = %v", err)
	}
	if len(list.Order) != 2 || list.Order[0] != "p2" || list.Order[1] != "p1" {
		t.Errorf("Order = %v, want [p2 p1] (newest-first)", list.Order)
	}
	if list.Posts["p1"].Message != "old" || list.Posts["p2"].Message != "new" {
		t.Errorf("Posts = %+v, want p1=old p2=new", list.Posts)
	}
	// beforePostID is forwarded as ?before=, perPage as ?per_page=.
	if got := rec.last().query.Get("before"); got != "p1" {
		t.Errorf("before query = %q, want p1", got)
	}
	if got := rec.last().query.Get("per_page"); got != "50" {
		t.Errorf("per_page query = %q, want 50", got)
	}
}

func TestGetPostThread_OK(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"GET /api/v4/posts/p1/thread": {
			status: 200,
			body:   `{"order":["p1","p2"],"posts":{"p1":{"id":"p1"},"p2":{"id":"p2","message":"reply"}}}`,
		},
	})
	c := newRestClient(srv.URL, "tok", nil)

	list, err := c.GetPostThread(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetPostThread err = %v", err)
	}
	if len(list.Order) != 2 || list.Order[0] != "p1" || list.Order[1] != "p2" {
		t.Errorf("Order = %v, want [p1 p2]", list.Order)
	}
	if list.Posts["p2"].Message != "reply" {
		t.Errorf("thread p2 = %+v, want message=\"reply\"", list.Posts["p2"])
	}
	if got := rec.last().path; got != "/api/v4/posts/p1/thread" {
		t.Errorf("path = %q, want /api/v4/posts/p1/thread", got)
	}
}

func TestReaction_AddAndRemove(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"POST /api/v4/reactions":                          {status: 200, body: `{}`},
		"DELETE /api/v4/users/me/posts/p9/reactions/eyes": {status: 200, body: `{}`},
	})
	c := newRestClient(srv.URL, "tok", nil)

	if err := c.AddReaction(context.Background(), "U1", "p9", "eyes"); err != nil {
		t.Fatalf("AddReaction err = %v", err)
	}
	// Verify the reaction request payload.
	var got map[string]string
	if err := json.Unmarshal([]byte(rec.last().body), &got); err != nil {
		t.Fatalf("AddReaction body not a JSON object: %v", err)
	}
	if got["user_id"] != "U1" || got["post_id"] != "p9" || got["emoji_name"] != "eyes" {
		t.Errorf("AddReaction body = %s, want user_id=U1 post_id=p9 emoji_name=eyes", rec.last().body)
	}
	if rec.last().authHeader != "Bearer tok" {
		t.Errorf("AddReaction Authorization = %q, want \"Bearer tok\"", rec.last().authHeader)
	}

	if err := c.RemoveReaction(context.Background(), "p9", "eyes"); err != nil {
		t.Fatalf("RemoveReaction err = %v", err)
	}
	last := rec.last()
	if last.method != http.MethodDelete {
		t.Errorf("RemoveReaction method = %q, want DELETE", last.method)
	}
	if last.path != "/api/v4/users/me/posts/p9/reactions/eyes" {
		t.Errorf("RemoveReaction path = %q, want /api/v4/users/me/posts/p9/reactions/eyes", last.path)
	}
}

func TestGetUsersByIDs_OK(t *testing.T) {
	srv, rec := mmTestServer(t, map[string]mmTestRoute{
		"POST /api/v4/users/ids": {
			status: 200,
			body:   `[{"id":"u1","username":"alice"},{"id":"u2","username":"bob"}]`,
		},
	})
	c := newRestClient(srv.URL, "tok", nil)

	users, err := c.GetUsersByIDs(context.Background(), []string{"u1", "u2"})
	if err != nil {
		t.Fatalf("GetUsersByIDs err = %v", err)
	}
	if len(users) != 2 || users[0].ID != "u1" || users[0].Username != "alice" || users[1].ID != "u2" || users[1].Username != "bob" {
		t.Errorf("users = %+v, want u1/alice + u2/bob", users)
	}
	// Request body is a JSON array of ids.
	var got []string
	if err := json.Unmarshal([]byte(rec.last().body), &got); err != nil {
		t.Fatalf("request body is not a JSON array: %v", err)
	}
	if len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Errorf("request body = %s, want [\"u1\",\"u2\"]", rec.last().body)
	}
	if rec.last().method != http.MethodPost {
		t.Errorf("method = %q, want POST", rec.last().method)
	}
	if rec.last().contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rec.last().contentType)
	}
}

func TestGetUsersByIDs_EmptyShortCircuits(t *testing.T) {
	srv, rec := mmTestServer(t, nil)
	c := newRestClient(srv.URL, "tok", nil)

	users, err := c.GetUsersByIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetUsersByIDs(nil) err = %v", err)
	}
	if users != nil {
		t.Errorf("users = %v, want nil (no ids → no request)", users)
	}
	if rec.count() != 0 {
		t.Errorf("server received %d requests, want 0 (empty ids short-circuits)", rec.count())
	}
}

func TestApiError_StringContainsMessage(t *testing.T) {
	ae := &apiError{StatusCode: 401, Message: "Invalid or expired token"}
	s := ae.Error()
	if !strings.Contains(s, "Invalid or expired token") {
		t.Errorf("Error() = %q, want it to include the message", s)
	}
	if !strings.Contains(s, "401") {
		t.Errorf("Error() = %q, want it to include the status code", s)
	}
}

func TestApiError_EmptyMessageStillReadable(t *testing.T) {
	ae := &apiError{StatusCode: 500}
	s := ae.Error()
	if !strings.Contains(s, "500") {
		t.Errorf("Error() = %q, want it to include the status code", s)
	}
}

func TestDisplayName(t *testing.T) {
	cases := []struct {
		name string
		u    mmUser
		want string
	}{
		{"nickname preferred over first/last", mmUser{Nickname: "Nick", FirstName: "B", LastName: "Ot", Username: "bot"}, "Nick"},
		{"first plus last", mmUser{FirstName: "B", LastName: "Ot", Username: "bot"}, "B Ot"},
		{"first only", mmUser{FirstName: "B", Username: "bot"}, "B"},
		{"last only", mmUser{LastName: "Ot", Username: "bot"}, "Ot"},
		{"username fallback", mmUser{Username: "bot"}, "bot"},
		{"empty user", mmUser{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayName(tc.u); got != tc.want {
				t.Errorf("displayName(%+v) = %q, want %q", tc.u, got, tc.want)
			}
		})
	}
}
