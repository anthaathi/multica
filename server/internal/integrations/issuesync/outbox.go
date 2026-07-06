package issuesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Outbox dispatch tuning. Claim-side staleness reclaim lives in the
// ClaimIssueSyncOutbox query (5 minutes).
const (
	outboxPollInterval = 3 * time.Second
	outboxBatchSize    = 20
	outboxMaxAttempts  = 8
	outboxBaseBackoff  = 30 * time.Second
	outboxMaxBackoff   = time.Hour
	outboxRetention    = 7 * 24 * time.Hour
)

// RunOutbox drains issue_sync_outbox until ctx is cancelled. Multiple
// replicas can run it concurrently — claiming uses FOR UPDATE SKIP LOCKED.
func (e *Engine) RunOutbox(ctx context.Context) {
	ticker := time.NewTicker(outboxPollInterval)
	defer ticker.Stop()
	sweep := time.NewTicker(time.Hour)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			cutoff := pgtype.Timestamptz{Time: time.Now().Add(-outboxRetention), Valid: true}
			if err := e.Queries.DeleteDoneIssueSyncOutbox(ctx, cutoff); err != nil && ctx.Err() == nil {
				slog.Warn("issuesync: outbox retention sweep failed", "error", err)
			}
		case <-ticker.C:
			rows, err := e.Queries.ClaimIssueSyncOutbox(ctx, outboxBatchSize)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("issuesync: outbox claim failed", "error", err)
				}
				continue
			}
			for _, row := range rows {
				e.dispatchOutbox(ctx, row)
			}
		}
	}
}

func (e *Engine) dispatchOutbox(ctx context.Context, row db.IssueSyncOutbox) {
	err := e.pushOne(ctx, row)
	if err == nil {
		if cerr := e.Queries.CompleteIssueSyncOutbox(ctx, row.ID); cerr != nil {
			slog.Warn("issuesync: outbox complete failed",
				"outbox_id", util.UUIDToString(row.ID), "error", cerr)
		}
		return
	}
	if errors.Is(err, errOutboxSkip) {
		// Not an error: the push no longer applies (source removed/disabled,
		// nothing linked, no-op content). Mark done without provider traffic.
		if cerr := e.Queries.CompleteIssueSyncOutbox(ctx, row.ID); cerr != nil {
			slog.Warn("issuesync: outbox complete failed",
				"outbox_id", util.UUIDToString(row.ID), "error", cerr)
		}
		return
	}

	if row.Attempts >= outboxMaxAttempts {
		slog.Error("issuesync: outbox row failed terminally",
			"outbox_id", util.UUIDToString(row.ID), "op", row.Op, "error", err)
		if ferr := e.Queries.FailIssueSyncOutbox(ctx, db.FailIssueSyncOutboxParams{
			ID:        row.ID,
			LastError: util.StrToText(err.Error()),
		}); ferr != nil {
			slog.Warn("issuesync: outbox fail-mark failed",
				"outbox_id", util.UUIDToString(row.ID), "error", ferr)
		}
		e.recordLinkError(ctx, row, err)
		return
	}

	backoff := time.Duration(math.Pow(2, float64(row.Attempts-1))) * outboxBaseBackoff
	if backoff > outboxMaxBackoff {
		backoff = outboxMaxBackoff
	}
	if rerr := e.Queries.RetryIssueSyncOutbox(ctx, db.RetryIssueSyncOutboxParams{
		ID:            row.ID,
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(backoff), Valid: true},
		LastError:     util.StrToText(err.Error()),
	}); rerr != nil {
		slog.Warn("issuesync: outbox retry-mark failed",
			"outbox_id", util.UUIDToString(row.ID), "error", rerr)
	}
}

// errOutboxSkip signals "nothing to do" rather than a transport failure.
var errOutboxSkip = errors.New("issuesync: outbox skip")

