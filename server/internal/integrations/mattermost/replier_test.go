package mattermost

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeReplySender records the last OutboundMessage Send was asked to post and
// how many times. It stands in for *mattermostSender so the replier's Send
// path (chunking + threading) is exercised without a real Mattermost server.
type fakeReplySender struct {
	sent  *channel.OutboundMessage
	calls int
}

func (f *fakeReplySender) Send(_ context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	f.calls++
	cp := out
	f.sent = &cp
	return channel.SendResult{MessageID: "post-1"}, nil
}

// fakeBindingMinter records the Mint args and returns a BindingToken whose Raw
// is configurable, so the NeedsBinding prompt's redeem URL can be asserted.
type fakeBindingMinter struct {
	raw     string
	gotWS   pgtype.UUID
	gotInst pgtype.UUID
	gotUser string
	calls   int
}

func (f *fakeBindingMinter) Mint(_ context.Context, ws, inst pgtype.UUID, user string) (BindingToken, error) {
	f.calls++
	f.gotWS, f.gotInst, f.gotUser = ws, inst, user
	return BindingToken{Raw: f.raw}, nil
}

// replierConfigJSON carries an identity-decryptable bot token (base64 of
// "test") so decodeCredentials succeeds inside post() with a nil Decrypter.
// server_url MUST be non-empty or decodeCredentials refuses the config.
const replierConfigJSON = `{"app_id":"https://chat.example.test#botuserid","server_url":"https://chat.example.test","bot_user_id":"botuserid","bot_username":"botuser","bot_token_encrypted":"dGVzdA=="}`

// mmReplier wires a replier whose sender factory is replaced by a fake, so
// Reply's outcomes are observable without a live Mattermost connection. The
// binding minter may be nil to exercise the "no binding configured" path.
func mmReplier(binding bindingMinter, sender replySender) *OutboundReplier {
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding: binding,
		Decrypt: nil,
		AppURL:  "https://app.example.com",
	})
	r.newSender = func(credentials) replySender { return sender }
	return r
}

func mmResolvedInstallation(t *testing.T) engine.ResolvedInstallation {
	t.Helper()
	return engine.ResolvedInstallation{
		ID:          mustUUID(t, "44444444-4444-4444-4444-444444444444"),
		WorkspaceID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		Active:      true,
		// Platform carries the config blob post() decodes for the bot token.
		Platform: db.ChannelInstallation{Config: []byte(replierConfigJSON)},
	}
}

func mmInboundForReply() channel.InboundMessage {
	return channel.InboundMessage{
		MessageID: "post-1700000000000",
		Source: channel.Source{
			ChannelType: TypeMattermost,
			ChatID:      "chatC1",
			ChatType:    channel.ChatTypeGroup,
			SenderID:    "mmALICE",
			ThreadID:    "post-root-1",
		},
	}
}

func TestReply_NeedsBinding_MintsAndPostsPrompt(t *testing.T) {
	sender := &fakeReplySender{}
	// A raw token carrying URL sub-delimiters (+, /) exercises url.QueryEscape
	// so the redeem link stays a single well-formed URL.
	minter := &fakeBindingMinter{raw: "rawTokenA/B+C"}
	r := mmReplier(minter, sender)
	inst := mmResolvedInstallation(t)
	msg := mmInboundForReply()

	r.Reply(context.Background(), inst, msg, engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "mmALICE",
	})

	if minter.calls != 1 {
		t.Fatalf("Mint called %d times, want 1", minter.calls)
	}
	if minter.gotUser != "mmALICE" {
		t.Errorf("Mint received user %q, want mmALICE", minter.gotUser)
	}
	if minter.gotWS != inst.WorkspaceID || minter.gotInst != inst.ID {
		t.Errorf("Mint received ws=%v inst=%v, want %v / %v",
			minter.gotWS, minter.gotInst, inst.WorkspaceID, inst.ID)
	}
	if sender.calls != 1 || sender.sent == nil {
		t.Fatalf("expected exactly one reply, got %d", sender.calls)
	}
	if sender.sent.ChatID != "chatC1" || sender.sent.ThreadID != "post-root-1" {
		t.Errorf("reply target = chat %q thread %q, want chatC1/post-root-1",
			sender.sent.ChatID, sender.sent.ThreadID)
	}
	// The redeem URL must carry the raw token URL-escaped, live under the
	// configured app host's mattermost bind path, and flag the 15-minute
	// expiry.
	wantURL := "https://app.example.com/mattermost/bind?token=rawTokenA%2FB%2BC"
	if !strings.Contains(sender.sent.Text, wantURL) {
		t.Errorf("prompt text = %q\nwant it to contain %q", sender.sent.Text, wantURL)
	}
	if !strings.Contains(sender.sent.Text, "[link your account]") {
		t.Errorf("prompt text = %q\nwant a markdown link label", sender.sent.Text)
	}
	if !strings.Contains(sender.sent.Text, "15 minutes") {
		t.Errorf("prompt text = %q\nwant the 15-minute expiry notice", sender.sent.Text)
	}
}

