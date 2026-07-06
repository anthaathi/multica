package issuesync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// stubTokenConn returns a fixed token + the httptest server URL, bypassing the
// DB + secretbox so ListIssues can be exercised without a live connection row.
func stubTokenConn(token, baseURL string) func(context.Context, string) (string, string, error) {
	return func(_ context.Context, _ string) (string, string, error) {
		return token, baseURL, nil
	}
}

func newTestGitLabProvider(t *testing.T, token, baseURL string) *GitLabProvider {
	t.Helper()
	return &GitLabProvider{
		Client:      &http.Client{},
		tokenForConn: stubTokenConn(token, baseURL),
	}
}

func TestGitLabListIssues(t *testing.T) {
	var (
		gotAuth   string
		gotPath   string
		gotPage   string
		callCount int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/42/issues", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotPage = r.URL.Query().Get("page")

		// Page 1 returns 2 issues; page 2 returns an empty list (exhausted).
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{
					"id": 1001, "iid": 11, "title": "First", "description": "body-1",
					"state": "opened", "web_url": "https://gitlab.example.com/g/p/-/issues/11",
					"labels": [{"title": "bug"}, {"title": "urgent"}],
					"assignees": [{"id": 7, "username": "alice", "name": "Alice"}],
					"author": {"id": 5, "username": "bob"},
					"updated_at": "2026-07-01T10:00:00Z"
				},
				{
					"id": 1002, "iid": 12, "title": "Second", "description": "",
					"state": "opened", "web_url": "https://gitlab.example.com/g/p/-/issues/12",
					"labels": [],
					"author": {"id": 5, "username": "bob"},
					"updated_at": "2026-07-02T10:00:00Z"
				}
			]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newTestGitLabProvider(t, "tok-abc", srv.URL)
	src := db.IssueSyncSource{
		ExternalRef: json.RawMessage(`{"project_id":42,"path_with_namespace":"group/project"}`),
	}

	issues, next, err := p.ListIssues(context.Background(), src, "")
	if err != nil {
		t.Fatalf("ListIssues page 1: %v", err)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tok-abc")
	}
	if !strings.HasSuffix(gotPath, "/api/v4/projects/42/issues") {
		t.Errorf("path = %q", gotPath)
	}
	if gotPage != "1" {
		t.Errorf("page query = %q, want %q", gotPage, "1")
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].ID != "1001" || issues[0].Key != "#11" {
		t.Errorf("issue[0] = %+v", issues[0])
	}
	if issues[0].Title != "First" || issues[0].Description != "body-1" {
		t.Errorf("issue[0] content = %+v", issues[0])
	}
	if len(issues[0].Labels) != 2 || issues[0].Labels[0] != "bug" {
		t.Errorf("issue[0] labels = %v", issues[0].Labels)
	}
	if issues[0].Assignee == nil || issues[0].Assignee.AccountID != "7" || issues[0].Assignee.Login != "alice" {
		t.Errorf("issue[0] assignee = %+v", issues[0].Assignee)
	}
	if issues[0].Author == nil || issues[0].Author.AccountID != "5" {
		t.Errorf("issue[0] author = %+v", issues[0].Author)
	}
	if issues[0].WebURL != "https://gitlab.example.com/g/p/-/issues/11" {
		t.Errorf("issue[0] web_url = %q", issues[0].WebURL)
	}

	// Page 1 returned < 100 items so the cursor should be empty.
	if next != "" {
		t.Errorf("next cursor = %q, want empty (page had < 100 items)", next)
	}

	// Drive a second page explicitly to confirm cursor paging works.
	issues2, _, err := p.ListIssues(context.Background(), src, "2")
	if err != nil {
		t.Fatalf("ListIssues page 2: %v", err)
	}
	if len(issues2) != 0 {
		t.Errorf("page 2 returned %d issues, want 0", len(issues2))
	}
	if callCount != 2 {
		t.Errorf("server call count = %d, want 2", callCount)
	}
}

func TestGitLabListContainers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("membership") != "true" {
			t.Errorf("membership param = %q, want true", r.URL.Query().Get("membership"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id": 1, "name": "widget", "path_with_namespace": "acme/widget", "web_url": "https://gitlab.example.com/acme/widget"},
			{"id": 2, "name": "gadget", "path_with_namespace": "acme/sub/gadget", "web_url": "https://gitlab.example.com/acme/sub/gadget"}
		]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newTestGitLabProvider(t, "tok", srv.URL)
	containers, err := p.ListContainers(context.Background(), "conn-uuid")
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("got %d containers, want 2", len(containers))
	}
	if containers[0].Key != "acme/widget" {
		t.Errorf("container[0].Key = %q, want %q", containers[0].Key, "acme/widget")
	}
	pid, _ := containers[0].Ref["project_id"].(int64)
	if pid != 1 {
		t.Errorf("container[0].Ref project_id = %v, want 1", containers[0].Ref["project_id"])
	}
}