func (e *Engine) recordLinkError(ctx context.Context, row db.IssueSyncOutbox, pushErr error) {
	link, err := e.Queries.GetExternalIssueLinkForIssue(ctx, db.GetExternalIssueLinkForIssueParams{
		IssueID:      row.IssueID,
		SyncSourceID: row.SyncSourceID,
	})
	if err != nil {
		return
	}
	msg := pushErr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if serr := e.Queries.SetExternalIssueLinkSyncError(ctx, db.SetExternalIssueLinkSyncErrorParams{
		ID:        link.ID,
		SyncError: util.StrToText(msg),
	}); serr != nil {
		slog.Warn("issuesync: link error record failed",
			"link_id", util.UUIDToString(link.ID), "error", serr)
	}
}

func (e *Engine) pushOne(ctx context.Context, row db.IssueSyncOutbox) error {
	src, err := e.Queries.GetIssueSyncSource(ctx, row.SyncSourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errOutboxSkip
	}
	if err != nil {
		return fmt.Errorf("load source: %w", err)
	}
	if !src.SyncEnabled {
		return errOutboxSkip
	}
	provider := e.Provider(src.Provider)
	if provider == nil {
		return fmt.Errorf("no provider registered for %q", src.Provider)
	}

	issue, err := e.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
		ID:          row.IssueID,
		WorkspaceID: src.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errOutboxSkip
	}
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}

	switch row.Op {
	case "create_remote":
		return e.pushCreateRemote(ctx, provider, src, issue)
	case "push_issue", "push_status":
		return e.pushIssueUpdate(ctx, provider, src, issue)
	case "push_comment":
		return e.pushComment(ctx, provider, src, issue, row.Payload)
	default:
		return fmt.Errorf("unknown outbox op %q", row.Op)
	}
}

// buildOutbound assembles the provider-facing content from current DB state.
// Reading at dispatch time (not enqueue time) collapses bursts of edits into
// one push and guarantees the pushed hash matches what the provider stores.
func (e *Engine) buildOutbound(ctx context.Context, src db.IssueSyncSource, issue db.Issue) (OutboundIssue, error) {
	out := OutboundIssue{
		Title:       issue.Title,
		Description: issue.Description.String,
	}
	if mapped, ok := MapOutboundStatus(src, issue.Status); ok {
		out.State = mapped
	}
	labels, err := e.Queries.ListLabelsByIssue(ctx, db.ListLabelsByIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return out, fmt.Errorf("list labels: %w", err)
	}
	for _, l := range labels {
		out.Labels = append(out.Labels, l.Name)
	}
	if issue.AssigneeType.Valid && issue.AssigneeType.String == "member" && issue.AssigneeID.Valid {
		identity, err := e.Queries.GetExternalIdentityByUser(ctx, db.GetExternalIdentityByUserParams{
			WorkspaceID: src.WorkspaceID,
			Provider:    src.Provider,
			UserID:      issue.AssigneeID,
		})
		if err == nil {
			out.AssigneeAccountID = identity.ExternalAccountID
		}
	}
	return out, nil
}

func (e *Engine) pushCreateRemote(ctx context.Context, provider Provider, src db.IssueSyncSource, issue db.Issue) error {
	if _, err := e.Queries.GetExternalIssueLinkForIssue(ctx, db.GetExternalIssueLinkForIssueParams{
		IssueID:      issue.ID,
		SyncSourceID: src.ID,
	}); err == nil {
		return errOutboxSkip // already linked (a retry raced, or webhook created the link)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check existing link: %w", err)
	}
	out, err := e.buildOutbound(ctx, src, issue)
	if err != nil {
		return err
	}
	remote, err := provider.CreateIssue(ctx, src, out)
	if err != nil {
		return fmt.Errorf("provider create: %w", err)
	}
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
		LastPushedHash: ContentHash(out.Title, out.Description, out.State, out.Labels, out.AssigneeAccountID),
	}); err != nil {
		return fmt.Errorf("record link: %w", err)
	}
	return nil
}

