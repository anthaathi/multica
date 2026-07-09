package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// mmInbound builds a minimal InboundMessage carrying only the routing fields
// mattermostSessionRouting reads (ChatType, ChatID, ThreadID, MessageID). It is
// the mattermost-package twin of slack.inbound — tests set Source.SenderID /
// Raw on the returned value as needed.
func mmInbound(chatType channel.ChatType, chatID, threadID, msgID string) channel.InboundMessage {
	return channel.InboundMessage{
		MessageID: msgID,
		Source: channel.Source{
			ChannelType: TypeMattermost,
			ChatID:      chatID,
			ChatType:    chatType,
			ThreadID:    threadID,
		},
	}
}

// fakeIdentityQueries implements identityQueries so the cross-installation
// account-link reuse path (MUL-3911) is exercised without a database. Each
// method returns its configured row/error verbatim and records the args the
// resolver handed it, so a test can assert the reuse lookup scope and the
// materialized binding's channel_type.
type fakeIdentityQueries struct {
	binding     db.ChannelUserBinding
	bindErr     error
	reusable    db.ChannelUserBinding
	reusableErr error
	memberErr   error
	createErr   error

	findCalls   int
	findWith    db.FindReusableChannelUserBindingParams
	createCalls int
	createWith  db.CreateChannelUserBindingParams
}

func (f *fakeIdentityQueries) GetChannelUserBindingByUserID(_ context.Context, _ db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error) {
	return f.binding, f.bindErr
}

func (f *fakeIdentityQueries) FindReusableChannelUserBinding(_ context.Context, arg db.FindReusableChannelUserBindingParams) (db.ChannelUserBinding, error) {
	f.findCalls++
	f.findWith = arg
	return f.reusable, f.reusableErr
}

func (f *fakeIdentityQueries) GetMemberByUserAndWorkspace(_ context.Context, _ db.GetMemberByUserAndWorkspaceParams) (db.Member, error) {
	return db.Member{}, f.memberErr
}

func (f *fakeIdentityQueries) CreateChannelUserBinding(_ context.Context, arg db.CreateChannelUserBindingParams) (db.ChannelUserBinding, error) {
	f.createCalls++
	f.createWith = arg
	return db.ChannelUserBinding{}, f.createErr
}

// TestSessionRouting pins mattermostSessionRouting's isolation contract: a DM is
// one continuous session keyed by channel id (replies thread but keep the key),
// while a group message is isolated by THREAD ROOT — the inbound root when
// replying in a thread, else the post's own id. The real channel id always lands
// in the config blob so outbound works even when the key is composite.
func TestSessionRouting(t *testing.T) {
	cases := []struct {
		name           string
		msg            channel.InboundMessage
		wantKey        string
		wantReplyThr   string
		wantChannelCfg string
	}{
		{
			name:           "DM top-level: one session per channel",
			msg:            mmInbound(channel.ChatTypeP2P, "D1", "", "m1"),
			wantKey:        "D1",
			wantReplyThr:   "",
			wantChannelCfg: "D1",
		},
		{
			name:           "DM replying in a thread: still one session, reply into the thread",
			msg:            mmInbound(channel.ChatTypeP2P, "D1", "troot", "m2"),
			wantKey:        "D1",
			wantReplyThr:   "troot",
			wantChannelCfg: "D1",
		},
		{
			name:           "channel top-level @mention: post's own id is the new thread root",
			msg:            mmInbound(channel.ChatTypeGroup, "C1", "", "m3"),
			wantKey:        "C1:m3",
			wantReplyThr:   "m3",
			wantChannelCfg: "C1",
		},
		{
			name:           "channel reply in a thread: isolated by thread root",
			msg:            mmInbound(channel.ChatTypeGroup, "C1", "troot", "m4"),
			wantKey:        "C1:troot",
			wantReplyThr:   "troot",
			wantChannelCfg: "C1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, cfg, reply := mattermostSessionRouting(tc.msg)
			if key != tc.wantKey {
				t.Errorf("bindingKey = %q, want %q", key, tc.wantKey)
			}
			if reply != tc.wantReplyThr {
				t.Errorf("replyThread = %q, want %q", reply, tc.wantReplyThr)
			}
			var parsed mattermostBindingConfig
			if err := json.Unmarshal(cfg, &parsed); err != nil {
				t.Fatalf("config blob is not a mattermostBindingConfig: %v", err)
			}
			if parsed.ChannelID != tc.wantChannelCfg {
				t.Errorf("config.ChannelID = %q, want %q", parsed.ChannelID, tc.wantChannelCfg)
			}
		})
	}
}

