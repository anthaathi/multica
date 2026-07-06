package issuesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ActorTypeSync is the event-bus ActorType stamped on every write the sync
// engine performs. The outbound listeners in cmd/server skip events carrying
// it, which is echo-suppression layer 1.
const ActorTypeSync = "issue_sync"

// Engine applies inbound remote events to local issues and enqueues outbound
// pushes. One Engine serves all providers; provider-specific transport lives
// behind the Provider interface.
type Engine struct {
	Queries      *db.Queries
	Bus          *events.Bus
	IssueService *service.IssueService
	Providers    map[string]Provider

	// IssuePayload/CommentPayload build the full WS broadcast payloads for
	// engine-driven writes. They are injected from cmd/server (which can see
	// the handler package's response builders) so this package does not
	// depend on handler types. Nil builders degrade to minimal
	// {"issue_id": ...} payloads — enough for cache invalidation.
	IssuePayload   func(ctx context.Context, issue db.Issue) map[string]any
	CommentPayload func(ctx context.Context, c db.Comment, issue db.Issue) map[string]any
}

// Provider returns the registered provider for a source, or nil.
func (e *Engine) Provider(name string) Provider {
	if e == nil || e.Providers == nil {
		return nil
	}
	return e.Providers[name]
}

// ContentHash fingerprints the syncable content of an issue. The same
// function fingerprints outbound pushes (stored as last_pushed_hash) and
// inbound webhook payloads, so an inbound event that hashes to the stored
// value is our own write echoing back.
func ContentHash(title, description, state string, labels []string, assigneeAccountID string) string {
	ls := append([]string(nil), labels...)
	sort.Strings(ls)
	h := sha256.New()
	for _, part := range []string{title, description, state, strings.Join(ls, ","), assigneeAccountID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ApplyRemote applies one normalized inbound event against the given source.
// It is idempotent: replays and echoes are dropped by the layered checks
// documented in the package comment.
func (e *Engine) ApplyRemote(ctx context.Context, src db.IssueSyncSource, evt IssueEvent) error {
	if !src.SyncEnabled {
		return nil
	}
	switch evt.Kind {
	case "issue":
		return e.applyRemoteIssue(ctx, src, evt.Issue)
	case "comment":
		if evt.Comment == nil {
			return errors.New("issuesync: comment event without comment")
		}
		return e.applyRemoteComment(ctx, src, evt.Issue, *evt.Comment)
	default:
		return fmt.Errorf("issuesync: unknown event kind %q", evt.Kind)
	}
}

func (e *Engine) applyRemoteIssue(ctx context.Context, src db.IssueSyncSource, remote ExternalIssue) error {
	link, err := e.Queries.GetExternalIssueLinkByExternalID(ctx, db.GetExternalIssueLinkByExternalIDParams{
		SyncSourceID: src.ID,
		ExternalID:   remote.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return e.createLocalIssue(ctx, src, remote)
	}
	if err != nil {
		return fmt.Errorf("issuesync: load link: %w", err)
	}

	// Layer 3: stale replay. Webhook deliveries are not ordered; anything at
	// or before the last applied remote clock is a duplicate.
	if link.RemoteUpdatedAt.Valid && !remote.UpdatedAt.After(link.RemoteUpdatedAt.Time) {
		return nil
	}
	// Layer 2: our own write echoing back.
	assigneeAccount := ""
	if remote.Assignee != nil {
		assigneeAccount = remote.Assignee.AccountID
	}
	if link.LastPushedHash != "" && ContentHash(remote.Title, remote.Description, remote.State, remote.Labels, assigneeAccount) == link.LastPushedHash {
		return e.touchRemoteState(ctx, link, remote)
	}

	issue, err := e.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
		ID:          link.IssueID,
		WorkspaceID: src.WorkspaceID,
	})
	if err != nil {
		return fmt.Errorf("issuesync: load linked issue: %w", err)
	}

	titleChanged := remote.Title != "" && remote.Title != issue.Title
	descChanged := remote.Description != issue.Description.String
	newStatus, statusChanged := MapInboundStatus(src, remote.State, issue.Status)
	assigneeType, assigneeID := issue.AssigneeType, issue.AssigneeID
	assigneeChanged := false
	if mappedUser, ok := e.resolveAssignee(ctx, src, remote.Assignee); ok {
		if !issue.AssigneeID.Valid || issue.AssigneeID != mappedUser {
			assigneeType = util.StrToText("member")
			assigneeID = mappedUser
			assigneeChanged = true
		}
	} else if remote.Assignee == nil && issue.AssigneeType.Valid && issue.AssigneeType.String == "member" {
		// Remote explicitly unassigned; only clear member assignees — agent
		// assignments are a local concept the remote side cannot see.
		assigneeType, assigneeID = pgtype.Text{}, pgtype.UUID{}
		assigneeChanged = true
	}

	if titleChanged || descChanged || statusChanged || assigneeChanged {
		params := db.UpdateIssueParams{
			ID:            issue.ID,
			AssigneeType:  assigneeType,
			AssigneeID:    assigneeID,
			StartDate:     issue.StartDate,
			DueDate:       issue.DueDate,
			ParentIssueID: issue.ParentIssueID,
			ProjectID:     issue.ProjectID,
			Stage:         issue.Stage,
		}
		if titleChanged {
			params.Title = util.StrToText(remote.Title)
		}
		if descChanged {
			params.Description = util.StrToText(remote.Description)
		}
		if statusChanged {
			params.Status = util.StrToText(newStatus)
		}
		updated, err := e.Queries.UpdateIssue(ctx, params)
		if err != nil {
			return fmt.Errorf("issuesync: update issue: %w", err)
		}
		e.publishIssueUpdated(ctx, updated, issue.Status, statusChanged, descChanged, assigneeChanged)
		issue = updated
	}

	if err := e.syncLabels(ctx, issue, remote.Labels); err != nil {
		slog.Warn("issuesync: label sync failed",
			"issue_id", util.UUIDToString(issue.ID), "error", err)
	}

	return e.touchRemoteState(ctx, link, remote)
}

func (e *Engine) touchRemoteState(ctx context.Context, link db.ExternalIssueLink, remote ExternalIssue) error {
	key := remote.Key
	if key == "" {
		key = link.ExternalKey
	}
	url := remote.WebURL
	if url == "" {
		url = link.WebUrl
	}
	return e.Queries.UpdateExternalIssueLinkRemoteState(ctx, db.UpdateExternalIssueLinkRemoteStateParams{
		ID:              link.ID,
		RemoteUpdatedAt: pgtype.Timestamptz{Time: remote.UpdatedAt, Valid: !remote.UpdatedAt.IsZero()},
		ExternalKey:     key,
		WebUrl:          url,
	})
}

// createLocalIssue mirrors a remote issue that has no local counterpart yet.
// The creator is the member who attached the sync source — external trackers
// have no Multica identity, and creator_id is NOT NULL. Sources whose creator
// row was deleted skip creation rather than invent an author.
func (e *Engine) createLocalIssue(ctx context.Context, src db.IssueSyncSource, remote ExternalIssue) error {
	if !src.CreatedBy.Valid {
		slog.Warn("issuesync: skip remote issue create, source has no creator",
			"source_id", util.UUIDToString(src.ID), "external_id", remote.ID)
		return nil
	}
	status := "todo"
	if mapped, ok := MapInboundStatus(src, remote.State, ""); ok {
		status = mapped
	}
	assigneeType, assigneeID := pgtype.Text{}, pgtype.UUID{}
	if mappedUser, ok := e.resolveAssignee(ctx, src, remote.Assignee); ok {
		assigneeType = util.StrToText("member")
		assigneeID = mappedUser
	}
	// Resolve subtask parent: if this issue has a parent on the remote side,
	// find the corresponding local issue via the link table and link it.
	var parentIssueID pgtype.UUID
	if remote.ParentExternalID != "" {
		if parentLink, err := e.Queries.GetExternalIssueLinkByExternalID(ctx, db.GetExternalIssueLinkByExternalIDParams{
			SyncSourceID: src.ID,
			ExternalID:   remote.ParentExternalID,
		}); err == nil {
			parentIssueID = parentLink.IssueID
		}
	}
	res, err := e.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:  src.WorkspaceID,
		Title:        remote.Title,
		Description:  util.StrToText(remote.Description),
		Status:       status,
		Priority:     "none",
		AssigneeType: assigneeType,
		AssigneeID:   assigneeID,
		CreatorType:  "member",
		CreatorID:    src.CreatedBy,
		ProjectID:    src.ProjectID,
		ParentIssueID: parentIssueID,
		OriginType:   util.StrToText("issue_sync"),
		// Backfill re-imports must not trip the duplicate guard on retry;
		// idempotency comes from the link table, not the title.
		AllowDuplicate: true,
	}, service.IssueCreateOpts{
		ActorType: ActorTypeSync,
		BroadcastPayload: func(issue db.Issue, _ []db.Attachment) map[string]any {
			return e.issuePayload(ctx, issue)
		},
	})
	if err != nil {
		return fmt.Errorf("issuesync: create local issue: %w", err)
	}
	issue := res.Issue
	if _, err := e.Queries.CreateExternalIssueLink(ctx, db.CreateExternalIssueLinkParams{
		WorkspaceID:  src.WorkspaceID,
		IssueID:      issue.ID,
		SyncSourceID: src.ID,
		ExternalID:   remote.ID,
		ExternalKey:  remote.Key,
		WebUrl:       remote.WebURL,
		RemoteUpdatedAt: pgtype.Timestamptz{
			Time:  remote.UpdatedAt,
			Valid: !remote.UpdatedAt.IsZero(),
		},
		LastPushedHash: "",
	}); err != nil {
		return fmt.Errorf("issuesync: create link: %w", err)
	}
	if err := e.syncLabels(ctx, issue, remote.Labels); err != nil {
		slog.Warn("issuesync: label sync failed",
			"issue_id", util.UUIDToString(issue.ID), "error", err)
	}
	return nil
}

