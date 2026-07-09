package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// mattermostChannel is ONE installation's events WebSocket connection. Under
// the BYO model every installation carries its own server URL + bot token, so
// it gets its own connection, exactly like Slack's per-installation Socket
// Mode. The engine.Supervisor builds one mattermostChannel per active
// installation (via the registered Factory) and owns the lease / reconnect
// lifecycle; Connect blocks on the receive loop.
//
// Inbound events are translated by the shared inbound.go helpers,
// parameterized by THIS installation's bot identity, and handed to the engine
// router, which resolves the installation by the stamped app_id (the
// per-installation routing key). Outbound replies primarily flow through the
// EventChatDone subscriber (NewOutbound); Send satisfies the Channel contract
// and posts with this installation's bot token.
type mattermostChannel struct {
	appID       string
	serverURL   string
	botUserID   string
	botUsername string
	botToken    string
	handler     channel.InboundHandler
	logger      *slog.Logger
}

func (c *mattermostChannel) Type() channel.Type { return TypeMattermost }

func (c *mattermostChannel) Capabilities() channel.Capability {
	return channel.CapText | channel.CapThreadReply
}

// Disconnect is a no-op: the WebSocket's whole lifetime is scoped to Connect
// (it returns when the run context is cancelled), so there is no long-lived
// resource to release here. Mirrors slackChannel.Disconnect.
func (c *mattermostChannel) Disconnect(ctx context.Context) error { return nil }

// Send posts an outbound reply with this installation's bot token, reusing the
// shared mattermostSender (chunking, threading).
func (c *mattermostChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	return newMattermostSender(newRestClient(c.serverURL, c.botToken, nil), c.logger).Send(ctx, out)
}

// Connect opens this installation's events WebSocket (authenticated with its
// bot token) and runs the receive loop until ctx is cancelled or the link
// drops. Only "posted" events are translated; everything else (hello, typing,
// status changes, …) is skipped.
func (c *mattermostChannel) Connect(ctx context.Context) error {
	if c.handler == nil {
		return errors.New("mattermost: inbound handler not configured")
	}
	if c.botToken == "" {
		return errors.New("mattermost: bot token not configured")
	}
	conn, err := dialWS(ctx, c.serverURL, c.botToken)
	if err != nil {
		return err
	}
	defer conn.close()

	mentionRe := compileMentionRe(c.botUsername)
	return conn.run(ctx, func(ctx context.Context, e wsEvent) error {
		if e.Event != "posted" {
			return nil
		}
		ev, err := decodePostedEvent(e)
		if err != nil {
			// A single undecodable post is skipped (logged), not fatal: failing
			// the connection would replay nothing — Mattermost has no redelivery.
			c.logger.WarnContext(ctx, "mattermost: decode posted event failed",
				"app_id", c.appID, "error", err)
			return nil
		}
		return c.dispatchPosted(ctx, ev, mentionRe)
	})
}

// dispatchPosted translates one posted event to a normalized inbound message
// and hands it to the engine. A non-nil handler error is an infrastructure
// failure; it propagates so the supervisor reconnects. A legitimate product
// drop returns nil.
func (c *mattermostChannel) dispatchPosted(ctx context.Context, ev postedEventData, mentionRe *regexp.Regexp) error {
	msg, ok := inboundFromPosted(c.appID, ev, c.botUserID, mentionRe)
	if !ok {
		return nil
	}
	return c.handler(ctx, msg)
}

// ChannelDeps are the shared dependencies the Mattermost Factory closes over.
// The engine inbound handler is supplied per-build via channel.Config.Handler;
// the Decrypter turns the installation's stored ciphertext token into
// plaintext.
type ChannelDeps struct {
	Decrypt Decrypter
	Logger  *slog.Logger
}

// RegisterMattermost registers the per-installation Mattermost Factory so the
// engine.Supervisor builds + supervises one mattermostChannel per active
// installation. "Adding Mattermost inbound" is this call plus the adapter — no
// engine edit (the same contract as slack.RegisterSlack / lark.RegisterFeishu).
func RegisterMattermost(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeMattermost, newMattermostFactory(deps))
}

func newMattermostFactory(deps ChannelDeps) channel.Factory {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(cfg channel.Config) (channel.Channel, error) {
		var ic installConfig
		if err := json.Unmarshal(cfg.Raw, &ic); err != nil {
			return nil, fmt.Errorf("mattermost: decode installation config: %w", err)
		}
		if ic.ServerURL == "" {
			return nil, errors.New("mattermost: installation has no server URL")
		}
		botToken, err := decryptToken(ic.BotTokenEncrypted, deps.Decrypt)
		if err != nil {
			return nil, fmt.Errorf("mattermost: decrypt bot token: %w", err)
		}
		if botToken == "" {
			return nil, errors.New("mattermost: installation has no bot token")
		}
		return &mattermostChannel{
			appID:       ic.AppID,
			serverURL:   ic.ServerURL,
			botUserID:   ic.BotUserID,
			botUsername: ic.BotUsername,
			botToken:    botToken,
			handler:     cfg.Handler,
			logger:      logger,
		}, nil
	}
}
