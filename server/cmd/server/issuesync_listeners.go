package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/integrations/issuesync"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// registerIssueSyncListeners wires outbound event-bus listeners that mirror
// local issue/comment changes to attached external trackers. Every listener
// skips events the sync engine itself caused (ActorType == "issue_sync") —
// that is echo-suppression layer 1, keeping inbound applies from re-enqueueing.
//
// The listeners are cheap: they resolve the issue's project, list its sync
// sources, and enqueue outbox rows. The actual provider push happens in the
// outbox worker (Engine.RunOutbox), which reads fresh state at dispatch time
// so a burst of edits collapses into one push.
func registerIssueSyncListeners(bus *events.Bus, queries *db.Queries, engine *issuesync.Engine) {
	if engine == nil {
		return
	}
	ctx := context.Background()

	// issue:created — mirror a brand-new issue to the project's push_default
	// source (the one new local issues are assumed to originate on). Issues
	// that already have a link (remote created first, webhook raced ahead)
	// get a push_issue instead.
	bus.Subscribe(protocol.EventIssueCreated, func(e events.Event) {
		if e.ActorType == issuesync.ActorTypeSync {
			return
		}
		issueID, ok := issueIDFromPayload(e)
		if !ok {
			return
		}
		enqueueIssueOutbox(ctx, queries, engine, issueID, true)
	})

	// issue:updated — push title/description/status/assignee/labels to every
	// source that already mirrors this issue.
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		if e.ActorType == issuesync.ActorTypeSync {
			return
		}
		issueID, ok := issueIDFromPayload(e)
		if !ok {
			return
		}
		enqueueIssueOutbox(ctx, queries, engine, issueID, false)
	})

	// issue_labels:changed — label divergence is part of push_issue, so this
	// is the same enqueue as an update.
	bus.Subscribe(protocol.EventIssueLabelsChanged, func(e events.Event) {
		if e.ActorType == issuesync.ActorTypeSync {
			return
		}
		issueID, ok := issueIDFromPayload(e)
		if !ok {
			return
		}
		enqueueIssueOutbox(ctx, queries, engine, issueID, false)
	})

	// comment:created / comment:updated — mirror the comment to every source
	// that mirrors the parent issue. Attribution is resolved here (the listener
	// can join user/agent names) and carried in the payload.
	bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		if e.ActorType == issuesync.ActorTypeSync {
			return
		}
		enqueueCommentOutbox(ctx, queries, engine, e)
	})
	bus.Subscribe(protocol.EventCommentUpdated, func(e events.Event) {
		if e.ActorType == issuesync.ActorTypeSync {
			return
		}
		enqueueCommentOutbox(ctx, queries, engine, e)
	})
}

// issueIDFromPayload extracts the issue UUID from an issue:created/updated or
// issue_labels:changed payload. created/updated nest a full IssueResponse under
// "issue"; labels:changed carries a bare "issue_id" string.
func issueIDFromPayload(e events.Event) (pgtype.UUID, bool) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return pgtype.UUID{}, false
	}
	if issue, ok := payload["issue"].(handler.IssueResponse); ok && issue.ID != "" {
		if id, err := util.ParseUUID(issue.ID); err == nil {
			return id, true
		}
	}
	if idStr, ok := payload["issue_id"].(string); ok && idStr != "" {
		if id, err := util.ParseUUID(idStr); err == nil {
			return id, true
		}
	}
	return pgtype.UUID{}, false
}

// enqueueIssueOutbox finds the issue's project sync sources and enqueues the
// right op per source. isCreate distinguishes a brand-new issue (create_remote
// on the push_default source when no link exists yet) from an update
// (push_issue on sources that already mirror the issue).
func enqueueIssueOutbox(ctx context.Context, queries *db.Queries, engine *issuesync.Engine, issueID pgtype.UUID, isCreate bool) {
	issue, err := queries.GetIssue(ctx, issueID)
	if err != nil {
		return
	}
	if !issue.ProjectID.Valid {
		return // issues outside a project have no sync sources
	}
	sources, err := queries.ListIssueSyncSourcesByProject(ctx, issue.ProjectID)
	if err != nil {
		slog.Warn("issuesync listener: list sources failed", "issue_id", util.UUIDToString(issueID), "error", err)
		return
	}
	for _, src := range sources {
		if !src.SyncEnabled {
			continue
		}
		_, err := queries.GetExternalIssueLinkForIssue(ctx, db.GetExternalIssueLinkForIssueParams{
			IssueID:      issue.ID,
			SyncSourceID: src.ID,
		})
		if err == nil {
			// Already mirrored — push the update.
			if err := engine.EnqueueOutbound(ctx, src, issue.ID, "push_issue", nil); err != nil {
				slog.Warn("issuesync listener: enqueue push_issue failed", "error", err)
			}
			continue
		}
		// No link yet. Only the push_default source creates the remote issue;
		// other sources wait for the inbound backfill/webhook to establish it.
		if isCreate && src.PushDefault {
			if err := engine.EnqueueOutbound(ctx, src, issue.ID, "create_remote", nil); err != nil {
				slog.Warn("issuesync listener: enqueue create_remote failed", "error", err)
			}
		}
	}
}

