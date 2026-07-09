package mattermost

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// uid builds a deterministic pgtype.UUID whose first byte is b (rest zero) so
// tests can read installation/binding/session ids at a glance. It is the
// mattermost-package twin of slack.uid — the helpers do not cross packages.
func uid(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[0] = b
	u.Valid = true
	return u
}

// fakeOutboundQueries satisfies outboundQueries for the outbound subscriber.
// Each read returns the configured row/error verbatim so a test can drive any
// branch (binding hit/miss, active/revoked installation, lookup failure).
type fakeOutboundQueries struct {
	binding    db.ChannelChatSessionBinding
	bindingErr error
	inst       db.ChannelInstallation
	instErr    error
}

func (f *fakeOutboundQueries) GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error) {
	return f.binding, f.bindingErr
}

func (f *fakeOutboundQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return f.inst, f.instErr
}

// fakeOutboundSender is a replySender fake: it records the last OutboundMessage
// handed to Send and how many times Send was called, so a test can assert the
// resolved target/thread/text without a live HTTP server.
type fakeOutboundSender struct {
	called int
	got    channel.OutboundMessage
	err    error
}

func (f *fakeOutboundSender) Send(_ context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	f.called++
	f.got = out
	return channel.SendResult{MessageID: "post_reply"}, f.err
}

// outboundConfigJSON is an installation config blob whose base64
// bot_token_encrypted decodes to the plaintext "mm-token" under a nil Decrypter
// (decodeCredentials treats a nil Decrypter's decoded bytes as plaintext). The
// field names mirror installConfig exactly: app_id, server_url, bot_user_id,
// bot_username, bot_token_encrypted. server_url is required (decodeCredentials
// rejects an empty one), so it is always populated. It is DISTINCT from the
// replier-owned replierConfigJSON.
const outboundConfigJSON = `{"app_id":"mm.example.test#bot1","server_url":"https://mm.example.test","bot_user_id":"bot1","bot_username":"multibot","bot_token_encrypted":"` +
	// base64.StdEncoding.EncodeToString([]byte("mm-token")) == "bW0tdG9rZW4="
	`bW0tdG9rZW4="}`

// chatDoneEvent builds an EventChatDone envelope carrying the typed
// ChatDonePayload (the form processEvent reads first). sessionID is the raw
// string form on the envelope; content lands on the payload.
func chatDoneEvent(sessionID, content string) events.Event {
	return events.Event{
		Type:          protocol.EventChatDone,
		ChatSessionID: sessionID,
		Payload:       protocol.ChatDonePayload{Content: content},
	}
}

func TestOutbound_PostsReplyToBoundMattermostChannel(t *testing.T) {
	q := &fakeOutboundQueries{
		// Composite "channel:threadRoot" isolation key — the real channel id
		// and reply thread are recovered from the binding config + last_thread_id.
		binding: db.ChannelChatSessionBinding{
			InstallationID: uid(1),
			ChannelChatID:  "abc:root1",
			Config:         []byte(`{"channel_id":"abc"}`),
			LastThreadID:   pgtype.Text{String: "root1", Valid: true},
		},
		inst: db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fs := &fakeOutboundSender{}
	o := NewOutbound(q, nil, nil)
	o.newSender = func(credentials) replySender { return fs }

	o.handleEvent(chatDoneEvent("00000000-0000-0000-0000-000000000001", "**all done**"))

	if fs.called != 1 {
		t.Fatalf("sender called %d times, want 1", fs.called)
	}
	if fs.got.ChatID != "abc" {
		t.Errorf("ChatID = %q, want the real channel from config (not the composite key)", fs.got.ChatID)
	}
	if fs.got.ThreadID != "root1" {
		t.Errorf("ThreadID = %q, want the recorded reply thread from last_thread_id", fs.got.ThreadID)
	}
	if fs.got.Text != "**all done**" {
		t.Errorf("Text = %q, want the raw content posted as-is", fs.got.Text)
	}
}

func TestOutboundTarget(t *testing.T) {
	cases := []struct {
		name           string
		binding        db.ChannelChatSessionBinding
		wantChannelID  string
		wantRootID     string
	}{
		{
			name: "composite key with config channel_id + thread",
			binding: db.ChannelChatSessionBinding{
				ChannelChatID: "abc:root1",
				Config:        []byte(`{"channel_id":"abc"}`),
				LastThreadID:  pgtype.Text{String: "root1", Valid: true},
			},
			wantChannelID: "abc",
			wantRootID:    "root1",
		},
		{
			name: "plain channel id (no composite), thread recorded",
			binding: db.ChannelChatSessionBinding{
				ChannelChatID: "abc",
				LastThreadID:  pgtype.Text{String: "root1", Valid: true},
			},
			wantChannelID: "abc",
			wantRootID:    "root1",
		},
		{
			name: "config wins over composite channel id",
			binding: db.ChannelChatSessionBinding{
				ChannelChatID: "compositeignored",
				Config:        []byte(`{"channel_id":"realchan"}`),
			},
			wantChannelID: "realchan",
			wantRootID:    "",
		},
		{
			name: "no thread recorded yields empty root",
			binding: db.ChannelChatSessionBinding{
				ChannelChatID: "abc",
			},
			wantChannelID: "abc",
			wantRootID:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, root := outboundTarget(tc.binding)
			if ch != tc.wantChannelID {
				t.Errorf("channelID = %q, want %q", ch, tc.wantChannelID)
			}
			if root != tc.wantRootID {
				t.Errorf("rootID = %q, want %q", root, tc.wantRootID)
			}
		})
	}
}

