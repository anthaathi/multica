package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Mattermost ResolverSet: the platform-specific seams the
// channel-agnostic engine.Router runs the inbound pipeline through. It mirrors
// the Slack ResolverSet and is built entirely on the generic channel_* queries
// (no new query, no schema change) plus the shared engine.ChatSession — so
// "adding Mattermost" stays "implement Channel + register a ResolverSet".

// originMattermostChat is the issue.origin_type label for issues created via
// the Mattermost /issue command (migration 149 adds it to the CHECK).
const originMattermostChat = "mattermost_chat"

// NewMattermostResolverSet assembles the Mattermost ResolverSet over the
// generated queries + a tx starter (for the shared session service). The
// replier delivers the outbound binding-prompt / status / issue-created
// notices; pass nil to disable them. typing shows the "processing" reaction on
// ingest; pass nil to disable it.
func NewMattermostResolverSet(q *db.Queries, tx engine.TxStarter, replier engine.OutboundReplier, typing *TypingIndicatorManager) engine.ResolverSet {
	set := engine.ResolverSet{
		Installation: &installationResolver{q: q},
		Identity:     &identityResolver{q: q},
		Dedup:        &deduper{q: q},
		Session: &sessionBinder{session: engine.NewChatSession(q, tx, TypeMattermost, engine.SessionTitles{
			Group:    "Mattermost channel",
			Direct:   "Mattermost direct message",
			Fallback: "Mattermost chat",
		})},
		Audit:      &auditor{q: q},
		Replier:    replier,
		OriginType: originMattermostChat,
	}
	// Guard against assigning a nil *TypingIndicatorManager into the interface
	// field (which would make set.Typing a non-nil typed-nil); mirrors Slack.
	if typing != nil {
		set.Typing = &mattermostTypingNotifier{mgr: typing}
	}
	return set
}

var (
	_ engine.InstallationResolver = (*installationResolver)(nil)
	_ engine.IdentityResolver     = (*identityResolver)(nil)
	_ engine.Deduper              = (*deduper)(nil)
	_ engine.SessionBinder        = (*sessionBinder)(nil)
	_ engine.Auditor              = (*auditor)(nil)
	_ engine.TypingNotifier       = (*mattermostTypingNotifier)(nil)
)

// mattermostBindingConfig is the opaque outbound routing persisted on the chat
// binding's config. When the binding key is a composite (channel thread), the
// real channel id lives here so the outbound path can post back.
type mattermostBindingConfig struct {
	ChannelID string `json:"channel_id"`
}

// mattermostSessionRouting derives, from one inbound message, the three things
// the session layer needs kept distinct (mirrors slackSessionRouting):
//
//   - bindingKey: the session-isolation key (stored as channel_chat_id). A DM
//     is one continuous session per channel, so the key is the channel id. A
//     channel/group message is isolated by THREAD ROOT — key = "channel:root"
//     — so two @bot threads in one channel are two sessions. The root is the
//     inbound root_id when replying in a thread, else the post's own id (a
//     top-level @mention starts a new root).
//   - config: the real channel id, so outbound works even when the key is
//     composite.
//   - replyThread: the root id to reply under (the thread root for groups; the
//     inbound root for DMs, which may be empty for a top-level send).
//
// It is a pure function so the isolation contract is unit-tested without a DB.
func mattermostSessionRouting(msg channel.InboundMessage) (bindingKey string, config []byte, replyThread string) {
	chatID := msg.Source.ChatID
	cfg, _ := json.Marshal(mattermostBindingConfig{ChannelID: chatID})
	if msg.Source.ChatType == channel.ChatTypeP2P {
		return chatID, cfg, msg.Source.ThreadID
	}
	threadRoot := msg.Source.ThreadID
	if threadRoot == "" {
		threadRoot = msg.MessageID
	}
	return chatID + ":" + threadRoot, cfg, threadRoot
}

func decodeMattermostRaw(msg channel.InboundMessage) (mattermostRawEvent, error) {
	var raw mattermostRawEvent
	if len(msg.Raw) == 0 {
		return mattermostRawEvent{}, errors.New("mattermost: inbound message Raw is empty")
	}
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		return mattermostRawEvent{}, fmt.Errorf("decode mattermost inbound raw: %w", err)
	}
	return raw, nil
}

func nullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// ---- installation routing ----

type installationResolver struct{ q *db.Queries }

