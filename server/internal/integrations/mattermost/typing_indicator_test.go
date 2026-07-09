package mattermost

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// mmReaction records one Add/Remove reaction call so a test can assert the
// indicator's lifecycle (post id, emoji, bot user id) without a live REST
// client. Uniquely prefixed so it cannot collide with another file's helper.
type mmReaction struct {
	userID string
	postID string
	emoji  string
}

// fakeReactorAPI is a restAPI fake that only does useful work on AddReaction /
// RemoveReaction; the other REST methods return zero values because the typing
// indicator never calls them. Errors are configurable to exercise the
// best-effort swallow paths.
type fakeReactorAPI struct {
	added   []mmReaction
	removed []mmReaction
	addErr  error
	remErr  error
}

func (f *fakeReactorAPI) AddReaction(_ context.Context, userID, postID, emoji string) error {
	f.added = append(f.added, mmReaction{userID: userID, postID: postID, emoji: emoji})
	return f.addErr
}

func (f *fakeReactorAPI) RemoveReaction(_ context.Context, postID, emoji string) error {
	f.removed = append(f.removed, mmReaction{postID: postID, emoji: emoji})
	return f.remErr
}

// The remaining restAPI surface is unused by the typing indicator.
func (f *fakeReactorAPI) GetMe(context.Context) (mmUser, error)               { return mmUser{}, nil }
func (f *fakeReactorAPI) CreatePost(context.Context, string, string, string) (mmPost, error) {
	return mmPost{}, nil
}
func (f *fakeReactorAPI) GetPostsForChannel(context.Context, string, int, string) (mmPostList, error) {
	return mmPostList{}, nil
}
func (f *fakeReactorAPI) GetPostThread(context.Context, string) (mmPostList, error) {
	return mmPostList{}, nil
}
func (f *fakeReactorAPI) GetUsersByIDs(context.Context, []string) ([]mmUser, error) {
	return nil, nil
}

// fakeTypingQueries satisfies TypingIndicatorQueries so Clear can re-resolve
// the installation's bot token from the binding without a database.
type fakeTypingQueries struct {
	binding    db.ChannelChatSessionBinding
	bindingErr error
	inst       db.ChannelInstallation
	instErr    error
}

func (f *fakeTypingQueries) GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error) {
	return f.binding, f.bindingErr
}

func (f *fakeTypingQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return f.inst, f.instErr
}

func TestIsPostTooOld(t *testing.T) {
	cases := []struct {
		name string
		ms   int64
		want bool
	}{
		{"now is fresh", time.Now().UnixMilli(), false},
		{"just under max age is fresh", time.Now().Add(-90 * time.Second).UnixMilli(), false},
		{"zero is treated as fresh", 0, false},
		{"negative is treated as fresh", -5, false},
		{"older than 2 minutes is stale", time.Now().Add(-5 * time.Minute).UnixMilli(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPostTooOld(tc.ms); got != tc.want {
				t.Errorf("isPostTooOld(%d) = %v, want %v", tc.ms, got, tc.want)
			}
		})
	}
}

