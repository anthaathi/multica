package mattermost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Mattermost OutboundReplier — the engine seam that delivers
// a verdict-driven reply back to the user. It posts through the same bot-token
// Send path as the EventChatDone outbound subscriber, so it needs no new
// transport. Mirrors the Slack replier.
//
// Outcomes handled:
//   - NeedsBinding: the sender is unbound. Mint a single-use binding token and
//     reply with a "link your account" prompt pointing at the in-product
//     redeem page. After they bind, their next message reaches the agent.
//   - AgentOffline / AgentArchived: a status notice so the user is not left
//     wondering why nothing happened.
//   - Ingested with an /issue created: a confirmation of the new issue.

const (
	agentOfflineText  = "⚠️ The agent is offline right now. Your message was received and will be handled once it's back online."
	agentArchivedText = "⚠️ This agent has been archived and can't respond. Please contact your workspace admin."
)

// bindingMinter is the binding-token surface the replier needs.
// *BindingTokenService satisfies it.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, mmUserID string) (BindingToken, error)
}

// replySender posts one reply. Satisfied by *mattermostSender, so the replier
// reuses Send's chunking and threading.
type replySender interface {
	Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error)
}

// OutboundReplier implements engine.OutboundReplier for Mattermost.
type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	newSender   func(creds credentials) replySender
	appURL      string
	bindingPath string
	logger      *slog.Logger
}

// OutboundReplierConfig configures the replier. Binding + AppURL are required
// for the NeedsBinding prompt to work; without them the prompt is skipped (the
// offline/archived/issue notices still fire).
type OutboundReplierConfig struct {
	Binding bindingMinter
	Decrypt Decrypter
	// AppURL is the Multica web app host the user clicks into to redeem the
	// binding token (e.g. https://multica.example). It comes from
	// MULTICA_APP_URL (falling back to FRONTEND_ORIGIN) and is intentionally
	// separate from MULTICA_PUBLIC_URL, the backend/API public URL — the bind
	// page (/mattermost/bind) is served by the web app, so the link must point
	// at the app host. Mirrors the Slack/Lark repliers.
	AppURL      string
	BindingPath string // default "/mattermost/bind"
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier. The sender factory mirrors the
// outbound subscriber: only the bot token is needed to post.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/mattermost/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	r := &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		logger:      logger,
	}
	r.newSender = func(c credentials) replySender {
		return newMattermostSender(newRestClient(c.ServerURL, c.BotToken, nil), logger)
	}
	return r
}

// Reply routes each outcome to its user-visible message. Errors are logged,
// not propagated: the replier runs detached from the inbound path.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		if err := r.sendBindingPrompt(ctx, inst, msg, res); err != nil {
			r.logger.WarnContext(ctx, "mattermost replier: binding prompt failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentOffline:
		if err := r.post(ctx, inst, msg, agentOfflineText); err != nil {
			r.logger.WarnContext(ctx, "mattermost replier: offline notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentArchived:
		if err := r.post(ctx, inst, msg, agentArchivedText); err != nil {
			r.logger.WarnContext(ctx, "mattermost replier: archived notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIngested:
		// Only a /issue-created message warrants a confirmation; a plain chat
		// message stays silent (the agent's own reply lands via EventChatDone).
		if res.IssueID.Valid {
			if err := r.post(ctx, inst, msg, issueCreatedText(res)); err != nil {
				r.logger.WarnContext(ctx, "mattermost replier: issue-created confirmation failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	}
}

func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil {
		return errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	// Mattermost renders standard Markdown, so a plain [label](url) link keeps
	// the base64url token intact (no mrkdwn escaping concerns).
	text := "👋 To start chatting with me, link your Mattermost account to Multica: [link your account](" +
		bindURL + ")\n(This link expires in 15 minutes.)"
	return r.post(ctx, inst, msg, text)
}

// post resolves the installation's bot token from the carried platform row and
// sends text back into the originating channel / thread.
func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	if _, err := r.newSender(creds).Send(ctx, channel.OutboundMessage{
		ChatID:   msg.Source.ChatID,
		Text:     text,
		ThreadID: msg.Source.ThreadID,
	}); err != nil {
		return fmt.Errorf("post mattermost reply: %w", err)
	}
	return nil
}

func issueCreatedText(res engine.Result) string {
	id := res.IssueIdentifier
	if id == "" {
		id = fmt.Sprintf("#%d", res.IssueNumber)
	}
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "✅ Created " + id
	}
	return "✅ Created " + id + " — " + title
}
