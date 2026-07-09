package mattermost

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// fakeSendAPI is the restAPI fake mattermostSender.Send is exercised against.
// It records every CreatePost call and returns either a forced error (when err
// is set) or the next configured post / an auto-incrementing id ("p1", "p2",
// …). The other restAPI methods are unused by Send and panic to surface misuse.
type fakeSendAPI struct {
	// posts, when non-empty, supplies CreatePost return values in call order;
	// otherwise an auto-incrementing id is returned per call.
	posts []mmPost
	// err, when non-nil, is returned from every CreatePost call.
	err error

	createCalls []fakeCreateCall
}

type fakeCreateCall struct {
	channelID string
	rootID    string
	message   string
}

func (f *fakeSendAPI) CreatePost(_ context.Context, channelID, rootID, message string) (mmPost, error) {
	f.createCalls = append(f.createCalls, fakeCreateCall{
		channelID: channelID,
		rootID:    rootID,
		message:   message,
	})
	if f.err != nil {
		return mmPost{}, f.err
	}
	if len(f.posts) > 0 {
		p := f.posts[0]
		f.posts = f.posts[1:]
		return p, nil
	}
	return mmPost{ID: "p" + strconv.Itoa(len(f.createCalls))}, nil
}

func (f *fakeSendAPI) GetMe(context.Context) (mmUser, error) { panic("fakeSendAPI: GetMe unused") }
func (f *fakeSendAPI) GetPostsForChannel(context.Context, string, int, string) (mmPostList, error) {
	panic("fakeSendAPI: GetPostsForChannel unused")
}
func (f *fakeSendAPI) GetPostThread(context.Context, string) (mmPostList, error) {
	panic("fakeSendAPI: GetPostThread unused")
}
func (f *fakeSendAPI) AddReaction(context.Context, string, string, string) error {
	panic("fakeSendAPI: AddReaction unused")
}
func (f *fakeSendAPI) RemoveReaction(context.Context, string, string) error {
	panic("fakeSendAPI: RemoveReaction unused")
}
func (f *fakeSendAPI) GetUsersByIDs(context.Context, []string) ([]mmUser, error) {
	panic("fakeSendAPI: GetUsersByIDs unused")
}

func TestChunkMessage(t *testing.T) {
	t.Run("under cap returns single chunk", func(t *testing.T) {
		got := chunkMessage("hello", 4000)
		if len(got) != 1 || got[0] != "hello" {
			t.Fatalf("chunkMessage = %#v, want single chunk [\"hello\"]", got)
		}
	})

	t.Run("exactly at cap returns single chunk", func(t *testing.T) {
		text := strings.Repeat("a", 4000)
		got := chunkMessage(text, 4000)
		if len(got) != 1 || got[0] != text {
			t.Fatalf("got %d chunks (first len-runes=%d), want 1 chunk equal to input",
				len(got), utf8.RuneCountInString(got[0]))
		}
	})

	t.Run("over cap splits into rune-bounded chunks", func(t *testing.T) {
		text := strings.Repeat("x", 10000) // > cap → expect 4000 + 4000 + 2000
		got := chunkMessage(text, 4000)
		if len(got) != 3 {
			t.Fatalf("got %d chunks, want 3", len(got))
		}
		var reassembled strings.Builder
		for i, c := range got {
			if rc := utf8.RuneCountInString(c); rc > 4000 {
				t.Errorf("chunk %d has %d runes, want ≤ 4000", i, rc)
			}
			reassembled.WriteString(c)
		}
		if reassembled.String() != text {
			t.Error("reassembled chunks do not equal the original input")
		}
	})

	t.Run("splits on rune boundaries not byte boundaries", func(t *testing.T) {
		// "世" is one rune but three UTF-8 bytes. Repeating it 4001 times must
		// split into a 4000-rune chunk + a 1-rune chunk — never a chunk that
		// ends in the middle of a multi-byte rune.
		ch := "世"
		text := strings.Repeat(ch, 4001)
		got := chunkMessage(text, 4000)
		if len(got) != 2 {
			t.Fatalf("got %d chunks, want 2", len(got))
		}
		if rc := utf8.RuneCountInString(got[0]); rc != 4000 {
			t.Errorf("first chunk has %d runes, want 4000", rc)
		}
		if rc := utf8.RuneCountInString(got[1]); rc != 1 {
			t.Errorf("second chunk has %d runes, want 1", rc)
		}
		for i, c := range got {
			if !utf8.ValidString(c) {
				t.Errorf("chunk %d is not valid UTF-8 — a multi-byte rune was split", i)
			}
			if strings.ContainsRune(c, '\ufffd') {
				t.Errorf("chunk %d contains the replacement rune — bad boundary", i)
			}
		}
	})

	t.Run("empty text returns single empty chunk", func(t *testing.T) {
		got := chunkMessage("", 4000)
		if len(got) != 1 || got[0] != "" {
			t.Fatalf("chunkMessage = %#v, want [\"\"]", got)
		}
	})

	t.Run("non-positive maxRunes returns single chunk", func(t *testing.T) {
		text := "some long text that would otherwise split"
		for _, max := range []int{0, -1, -100} {
			got := chunkMessage(text, max)
			if len(got) != 1 || got[0] != text {
				t.Errorf("maxRunes=%d: chunkMessage = %#v, want [%q]", max, got, text)
			}
		}
	})
}

