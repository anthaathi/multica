package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/integrations/issuesync"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestExtractCommentFields_AgentMapPayload pins the dual-shape contract shared
// with notification/subscriber listeners: TaskService.createAgentComment
// publishes comment as map[string]any, while HTTP CreateComment uses
// handler.CommentResponse. Outbound Jira/GitHub/GitLab sync must accept both.
func TestExtractCommentFields_AgentMapPayload(t *testing.T) {
	commentID := "11111111-1111-1111-1111-111111111111"
	issueID := "22222222-2222-2222-2222-222222222222"
	agentID := "33333333-3333-3333-3333-333333333333"

	got, ok := extractCommentFields(map[string]any{
		"id":          commentID,
		"issue_id":    issueID,
		"author_type": "agent",
		"author_id":   agentID,
		"content":     "fixed the login redirect",
		"type":        "comment",
	})
	if !ok {
		t.Fatal("expected agent map payload to extract")
	}
	if got.ID != commentID || got.IssueID != issueID {
		t.Fatalf("ids = %q/%q, want %q/%q", got.ID, got.IssueID, commentID, issueID)
	}
	if got.AuthorType != "agent" || got.AuthorID != agentID {
		t.Fatalf("author = %q/%q, want agent/%q", got.AuthorType, got.AuthorID, agentID)
	}
	if got.Content != "fixed the login redirect" {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestExtractCommentFields_CommentResponse(t *testing.T) {
	got, ok := extractCommentFields(handler.CommentResponse{
		ID:         "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		IssueID:    "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		AuthorType: "member",
		AuthorID:   "cccccccc-cccc-cccc-cccc-cccccccccccc",
		Content:    "looks good",
		Type:       "comment",
	})
	if !ok {
		t.Fatal("expected CommentResponse to extract")
	}
	if got.AuthorType != "member" || got.Content != "looks good" {
		t.Fatalf("got author=%q content=%q", got.AuthorType, got.Content)
	}
}

func TestExtractCommentFields_RejectsEmpty(t *testing.T) {
	if _, ok := extractCommentFields(map[string]any{"content": "no ids"}); ok {
		t.Fatal("expected empty id map to be rejected")
	}
	if _, ok := extractCommentFields(handler.CommentResponse{Content: "no ids"}); ok {
		t.Fatal("expected empty CommentResponse to be rejected")
	}
	if _, ok := extractCommentFields("not a comment"); ok {
		t.Fatal("expected non-comment value to be rejected")
	}
}

// TestIssueSync_AgentMapCommentEnqueuesPushComment is the end-to-end
// regression for ANT-500: agent comments published via TaskService as
// map[string]any must enqueue issue_sync_outbox push_comment rows, same as
// member comments that travel as handler.CommentResponse.
func TestIssueSync_AgentMapCommentEnqueuesPushComment(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	// Older / partially-migrated environments may not have issue sync tables yet.
	var hasSourceTable bool
	if err := testPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'issue_sync_source'
		)
	`).Scan(&hasSourceTable); err != nil || !hasSourceTable {
		t.Skip("issue_sync_source table not available")
	}
	queries := db.New(testPool)
	engine := &issuesync.Engine{Queries: queries}

	projectID, issueID, sourceID, agentID, commentID := seedIssueSyncCommentFixture(t)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_sync_outbox WHERE issue_id = $1`, issueID)
		_, _ = testPool.Exec(ctx, `DELETE FROM external_issue_link WHERE issue_id = $1`, issueID)
		_, _ = testPool.Exec(ctx, `DELETE FROM comment WHERE id = $1`, commentID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_sync_source WHERE id = $1`, sourceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
		_, _ = testPool.Exec(ctx, `DELETE FROM project WHERE id = $1`, projectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID)
	})

	bus := events.New()
	registerIssueSyncListeners(bus, queries, engine)

	// Agent task path: map payload (the shape that previously dropped silently).
	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "agent",
		ActorID:     agentID,
		Payload: map[string]any{
			"comment": map[string]any{
				"id":          commentID,
				"issue_id":    issueID,
				"author_type": "agent",
				"author_id":   agentID,
				"content":     "agent result comment",
				"type":        "comment",
			},
			"issue_title":  "sync fixture",
			"issue_status": "todo",
		},
	})

	assertPushCommentEnqueued(t, issueID, commentID, "Grok")

	// Clear outbox and re-publish via HTTP CommentResponse shape to prove
	// the dual path still works after the map fix.
	if _, err := testPool.Exec(ctx, `DELETE FROM issue_sync_outbox WHERE issue_id = $1`, issueID); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}
	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"comment": handler.CommentResponse{
				ID:         commentID,
				IssueID:    issueID,
				AuthorType: "member",
				AuthorID:   testUserID,
				Content:    "member follow-up",
				Type:       "comment",
			},
		},
	})
	assertPushCommentEnqueued(t, issueID, commentID, "Integration Tester")
}
func seedIssueSyncCommentFixture(t *testing.T) (projectID, issueID, sourceID, agentID, commentID string) {
	t.Helper()
	ctx := context.Background()
	queries := db.New(testPool)

	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, status, priority)
		VALUES ($1, 'issuesync comment fixture', 'planned', 'none')
		RETURNING id::text
	`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, title, status, priority, creator_type, creator_id, position, number)
		VALUES ($1, $2, 'sync fixture', 'todo', 'medium', 'member', $3, 0,
		        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id::text
	`, testWorkspaceID, projectID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, last_seen_at
		)
		VALUES ($1, NULL, 'issuesync fixture runtime', 'local', 'mul-ant500', 'offline', '{}'::jsonb, '{}'::jsonb, now())
		RETURNING id::text
	`, testWorkspaceID).Scan(&runtimeID); err != nil {
		// Older schemas used different provider/device columns; fall back to a
		// minimal shape if the full insert fails in this environment.
		if err2 := testPool.QueryRow(ctx, `
			INSERT INTO agent_runtime (workspace_id, name, runtime_mode, status, device_info, metadata, last_seen_at)
			VALUES ($1, 'issuesync fixture runtime', 'local', 'offline', '{}'::jsonb, '{}'::jsonb, now())
			RETURNING id::text
		`, testWorkspaceID).Scan(&runtimeID); err2 != nil {
			t.Fatalf("seed runtime: %v / %v", err, err2)
		}
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, 'Grok', 'issuesync agent fixture', 'local', '{}'::jsonb, $2, 'workspace', 1, $3)
		RETURNING id::text
	`, testWorkspaceID, runtimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'agent', $3, 'agent result comment', 'comment')
		RETURNING id::text
	`, issueID, testWorkspaceID, agentID).Scan(&commentID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	connID := uuid.New()
	src, err := queries.CreateIssueSyncSource(ctx, db.CreateIssueSyncSourceParams{
		WorkspaceID:   util.MustParseUUID(testWorkspaceID),
		ProjectID:     util.MustParseUUID(projectID),
		Provider:      "jira",
		ConnectionID:  util.MustParseUUID(connID.String()),
		ExternalRef:   []byte(`{"project_id":"10001","key":"ANT"}`),
		ExternalKey:   "ANT",
		StatusMapping: []byte(`{}`),
		PushDefault:   true,
		CreatedBy:     util.MustParseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("seed sync source: %v", err)
	}
	sourceID = util.UUIDToString(src.ID)

	if _, err := queries.CreateExternalIssueLink(ctx, db.CreateExternalIssueLinkParams{
		WorkspaceID:    util.MustParseUUID(testWorkspaceID),
		IssueID:        util.MustParseUUID(issueID),
		SyncSourceID:   src.ID,
		ExternalID:     "10042",
		ExternalKey:    "ANT-42",
		WebUrl:         "https://example.atlassian.net/browse/ANT-42",
		LastPushedHash: "",
	}); err != nil {
		t.Fatalf("seed external link: %v", err)
	}
	return projectID, issueID, sourceID, agentID, commentID
}

func assertPushCommentEnqueued(t *testing.T, issueID, commentID, wantAuthorLabel string) {
	t.Helper()
	var op string
	var payload []byte
	err := testPool.QueryRow(context.Background(), `
		SELECT op, payload
		FROM issue_sync_outbox
		WHERE issue_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, issueID).Scan(&op, &payload)
	if err != nil {
		t.Fatalf("load outbox row: %v", err)
	}
	if op != "push_comment" {
		t.Fatalf("op = %q, want push_comment", op)
	}
	var body struct {
		CommentID   string `json:"comment_id"`
		AuthorLabel string `json:"author_label"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body.CommentID != commentID {
		t.Fatalf("comment_id = %q, want %q", body.CommentID, commentID)
	}
	if body.AuthorLabel != wantAuthorLabel {
		t.Fatalf("author_label = %q, want %q", body.AuthorLabel, wantAuthorLabel)
	}
}