// TestThreadIsolation is the resolver-level isolation guard: two top-level
// @bot posts in the SAME channel must derive DISTINCT session binding keys,
// while a follow-up reply in thread 1 derives thread 1's key.
func TestThreadIsolation(t *testing.T) {
	thread1Root, _, _ := mattermostSessionRouting(mmInbound(channel.ChatTypeGroup, "C1", "", "111"))        // @mention starts thread 1
	thread2Root, _, _ := mattermostSessionRouting(mmInbound(channel.ChatTypeGroup, "C1", "", "222"))        // @mention starts thread 2
	thread1Reply, _, _ := mattermostSessionRouting(mmInbound(channel.ChatTypeGroup, "C1", "111", "333")) // reply in thread 1

	if thread1Root == thread2Root {
		t.Errorf("two top-level @bot posts in one channel must isolate: %q == %q", thread1Root, thread2Root)
	}
	if thread1Reply != thread1Root {
		t.Errorf("a follow-up reply must reuse thread 1's key: %q != %q", thread1Reply, thread1Root)
	}
}

// TestNewMattermostResolverSet verifies the ResolverSet is fully wired, stamps
// the mattermost origin_type, and keeps Replier/Typing nil (not typed-nil) when
// their args are nil — while a real replier + typing manager thread through.
func TestNewMattermostResolverSet(t *testing.T) {
	set := NewMattermostResolverSet(nil, nil, nil, nil)
	if set.Installation == nil || set.Identity == nil || set.Dedup == nil || set.Session == nil || set.Audit == nil {
		t.Error("resolver set must populate all required resolvers")
	}
	if set.OriginType != originMattermostChat {
		t.Errorf("OriginType = %q, want %q", set.OriginType, originMattermostChat)
	}
	if set.Replier != nil {
		t.Error("a nil replier arg must leave Replier nil (not a typed-nil interface)")
	}
	if set.Typing != nil {
		t.Error("a nil typing arg must leave Typing nil (not a typed-nil interface)")
	}

	// A real replier + typing manager thread through unchanged.
	set = NewMattermostResolverSet(nil, nil, NewOutboundReplier(OutboundReplierConfig{}), NewTypingIndicatorManager(nil, nil, nil))
	if set.Replier == nil {
		t.Error("a non-nil replier must populate ResolverSet.Replier")
	}
	if set.Typing == nil {
		t.Error("a non-nil typing manager must populate ResolverSet.Typing")
	}
}

