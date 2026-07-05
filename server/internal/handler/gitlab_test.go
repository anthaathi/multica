package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDeriveMRState(t *testing.T) {
	cases := []struct {
		name  string
		state string
		draft bool
		want  string
	}{
		{"opened", "opened", false, "open"},
		{"opened_draft", "opened", true, "draft"},
		{"merged", "merged", false, "merged"},
		{"merged_ignores_draft", "merged", true, "merged"},
		{"closed", "closed", false, "closed"},
		{"closed_ignores_draft", "closed", true, "closed"},
		{"locked_maps_open", "locked", false, "open"},
		{"unknown_defaults_open", "weird", false, "open"},
		{"unknown_draft", "weird", true, "draft"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveMRState(tc.state, tc.draft); got != tc.want {
				t.Fatalf("deriveMRState(%q, %v) = %q, want %q", tc.state, tc.draft, got, tc.want)
			}
		})
	}
}

func TestDeriveMRMergeStatus(t *testing.T) {
	cases := []struct {
		name      string
		action    string
		status    string
		pushed    bool
		wantClear bool
		wantValue string // only checked when !wantClear
		wantValid bool
	}{
		{"open_clears", "open", "can_be_merged", false, true, "", false},
		{"reopen_clears", "reopen", "cannot_be_merged", false, true, "", false},
		{"push_clears", "update", "can_be_merged", true, true, "", false},
		{"concrete_value_written", "update", "can_be_merged", false, false, "can_be_merged", true},
		{"empty_preserves", "update", "", false, false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, clear := deriveMRMergeStatus(tc.action, tc.status, tc.pushed)
			if clear != tc.wantClear {
				t.Fatalf("clear = %v, want %v", clear, tc.wantClear)
			}
			if !tc.wantClear {
				if val.Valid != tc.wantValid {
					t.Fatalf("val.Valid = %v, want %v", val.Valid, tc.wantValid)
				}
				if tc.wantValid && val.String != tc.wantValue {
					t.Fatalf("val.String = %q, want %q", val.String, tc.wantValue)
				}
			}
		})
	}
}

func TestParseGitLabTime(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		valid bool
	}{
		{"empty", "", false},
		{"rfc3339", "2021-04-19T14:00:00Z", true},
		{"webhook_utc", "2021-04-19 14:00:00 UTC", true},
		{"webhook_offset", "2021-04-19 14:00:00 +0000", true},
		{"garbage", "not-a-time", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGitLabTime(tc.in)
			if got.Valid != tc.valid {
				t.Fatalf("parseGitLabTime(%q).Valid = %v, want %v", tc.in, got.Valid, tc.valid)
			}
		})
	}
}

func TestParseGitLabTimeRequiredFallsBackToNow(t *testing.T) {
	got := parseGitLabTimeRequired("")
	if !got.Valid {
		t.Fatal("expected a valid fallback timestamp")
	}
	if time.Since(got.Time) > time.Minute {
		t.Fatalf("fallback timestamp not near now: %v", got.Time)
	}
}

func TestSplitPathWithNamespace(t *testing.T) {
	cases := []struct {
		in            string
		projectName   string
		wantNamespace string
		wantProject   string
	}{
		{"group/proj", "proj", "group", "proj"},
		{"group/subgroup/proj", "proj", "group/subgroup", "proj"},
		{"proj", "proj", "", "proj"},
		{"", "fallback", "", "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ns, proj := splitPathWithNamespace(tc.in, tc.projectName)
			if ns != tc.wantNamespace || proj != tc.wantProject {
				t.Fatalf("splitPathWithNamespace(%q) = (%q, %q), want (%q, %q)", tc.in, ns, proj, tc.wantNamespace, tc.wantProject)
			}
		})
	}
}

func TestBaseURLFromWebURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://gitlab.example.com/group/proj", "https://gitlab.example.com"},
		{"https://gitlab.example.com:8443/g/p/-/merge_requests/1", "https://gitlab.example.com:8443"},
		{"http://localhost/g/p", "http://localhost"},
		{"", ""},
		{"not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := baseURLFromWebURL(tc.in); got != tc.want {
				t.Fatalf("baseURLFromWebURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGitlabMergeableState(t *testing.T) {
	cases := []struct {
		name string
		in   pgtype.Text
		want *string
	}{
		{"null", pgtype.Text{}, nil},
		{"empty", pgtype.Text{String: "", Valid: true}, nil},
		{"can_be_merged", pgtype.Text{String: "can_be_merged", Valid: true}, ptr("clean")},
		{"cannot_be_merged", pgtype.Text{String: "cannot_be_merged", Valid: true}, ptr("dirty")},
		{"passthrough", pgtype.Text{String: "checking", Valid: true}, ptr("checking")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gitlabMergeableState(tc.in)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("nil mismatch: got %v want %v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

func TestSignVerifyGitLabState(t *testing.T) {
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", "test-secret")
	ws := "11111111-1111-1111-1111-111111111111"
	token, err := signGitLabState(ws)
	if err != nil {
		t.Fatalf("signGitLabState: %v", err)
	}
	got, ok := verifyGitLabState(token)
	if !ok || got != ws {
		t.Fatalf("verifyGitLabState = (%q, %v), want (%q, true)", got, ok, ws)
	}
	// Tampered signature is rejected.
	if _, ok := verifyGitLabState(token + "x"); ok {
		t.Fatal("expected tampered token to be rejected")
	}
	// A different secret rejects the token.
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", "other-secret")
	if _, ok := verifyGitLabState(token); ok {
		t.Fatal("expected token signed with old secret to be rejected")
	}
}

func TestVerifyGitLabStateUnconfigured(t *testing.T) {
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", "")
	if _, ok := verifyGitLabState("a.b.c"); ok {
		t.Fatal("unconfigured verify should fail closed")
	}
}

func TestHostFromURL(t *testing.T) {
	cases := map[string]string{
		"https://gitlab.example.com":       "gitlab.example.com",
		"https://GitLab.Example.com/x":     "gitlab.example.com",
		"https://gitlab.example.com:8443/": "gitlab.example.com:8443",
	}
	for in, want := range cases {
		if got := hostFromURL(in); got != want {
			t.Fatalf("hostFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// gitlabTestBox builds a throwaway secretbox for encrypting connection secrets
// in tests (mirrors the Slack install test's testBox helper).
func gitlabTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// TestGitLabWebhook_MergedMR_AdvancesLinkedIssueToDone drives the full webhook
// path: a Merge Request Hook that declares a closing keyword mirrors the MR,
// links it to the referenced issue, and advances the issue to done on merge.
func TestGitLabWebhook_MergedMR_AdvancesLinkedIssueToDone(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()

	box := gitlabTestBox(t)
	prevBox := testHandler.GitLabBox
	testHandler.GitLabBox = box
	t.Cleanup(func() { testHandler.GitLabBox = prevBox })

	// Seed an issue we expect the webhook to close out.
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "MR auto-merge test",
		"status": "in_progress",
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	json.NewDecoder(w.Body).Decode(&created)

	const projectID int64 = 4242
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_merge_request WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM gitlab_merge_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM gitlab_connection WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, created.ID)
	})

	const secret = "gitlab-webhook-secret-abc"
	sealed, err := box.Seal([]byte(secret))
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	access, err := box.Seal([]byte("access-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	if _, err := testHandler.Queries.CreateGitLabConnection(ctx, db.CreateGitLabConnectionParams{
		WorkspaceID:            parseUUID(testWorkspaceID),
		GitlabBaseUrl:          "https://gitlab.example.com",
		GitlabUserID:           555,
		GitlabUsername:         "dev",
		AccessTokenEncrypted:   access,
		WebhookSecretEncrypted: sealed,
	}); err != nil {
		t.Fatalf("CreateGitLabConnection: %v", err)
	}

	body := map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "dev", "avatar_url": ""},
		"project": map[string]any{
			"id":                  projectID,
			"name":                "widget",
			"path_with_namespace": "acme/widget",
			"web_url":             "https://gitlab.example.com/acme/widget",
		},
		"object_attributes": map[string]any{
			"iid":           7,
			"title":         "Fix login " + created.Identifier,
			"description":   "Closes " + created.Identifier,
			"state":         "merged",
			"action":        "merge",
			"url":           "https://gitlab.example.com/acme/widget/-/merge_requests/7",
			"source_branch": "fix/login",
			"merge_status":  "can_be_merged",
			"draft":         false,
			"created_at":    "2026-04-28 00:00:00 UTC",
			"updated_at":    "2026-04-29 00:00:00 UTC",
			"merged_at":     "2026-04-29 00:00:00 UTC",
			"last_commit":   map[string]any{"id": "abc123"},
		},
	}
	raw, _ := json.Marshal(body)

	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab", bytes.NewReader(raw))
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", secret)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook: expected 202, got %d (%s)", rec.Code, rec.Body.String())
	}

	mr, err := testHandler.Queries.GetGitLabMergeRequest(ctx, db.GetGitLabMergeRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		ProjectID:   projectID,
		MrIid:       7,
	})
	if err != nil {
		t.Fatalf("GetGitLabMergeRequest: %v", err)
	}
	if mr.State != "merged" {
		t.Errorf("expected mr state merged, got %q", mr.State)
	}
	if mr.NamespacePath != "acme" || mr.ProjectPath != "widget" {
		t.Errorf("unexpected namespace/path: %q/%q", mr.NamespacePath, mr.ProjectPath)
	}

	linked, err := testHandler.Queries.ListMergeRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListMergeRequestsByIssue: %v", err)
	}
	if len(linked) != 1 {
		t.Fatalf("expected 1 linked MR, got %d", len(linked))
	}

	updated, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("expected issue status 'done', got %q", updated.Status)
	}
}

// TestGitLabWebhook_InvalidToken rejects a delivery whose X-Gitlab-Token does
// not match any connection for the delivering instance.
func TestGitLabWebhook_InvalidToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := gitlabTestBox(t)
	prevBox := testHandler.GitLabBox
	testHandler.GitLabBox = box
	t.Cleanup(func() { testHandler.GitLabBox = prevBox })

	body := map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"web_url": "https://gitlab.example.com/acme/widget"},
		"object_attributes": map[string]any{
			"iid": 1, "state": "opened", "created_at": "2026-04-28 00:00:00 UTC",
		},
	}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab", bytes.NewReader(raw))
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", "wrong-secret")
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown token, got %d", rec.Code)
	}
}

// TestGitLabWebhook_MalformedPayload returns 400 rather than panicking on
// unparseable JSON (API-compat rule: tolerate response/request drift).
func TestGitLabWebhook_MalformedPayload(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := gitlabTestBox(t)
	prevBox := testHandler.GitLabBox
	testHandler.GitLabBox = box
	t.Cleanup(func() { testHandler.GitLabBox = prevBox })

	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab", bytes.NewReader([]byte("{not json")))
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", "anything")
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed payload, got %d", rec.Code)
	}
}