func TestGitLabIssueToExternal(t *testing.T) {
	t.Run("webhook_envelope", func(t *testing.T) {
		raw := json.RawMessage(`{
			"object_kind": "issue",
			"user": {"id": 5, "username": "bob"},
			"project": {"path_with_namespace": "acme/widget"},
			"object_attributes": {
				"id": 42, "iid": 7, "title": "Fix bug", "description": "details",
				"state": "opened", "action": "open",
				"url": "https://gitlab.example.com/acme/widget/-/issues/7",
				"updated_at": "2026-07-03T12:00:00Z",
				"labels": [{"title": "bug"}],
				"assignees": [{"id": 9, "username": "carol"}],
				"author": {"id": 5, "username": "bob"}
			}
		}`)
		ext, ok := GitLabIssueToExternal(raw)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if ext.ID != "42" || ext.Key != "#7" {
			t.Errorf("ID/Key = %q/%q, want 42/#7", ext.ID, ext.Key)
		}
		if ext.Title != "Fix bug" || ext.Description != "details" {
			t.Errorf("title/desc = %q/%q", ext.Title, ext.Description)
		}
		if ext.WebURL != "https://gitlab.example.com/acme/widget/-/issues/7" {
			t.Errorf("web_url = %q (expected 'url' from object_attributes)", ext.WebURL)
		}
		if len(ext.Labels) != 1 || ext.Labels[0] != "bug" {
			t.Errorf("labels = %v", ext.Labels)
		}
		if ext.Assignee == nil || ext.Assignee.AccountID != "9" {
			t.Errorf("assignee = %+v", ext.Assignee)
		}
	})

	t.Run("rest_response", func(t *testing.T) {
		raw := json.RawMessage(`{
			"id": 88, "iid": 3, "title": "REST issue", "description": "d",
			"state": "closed", "web_url": "https://gitlab.example.com/g/p/-/issues/3",
			"labels": [{"title": "done"}],
			"author": {"id": 1, "username": "dev"},
			"updated_at": "2026-07-04T08:00:00Z"
		}`)
		ext, ok := GitLabIssueToExternal(raw)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if ext.ID != "88" || ext.Key != "#3" {
			t.Errorf("ID/Key = %q/%q", ext.ID, ext.Key)
		}
		if ext.State != "closed" {
			t.Errorf("state = %q, want closed", ext.State)
		}
		if ext.WebURL != "https://gitlab.example.com/g/p/-/issues/3" {
			t.Errorf("web_url = %q", ext.WebURL)
		}
	})

	t.Run("top_level_labels_fallback", func(t *testing.T) {
		// Some GitLab deliveries put labels at the envelope top level rather
		// than inside object_attributes.
		raw := json.RawMessage(`{
			"object_attributes": {"id": 5, "iid": 1, "title": "t", "state": "opened", "url": "u"},
			"labels": [{"title": "env-label"}]
		}`)
		ext, ok := GitLabIssueToExternal(raw)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if len(ext.Labels) != 1 || ext.Labels[0] != "env-label" {
			t.Errorf("labels = %v, want [env-label]", ext.Labels)
		}
	})

	t.Run("missing_id", func(t *testing.T) {
		if _, ok := GitLabIssueToExternal(json.RawMessage(`{"iid": 1}`)); ok {
			t.Error("expected ok=false for missing id")
		}
		if _, ok := GitLabIssueToExternal(json.RawMessage(`not json`)); ok {
			t.Error("expected ok=false for bad json")
		}
	})
}

