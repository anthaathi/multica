package issuesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestJiraContainerKey(t *testing.T) {
	if got := JiraContainerKey("proj"); got != "PROJ" {
		t.Fatalf("JiraContainerKey(proj) = %q, want PROJ", got)
	}
	if got := JiraContainerKey("  MixedCase  "); got != "MIXEDCASE" {
		t.Fatalf("JiraContainerKey = %q, want MIXEDCASE", got)
	}
}

func TestJiraIssueToExternal(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "10042",
		"key": "PROJ-7",
		"fields": {
			"summary": "Fix the login redirect",
			"description": {"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Steps to repro"}]}]},
			"status": {"name": "In Progress", "statusCategory": {"key": "indeterminate"}},
			"labels": ["bug", "ui"],
			"assignee": {"accountId": "5b10ac", "displayName": "Alice", "emailAddress": "alice@example.com", "avatarUrl": "https://x/a.png"},
			"reporter": {"accountId": "5b11bd", "displayName": "Bob"},
			"project": {"id": "10000", "key": "PROJ", "name": "Project"},
			"updated": "2026-07-01T10:00:00.000+0000"
		}
	}`)
	ext, ok := JiraIssueToExternal(raw)
	if !ok {
		t.Fatal("expected ok for a valid issue")
	}
	if ext.ID != "10042" {
		t.Errorf("ID = %q, want 10042", ext.ID)
	}
	if ext.Key != "PROJ-7" {
		t.Errorf("Key = %q, want PROJ-7", ext.Key)
	}
	if ext.Title != "Fix the login redirect" {
		t.Errorf("Title = %q", ext.Title)
	}
	if ext.Description != "Steps to repro" {
		t.Errorf("Description = %q, want Steps to repro", ext.Description)
	}
	// State MUST be the status-category key (what mapping.go keys on), not the
	// workflow-specific status name ("In Progress").
	if ext.State != "indeterminate" {
		t.Errorf("State = %q, want indeterminate (statusCategory key)", ext.State)
	}
	if len(ext.Labels) != 2 || ext.Labels[0] != "bug" || ext.Labels[1] != "ui" {
		t.Errorf("Labels = %v, want [bug ui]", ext.Labels)
	}
	if ext.Assignee == nil || ext.Assignee.AccountID != "5b10ac" || ext.Assignee.DisplayName != "Alice" {
		t.Errorf("Assignee = %+v", ext.Assignee)
	}
	if ext.Author == nil || ext.Author.AccountID != "5b11bd" {
		t.Errorf("Author = %+v, want reporter 5b11bd", ext.Author)
	}
	if ext.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be parsed")
	}
}

func TestJiraIssueToExternalNullDescriptionAndDoneCategory(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "10043",
		"key": "PROJ-8",
		"fields": {
			"summary": "Closed one",
			"description": null,
			"status": {"name": "Done", "statusCategory": {"key": "done"}},
			"updated": "2026-07-02T12:00:00Z"
		}
	}`)
	ext, ok := JiraIssueToExternal(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if ext.Description != "" {
		t.Errorf("Description = %q, want empty for null", ext.Description)
	}
	if ext.State != "done" {
		t.Errorf("State = %q, want done", ext.State)
	}
}

// POST /issue returns {id, key, self} with no fields block. Conversion must
// not panic on the missing status and must still yield the identity so the
// link row can be recorded (otherwise the outbox retries the create and
// duplicates the remote issue).
func TestJiraIssueToExternalCreateResponseWithoutFields(t *testing.T) {
	raw := json.RawMessage(`{"id":"10802","key":"SCRUM-257","self":"https://x.atlassian.net/rest/api/3/issue/10802"}`)
	ext, ok := JiraIssueToExternal(raw)
	if !ok {
		t.Fatal("expected ok for a payload with an id")
	}
	if ext.ID != "10802" || ext.Key != "SCRUM-257" {
		t.Errorf("identity = %q/%q, want 10802/SCRUM-257", ext.ID, ext.Key)
	}
	if ext.State != "" {
		t.Errorf("State = %q, want empty when status is absent", ext.State)
	}
}