func TestOutboundRootID(t *testing.T) {
	t.Run("ReplyTo wins over ThreadID", func(t *testing.T) {
		out := channel.OutboundMessage{ThreadID: "thread-1", ReplyTo: "reply-9"}
		if got := outboundRootID(out); got != "reply-9" {
			t.Errorf("outboundRootID = %q, want ReplyTo %q", got, "reply-9")
		}
	})

	t.Run("empty ReplyTo falls back to ThreadID", func(t *testing.T) {
		out := channel.OutboundMessage{ThreadID: "thread-1"}
		if got := outboundRootID(out); got != "thread-1" {
			t.Errorf("outboundRootID = %q, want ThreadID %q", got, "thread-1")
		}
	})

	t.Run("both empty returns empty", func(t *testing.T) {
		if got := outboundRootID(channel.OutboundMessage{}); got != "" {
			t.Errorf("outboundRootID = %q, want empty", got)
		}
	})
}

func TestSend_SingleChunkEmptyRoot(t *testing.T) {
	api := &fakeSendAPI{}
	s := newMattermostSender(api, nil)

	res, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "CH1",
		Text:   "hello",
	})
	if err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if len(api.createCalls) != 1 {
		t.Fatalf("CreatePost called %d times, want 1", len(api.createCalls))
	}
	c := api.createCalls[0]
	if c.channelID != "CH1" {
		t.Errorf("channelID = %q, want CH1", c.channelID)
	}
	if c.rootID != "" {
		t.Errorf("rootID = %q, want empty when neither ReplyTo nor ThreadID set", c.rootID)
	}
	if c.message != "hello" {
		t.Errorf("message = %q, want hello", c.message)
	}
	if res.MessageID != "p1" {
		t.Errorf("MessageID = %q, want p1 (the single posted id)", res.MessageID)
	}
}

func TestSend_SetsRootToReplyTo(t *testing.T) {
	api := &fakeSendAPI{}
	s := newMattermostSender(api, nil)

	_, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID:  "CH1",
		Text:    "reply",
		ReplyTo: "root-abc",
	})
	if err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if len(api.createCalls) != 1 || api.createCalls[0].rootID != "root-abc" {
		t.Errorf("createCalls = %#v, want one call with rootID=root-abc", api.createCalls)
	}
}

func TestSend_MultiChunkReturnsLastID(t *testing.T) {
	api := &fakeSendAPI{} // auto-increments ids p1, p2 per call
	s := newMattermostSender(api, nil)

	// 6000 runes splits into two ≤4000-rune chunks.
	text := strings.Repeat("a", 6000)
	res, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "CH1",
		Text:   text,
	})
	if err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if len(api.createCalls) != 2 {
		t.Fatalf("CreatePost called %d times, want 2", len(api.createCalls))
	}
	for i, c := range api.createCalls {
		if rc := utf8.RuneCountInString(c.message); rc > 4000 {
			t.Errorf("chunk %d has %d runes, want ≤ 4000", i, rc)
		}
		if c.channelID != "CH1" {
			t.Errorf("chunk %d channelID = %q, want CH1", i, c.channelID)
		}
	}
	// res carries the id of the LAST posted chunk.
	if res.MessageID != "p2" {
		t.Errorf("MessageID = %q, want last chunk id p2", res.MessageID)
	}
}

func TestSend_APIErrorWrapsCreatePost(t *testing.T) {
	api := &fakeSendAPI{err: errors.New("boom 503")}
	s := newMattermostSender(api, nil)

	_, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "CH1",
		Text:   "hello",
	})
	if err == nil {
		t.Fatal("Send err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "create post") {
		t.Errorf("err = %q, want it to wrap \"create post\"", err.Error())
	}
	if !strings.Contains(err.Error(), "boom 503") {
		t.Errorf("err = %q, want it to carry the wrapped cause", err.Error())
	}
}

func TestSend_NilAPI(t *testing.T) {
	s := newMattermostSender(nil, nil)

	_, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "CH1",
		Text:   "hello",
	})
	if err == nil {
		t.Fatal("Send err = nil, want error")
	}
	if !strings.Contains(err.Error(), "api client not configured") {
		t.Errorf("err = %q, want it to contain \"api client not configured\"", err.Error())
	}
}