func (r *installationResolver) ResolveInstallation(ctx context.Context, msg channel.InboundMessage) (engine.ResolvedInstallation, error) {
	raw, err := decodeMattermostRaw(msg)
	if err != nil {
		return engine.ResolvedInstallation{}, err
	}
	inst, err := r.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
		ChannelType: string(TypeMattermost),
		// Route by the stamped app_id ("server#botUserID"): each installation's
		// own WebSocket connection only ever delivers events for its own bot, and
		// the key embeds the server, so — unlike Slack's shared-app-id case — no
		// additional team cross-check is needed.
		AppID: raw.AppID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedInstallation{}, engine.ErrInstallationNotFound
		}
		return engine.ResolvedInstallation{}, err
	}
	return engine.ResolvedInstallation{
		ID:              inst.ID,
		WorkspaceID:     inst.WorkspaceID,
		AgentID:         inst.AgentID,
		InstallerUserID: inst.InstallerUserID,
		Active:          inst.Status == "active",
		Platform:        inst,
	}, nil
}

// ---- identity ----

// identityQueries is the slice of generated queries the identityResolver
// needs. It is an interface (not *db.Queries) so the cross-installation reuse
// path is unit-tested with fakes. *db.Queries satisfies it.
type identityQueries interface {
	GetChannelUserBindingByUserID(ctx context.Context, arg db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error)
	FindReusableChannelUserBinding(ctx context.Context, arg db.FindReusableChannelUserBindingParams) (db.ChannelUserBinding, error)
	GetMemberByUserAndWorkspace(ctx context.Context, arg db.GetMemberByUserAndWorkspaceParams) (db.Member, error)
	CreateChannelUserBinding(ctx context.Context, arg db.CreateChannelUserBindingParams) (db.ChannelUserBinding, error)
}

type identityResolver struct{ q identityQueries }

func (r *identityResolver) ResolveSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (engine.ResolvedIdentity, error) {
	senderID := msg.Source.SenderID
	binding, err := r.q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{
		InstallationID: inst.ID,
		ChannelUserID:  senderID,
	})
	reused := false
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, err
		}
		// Not linked to THIS installation. Before prompting, reuse a link the same
		// Mattermost user already made to another installation of the same server
		// in this workspace (MUL-3911 semantics): user ids are stable per server,
		// so one link per server, not per bot.
		cand, ok, ferr := r.reusableBinding(ctx, inst, senderID)
		if ferr != nil {
			return engine.ResolvedIdentity{}, ferr
		}
		if !ok {
			return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
		}
		binding, reused = cand, true
	}
	// Binding existence no longer proves membership (no FK); re-check. For a
	// reused link this also gates materialization: we never persist a binding for
	// a user who has since left the workspace.
	if _, err := r.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      binding.MulticaUserID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if reused {
				// Same human, no longer a member: prompt a fresh link rather than
				// surface "not a member" for a bot they never linked.
				return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
			}
			return engine.ResolvedIdentity{}, engine.ErrSenderNotMember
		}
		return engine.ResolvedIdentity{}, err
	}
	if reused {
		// Materialize the reused link as a binding on THIS installation so later
		// messages resolve on the fast per-installation path and are pruned with
		// the member like any other. Idempotent via ON CONFLICT.
		if _, err := r.q.CreateChannelUserBinding(ctx, db.CreateChannelUserBindingParams{
			WorkspaceID:    inst.WorkspaceID,
			MulticaUserID:  binding.MulticaUserID,
			InstallationID: inst.ID,
			ChannelType:    string(TypeMattermost),
			ChannelUserID:  senderID,
			Config:         []byte(`{}`),
		}); err != nil {
			return engine.ResolvedIdentity{}, fmt.Errorf("materialize reused mattermost binding: %w", err)
		}
	}
	return engine.ResolvedIdentity{UserID: binding.MulticaUserID}, nil
}

// reusableBinding looks for a link the same Mattermost user already made to
// ANOTHER installation of the SAME workspace + SAME server, so a second bot on
// one server need not re-prompt. The generic FindReusableChannelUserBinding
// keys on ci.config->>'team_id', which this adapter stores the normalized
// server URL in (the documented slot repurposing — see installConfig).
// ok=false (nil error) means "no reuse — prompt to link".
func (r *identityResolver) reusableBinding(ctx context.Context, inst engine.ResolvedInstallation, senderID string) (db.ChannelUserBinding, bool, error) {
	ci, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return db.ChannelUserBinding{}, false, nil
	}
	serverURL := installServerURL(ci.Config)
	if serverURL == "" {
		return db.ChannelUserBinding{}, false, nil
	}
	cand, err := r.q.FindReusableChannelUserBinding(ctx, db.FindReusableChannelUserBindingParams{
		WorkspaceID:   inst.WorkspaceID,
		ChannelType:   string(TypeMattermost),
		ChannelUserID: senderID,
		TeamID:        serverURL,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChannelUserBinding{}, false, nil
		}
		return db.ChannelUserBinding{}, false, err
	}
	return cand, true, nil
}