func TestGitLabCommentToExternal(t *testing.T) {
	t.Run("webhook_envelope", func(t *testing.T) {
		raw := json.RawMessage(`{
			"object_kind": "note",
			"user": {"id": 5, "username": "bob", "name": "Bob"},
			"object_attributes": {
				"id": 9001, "body": "looks good",
				"noteable_type": "Issue",
				"url": "https://gitlab.example.com/acme/widget/-/issues/7#note_9001",
				"updated_at": "2026-07-05T09:00:00Z"
			}
		}`)
		c, ok := GitLabCommentToExternal(raw)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if c.ID != "9001" || c.Body != "looks good" {
			t.Errorf("ID/Body = %q/%q", c.ID, c.Body)
		}
		if c.Author == nil || c.Author.AccountID != "5" || c.Author.Login != "bob" {
			t.Errorf("author = %+v (expected top-level user)", c.Author)
		}
		if c.WebURL == "" {
			t.Error("expected non-empty web_url")
		}
	})

	t.Run("rest_response", func(t *testing.T) {
		raw := json.RawMessage(`{
			"id": 200, "body": "rest comment",
			"author": {"id": 3, "username": "dave", "name": "Dave"},
			"updated_at": "2026-07-06T01:00:00Z"
		}`)
		c, ok := GitLabCommentToExternal(raw)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if c.ID != "200" {
			t.Errorf("ID = %q, want 200", c.ID)
		}
		if c.Author == nil || c.Author.AccountID != "3" {
			t.Errorf("author = %+v (expected embedded author)", c.Author)
		}
	})

	t.Run("missing_id", func(t *testing.T) {
		if _, ok := GitLabCommentToExternal(json.RawMessage(`{"body": "x"}`)); ok {
			t.Error("expected ok=false for missing id")
		}
	})
}

func TestGitLabContainerKey(t *testing.T) {
	if k := GitLabContainerKey("Acme/Widget"); k != "acme/widget" {
		t.Errorf("GitLabContainerKey = %q, want acme/widget", k)
	}
	if k := GitLabContainerKey("  Group/Sub/Proj  "); k != "group/sub/proj" {
		t.Errorf("GitLabContainerKey = %q, want group/sub/proj", k)
	}
}

func TestGitLabStateEvent(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"opened", "reopen"},
		{"closed", "close"},
		{"", ""},
		{"weird", ""},
	}
	for _, tc := range cases {
		if got := gitlabStateEvent(tc.state); got != tc.want {
			t.Errorf("gitlabStateEvent(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestGitLabIssueBody checks the outbound payload shape: GitLab assigns by
// numeric id (assignee_ids), not login.
func TestGitLabIssueBody(t *testing.T) {
	t.Run("with_assignee", func(t *testing.T) {
		out := OutboundIssue{
			Title: "T", Description: "D", State: "closed",
			Labels: []string{"bug"}, AssigneeAccountID: "42",
		}
		body := gitlabIssueBody(out)
		ids, ok := body["assignee_ids"].([]int64)
		if !ok || len(ids) != 1 || ids[0] != 42 {
			t.Errorf("assignee_ids = %v, want [42]", body["assignee_ids"])
		}
		if body["state_event"] != "close" {
			t.Errorf("state_event = %v, want close", body["state_event"])
		}
		labels, _ := body["labels"].([]string)
		if len(labels) != 1 || labels[0] != "bug" {
			t.Errorf("labels = %v", body["labels"])
		}
	})

	t.Run("no_assignee_nil_labels", func(t *testing.T) {
		out := OutboundIssue{Title: "T"}
		body := gitlabIssueBody(out)
		if _, exists := body["assignee_ids"]; exists {
			t.Error("assignee_ids should be absent when unassigned")
		}
		labels, _ := body["labels"].([]string)
		if labels == nil {
			t.Error("nil labels should normalize to empty slice, not nil")
		}
	})
}

// Ensure request bodies are actually JSON-marshaled and the server sees them.
func TestGitLabCreateIssueRoundTrip(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/42/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 555, "iid": 1, "title": "New", "description": "desc",
			"state": "opened", "web_url": "https://gitlab.example.com/g/p/-/issues/1",
			"updated_at": "2026-07-06T00:00:00Z"
		}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newTestGitLabProvider(t, "tok", srv.URL)
	src := db.IssueSyncSource{
		ExternalRef: json.RawMessage(`{"project_id":42,"path_with_namespace":"g/p"}`),
	}
	ext, err := p.CreateIssue(context.Background(), src, OutboundIssue{
		Title: "New", Description: "desc", Labels: []string{"x"}, AssigneeAccountID: "9",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if ext.ID != "555" || ext.Key != "#1" {
		t.Errorf("returned issue = %+v", ext)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("sent body not JSON: %v", err)
	}
	if sent["title"] != "New" {
		t.Errorf("sent title = %v", sent["title"])
	}
	if ids, _ := sent["assignee_ids"].([]any); len(ids) != 1 {
		t.Errorf("sent assignee_ids = %v", sent["assignee_ids"])
	}
	// state_event must NOT be sent on POST (issues start opened).
	if _, hasState := sent["state_event"]; hasState {
		t.Error("state_event should be absent on POST")
	}
}