func (e *Engine) applyRemoteComment(ctx context.Context, src db.IssueSyncSource, remoteIssue ExternalIssue, remote ExternalComment) error {
	link, err := e.Queries.GetExternalIssueLinkByExternalID(ctx, db.GetExternalIssueLinkByExternalIDParams{
		SyncSourceID: src.ID,
		ExternalID:   remoteIssue.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Comment on an issue we don't mirror (e.g. attached after the issue
		// closed, or backfill still running) — drop; backfill will catch up.
		return nil
	}
	if err != nil {
		return fmt.Errorf("issuesync: load link for comment: %w", err)
	}

	body := attributedBody(src.Provider, remote.Author, remote.Body)

	existing, err := e.Queries.GetExternalCommentLinkByExternalID(ctx, db.GetExternalCommentLinkByExternalIDParams{
		IssueLinkID:       link.ID,
		ExternalCommentID: remote.ID,
	})
	if err == nil {
		// Echo of a comment we pushed outbound, or an edit of a mirrored one.
		if existing.Origin == "local" {
			return nil
		}
		if existing.LastPushedHash == ContentHash("", body, "", nil, "") {
			return nil
		}
		updated, err := e.Queries.UpdateComment(ctx, db.UpdateCommentParams{
			ID:      existing.CommentID,
			Content: body,
		})
		if err != nil {
			return fmt.Errorf("issuesync: update mirrored comment: %w", err)
		}
		if err := e.Queries.UpdateExternalCommentLinkPushedHash(ctx, db.UpdateExternalCommentLinkPushedHashParams{
			ID:             existing.ID,
			LastPushedHash: ContentHash("", body, "", nil, ""),
		}); err != nil {
			return fmt.Errorf("issuesync: bump comment hash: %w", err)
		}
		e.publishComment(ctx, protocol.EventCommentUpdated, updated, link)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("issuesync: load comment link: %w", err)
	}

	// author_type='system' with the zero UUID, following the child-done
	// system comment precedent — remote authors have no Multica identity;
	// attribution lives in the content prefix.
	comment, err := e.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     link.IssueID,
		WorkspaceID: src.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     body,
		Type:        "comment",
	})
	if err != nil {
		return fmt.Errorf("issuesync: create mirrored comment: %w", err)
	}
	if _, err := e.Queries.CreateExternalCommentLink(ctx, db.CreateExternalCommentLinkParams{
		IssueLinkID:       link.ID,
		CommentID:         comment.ID,
		ExternalCommentID: remote.ID,
		Origin:            "remote",
		LastPushedHash:    ContentHash("", body, "", nil, ""),
	}); err != nil {
		return fmt.Errorf("issuesync: create comment link: %w", err)
	}
	e.publishComment(ctx, protocol.EventCommentCreated, comment, link)
	return nil
}