// enqueueCommentOutbox mirrors a local comment to every source that mirrors
// the parent issue. The author label (member/agent display name) is resolved
// here so the outbox worker doesn't re-fetch user rows at dispatch time.
//
// Comment payloads arrive in two shapes:
//   - handler.CommentResponse from the HTTP CreateComment/UpdateComment path
//   - map[string]any from TaskService.createAgentComment (agent task fallback
//     and system failure comments). The map form must be accepted or agent
//     comments never reach external trackers (Jira/GitHub/GitLab).
func enqueueCommentOutbox(ctx context.Context, queries *db.Queries, engine *issuesync.Engine, e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	comment, ok := extractCommentFields(payload["comment"])
	if !ok {
		return
	}
	issueID, err := util.ParseUUID(comment.IssueID)
	if err != nil {
		return
	}
	issue, err := queries.GetIssue(ctx, issueID)
	if err != nil || !issue.ProjectID.Valid {
		return
	}
	sources, err := queries.ListIssueSyncSourcesByProject(ctx, issue.ProjectID)
	if err != nil {
		return
	}
	authorLabel := resolveAuthorLabel(ctx, queries, comment.AuthorType, comment.AuthorID)
	for _, src := range sources {
		if !src.SyncEnabled {
			continue
		}
		// Only push comments to sources that already mirror the issue —
		// commenting on an un-mirrored issue does not retroactively create it.
		if _, err := queries.GetExternalIssueLinkForIssue(ctx, db.GetExternalIssueLinkForIssueParams{
			IssueID:      issueID,
			SyncSourceID: src.ID,
		}); err != nil {
			continue
		}
		if err := engine.EnqueueOutbound(ctx, src, issueID, "push_comment", map[string]any{
			"comment_id":   comment.ID,
			"author_label": authorLabel,
		}); err != nil {
			slog.Warn("issuesync listener: enqueue push_comment failed", "error", err)
		}
	}
}

// extractCommentFields normalizes a comment payload that may be either a
// handler.CommentResponse (HTTP path) or a map[string]any (TaskService
// createAgentComment path) into the fields needed for outbound enqueue.
func extractCommentFields(v any) (handler.CommentResponse, bool) {
	switch c := v.(type) {
	case handler.CommentResponse:
		if c.ID == "" || c.IssueID == "" {
			return handler.CommentResponse{}, false
		}
		return c, true
	case map[string]any:
		comment := handler.CommentResponse{}
		comment.ID, _ = c["id"].(string)
		comment.IssueID, _ = c["issue_id"].(string)
		comment.AuthorType, _ = c["author_type"].(string)
		comment.AuthorID, _ = c["author_id"].(string)
		comment.Content, _ = c["content"].(string)
		comment.Type, _ = c["type"].(string)
		if comment.ID == "" || comment.IssueID == "" {
			return handler.CommentResponse{}, false
		}
		return comment, true
	default:
		return handler.CommentResponse{}, false
	}
}

// resolveAuthorLabel returns a display name for the comment author so the
// mirrored comment keeps attribution ("Jane (via Multica): ..."). Empty when
// the author is a system account or the lookup fails — the outbox then omits
// the prefix.
func resolveAuthorLabel(ctx context.Context, queries *db.Queries, authorType, authorID string) string {
	if authorID == "" {
		return ""
	}
	id, err := util.ParseUUID(authorID)
	if err != nil {
		return ""
	}
	switch authorType {
	case "member", "user":
		if u, err := queries.GetUser(ctx, id); err == nil && u.Name != "" {
			return u.Name
		}
	case "agent":
		if a, err := queries.GetAgent(ctx, id); err == nil && a.Name != "" {
			return a.Name
		}
	}
	return ""
}
