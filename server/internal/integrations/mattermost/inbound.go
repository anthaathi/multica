package mattermost

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// This file holds the platform-neutral translation from a Mattermost "posted"
// event to the engine's normalized channel.InboundMessage. These are free
// functions parameterized by the bot identity, so the per-installation
// connection (mattermost_channel.go) threads in its own installed bot's user
// id / username when translating each event.

// mattermostRawEvent carries the Mattermost-specific fields the cross-platform
// envelope does not — read back only inside the Mattermost resolvers. AppID is
// the installation routing key ("server#botUserID") STAMPED BY THE CONNECTION
// that received the event: each installation has its own WebSocket, so the
// origin is known deterministically rather than parsed from event fields.
type mattermostRawEvent struct {
	AppID       string `json:"app_id"`
	EventType   string `json:"event_type"`
	ChannelType string `json:"channel_type,omitempty"` // Mattermost "D"/"G"/"O"/"P"
	// CreateAt is the post's create_at (ms since epoch). Post ids carry no
	// timestamp (unlike Slack ts), so the typing indicator's replay max-age
	// guard reads it from here.
	CreateAt int64 `json:"create_at,omitempty"`
}

// postProps is the slice of post.props the loop guard reads: posts created by
// bot accounts / webhooks carry from_bot / from_webhook markers ("true"
// strings) so integrations do not feed on each other's output.
type postProps struct {
	FromBot     string `json:"from_bot"`
	FromWebhook string `json:"from_webhook"`
}

// compileMentionRe builds the regexp that matches an @-mention of botUsername
// in a raw Mattermost message ("@bot" delimited by a non-word char or the
// string edge; usernames are [a-z0-9.\-_]). It is the FALLBACK signal only —
// the posted event's server-parsed mentions array is primary (it survives a
// bot username change, this regex does not). An empty username yields nil,
// which makes text-based mention detection a no-op.
func compileMentionRe(botUsername string) *regexp.Regexp {
	if botUsername == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(botUsername) + `\b`)
}

// inboundFromPosted normalizes one "posted" event. It returns ok=false for
// posts that must not reach the core: the bot's own posts and other
// bots'/webhooks' posts (loop guard), and system posts (joins, headers, …;
// only brand-new user messages are ingested).
//
// Group addressing policy mirrors Slack (v1, deliberate): a group message is
// addressed to the bot only when the server's mention parse includes the bot
// user id or the text carries a literal @botusername. Mention-free follow-ups
// in a thread the bot is engaged in require re-mentioning the bot. P2P (DM)
// ingests every message.
func inboundFromPosted(appID string, ev postedEventData, botUserID string, mentionRe *regexp.Regexp) (channel.InboundMessage, bool) {
	post := ev.Post
	if post.ID == "" || post.UserID == "" {
		return channel.InboundMessage{}, false
	}
	if botUserID != "" && post.UserID == botUserID {
		return channel.InboundMessage{}, false
	}
	if isBotOrWebhookPost(post.Props) {
		return channel.InboundMessage{}, false
	}
	if post.Type != "" {
		return channel.InboundMessage{}, false // system post (join/leave/header/…)
	}

	chatType := mattermostChatType(ev.ChannelType)
	addressed := chatType == channel.ChatTypeP2P ||
		mentionsBotID(ev.Mentions, botUserID) ||
		mentionsBotText(post.Message, mentionRe)

	raw, _ := json.Marshal(mattermostRawEvent{
		AppID:       appID,
		EventType:   "posted",
		ChannelType: ev.ChannelType,
		CreateAt:    post.CreateAt,
	})
	var reply *channel.ReplyCtx
	if post.RootID != "" {
		reply = &channel.ReplyCtx{MessageID: post.RootID, RootID: post.RootID}
	}
	return channel.InboundMessage{
		EventID:        post.ID,
		MessageID:      post.ID,
		Type:           channel.MsgTypeText,
		Text:           cleanText(post.Message, mentionRe),
		ReplyTo:        reply,
		AddressedToBot: addressed,
		Source: channel.Source{
			ChannelType: TypeMattermost,
			ChatID:      post.ChannelID,
			ChatType:    chatType,
			SenderID:    post.UserID,
			ThreadID:    post.RootID,
		},
		Raw: raw,
	}, true
}

// isBotOrWebhookPost reports whether post props mark the author as a bot or an
// incoming webhook (the values are the strings "true" when set).
func isBotOrWebhookPost(props json.RawMessage) bool {
	if len(props) == 0 {
		return false
	}
	var p postProps
	if err := json.Unmarshal(props, &p); err != nil {
		return false
	}
	return p.FromBot == "true" || p.FromWebhook == "true"
}

// mentionsBotID reports whether the server's mention parse includes the bot.
func mentionsBotID(mentions []string, botUserID string) bool {
	if botUserID == "" {
		return false
	}
	for _, id := range mentions {
		if id == botUserID {
			return true
		}
	}
	return false
}

// mentionsBotText reports whether text contains a literal @botusername.
func mentionsBotText(text string, mentionRe *regexp.Regexp) bool {
	return mentionRe != nil && mentionRe.MatchString(text)
}

// cleanText strips a leading/embedded bot mention token and trims surrounding
// whitespace so the core sees the user's actual prompt, not "@bot hi".
func cleanText(text string, mentionRe *regexp.Regexp) string {
	if mentionRe != nil {
		text = mentionRe.ReplaceAllString(text, "")
	}
	return strings.TrimSpace(text)
}

// mattermostChatType maps a Mattermost channel type to the normalized
// ChatType. Only a 1:1 direct message ("D") is p2p; group DMs ("G") and
// public/private channels ("O"/"P") are groups, which route through the
// engine's "must address the bot" filter — plain chatter in a group DM is not
// mistaken for a prompt to the bot (mirrors Slack's mpim handling).
func mattermostChatType(channelType string) channel.ChatType {
	if channelType == "D" {
		return channel.ChatTypeP2P
	}
	return channel.ChatTypeGroup
}
