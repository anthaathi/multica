package mattermost

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeHistoryQueries satisfies historyQueries so the reader's binding +
// installation lookups are driven without a database. Each read returns the
// configured row/error verbatim so a test can drive the bound / no-binding /
// inactive-install branches.
type fakeHistoryQueries struct {
	binding    db.ChannelChatSessionBinding
	bindingErr error
	inst       db.ChannelInstallation
	instErr    error
}

func (f *fakeHistoryQueries) GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error) {
	return f.binding, f.bindingErr
}

func (f *fakeHistoryQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return f.inst, f.instErr
}

// fakeHistoryAPI is a restAPI fake for the reader's client: it serves the
// channel-posts / thread / users-by-ids calls the reader makes and records the
// last arguments so a test can assert the resolved channel id, page limit, and
// thread anchor. The REST surface the reader never touches (GetMe / CreatePost
// / reactions) returns zero values.
type fakeHistoryAPI struct {
	posts    mmPostList
	thread   mmPostList
	users    []mmUser
	usersErr error

	postsCalls  int
	threadCalls int
	usersCalls  int

	lastChannelID string
	lastLimit     int
	lastBefore    string
	lastThreadID  string
	lastUserIDs   []string
}

func (f *fakeHistoryAPI) GetMe(context.Context) (mmUser, error) { return mmUser{}, nil }

func (f *fakeHistoryAPI) CreatePost(context.Context, string, string, string) (mmPost, error) {
	return mmPost{}, nil
}

func (f *fakeHistoryAPI) GetPostsForChannel(_ context.Context, channelID string, perPage int, beforePostID string) (mmPostList, error) {
	f.postsCalls++
	f.lastChannelID = channelID
	f.lastLimit = perPage
	f.lastBefore = beforePostID
	return f.posts, nil
}

func (f *fakeHistoryAPI) GetPostThread(_ context.Context, postID string) (mmPostList, error) {
	f.threadCalls++
	f.lastThreadID = postID
	return f.thread, nil
}

func (f *fakeHistoryAPI) AddReaction(context.Context, string, string, string) error { return nil }

func (f *fakeHistoryAPI) RemoveReaction(context.Context, string, string) error { return nil }

func (f *fakeHistoryAPI) GetUsersByIDs(_ context.Context, ids []string) ([]mmUser, error) {
	f.usersCalls++
	f.lastUserIDs = ids
	return f.users, f.usersErr
}

// mmActiveInstall is an active Mattermost installation whose config
// decodeCredentials accepts under a nil Decrypter (outboundConfigJSON is owned
// by outbound_test.go). The bot user id is "bot1".
func mmActiveInstall() db.ChannelInstallation {
	return db.ChannelInstallation{Status: "active", Config: []byte(outboundConfigJSON)}
}

// mmGroupBinding builds a group session binding pinned to channel "C1" whose
// own thread root is threadRoot (mirrors slack.groupBinding).
func mmGroupBinding(threadRoot string) db.ChannelChatSessionBinding {
	b := db.ChannelChatSessionBinding{
		InstallationID: uid(2),
		ChannelChatID:  "C1:" + threadRoot,
		ChatType:       string(channel.ChatTypeGroup),
		Config:         []byte(`{"channel_id":"C1"}`),
	}
	if threadRoot != "" {
		b.LastThreadID = pgtype.Text{String: threadRoot, Valid: true}
	}
	return b
}

// mmDMBinding builds a DM session binding pinned to channel "D1" with no thread.
func mmDMBinding() db.ChannelChatSessionBinding {
	return db.ChannelChatSessionBinding{
		InstallationID: uid(2),
		ChannelChatID:  "D1",
		ChatType:       string(channel.ChatTypeP2P),
		Config:         []byte(`{"channel_id":"D1"}`),
	}
}

// newMMHistory wires a reader whose client factory is replaced by a fake, so
// ChannelOverview / Thread are observable without a live Mattermost server. A
// nil Decrypter treats the stored token bytes as plaintext.
func newMMHistory(q historyQueries, api restAPI) *History {
	h := NewHistory(q, nil, nil)
	h.newClient = func(credentials) restAPI { return api }
	return h
}