func TestReply_NeedsBinding_EmptySender_NoMintNoSend(t *testing.T) {
	sender := &fakeReplySender{}
	minter := &fakeBindingMinter{raw: "tok"}
	r := mmReplier(minter, sender)
	// No result.Sender AND no msg.Source.SenderID: sendBindingPrompt returns
	// early with an error before minting or posting.
	msg := mmInboundForReply()
	msg.Source.SenderID = ""

	r.Reply(context.Background(), mmResolvedInstallation(t), msg, engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
	})

	if minter.calls != 0 {
		t.Errorf("Mint must not be called without a sender, got %d", minter.calls)
	}
	if sender.calls != 0 {
		t.Errorf("no prompt must be posted without a sender, got %d sends", sender.calls)
	}
}

func TestReply_NeedsBinding_NoBindingConfig_Skips(t *testing.T) {
	sender := &fakeReplySender{}
	// A replier configured without a Binding minter cannot mint, so the prompt
	// is skipped (the offline/archived/issue notices still fire).
	r := mmReplier(nil, sender)
	r.Reply(context.Background(), mmResolvedInstallation(t), mmInboundForReply(), engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "mmALICE",
	})
	if sender.calls != 0 {
		t.Errorf("NeedsBinding with no binding minter must not post, got %d sends", sender.calls)
	}
}

func TestReply_AgentOfflineAndArchived_PostNotices(t *testing.T) {
	for _, tc := range []struct {
		outcome engine.Outcome
		want    string
	}{
		{engine.OutcomeAgentOffline, agentOfflineText},
		{engine.OutcomeAgentArchived, agentArchivedText},
	} {
		sender := &fakeReplySender{}
		r := mmReplier(&fakeBindingMinter{}, sender)
		r.Reply(context.Background(), mmResolvedInstallation(t), mmInboundForReply(),
			engine.Result{Outcome: tc.outcome})
		if sender.calls != 1 || sender.sent == nil {
			t.Errorf("outcome %s: got %d sends, want 1", tc.outcome, sender.calls)
			continue
		}
		if sender.sent.Text != tc.want {
			t.Errorf("outcome %s: text = %q, want %q", tc.outcome, sender.sent.Text, tc.want)
		}
	}
}

func TestReply_IngestedWithIssue_Confirms(t *testing.T) {
	sender := &fakeReplySender{}
	r := mmReplier(&fakeBindingMinter{}, sender)
	r.Reply(context.Background(), mmResolvedInstallation(t), mmInboundForReply(), engine.Result{
		Outcome:         engine.OutcomeIngested,
		IssueID:         mustUUID(t, "55555555-5555-5555-5555-555555555555"),
		IssueIdentifier: "MUL-42",
		IssueTitle:      "Fix the thing",
	})
	if sender.calls != 1 || sender.sent == nil {
		t.Fatalf("expected one confirmation, got %d", sender.calls)
	}
	if sender.sent.Text != "✅ Created MUL-42 — Fix the thing" {
		t.Errorf("confirmation text = %q", sender.sent.Text)
	}
}

func TestReply_IngestedWithoutIssue_Silent(t *testing.T) {
	sender := &fakeReplySender{}
	r := mmReplier(&fakeBindingMinter{}, sender)
	// A plain chat message (no /issue) must NOT post — the agent's own reply
	// lands via the EventChatDone outbound subscriber.
	r.Reply(context.Background(), mmResolvedInstallation(t), mmInboundForReply(), engine.Result{
		Outcome: engine.OutcomeIngested,
	})
	if sender.calls != 0 {
		t.Errorf("plain ingested message must stay silent, got %d sends", sender.calls)
	}
}

func TestReply_Dropped_Silent(t *testing.T) {
	sender := &fakeReplySender{}
	r := mmReplier(&fakeBindingMinter{}, sender)
	r.Reply(context.Background(), mmResolvedInstallation(t), mmInboundForReply(),
		engine.Result{Outcome: engine.OutcomeDropped})
	if sender.calls != 0 {
		t.Errorf("dropped outcome must stay silent, got %d sends", sender.calls)
	}
}

func TestIssueCreatedText(t *testing.T) {
	cases := []struct {
		name string
		res  engine.Result
		want string
	}{
		{"identifier and title", engine.Result{IssueIdentifier: "MUL-7", IssueTitle: "Title"}, "✅ Created MUL-7 — Title"},
		{"identifier without title", engine.Result{IssueIdentifier: "MUL-7"}, "✅ Created MUL-7"},
		{"only issue number", engine.Result{IssueNumber: 9}, "✅ Created #9"},
		{"whitespace-only title trimmed", engine.Result{IssueIdentifier: "MUL-1", IssueTitle: "   "}, "✅ Created MUL-1"},
		{"zero number fallback", engine.Result{}, "✅ Created #0"},
	}
	for _, c := range cases {
		if got := issueCreatedText(c.res); got != c.want {
			t.Errorf("%s: issueCreatedText = %q, want %q", c.name, got, c.want)
		}
	}
}