// ---- dedup ----

type deduper struct{ q *db.Queries }

func (r *deduper) Claim(ctx context.Context, installationID pgtype.UUID, messageID string) (pgtype.UUID, error) {
	claim, err := r.q.ClaimChannelInboundDedup(ctx, db.ClaimChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, engine.ErrDuplicate
		}
		return pgtype.UUID{}, err
	}
	return claim.ClaimToken, nil
}

func (r *deduper) Mark(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.MarkChannelInboundDedupProcessed(ctx, db.MarkChannelInboundDedupProcessedParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

func (r *deduper) Release(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.ReleaseChannelInboundDedup(ctx, db.ReleaseChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

// ---- session bind / append ----

type sessionBinder struct{ session *engine.ChatSession }

func (r *sessionBinder) EnsureSession(ctx context.Context, p engine.EnsureSessionParams) (pgtype.UUID, error) {
	bindingKey, config, _ := mattermostSessionRouting(p.Message)
	return r.session.EnsureSession(ctx, engine.EnsureSessionInput{
		WorkspaceID:    p.Installation.WorkspaceID,
		AgentID:        p.Installation.AgentID,
		InstallationID: p.Installation.ID,
		Sender:         p.Sender,
		BindingKey:     bindingKey,
		BindingConfig:  config,
		ChatType:       p.Message.Source.ChatType,
	})
}

func (r *sessionBinder) AppendMessage(ctx context.Context, p engine.AppendParams) (engine.AppendResult, error) {
	_, _, replyThread := mattermostSessionRouting(p.Message)
	return r.session.AppendUserMessage(ctx, engine.AppendInput{
		SessionID:      p.SessionID,
		Sender:         p.Sender,
		InstallationID: p.InstallationID,
		Body:           p.Message.Text,
		// Mattermost text is not enriched, so the command source is the body
		// itself.
		CommandText: p.Message.Text,
		MessageID:   p.Message.MessageID,
		ThreadID:    replyThread,
		ClaimToken:  p.ClaimToken,
	})
}

// ---- audit ----

type auditor struct{ q *db.Queries }

func (r *auditor) RecordDrop(ctx context.Context, instID pgtype.UUID, msg channel.InboundMessage, reason engine.DropReason) error {
	raw, _ := decodeMattermostRaw(msg) // event_type is best-effort; a decode miss still audits the drop
	return r.q.RecordChannelInboundDrop(ctx, db.RecordChannelInboundDropParams{
		ChannelType:      string(TypeMattermost),
		EventType:        raw.EventType,
		DropReason:       string(reason),
		InstallationID:   instID,
		ChannelChatID:    nullText(msg.Source.ChatID),
		ChannelEventID:   nullText(msg.EventID),
		ChannelMessageID: nullText(msg.MessageID),
	})
}

// ---- typing indicator ----

type mattermostTypingNotifier struct{ mgr *TypingIndicatorManager }

// OnIngested fires when a message is successfully ingested. It reacts to the
// user's post so the user sees the bot is processing it. The resolved
// installation carries the bot token in its Config blob — the
// InstallationResolver stashed the db.ChannelInstallation row in Platform, the
// documented adapter boundary the core never reads.
func (n *mattermostTypingNotifier) OnIngested(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, sessionID pgtype.UUID) {
	ci, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return
	}
	raw, _ := decodeMattermostRaw(msg) // create_at is best-effort (0 = treat as fresh)
	n.mgr.Add(ctx, ci, sessionID, msg.MessageID, raw.CreateAt)
}

// OnSettled clears the reaction when the run trigger enqueued no task (agent
// offline / archived, or an enqueue failure) — the bus-driven clear on
// chat-done / task-failed never fires for those, so without this the 👀
// sticks.
func (n *mattermostTypingNotifier) OnSettled(ctx context.Context, sessionID pgtype.UUID) {
	n.mgr.Clear(ctx, sessionID)
}