// TestDecodeMattermostRaw round-trips the stamped app id out of the opaque Raw
// payload (the installation routing key) and rejects an empty Raw.
func TestDecodeMattermostRaw(t *testing.T) {
	t.Run("round-trips app id", func(t *testing.T) {
		raw, err := json.Marshal(mattermostRawEvent{AppID: "https://mm.example.test#bot1", EventType: "posted"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := decodeMattermostRaw(channel.InboundMessage{Raw: raw})
		if err != nil {
			t.Fatalf("decodeMattermostRaw: %v", err)
		}
		if got.AppID != "https://mm.example.test#bot1" {
			t.Errorf("AppID = %q, want https://mm.example.test#bot1", got.AppID)
		}
		if got.EventType != "posted" {
			t.Errorf("EventType = %q, want posted", got.EventType)
		}
	})
	t.Run("empty raw errors", func(t *testing.T) {
		if _, err := decodeMattermostRaw(channel.InboundMessage{}); err == nil {
			t.Error("an empty Raw must error, got nil")
		}
	})
	t.Run("malformed raw errors", func(t *testing.T) {
		if _, err := decodeMattermostRaw(channel.InboundMessage{Raw: []byte("{not-json")}); err == nil {
			t.Error("malformed Raw must error, got nil")
		}
	})
}

// TestReusableBinding covers the identity resolver's decision to reuse an
// existing account link across installations of the SAME Mattermost server +
// Multica workspace, instead of re-prompting the user for every new bot. The
// reuse scope is the stored server_url (the team_id slot) — read directly via
// reusableBinding and end-to-end through ResolveSender.
func TestReusableBinding(t *testing.T) {
	const senderID = "mmALICE"
	wsID := uid(0x11)
	instB := uid(0xBB) // the installation the message arrives on
	instA := uid(0xAA) // the installation the user already linked
	userID := uid(0x77)

	// inst builds the ResolvedInstallation the message routes to; serverURL is
	// what its stored config carries (the identity-reuse scope key). An empty
	// serverURL means a legacy install with no recorded server.
	inst := func(serverURL string) engine.ResolvedInstallation {
		cfg, _ := json.Marshal(installConfig{AppID: "mm.example.test#bot1", ServerURL: serverURL})
		return engine.ResolvedInstallation{
			ID:          instB,
			WorkspaceID: wsID,
			Platform:    db.ChannelInstallation{ID: instB, WorkspaceID: wsID, Config: cfg},
		}
	}
	const serverURL = "https://mm.example.test"
	msg := mmInbound(channel.ChatTypeP2P, "D1", "", "1.0")
	msg.Source.SenderID = senderID

	t.Run("direct binding resolves without a reuse lookup or write", func(t *testing.T) {
		f := &fakeIdentityQueries{binding: db.ChannelUserBinding{MulticaUserID: userID}}
		got, err := (&identityResolver{q: f}).ResolveSender(context.Background(), inst(serverURL), msg)
		if err != nil {
			t.Fatalf("ResolveSender err = %v", err)
		}
		if got.UserID != userID {
			t.Errorf("UserID = %v, want %v", got.UserID, userID)
		}
		if f.findCalls != 0 || f.createCalls != 0 {
			t.Errorf("directly-bound sender must not trigger reuse (find=%d create=%d)", f.findCalls, f.createCalls)
		}
	})

	t.Run("unlinked sender reuses a same-server link and materializes it as mattermost", func(t *testing.T) {
		f := &fakeIdentityQueries{
			bindErr:  pgx.ErrNoRows,
			reusable: db.ChannelUserBinding{MulticaUserID: userID, InstallationID: instA},
		}
		got, err := (&identityResolver{q: f}).ResolveSender(context.Background(), inst(serverURL), msg)
		if err != nil {
			t.Fatalf("ResolveSender err = %v", err)
		}
		if got.UserID != userID {
			t.Errorf("UserID = %v, want reused %v", got.UserID, userID)
		}
		if f.findCalls != 1 {
			t.Fatalf("reuse lookup must run exactly once, ran %d", f.findCalls)
		}
		// The reuse scope is the stored server URL (team_id slot), scoped to this
		// workspace + sender + mattermost channel type.
		if f.findWith.TeamID != serverURL || f.findWith.ChannelUserID != senderID ||
			f.findWith.ChannelType != string(TypeMattermost) || f.findWith.WorkspaceID != wsID {
			t.Errorf("reuse lookup args = %+v", f.findWith)
		}
		if f.createCalls != 1 {
			t.Fatalf("reused link must be materialized on THIS installation, create ran %d", f.createCalls)
		}
		if f.createWith.InstallationID != instB || f.createWith.MulticaUserID != userID || f.createWith.ChannelUserID != senderID {
			t.Errorf("materialized binding args = %+v (want install=%v user=%v sender=%q)",
				f.createWith, instB, userID, senderID)
		}
		if f.createWith.ChannelType != string(TypeMattermost) {
			t.Errorf("materialized binding ChannelType = %q, want %q", f.createWith.ChannelType, TypeMattermost)
		}
	})

	t.Run("no direct binding and nothing to reuse prompts a link", func(t *testing.T) {
		f := &fakeIdentityQueries{bindErr: pgx.ErrNoRows, reusableErr: pgx.ErrNoRows}
		_, err := (&identityResolver{q: f}).ResolveSender(context.Background(), inst(serverURL), msg)
		if !errors.Is(err, engine.ErrSenderUnbound) {
			t.Fatalf("err = %v, want ErrSenderUnbound", err)
		}
		if f.createCalls != 0 {
			t.Errorf("nothing to reuse must not write a binding, create ran %d", f.createCalls)
		}
	})

	t.Run("reusable link whose user left the workspace prompts a fresh link", func(t *testing.T) {
		f := &fakeIdentityQueries{
			bindErr:   pgx.ErrNoRows,
			reusable:  db.ChannelUserBinding{MulticaUserID: userID, InstallationID: instA},
			memberErr: pgx.ErrNoRows,
		}
		_, err := (&identityResolver{q: f}).ResolveSender(context.Background(), inst(serverURL), msg)
		if !errors.Is(err, engine.ErrSenderUnbound) {
			t.Fatalf("err = %v, want ErrSenderUnbound (fresh link, not not-member)", err)
		}
		if f.createCalls != 0 {
			t.Errorf("must not materialize a binding for a non-member, create ran %d", f.createCalls)
		}
	})

	t.Run("legacy installation with no server never attempts reuse", func(t *testing.T) {
		f := &fakeIdentityQueries{bindErr: pgx.ErrNoRows}
		_, err := (&identityResolver{q: f}).ResolveSender(context.Background(), inst(""), msg)
		if !errors.Is(err, engine.ErrSenderUnbound) {
			t.Fatalf("err = %v, want ErrSenderUnbound", err)
		}
		if f.findCalls != 0 {
			t.Errorf("an install with no recorded server must not attempt cross-install reuse, find ran %d", f.findCalls)
		}
	})

	t.Run("directly-bound non-member surfaces not-member", func(t *testing.T) {
		f := &fakeIdentityQueries{binding: db.ChannelUserBinding{MulticaUserID: userID}, memberErr: pgx.ErrNoRows}
		_, err := (&identityResolver{q: f}).ResolveSender(context.Background(), inst(serverURL), msg)
		if !errors.Is(err, engine.ErrSenderNotMember) {
			t.Fatalf("err = %v, want ErrSenderNotMember", err)
		}
	})

	// reusableBinding directly: a returned candidate is reused, and the lookup
	// itself never creates a binding (materialization is ResolveSender's job).
	t.Run("reusableBinding returns the candidate without creating", func(t *testing.T) {
		f := &fakeIdentityQueries{reusable: db.ChannelUserBinding{MulticaUserID: userID, InstallationID: instA}}
		got, ok, err := (&identityResolver{q: f}).reusableBinding(context.Background(), inst(serverURL), senderID)
		if err != nil || !ok || got.MulticaUserID != userID {
			t.Fatalf("reusableBinding = %+v ok=%v err=%v, want reused user %v", got, ok, err, userID)
		}
		if f.createCalls != 0 {
			t.Errorf("reusableBinding must not create a binding itself, create ran %d", f.createCalls)
		}
	})

	// When FindReusableChannelUserBinding returns ErrNoRows the lookup falls
	// through (ok=false, nil error) so ResolveSender can surface the link prompt.
	t.Run("reusableBinding with no candidate falls through", func(t *testing.T) {
		f := &fakeIdentityQueries{reusableErr: pgx.ErrNoRows}
		_, ok, err := (&identityResolver{q: f}).reusableBinding(context.Background(), inst(serverURL), senderID)
		if err != nil || ok {
			t.Fatalf("reusableBinding = ok=%v err=%v, want ok=false nil err", ok, err)
		}
	})
}
