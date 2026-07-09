package mattermost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// maxMessageRunes caps a single outbound post body. Mattermost's default
// MaxPostSize is 16383 characters but admins can lower it (the historical
// default was 4000), so we chunk at the conservative bound.
const maxMessageRunes = 4000

// mattermostSender posts agent replies back to Mattermost via
// POST /api/v4/posts. It is the OUTBOUND half (inbound runs on the
// per-installation WebSocket in mattermost_channel.go). Mattermost renders
// standard Markdown natively, so — unlike Slack's mrkdwn conversion — the
// agent's reply is posted as-is. The installation identity (workspace / agent /
// installer) is resolved per message by the Router, so it is absent here.
type mattermostSender struct {
	api    restAPI
	logger *slog.Logger
}

// newMattermostSender builds a Send-only client. Kept separate from the
// outbound subscriber so tests can inject a client pointed at an httptest
// server.
func newMattermostSender(api restAPI, logger *slog.Logger) *mattermostSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &mattermostSender{api: api, logger: logger}
}

// Send delivers a minimal text reply, threading under out's root when set so a
// decoupled reply lands back in the originating thread. Long bodies are
// chunked under the per-post cap; the returned SendResult carries the id of
// the LAST posted chunk.
func (c *mattermostSender) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	if c.api == nil {
		return channel.SendResult{}, errors.New("mattermost: api client not configured")
	}
	rootID := outboundRootID(out)
	var lastID string
	for _, chunk := range chunkMessage(out.Text, maxMessageRunes) {
		post, err := c.api.CreatePost(ctx, out.ChatID, rootID, chunk)
		if err != nil {
			return channel.SendResult{}, fmt.Errorf("mattermost: create post: %w", err)
		}
		lastID = post.ID
	}
	return channel.SendResult{MessageID: lastID}, nil
}

// outboundRootID picks the thread root for an outbound reply: an explicit
// quote target wins, else the thread the inbound message belonged to.
func outboundRootID(out channel.OutboundMessage) string {
	if out.ReplyTo != "" {
		return out.ReplyTo
	}
	return out.ThreadID
}

// chunkMessage splits text into <=maxRunes-rune pieces on rune boundaries so a
// long agent reply does not exceed the server's per-post cap. An empty body
// yields a single empty chunk (the caller guards against empty sends
// upstream).
func chunkMessage(text string, maxRunes int) []string {
	if maxRunes <= 0 || len([]rune(text)) <= maxRunes {
		return []string{text}
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		n := maxRunes
		if n > len(runes) {
			n = len(runes)
		}
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}
	return chunks
}