func TestTypingIndicator_Add(t *testing.T) {
	t.Run("reacts with eyes and records state", func(t *testing.T) {
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(&fakeTypingQueries{}, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		m.Add(context.Background(),
			db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
			uid(7), "post1", time.Now().UnixMilli())

		if len(fr.added) != 1 {
			t.Fatalf("AddReaction called %d times, want 1", len(fr.added))
		}
		if fr.added[0].emoji != typingEmoji {
			t.Errorf("emoji = %q, want %q", fr.added[0].emoji, typingEmoji)
		}
		if fr.added[0].emoji != "eyes" {
			t.Errorf("typingEmoji = %q, want \"eyes\"", typingEmoji)
		}
		if fr.added[0].postID != "post1" {
			t.Errorf("postID = %q, want post1", fr.added[0].postID)
		}
		if fr.added[0].userID != "bot1" { // creds.BotUserID from outboundConfigJSON
			t.Errorf("userID = %q, want bot1 (the installation's bot user id)", fr.added[0].userID)
		}
	})

	t.Run("empty post id is a no-op", func(t *testing.T) {
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(&fakeTypingQueries{}, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		m.Add(context.Background(),
			db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
			uid(7), "", time.Now().UnixMilli())

		if len(fr.added) != 0 {
			t.Errorf("empty post id must not react, added %d", len(fr.added))
		}
	})

	t.Run("stale create_at is skipped", func(t *testing.T) {
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(&fakeTypingQueries{}, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		m.Add(context.Background(),
			db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
			uid(7), "post1", time.Now().Add(-5*time.Minute).UnixMilli())

		if len(fr.added) != 0 {
			t.Errorf("stale post must not react, added %d", len(fr.added))
		}
	})

	t.Run("credential decode error is swallowed", func(t *testing.T) {
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(&fakeTypingQueries{}, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		// An undecodable config (missing server_url) makes decodeCredentials
		// fail; the error must be swallowed and no reaction added.
		m.Add(context.Background(),
			db.ChannelInstallation{Config: []byte("not-json")},
			uid(7), "post1", time.Now().UnixMilli())

		if len(fr.added) != 0 {
			t.Errorf("decode error must not react, added %d", len(fr.added))
		}
	})
}

func TestTypingIndicator_Clear(t *testing.T) {
	t.Run("removes recorded reaction via re-resolved credentials", func(t *testing.T) {
		sessionID := uid(7)
		q := &fakeTypingQueries{
			binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
			inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
		}
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(q, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		m.Add(context.Background(),
			db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
			sessionID, "post1", time.Now().UnixMilli())

		m.Clear(context.Background(), sessionID)

		if len(fr.removed) != 1 {
			t.Fatalf("RemoveReaction called %d times, want 1", len(fr.removed))
		}
		if fr.removed[0].postID != "post1" {
			t.Errorf("removed postID = %q, want post1", fr.removed[0].postID)
		}
		if fr.removed[0].emoji != typingEmoji {
			t.Errorf("removed emoji = %q, want %q", fr.removed[0].emoji, typingEmoji)
		}
	})

	t.Run("state dropped on clear, second clear is a no-op", func(t *testing.T) {
		sessionID := uid(7)
		q := &fakeTypingQueries{
			binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
			inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
		}
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(q, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		m.Add(context.Background(),
			db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
			sessionID, "post1", time.Now().UnixMilli())

		m.Clear(context.Background(), sessionID)
		m.Clear(context.Background(), sessionID)

		if len(fr.removed) != 1 {
			t.Errorf("second clear must be a no-op, removed %d times", len(fr.removed))
		}
	})

	t.Run("session with no recorded state is a no-op", func(t *testing.T) {
		q := &fakeTypingQueries{
			binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
			inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
		}
		fr := &fakeReactorAPI{}
		m := NewTypingIndicatorManager(q, nil, nil)
		m.newAPI = func(credentials) restAPI { return fr }

		m.Clear(context.Background(), uid(7)) // never Added

		if len(fr.removed) != 0 {
			t.Errorf("clear with no state must not remove, removed %d", len(fr.removed))
		}
	})
}

func TestChatSessionIDFromEvent(t *testing.T) {
	sessionID := uid(7)
	sessionStr := util.UUIDToString(sessionID)

	cases := []struct {
		name string
		evt  events.Event
		want bool
	}{
		{
			name: "EventChatDone envelope carries ChatSessionID",
			evt:  events.Event{Type: protocol.EventChatDone, ChatSessionID: sessionStr},
			want: true,
		},
		{
			name: "EventTaskFailed broadcast payload carries chat_session_id",
			evt:  events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"chat_session_id": sessionStr}},
			want: true,
		},
		{
			name: "non-chat event (no session anywhere)",
			evt:  events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"task_id": "t1"}},
			want: false,
		},
		{
			name: "envelope with invalid uuid string",
			evt:  events.Event{Type: protocol.EventChatDone, ChatSessionID: "not-a-uuid"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := chatSessionIDFromEvent(tc.evt)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && got != sessionID {
				t.Errorf("session id = %v, want %v", got, sessionID)
			}
		})
	}
}

func TestTypingIndicator_RegisterClearsOnChatDone(t *testing.T) {
	sessionID := uid(7)
	q := &fakeTypingQueries{
		binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
		inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fr := &fakeReactorAPI{}
	m := NewTypingIndicatorManager(q, nil, nil)
	m.newAPI = func(credentials) restAPI { return fr }
	m.Add(context.Background(),
		db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
		sessionID, "post1", time.Now().UnixMilli())

	bus := events.New()
	m.Register(bus)

	bus.Publish(events.Event{
		Type:          protocol.EventChatDone,
		ChatSessionID: util.UUIDToString(sessionID),
	})

	if len(fr.removed) != 1 {
		t.Fatalf("EventChatDone must clear the reaction, removed %d", len(fr.removed))
	}
}

func TestTypingIndicator_RegisterClearsOnTaskFailed(t *testing.T) {
	sessionID := uid(7)
	q := &fakeTypingQueries{
		binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
		inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fr := &fakeReactorAPI{}
	m := NewTypingIndicatorManager(q, nil, nil)
	m.newAPI = func(credentials) restAPI { return fr }
	m.Add(context.Background(),
		db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
		sessionID, "post1", time.Now().UnixMilli())

	bus := events.New()
	m.Register(bus)

	// EventTaskFailed carries the session id only in the broadcast payload map,
	// not on the envelope — the clear handler must read it from there.
	bus.Publish(events.Event{
		Type:    protocol.EventTaskFailed,
		Payload: map[string]any{"chat_session_id": util.UUIDToString(sessionID)},
	})

	if len(fr.removed) != 1 {
		t.Fatalf("EventTaskFailed must clear the reaction, removed %d", len(fr.removed))
	}
}

func TestTypingIndicator_IgnoresNonChatEvent(t *testing.T) {
	sessionID := uid(7)
	q := &fakeTypingQueries{
		binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
		inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fr := &fakeReactorAPI{}
	m := NewTypingIndicatorManager(q, nil, nil)
	m.newAPI = func(credentials) restAPI { return fr }
	m.Add(context.Background(),
		db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
		sessionID, "post1", time.Now().UnixMilli())

	bus := events.New()
	m.Register(bus)

	// An issue/autopilot event with no chat session must not clear.
	bus.Publish(events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"task_id": "t1"}})

	if len(fr.removed) != 0 {
		t.Errorf("non-chat event must not clear, removed %d", len(fr.removed))
	}
	// Reaction state is still intact.
	m.mu.RLock()
	states := m.states[util.UUIDToString(sessionID)]
	m.mu.RUnlock()
	if len(states) != 1 {
		t.Errorf("state must survive a non-chat event, got %d", len(states))
	}
}

func TestOnSettled(t *testing.T) {
	sessionID := uid(7)
	q := &fakeTypingQueries{
		binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
		inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fr := &fakeReactorAPI{}
	m := NewTypingIndicatorManager(q, nil, nil)
	m.newAPI = func(credentials) restAPI { return fr }
	m.Add(context.Background(),
		db.ChannelInstallation{Config: []byte(outboundConfigJSON)},
		sessionID, "post1", time.Now().UnixMilli())

	// When the run trigger enqueues no task, no task-lifecycle event fires, so
	// the engine clears the indicator through OnSettled instead of the bus.
	(&mattermostTypingNotifier{mgr: m}).OnSettled(context.Background(), sessionID)

	if len(fr.removed) != 1 || fr.removed[0].postID != "post1" {
		t.Fatalf("OnSettled must clear the reaction, removed = %+v", fr.removed)
	}
}

func TestOnIngested(t *testing.T) {
	sessionID := uid(7)
	q := &fakeTypingQueries{
		binding: db.ChannelChatSessionBinding{InstallationID: uid(1)},
		inst:    db.ChannelInstallation{ID: uid(1), Status: "active", Config: []byte(outboundConfigJSON)},
	}
	fr := &fakeReactorAPI{}
	m := NewTypingIndicatorManager(q, nil, nil)
	m.newAPI = func(credentials) restAPI { return fr }

	// OnIngested recovers the installation row from Platform and the post's
	// create_at from msg.Raw (0 / missing = treat as fresh).
	inst := engine.ResolvedInstallation{Platform: db.ChannelInstallation{Config: []byte(outboundConfigJSON)}}
	msg := channel.InboundMessage{
		MessageID: "post1",
		Raw:       []byte(`{"create_at":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `,"event_type":"posted"}`),
	}

	(&mattermostTypingNotifier{mgr: m}).OnIngested(context.Background(), inst, msg, sessionID)

	if len(fr.added) != 1 {
		t.Fatalf("OnIngested must react, added %d", len(fr.added))
	}
	if fr.added[0].postID != "post1" {
		t.Errorf("postID = %q, want post1", fr.added[0].postID)
	}
	if fr.added[0].emoji != typingEmoji {
		t.Errorf("emoji = %q, want %q", fr.added[0].emoji, typingEmoji)
	}
}

func TestOnIngested_UnsupportedPlatform(t *testing.T) {
	fr := &fakeReactorAPI{}
	m := NewTypingIndicatorManager(&fakeTypingQueries{}, nil, nil)
	m.newAPI = func(credentials) restAPI { return fr }

	// A non-Mattermost Platform must short-circuit without reacting.
	(&mattermostTypingNotifier{mgr: m}).OnIngested(context.Background(),
		engine.ResolvedInstallation{Platform: "not-an-installation"},
		channel.InboundMessage{MessageID: "post1"}, uid(7))

	if len(fr.added) != 0 {
		t.Errorf("unsupported platform must not react, added %d", len(fr.added))
	}
}