// attributedBody prefixes mirrored comment content with its remote author so
// the local thread keeps attribution even though the row author is 'system'.
func attributedBody(provider string, author *ExternalUser, body string) string {
	name := ""
	if author != nil {
		name = author.DisplayName
		if name == "" {
			name = author.Login
		}
	}
	label := providerDisplayName(provider)
	if name == "" {
		return fmt.Sprintf("**via %s:** %s", label, body)
	}
	return fmt.Sprintf("**%s (via %s):** %s", name, label, body)
}

func providerDisplayName(provider string) string {
	switch provider {
	case ProviderGitHub:
		return "GitHub"
	case ProviderGitLab:
		return "GitLab"
	case ProviderJira:
		return "Jira"
	}
	return provider
}

// resolveAssignee maps a remote assignee to a workspace member (user UUID)
// via the external_identity table, auto-matching by email on first sight.
// ok=false means unassigned or unmappable.
func (e *Engine) resolveAssignee(ctx context.Context, src db.IssueSyncSource, remote *ExternalUser) (pgtype.UUID, bool) {
	if remote == nil || remote.AccountID == "" {
		return pgtype.UUID{}, false
	}
	var userID pgtype.UUID
	if remote.Email != "" {
		if id, err := e.Queries.FindWorkspaceUserByEmail(ctx, db.FindWorkspaceUserByEmailParams{
			WorkspaceID: src.WorkspaceID,
			Email:       remote.Email,
		}); err == nil {
			userID = id
		}
	}
	identity, err := e.Queries.UpsertExternalIdentity(ctx, db.UpsertExternalIdentityParams{
		WorkspaceID:       src.WorkspaceID,
		Provider:          src.Provider,
		ExternalAccountID: remote.AccountID,
		ExternalLogin:     util.StrToText(remote.Login),
		DisplayName:       util.StrToText(remote.DisplayName),
		Email:             util.StrToText(remote.Email),
		AvatarUrl:         util.StrToText(remote.AvatarURL),
		UserID:            userID,
	})
	if err != nil {
		slog.Warn("issuesync: identity upsert failed",
			"provider", src.Provider, "account", remote.AccountID, "error", err)
		return pgtype.UUID{}, false
	}
	return identity.UserID, identity.UserID.Valid
}