func TestChatDoneContent(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"typed payload", protocol.ChatDonePayload{Content: "hi"}, "hi"},
		{"map form with string content", map[string]any{"content": "hi"}, "hi"},
		{"map form with non-string content", map[string]any{"content": 123}, ""},
		{"map form missing content", map[string]any{"task_id": "t1"}, ""},
		{"nil payload", nil, ""},
		{"string payload", "hi", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatDoneContent(tc.in); got != tc.want {
				t.Errorf("chatDoneContent(%T) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestOutboundProcessEvent(t *testing.T) {
	const sid = "00000000-0000-0000-0000-000000000001"
	activeInst := db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)}
	boundBinding := db.ChannelChatSessionBinding{
		InstallationID: uid(1),
		ChannelChatID:  "abc:root1",
		Config:         []byte(`{"channel_id":"abc"}`),
		LastThreadID:   pgtype.Text{String: "root1", Valid: true},
	}

	cases := []struct {
		name       string
		q          *fakeOutboundQueries
		evt        events.Event
		wantErr    bool
		wantCalled int
	}{
		{
			name:       "mattermost binding + active install + content → posts",
			q:          &fakeOutboundQueries{binding: boundBinding, inst: activeInst},
			evt:        chatDoneEvent(sid, "hello"),
			wantCalled: 1,
		},
		{
			name:       "empty completion content → no send",
			q:          &fakeOutboundQueries{binding: boundBinding, inst: activeInst},
			evt:        chatDoneEvent(sid, ""),
			wantCalled: 0,
		},
		{
			name:       "no mattermost binding (ErrNoRows) → no send, nil error",
			q:          &fakeOutboundQueries{bindingErr: pgx.ErrNoRows},
			evt:        chatDoneEvent(sid, "hi"),
			wantCalled: 0,
		},
		{
			name:       "revoked installation → no send",
			q:          &fakeOutboundQueries{binding: boundBinding, inst: db.ChannelInstallation{ID: uid(1), Status: "revoked", Config: []byte(outboundConfigJSON)}},
			evt:        chatDoneEvent(sid, "hi"),
			wantCalled: 0,
		},
		{
			name:       "invalid chat session uuid → no send, nil error",
			q:          &fakeOutboundQueries{binding: boundBinding, inst: activeInst},
			evt:        chatDoneEvent("not-a-uuid", "hi"),
			wantCalled: 0,
		},
		{
			name:       "installation lookup failure → error propagates",
			q:          &fakeOutboundQueries{binding: boundBinding, instErr: context.DeadlineExceeded},
			evt:        chatDoneEvent(sid, "hi"),
			wantErr:    true,
			wantCalled: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeOutboundSender{}
			o := NewOutbound(tc.q, nil, nil)
			o.newSender = func(credentials) replySender { return fs }

			err := o.processEvent(context.Background(), tc.evt)
			if tc.wantErr && err == nil {
				t.Fatalf("processEvent err = nil, want non-nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("processEvent err = %v, want nil", err)
			}
			if fs.called != tc.wantCalled {
				t.Errorf("sender called %d times, want %d", fs.called, tc.wantCalled)
			}
		})
	}
}

func TestOutbound_Register(t *testing.T) {
	q := &fakeOutboundQueries{
		binding: db.ChannelChatSessionBinding{
			InstallationID: uid(1),
			ChannelChatID:  "abc:root1",
			Config:         []byte(`{"channel_id":"abc"}`),
			LastThreadID:   pgtype.Text{String: "root1", Valid: true},
		},
		inst: db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fs := &fakeOutboundSender{}
	o := NewOutbound(q, nil, nil)
	o.newSender = func(credentials) replySender { return fs }

	bus := events.New()
	o.Register(bus)

	// Publishing the subscribed event must drive the handler synchronously
	// (the bus delivers in registration order) and post the reply.
	bus.Publish(chatDoneEvent("00000000-0000-0000-0000-000000000002", "via bus"))

	if fs.called != 1 {
		t.Fatalf("bus.Publish did not drive the handler: sender called %d times, want 1", fs.called)
	}
	if fs.got.ChatID != "abc" || fs.got.ThreadID != "root1" {
		t.Errorf("posted to ChatID=%q ThreadID=%q, want abc/root1", fs.got.ChatID, fs.got.ThreadID)
	}
}

// keep base64 referenced (the const documents its derivation above) so a future
// edit swapping in base64.StdEncoding.EncodeToString compiles without churn.
var _ = base64.StdEncoding