// mmFindByID returns the page message with the given id, or nil.
func mmFindByID(msgs []channel.HistoryMessage, id string) *channel.HistoryMessage {
	for i := range msgs {
		if msgs[i].ID == id {
			return &msgs[i]
		}
	}
	return nil
}

// TestOrderedPosts flattens a {order, posts} list into oldest-first posts. The
// order array is newest-first; ids missing from the posts map are skipped, and
// the result is sorted by create_at ascending regardless of order.
func TestOrderedPosts(t *testing.T) {
	list := mmPostList{
		Order: []string{"3", "2", "1"}, // newest-first
		Posts: map[string]mmPost{
			"1": {ID: "1", CreateAt: 100},
			"2": {ID: "2", CreateAt: 200},
			"3": {ID: "3", CreateAt: 300},
		},
	}
	got := orderedPosts(list)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	for i, want := range []string{"1", "2", "3"} {
		if got[i].ID != want {
			t.Errorf("got[%d].ID = %q, want %q (oldest-first)", i, got[i].ID, want)
		}
	}

	// ids missing from the posts map are skipped — no zero-value placeholders.
	list2 := mmPostList{
		Order: []string{"2", "ghost", "1"},
		Posts: map[string]mmPost{
			"1": {ID: "1", CreateAt: 100},
			"2": {ID: "2", CreateAt: 200},
		},
	}
	got2 := orderedPosts(list2)
	if len(got2) != 2 || got2[0].ID != "1" || got2[1].ID != "2" {
		t.Fatalf("missing ids must be skipped, got %+v", got2)
	}

	// create_at drives the order, not the order array's position.
	list3 := mmPostList{
		Order: []string{"a", "b"},
		Posts: map[string]mmPost{
			"a": {ID: "a", CreateAt: 500},
			"b": {ID: "b", CreateAt: 400},
		},
	}
	got3 := orderedPosts(list3)
	if len(got3) != 2 || got3[0].ID != "b" || got3[1].ID != "a" {
		t.Fatalf("expected create_at ascending [b,a], got %+v", got3)
	}
}