// syncLabels reconciles the issue's labels with the remote set by name
// (case-insensitive), creating missing workspace labels with a neutral color.
// Only divergence is written; no-op reconciles don't touch the DB.
func (e *Engine) syncLabels(ctx context.Context, issue db.Issue, remoteLabels []string) error {
	current, err := e.Queries.ListLabelsByIssue(ctx, db.ListLabelsByIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(remoteLabels))
	for _, l := range remoteLabels {
		if s := strings.TrimSpace(l); s != "" {
			want[strings.ToLower(s)] = true
		}
	}
	have := make(map[string]db.IssueLabel, len(current))
	for _, l := range current {
		have[strings.ToLower(l.Name)] = l
	}

	changed := false
	for lower, label := range have {
		if !want[lower] {
			if err := e.Queries.DetachLabelFromIssue(ctx, db.DetachLabelFromIssueParams{
				IssueID:     issue.ID,
				LabelID:     label.ID,
				WorkspaceID: issue.WorkspaceID,
			}); err != nil {
				return err
			}
			changed = true
		}
	}
	if len(want) > 0 {
		all, err := e.Queries.ListLabels(ctx, issue.WorkspaceID)
		if err != nil {
			return err
		}
		byName := make(map[string]db.IssueLabel, len(all))
		for _, l := range all {
			byName[strings.ToLower(l.Name)] = l
		}
		for _, raw := range remoteLabels {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			lower := strings.ToLower(name)
			if _, attached := have[lower]; attached {
				continue
			}
			label, exists := byName[lower]
			if !exists {
				label, err = e.Queries.CreateLabel(ctx, db.CreateLabelParams{
					WorkspaceID: issue.WorkspaceID,
					Name:        name,
					Color:       "#6b7280",
				})
				if err != nil {
					return err
				}
				byName[lower] = label
			}
			if err := e.Queries.AttachLabelToIssue(ctx, db.AttachLabelToIssueParams{
				IssueID:     issue.ID,
				LabelID:     label.ID,
				WorkspaceID: issue.WorkspaceID,
			}); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed && e.Bus != nil {
		e.Bus.Publish(events.Event{
			Type:        protocol.EventIssueLabelsChanged,
			WorkspaceID: util.UUIDToString(issue.WorkspaceID),
			ActorType:   ActorTypeSync,
			Payload:     map[string]any{"issue_id": util.UUIDToString(issue.ID)},
		})
	}
	return nil
}

func (e *Engine) issuePayload(ctx context.Context, issue db.Issue) map[string]any {
	if e.IssuePayload != nil {
		return e.IssuePayload(ctx, issue)
	}
	return map[string]any{"issue_id": util.UUIDToString(issue.ID)}
}

func (e *Engine) publishIssueUpdated(ctx context.Context, issue db.Issue, prevStatus string, statusChanged, descriptionChanged, assigneeChanged bool) {
	if e.Bus == nil {
		return
	}
	payload := e.issuePayload(ctx, issue)
	payload["status_changed"] = statusChanged
	payload["description_changed"] = descriptionChanged
	payload["assignee_changed"] = assigneeChanged
	if statusChanged {
		payload["prev_status"] = prevStatus
	}
	e.Bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   ActorTypeSync,
		Payload:     payload,
	})
}