func TestJiraIssueToExternalMissingID(t *testing.T) {
	if _, ok := JiraIssueToExternal(json.RawMessage(`{"key":"X-1","fields":{}}`)); ok {
		t.Fatal("expected ok=false for missing id")
	}
	if _, ok := JiraIssueToExternal(json.RawMessage(`{not json`)); ok {
		t.Fatal("expected ok=false for bad json")
	}
}

func TestJiraCommentToExternal(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "9001",
		"body": {"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Looks good","marks":[{"type":"strong"}]}]}]},
		"author": {"accountId": "5a", "displayName": "Carol"},
		"updateAuthor": {"accountId": "5b", "displayName": "Dave"},
		"updated": "2026-07-03T09:30:00.000+0000"
	}`)
	c, ok := JiraCommentToExternal(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if c.ID != "9001" {
		t.Errorf("ID = %q", c.ID)
	}
	if c.Body != "**Looks good**" {
		t.Errorf("Body = %q, want **Looks good**", c.Body)
	}
	// updateAuthor is preferred over author (the last editor).
	if c.Author == nil || c.Author.AccountID != "5b" || c.Author.DisplayName != "Dave" {
		t.Errorf("Author = %+v, want updateAuthor Dave", c.Author)
	}
}

// ── Refresh-token rotation ───────────────────────────────────────────────────

// mockJiraDB captures the args of the last Exec so a test can assert what was
// persisted without a live database. Only Exec is exercised by
// UpdateJiraConnectionTokens.
type mockJiraDB struct {
	execArgs []any
}

func (m *mockJiraDB) Exec(_ context.Context, _ string, args ...interface{}) (pgconn.CommandTag, error) {
	m.execArgs = args
	return pgconn.CommandTag{}, nil
}

func (m *mockJiraDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, nil
}

func (m *mockJiraDB) QueryRow(context.Context, string, ...interface{}) pgx.Row { return nil }

// TestJiraRefreshTokenRotation verifies the CRITICAL rotation contract: a
// refresh-token exchange persists the NEW refresh_token from the response (Jira
// Cloud rotates refresh tokens and invalidates the old one). If the old token
// were re-persisted, every subsequent refresh — and thus every API call after
// the first expiry — would fail with invalid_grant.
func TestJiraRefreshTokenRotation(t *testing.T) {
	// Token endpoint returning a ROTATED refresh token.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer ts.Close()
	orig := jiraTokenEndpoint
	jiraTokenEndpoint = ts.URL
	t.Cleanup(func() { jiraTokenEndpoint = orig })

	box, err := secretbox.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	oldRefreshSealed, err := box.Seal([]byte("old-refresh"))
	if err != nil {
		t.Fatalf("seal old refresh: %v", err)
	}

	conn := db.JiraConnection{
		ID:                    pgtype.UUID{Valid: true},
		RefreshTokenEncrypted: oldRefreshSealed,
		TokenExpiresAt:        pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, // expired
	}
	mock := &mockJiraDB{}
	p := &JiraProvider{Queries: db.New(mock), Box: box}

	tok, err := p.refreshAndStore(context.Background(), conn)
	if err != nil {
		t.Fatalf("refreshAndStore: %v", err)
	}
	if tok != "new-access" {
		t.Fatalf("returned token = %q, want new-access", tok)
	}
	// UpdateJiraConnectionTokens Exec args: [id, access_encrypted, refresh_encrypted, expires_at]
	if len(mock.execArgs) < 3 {
		t.Fatalf("expected at least 3 exec args, got %d (%v)", len(mock.execArgs), mock.execArgs)
	}
	sealed, ok := mock.execArgs[2].([]byte)
	if !ok {
		t.Fatalf("refresh token arg is %T, want []byte", mock.execArgs[2])
	}
	plain, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("decrypt persisted refresh token: %v", err)
	}
	if string(plain) != "rotated-refresh" {
		t.Fatalf("persisted refresh token = %q, want rotated-refresh — rotation MUST persist the new token", string(plain))
	}
	if string(plain) == "old-refresh" {
		t.Fatal("FAIL: the OLD refresh token was persisted; the rotated one must be written or the connection bricks on the next refresh")
	}
}