func (e *Engine) pushIssueUpdate(ctx context.Context, provider Provider, src db.IssueSyncSource, issue db.Issue) error {
	link, err := e.Queries.GetExternalIssueLinkForIssue(ctx, db.GetExternalIssueLinkForIssueParams{
		IssueID:      issue.ID,
		SyncSourceID: src.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errOutboxSkip // issue is not mirrored on this source
	}
	if err != nil {
		return fmt.Errorf("load link: %w", err)
	}
	out, err := e.buildOutbound(ctx, src, issue)
	if err != nil {
		return err
	}
	hash := ContentHash(out.Title, out.Description, out.State, out.Labels, out.AssigneeAccountID)
	if hash == link.LastPushedHash {
		return errOutboxSkip // nothing changed since the last push
	}
	remote, err := provider.UpdateIssue(ctx, src, link.ExternalID, out)
	if err != nil {
		return fmt.Errorf("provider update: %w", err)
	}
	if err := e.Queries.UpdateExternalIssueLinkPushedHash(ctx, db.UpdateExternalIssueLinkPushedHashParams{
		ID:             link.ID,
		LastPushedHash: hash,
	}); err != nil {
		return fmt.Errorf("record pushed hash: %w", err)
	}
	if remote != nil && !remote.UpdatedAt.IsZero() {
		if err := e.Queries.UpdateExternalIssueLinkRemoteState(ctx, db.UpdateExternalIssueLinkRemoteStateParams{
			ID:              link.ID,
			RemoteUpdatedAt: pgtype.Timestamptz{Time: remote.UpdatedAt, Valid: true},
			ExternalKey:     link.ExternalKey,
			WebUrl:          link.WebUrl,
		}); err != nil {
			return fmt.Errorf("record remote state: %w", err)
		}
	}
	return nil
}

// outboxCommentPayload is the payload stored for push_comment rows.
// AuthorLabel is resolved at enqueue time by the listener (which can join
// user/agent names); the comment body is read fresh at dispatch time.
type outboxCommentPayload struct {
	CommentID   string `json:"comment_id"`
	AuthorLabel string `json:"author_label,omitempty"`
}

func (e *Engine) pushComment(ctx context.Context, provider Provider, src db.IssueSyncSource, issue db.Issue, raw []byte) error {
	var payload outboxCommentPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	commentID, err := util.ParseUUID(payload.CommentID)
	if err != nil {
		return fmt.Errorf("bad comment_id in payload: %w", err)
	}
	link, err := e.Queries.GetExternalIssueLinkForIssue(ctx, db.GetExternalIssueLinkForIssueParams{
		IssueID:      issue.ID,
		SyncSourceID: src.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errOutboxSkip
	}
	if err != nil {
		return fmt.Errorf("load link: %w", err)
	}
	comment, err := e.Queries.GetComment(ctx, commentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errOutboxSkip // deleted before we pushed
	}
	if err != nil {
		return fmt.Errorf("load comment: %w", err)
	}

	body := comment.Content
	if label := strings.TrimSpace(payload.AuthorLabel); label != "" {
		body = fmt.Sprintf("**%s (via Multica):** %s", label, body)
	}

	existing, err := e.Queries.GetExternalCommentLinkByComment(ctx, db.GetExternalCommentLinkByCommentParams{
		CommentID:   commentID,
		IssueLinkID: link.ID,
	})
	if err == nil {
		if existing.LastPushedHash == ContentHash("", body, "", nil, "") {
			return errOutboxSkip
		}
		if _, perr := provider.UpdateComment(ctx, src, link.ExternalID, existing.ExternalCommentID, body); perr != nil {
			return fmt.Errorf("provider comment update: %w", perr)
		}
		if uerr := e.Queries.UpdateExternalCommentLinkPushedHash(ctx, db.UpdateExternalCommentLinkPushedHashParams{
			ID:             existing.ID,
			LastPushedHash: ContentHash("", body, "", nil, ""),
		}); uerr != nil {
			return fmt.Errorf("record comment hash: %w", uerr)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load comment link: %w", err)
	}

	remote, err := provider.CreateComment(ctx, src, link.ExternalID, body)
	if err != nil {
		return fmt.Errorf("provider comment create: %w", err)
	}
	if _, err := e.Queries.CreateExternalCommentLink(ctx, db.CreateExternalCommentLinkParams{
		IssueLinkID:       link.ID,
		CommentID:         commentID,
		ExternalCommentID: remote.ID,
		Origin:            "local",
		LastPushedHash:    ContentHash("", body, "", nil, ""),
	}); err != nil {
		return fmt.Errorf("record comment link: %w", err)
	}
	return nil
}