func (e *Engine) publishComment(ctx context.Context, eventType string, comment db.Comment, link db.ExternalIssueLink) {
	if e.Bus == nil {
		return
	}
	issue, err := e.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
		ID:          link.IssueID,
		WorkspaceID: link.WorkspaceID,
	})
	if err != nil {
		return
	}
	var payload map[string]any
	if e.CommentPayload != nil {
		payload = e.CommentPayload(ctx, comment, issue)
	} else {
		payload = map[string]any{
			"comment_id": util.UUIDToString(comment.ID),
			"issue_id":   util.UUIDToString(issue.ID),
		}
	}
	e.Bus.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   ActorTypeSync,
		Payload:     payload,
	})
}

// EnqueueOutbound records an outbound push for the worker. payload is
// op-specific (push_comment carries {"comment_id": ...}); nil marshals to {}.
func (e *Engine) EnqueueOutbound(ctx context.Context, src db.IssueSyncSource, issueID pgtype.UUID, op string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = e.Queries.EnqueueIssueSyncOutbox(ctx, db.EnqueueIssueSyncOutboxParams{
		WorkspaceID:  src.WorkspaceID,
		SyncSourceID: src.ID,
		IssueID:      issueID,
		Op:           op,
		Payload:      raw,
	})
	return err
}

// RunBackfill pages through the source's remote issues, applying each through
// the normal inbound path. Resumable: the cursor persists after every page so
// a crash resumes rather than restarts. Terminal states: done / failed.
func (e *Engine) RunBackfill(ctx context.Context, src db.IssueSyncSource) {
	provider := e.Provider(src.Provider)
	if provider == nil {
		return
	}
	setState := func(status string, cursor string) {
		if err := e.Queries.UpdateIssueSyncSourceBackfill(ctx, db.UpdateIssueSyncSourceBackfillParams{
			ID:             src.ID,
			BackfillStatus: status,
			BackfillCursor: util.StrToText(cursor),
		}); err != nil {
			slog.Warn("issuesync: backfill state update failed",
				"source_id", util.UUIDToString(src.ID), "error", err)
		}
	}
	cursor := src.BackfillCursor.String
	setState("running", cursor)
	for {
		issues, next, err := provider.ListIssues(ctx, src, cursor)
		if err != nil {
			slog.Error("issuesync: backfill page failed",
				"source_id", util.UUIDToString(src.ID), "cursor", cursor, "error", err)
			setState("failed", cursor)
			return
		}
		for _, remote := range issues {
			if err := e.ApplyRemote(ctx, src, IssueEvent{Kind: "issue", Issue: remote}); err != nil {
				slog.Warn("issuesync: backfill apply failed",
					"source_id", util.UUIDToString(src.ID), "external_id", remote.ID, "error", err)
			}
		}
		if next == "" {
			setState("done", "")
			return
		}
		cursor = next
		setState("running", cursor)
	}
}
