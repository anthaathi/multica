package mattermost

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// mkPostedData builds a postedEventData for inboundFromPosted tests. The post
// gets a stable id/create_at and the given channel type / channel / sender /
// message; mentions, RootID, Type and Props are left zero so callers can set
// them per-case.
func mkPostedData(channelType, channelID, userID, message string) postedEventData {
	return postedEventData{
		Post: mmPost{
			ID:        "msg1",
			CreateAt:  1700000000000,
			UserID:    userID,
			ChannelID: channelID,
			Message:   message,
		},
		ChannelType: channelType,
	}
}

// mkMentionRe compiles the bot-mention fallback regex (or returns nil for an
// empty username). It is a thin wrapper over compileMentionRe used by the
// inbound cases so the seam under test stays explicit.
func mkMentionRe(botUsername string) *regexp.Regexp {
	return compileMentionRe(botUsername)
}

func TestInboundFromPosted_DM(t *testing.T) {
	ev := mkPostedData("D", "dm1", "ualice", "hi bot")
	msg, ok := inboundFromPosted("srv#ubot", ev, "ubot", mkMentionRe("bot"))
	if !ok {
		t.Fatal("DM should be ingested")
	}
	if msg.Source.ChatType != channel.ChatTypeP2P {
		t.Errorf("ChatType = %q, want p2p", msg.Source.ChatType)
	}
	if !msg.AddressedToBot {
		t.Error("DM should always be addressed to bot")
	}
	if msg.Source.ChatID != "dm1" {
		t.Errorf("ChatID = %q, want dm1", msg.Source.ChatID)
	}
	if msg.Source.SenderID != "ualice" {
		t.Errorf("SenderID = %q, want ualice", msg.Source.SenderID)
	}
	if msg.Text != "hi bot" {
		t.Errorf("Text = %q, want cleaned text", msg.Text)
	}
	if msg.Source.ChannelType != TypeMattermost {
		t.Errorf("ChannelType = %q, want mattermost", msg.Source.ChannelType)
	}
	if msg.EventID != "msg1" || msg.MessageID != "msg1" {
		t.Errorf("EventID/MessageID = %q/%q, want msg1", msg.EventID, msg.MessageID)
	}
}

func TestInboundFromPosted_ChannelMention(t *testing.T) {
	for _, ct := range []string{"G", "O", "P"} {
		t.Run(ct, func(t *testing.T) {
			ev := mkPostedData(ct, "chan1", "ualice", "@bot create issue")
			ev.Mentions = []string{"ubot"} // server-parsed mention includes bot
			msg, ok := inboundFromPosted("srv#ubot", ev, "ubot", mkMentionRe("bot"))
			if !ok {
				t.Fatalf("channel type %s with bot mention should ingest", ct)
			}
			if msg.Source.ChatType != channel.ChatTypeGroup {
				t.Errorf("ChatType = %q, want group", msg.Source.ChatType)
			}
			if !msg.AddressedToBot {
				t.Error("channel message mentioning the bot should be addressed to bot")
			}
		})
	}
}

func TestInboundFromPosted_ChannelTextMentionFallback(t *testing.T) {
	ev := mkPostedData("O", "chan1", "ualice", "@bot please help")
	// No server-parsed mentions array → the literal @botusername regex is the
	// fallback signal.
	msg, ok := inboundFromPosted("srv#ubot", ev, "ubot", mkMentionRe("bot"))
	if !ok {
		t.Fatal("channel message with literal @bot text should ingest via regex fallback")
	}
	if msg.Source.ChatType != channel.ChatTypeGroup {
		t.Errorf("ChatType = %q, want group", msg.Source.ChatType)
	}
	if !msg.AddressedToBot {
		t.Error("should be addressed to bot via the regex fallback")
	}
	if msg.Text != "please help" {
		t.Errorf("Text = %q, want mention stripped to \"please help\"", msg.Text)
	}
}

func TestInboundFromPosted_ChannelNoMention(t *testing.T) {
	ev := mkPostedData("O", "chan1", "ualice", "just chatting with the team")
	// Real behavior: inboundFromPosted does NOT drop non-addressed group
	// messages itself — it ingests them with AddressedToBot=false and lets the
	// engine's group filter decide (mirrors the Slack adapter). The assignment
	// text "ok==false (skip)" describes the engine-side drop, not this seam.
	msg, ok := inboundFromPosted("srv#ubot", ev, "ubot", mkMentionRe("bot"))
	if !ok {
		t.Fatal("non-mention group message should still be ingested (ok=true); engine group filter drops it")
	}
	if msg.Source.ChatType != channel.ChatTypeGroup {
		t.Errorf("ChatType = %q, want group", msg.Source.ChatType)
	}
	if msg.AddressedToBot {
		t.Error("channel message without mention must NOT be addressed to bot")
	}
}