// TestClampHistoryLimit pins the page-size bounds: 0/negative → default,
// over-max → max, in-range → unchanged.
func TestClampHistoryLimit(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, defaultHistoryLimit},
		{-5, defaultHistoryLimit},
		{1, 1},
		{defaultHistoryLimit, defaultHistoryLimit},
		{maxHistoryLimit, maxHistoryLimit},
		{maxHistoryLimit + 1, maxHistoryLimit},
		{99999, maxHistoryLimit},
	}
	for _, c := range cases {
		if got := clampHistoryLimit(c.in); got != c.want {
			t.Errorf("clampHistoryLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestFormatPostTS renders a create_at (ms since epoch) as a fixed-width 13-char
// decimal string so pages sort lexicographically; negatives clamp to 0.
func TestFormatPostTS(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0000000000000"},
		{1, "0000000000001"},
		{1700000000123, "1700000000123"},
	}
	for _, c := range cases {
		got := formatPostTS(c.in)
		if got != c.want {
			t.Errorf("formatPostTS(%d) = %q, want %q", c.in, got, c.want)
		}
		if len(got) != 13 {
			t.Errorf("formatPostTS(%d) length = %d, want 13", c.in, len(got))
		}
	}
	if got := formatPostTS(-42); got != "0000000000000" {
		t.Errorf("formatPostTS(-42) = %q, want 0000000000000", got)
	}
}

// TestHistoryTarget recovers the real channel id + the session's own thread root
// from a binding: the config channel_id wins over the (possibly composite)
// channel_chat_id, and the thread root comes from last_thread_id, falling back
// to the composite-key suffix; empty for a DM.
func TestHistoryTarget(t *testing.T) {
	t.Run("composite key with config and last_thread_id", func(t *testing.T) {
		b := db.ChannelChatSessionBinding{
			ChannelChatID: "chan:ignored",
			Config:        []byte(`{"channel_id":"realchan"}`),
			LastThreadID:  pgtype.Text{String: "roothere", Valid: true},
		}
		ch, root := historyTarget(b)
		if ch != "realchan" {
			t.Errorf("channelID = %q, want realchan (from config)", ch)
		}
		if root != "roothere" {
			t.Errorf("threadRoot = %q, want roothere (from last_thread_id)", root)
		}
	})

	t.Run("composite key with no last_thread_id falls back to the suffix", func(t *testing.T) {
		b := db.ChannelChatSessionBinding{
			ChannelChatID: "chan:rootsuffix",
			Config:        []byte(`{"channel_id":"realchan"}`),
		}
		ch, root := historyTarget(b)
		if ch != "realchan" || root != "rootsuffix" {
			t.Errorf("got %q/%q, want realchan/rootsuffix", ch, root)
		}
	})

	t.Run("plain DM binding: channel id, empty root", func(t *testing.T) {
		ch, root := historyTarget(mmDMBinding())
		if ch != "D1" || root != "" {
			t.Errorf("dm: got %q/%q, want D1/<empty>", ch, root)
		}
	})
}

// TestLabel covers historyLabeler: the bot is "Bot", a resolved human gets their
// real display name, and an unresolved sender falls back to a stable positional
// "User N" that does not re-advance for a repeat sender.
func TestLabel(t *testing.T) {
	l := newHistoryLabeler(map[string]string{"u_alice": "Alice"})

	if got := l.label(mmPost{UserID: "bot1"}, true); got != "Bot" {
		t.Errorf("own bot post = %q, want Bot", got)
	}
	if got := l.label(mmPost{UserID: "u_alice"}, false); got != "Alice" {
		t.Errorf("known user = %q, want Alice", got)
	}

	// Unknown senders get positional labels, stable per sender within one page.
	first := l.label(mmPost{UserID: "u_bob"}, false)
	second := l.label(mmPost{UserID: "u_bob"}, false) // same sender → same label
	other := l.label(mmPost{UserID: "u_carol"}, false) // new sender → next position

	if first != "User 1" {
		t.Errorf("first unknown = %q, want User 1", first)
	}
	if second != first {
		t.Errorf("same unknown sender must keep its label: %q != %q", second, first)
	}
	if other != "User 2" {
		t.Errorf("second distinct unknown = %q, want User 2", other)
	}
}

// TestChannelOverview verifies `chat history` reads the channel's posts,
// normalizes oldest-first, keeps only top-level posts, and tags thread heads
// with their id + reply count (derived from the fetched page) without expanding
// thread contents. The channel is resolved from the binding config, never taken
// from the caller.
func TestChannelOverview(t *testing.T) {
	q := &fakeHistoryQueries{binding: mmGroupBinding("100"), inst: mmActiveInstall()}
	api := &fakeHistoryAPI{
		// Mattermost returns newest-first in Order; posts map by id.
		posts: mmPostList{
			Order: []string{"105", "102", "101", "100"},
			Posts: map[string]mmPost{
				"100": {ID: "100", UserID: "u_carol", ChannelID: "C1", Message: "@bot take a look", CreateAt: 100},
				"101": {ID: "101", UserID: "u_bob", ChannelID: "C1", Message: "fyi unrelated", CreateAt: 101},
				"102": {ID: "102", UserID: "u_alice", ChannelID: "C1", Message: "deploy discussion", CreateAt: 102},
				// a reply in the page → counts toward thread "102".
				"105": {ID: "105", UserID: "bot1", ChannelID: "C1", RootID: "102", Message: "on it", CreateAt: 105},
			},
		},
		users: []mmUser{{ID: "u_alice", Username: "alice", Nickname: "Alice"}},
	}
	h := newMMHistory(q, api)

	page, err := h.ChannelOverview(context.Background(), uid(9), channel.HistoryOptions{})
	if err != nil {
		t.Fatalf("ChannelOverview: %v", err)
	}
	if api.postsCalls != 1 || api.threadCalls != 0 {
		t.Fatalf("expected GetPostsForChannel only, got posts=%d thread=%d", api.postsCalls, api.threadCalls)
	}
	// The channel is server-derived from the binding config ("C1"), not the
	// composite key ("C1:100").
	if api.lastChannelID != "C1" {
		t.Errorf("channel id = %q, want C1 (resolved from binding config)", api.lastChannelID)
	}
	if page.ChannelType != string(TypeMattermost) {
		t.Errorf("channel_type = %q, want mattermost", page.ChannelType)
	}
	// Overview shows top-level posts only (the reply "105" is excluded).
	if len(page.Messages) != 3 {
		t.Fatalf("expected 3 top-level msgs oldest-first, got %d: %+v", len(page.Messages), page.Messages)
	}
	if page.Messages[0].ID != "100" || page.Messages[2].ID != "102" {
		t.Fatalf("expected oldest-first [100,101,102], got %+v", page.Messages)
	}

	parent := mmFindByID(page.Messages, "102")
	if parent == nil {
		t.Fatal("thread head 102 missing from page")
	}
	if parent.ThreadID != "102" || parent.ReplyCount != 1 || parent.LatestReply != formatPostTS(105) {
		t.Errorf("thread head metadata wrong: %+v", parent)
	}
	if parent.Author != "Alice" {
		t.Errorf("thread head author = %q, want Alice (resolved display name)", parent.Author)
	}
	if parent.Role != channel.HistoryRoleUser {
		t.Errorf("thread head role = %q, want user", parent.Role)
	}
	// The bot's own reply was filtered out of the overview (it is not a root),
	// but a top-level bot post would label "Bot" + assistant role — verify via
	// a plain message carrying no thread metadata.
	if plain := mmFindByID(page.Messages, "101"); plain == nil || plain.ThreadID != "" || plain.ReplyCount != 0 {
		t.Fatalf("plain message must carry no thread metadata: %+v", plain)
	}
}

// TestChannelOverviewNoBinding: a session with no Mattermost binding surfaces
// ErrNoMattermostSession (an empty read, not a failed one).
func TestChannelOverviewNoBinding(t *testing.T) {
	q := &fakeHistoryQueries{bindingErr: pgx.ErrNoRows}
	h := newMMHistory(q, &fakeHistoryAPI{})
	_, err := h.ChannelOverview(context.Background(), uid(9), channel.HistoryOptions{})
	if !errors.Is(err, ErrNoMattermostSession) {
		t.Fatalf("err = %v, want ErrNoMattermostSession", err)
	}
}

// TestChannelOverviewLimitClamp: an over-max requested limit is clamped to
// maxHistoryLimit before the platform call.
func TestChannelOverviewLimitClamp(t *testing.T) {
	q := &fakeHistoryQueries{binding: mmGroupBinding("50"), inst: mmActiveInstall()}
	api := &fakeHistoryAPI{}
	h := newMMHistory(q, api)
	if _, err := h.ChannelOverview(context.Background(), uid(9), channel.HistoryOptions{Limit: 99999}); err != nil {
		t.Fatalf("ChannelOverview: %v", err)
	}
	if api.lastLimit != maxHistoryLimit {
		t.Errorf("limit = %d, want clamp to %d", api.lastLimit, maxHistoryLimit)
	}
}

// TestThread reads the session's own thread via GetPostThread: posts come back
// oldest-first, and posts that do not belong to the session's pinned channel
// are filtered out (the channel pin is the security boundary for a thread id).
func TestThread(t *testing.T) {
	q := &fakeHistoryQueries{binding: mmGroupBinding("50"), inst: mmActiveInstall()}
	api := &fakeHistoryAPI{
		thread: mmPostList{
			Order: []string{"99", "52", "51", "50"}, // newest-first
			Posts: map[string]mmPost{
				"50": {ID: "50", UserID: "u_alice", ChannelID: "C1", Message: "root", CreateAt: 50},
				"51": {ID: "51", UserID: "u_alice", ChannelID: "C1", Message: "second", CreateAt: 51},
				"52": {ID: "52", UserID: "u_alice", ChannelID: "C1", Message: "third", CreateAt: 52},
				// a post from ANOTHER channel must be filtered out, not leaked.
				"99": {ID: "99", UserID: "u_alice", ChannelID: "OTHER", Message: "leak", CreateAt: 60},
			},
		},
		users: []mmUser{{ID: "u_alice", Nickname: "Alice"}},
	}
	h := newMMHistory(q, api)

	page, err := h.Thread(context.Background(), uid(9), "", channel.HistoryOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if api.threadCalls != 1 || api.postsCalls != 0 {
		t.Fatalf("expected GetPostThread only, got thread=%d posts=%d", api.threadCalls, api.postsCalls)
	}
	// Empty threadID reads the session's own thread root ("50" from the binding).
	if api.lastThreadID != "50" {
		t.Errorf("thread anchored at %q, want 50 (session's own root)", api.lastThreadID)
	}
	if page.ThreadID != "50" {
		t.Errorf("page.ThreadID = %q, want 50", page.ThreadID)
	}
	if page.ChannelType != string(TypeMattermost) {
		t.Errorf("channel_type = %q, want mattermost", page.ChannelType)
	}
	// Three in-channel posts oldest-first; the other-channel post is dropped.
	if len(page.Messages) != 3 {
		t.Fatalf("expected 3 in-channel posts oldest-first (other-channel filtered), got %d: %+v",
			len(page.Messages), page.Messages)
	}
	if page.Messages[0].ID != "50" || page.Messages[1].ID != "51" || page.Messages[2].ID != "52" {
		t.Fatalf("expected oldest-first [50,51,52], got %+v", page.Messages)
	}
	if mmFindByID(page.Messages, "99") != nil {
		t.Error("a post from another channel must be filtered out of the thread")
	}
	if page.Messages[0].Author != "Alice" {
		t.Errorf("author = %q, want Alice", page.Messages[0].Author)
	}
}

// TestThreadByID reads a specific (non-current) thread within the session's
// pinned channel; the channel pin still applies.
func TestThreadByID(t *testing.T) {
	q := &fakeHistoryQueries{binding: mmGroupBinding("50"), inst: mmActiveInstall()}
	api := &fakeHistoryAPI{
		thread: mmPostList{
			Order: []string{"77"},
			Posts: map[string]mmPost{
				"77": {ID: "77", UserID: "u_alice", ChannelID: "C1", Message: "x", CreateAt: 77},
				"78": {ID: "78", UserID: "u_alice", ChannelID: "OTHER", Message: "leak", CreateAt: 78},
			},
		},
		users: []mmUser{{ID: "u_alice", Nickname: "Alice"}},
	}
	h := newMMHistory(q, api)

	page, err := h.Thread(context.Background(), uid(9), "70", channel.HistoryOptions{})
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if api.lastThreadID != "70" {
		t.Errorf("thread anchored at %q, want the passed id 70", api.lastThreadID)
	}
	if page.ThreadID != "70" {
		t.Errorf("page.ThreadID = %q, want 70", page.ThreadID)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != "77" {
		t.Fatalf("expected only the in-channel post 77, got %+v", page.Messages)
	}
}

// TestThreadDMUsesChannel: a DM has no thread root, so a thread read with no id
// falls back to the channel's linear conversation (GetPostsForChannel).
func TestThreadDMUsesChannel(t *testing.T) {
	q := &fakeHistoryQueries{binding: mmDMBinding(), inst: mmActiveInstall()}
	api := &fakeHistoryAPI{
		posts: mmPostList{
			Order: []string{"100"},
			Posts: map[string]mmPost{
				"100": {ID: "100", UserID: "u_alice", ChannelID: "D1", Message: "hi", CreateAt: 100},
			},
		},
		users: []mmUser{{ID: "u_alice", Nickname: "Alice"}},
	}
	h := newMMHistory(q, api)

	page, err := h.Thread(context.Background(), uid(9), "", channel.HistoryOptions{})
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if api.postsCalls != 1 || api.threadCalls != 0 {
		t.Fatalf("DM thread must use GetPostsForChannel, got posts=%d thread=%d", api.postsCalls, api.threadCalls)
	}
	if api.lastChannelID != "D1" {
		t.Errorf("DM channel = %q, want D1", api.lastChannelID)
	}
	if page.ThreadID != "" {
		t.Errorf("DM has no thread id, got %q", page.ThreadID)
	}
}