func TestInboundFromPosted_ThreadReply(t *testing.T) {
	ev := mkPostedData("O", "chan1", "ualice", "@bot follow up")
	ev.Post.RootID = "root123"
	msg, ok := inboundFromPosted("srv#ubot", ev, "ubot", mkMentionRe("bot"))
	if !ok {
		t.Fatal("thread reply should ingest")
	}
	if msg.Source.ThreadID != "root123" {
		t.Errorf("ThreadID = %q, want root123", msg.Source.ThreadID)
	}
	if msg.ReplyTo == nil {
		t.Fatal("ReplyTo must be set for a thread reply")
	}
	if msg.ReplyTo.MessageID != "root123" || msg.ReplyTo.RootID != "root123" {
		t.Errorf("ReplyTo = %+v, want MessageID=RootID=root123", msg.ReplyTo)
	}
}

func TestInboundFromPosted_SkipsLoopAndSystemPosts(t *testing.T) {
	base := mkPostedData("D", "c1", "ualice", "hi")
	re := mkMentionRe("bot")

	cases := []struct {
		name string
		mut  func(*postedEventData)
	}{
		{"own post", func(e *postedEventData) { e.Post.UserID = "ubot" }},
		{"from_bot props", func(e *postedEventData) { e.Post.Props = json.RawMessage(`{"from_bot":"true"}`) }},
		{"from_webhook props", func(e *postedEventData) { e.Post.Props = json.RawMessage(`{"from_webhook":"true"}`) }},
		{"system post type", func(e *postedEventData) { e.Post.Type = "system_join_leave" }},
		{"empty user id", func(e *postedEventData) { e.Post.UserID = "" }},
		{"empty post id", func(e *postedEventData) { e.Post.ID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := base
			tc.mut(&ev)
			if _, ok := inboundFromPosted("srv#ubot", ev, "ubot", re); ok {
				t.Errorf("%s must not be ingested", tc.name)
			}
		})
	}
}

func TestInboundFromPosted_BotPropsFalseIsKept(t *testing.T) {
	// A from_bot/from_webhook marker that is not the literal "true" is NOT a
	// loop marker — the post is ingested (real user behind a webhook still
	// addresses the bot).
	ev := mkPostedData("D", "c1", "ualice", "hi")
	ev.Post.Props = json.RawMessage(`{"from_bot":"false"}`)
	if _, ok := inboundFromPosted("srv#ubot", ev, "ubot", mkMentionRe("bot")); !ok {
		t.Error("from_bot:false should not be treated as a bot loop post")
	}
}

func TestCleanText(t *testing.T) {
	re := mkMentionRe("bot")
	if got := cleanText("@bot hi there", re); got != "hi there" {
		t.Errorf("cleanText(@bot hi there) = %q, want \"hi there\"", got)
	}
	if got := cleanText("no mention here", re); got != "no mention here" {
		t.Errorf("cleanText(no mention) = %q, want unchanged", got)
	}
	// Leading mention with extra surrounding whitespace.
	if got := cleanText("  @bot   spaced  ", re); got != "spaced" {
		t.Errorf("cleanText(spaced) = %q, want \"spaced\"", got)
	}
	// Nil regex: no stripping, only trim.
	if got := cleanText("  hi  ", nil); got != "hi" {
		t.Errorf("cleanText nil re = %q, want \"hi\"", got)
	}
}

func TestMattermostChatType(t *testing.T) {
	cases := []struct {
		in   string
		want channel.ChatType
	}{
		{"D", channel.ChatTypeP2P},
		{"G", channel.ChatTypeGroup},
		{"O", channel.ChatTypeGroup},
		{"P", channel.ChatTypeGroup},
		{"", channel.ChatTypeGroup},  // default branch
		{"X", channel.ChatTypeGroup}, // unknown → group
	}
	for _, tc := range cases {
		if got := mattermostChatType(tc.in); got != tc.want {
			t.Errorf("mattermostChatType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCompileMentionRe(t *testing.T) {
	if re := compileMentionRe(""); re != nil {
		t.Errorf("compileMentionRe(\"\") = %v, want nil", re)
	}

	re := compileMentionRe("bot")
	if re == nil {
		t.Fatal("compileMentionRe(\"bot\") = nil, want non-nil")
	}
	if !re.MatchString("@bot hello") {
		t.Error("should match @bot at start of message")
	}
	if !re.MatchString("hey @bot") {
		t.Error("should match @bot embedded in message")
	}
	if !re.MatchString("@Bot hi") {
		t.Error("should match case-insensitively")
	}
	// Word boundary: @botthing must NOT match — the username token continues.
	if re.MatchString("@botthing") {
		t.Error("@botthing should NOT match (username token continues past @bot)")
	}
	// A bare "bot" without @ is not a mention.
	if re.MatchString("hi bot") {
		t.Error("\"hi bot\" should NOT match (no @ prefix)")
	}
}
