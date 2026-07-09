package mattermost

import (
	"encoding/json"
	"errors"
	"testing"
)

// wsWrapString marshals v to JSON, then wraps that JSON document AS A STRING —
// mimicking Mattermost's double-encoding where data.post and data.mentions are
// JSON values carried inside a JSON string inside the envelope.
func wsWrapString(t *testing.T, v any) json.RawMessage {
	t.Helper()
	inner, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	wrapped, err := json.Marshal(string(inner))
	if err != nil {
		t.Fatalf("wrap as string: %v", err)
	}
	return wrapped
}

// wsPostedEvent builds a "posted" wsEvent with the post (and optional mentions)
// double-encoded exactly as the server emits them. A nil mentions slice omits
// the field entirely (the "no mentions" path).
func wsPostedEvent(t *testing.T, post mmPost, channelType string, mentions []string) wsEvent {
	t.Helper()
	data := map[string]json.RawMessage{
		"post": wsWrapString(t, post),
	}
	if channelType != "" {
		ct, _ := json.Marshal(channelType)
		data["channel_type"] = ct
	}
	if mentions != nil {
		data["mentions"] = wsWrapString(t, mentions)
	}
	return wsEvent{Event: "posted", Data: data}
}

func TestDecodePostedEvent(t *testing.T) {
	post := mmPost{
		ID:        "p1",
		CreateAt:  1700000000123,
		UserID:    "u1",
		ChannelID: "c1",
		RootID:    "r1",
		Message:   "hello @bot",
	}

	t.Run("full post and mentions", func(t *testing.T) {
		ev := wsPostedEvent(t, post, "D", []string{"u1", "ubot"})
		got, err := decodePostedEvent(ev)
		if err != nil {
			t.Fatalf("decodePostedEvent: %v", err)
		}
		if got.Post.ID != "p1" {
			t.Errorf("Post.ID = %q", got.Post.ID)
		}
		if got.Post.UserID != "u1" {
			t.Errorf("Post.UserID = %q", got.Post.UserID)
		}
		if got.Post.ChannelID != "c1" {
			t.Errorf("Post.ChannelID = %q", got.Post.ChannelID)
		}
		if got.Post.Message != "hello @bot" {
			t.Errorf("Post.Message = %q", got.Post.Message)
		}
		if got.Post.RootID != "r1" {
			t.Errorf("Post.RootID = %q, want r1", got.Post.RootID)
		}
		if got.Post.CreateAt != 1700000000123 {
			t.Errorf("Post.CreateAt = %d, want 1700000000123", got.Post.CreateAt)
		}
		if got.ChannelType != "D" {
			t.Errorf("ChannelType = %q, want D", got.ChannelType)
		}
		if len(got.Mentions) != 2 || got.Mentions[0] != "u1" || got.Mentions[1] != "ubot" {
			t.Errorf("Mentions = %+v, want [u1 ubot]", got.Mentions)
		}
	})

	t.Run("no mentions field is nil-ok", func(t *testing.T) {
		ev := wsPostedEvent(t, post, "D", nil)
		got, err := decodePostedEvent(ev)
		if err != nil {
			t.Fatalf("decodePostedEvent: %v", err)
		}
		if got.Mentions != nil {
			t.Errorf("Mentions = %+v, want nil when field absent", got.Mentions)
		}
		if got.Post.ID != "p1" {
			t.Errorf("Post.ID = %q (post must still decode)", got.Post.ID)
		}
	})

	t.Run("empty mentions string is nil-ok", func(t *testing.T) {
		// data.mentions present but the inner string is "" (empty array case).
		data := map[string]json.RawMessage{
			"post":     wsWrapString(t, post),
			"mentions": json.RawMessage(`""`),
		}
		got, err := decodePostedEvent(wsEvent{Event: "posted", Data: data})
		if err != nil {
			t.Fatalf("decodePostedEvent: %v", err)
		}
		if got.Mentions != nil {
			t.Errorf("Mentions = %+v, want nil for empty inner string", got.Mentions)
		}
	})

	t.Run("post string not valid JSON errors", func(t *testing.T) {
		// data.post is a valid JSON string, but its content is not a valid
		// JSON document → the second unmarshal fails.
		inner, _ := json.Marshal("{broken") // a valid JSON string "{broken"
		data := map[string]json.RawMessage{"post": inner}
		_, err := decodePostedEvent(wsEvent{Event: "posted", Data: data})
		if err == nil {
			t.Error("expected error when post inner string is invalid JSON")
		}
	})

	t.Run("post raw value not valid JSON errors", func(t *testing.T) {
		// data.post value itself is not valid JSON → the first unmarshal fails.
		data := map[string]json.RawMessage{"post": json.RawMessage("not-json-at-all")}
		_, err := decodePostedEvent(wsEvent{Event: "posted", Data: data})
		if err == nil {
			t.Error("expected error when data.post is not valid JSON")
		}
	})

	t.Run("missing post field errors", func(t *testing.T) {
		_, err := decodePostedEvent(wsEvent{Event: "posted", Data: map[string]json.RawMessage{}})
		if err == nil {
			t.Error("expected error when post field is absent")
		}
	})
}

func TestWebsocketURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{"https", "https://host", "wss://host/api/v4/websocket", nil},
		{"http with port", "http://host:8065", "ws://host:8065/api/v4/websocket", nil},
		{"https subpath preserved", "https://host/mm", "wss://host/mm/api/v4/websocket", nil},
		{"http subpath preserved", "http://host:8065/sub", "ws://host:8065/sub/api/v4/websocket", nil},
		{"https trailing slash", "https://host/", "wss://host/api/v4/websocket", nil},
		{"non-http scheme", "ftp://host", "", ErrInvalidServerURL},
		{"no scheme", "host", "", ErrInvalidServerURL},
		{"empty", "", "", ErrInvalidServerURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := websocketURL(tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("websocketURL(%q) err = %v, want %v", tc.in, err, tc.err)
				}
				if got != "" {
					t.Errorf("websocketURL(%q) = %q, want empty on error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("websocketURL(%q) unexpected err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("websocketURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWsAuthChallenge(t *testing.T) {
	c := wsAuthChallenge{
		Seq:    1,
		Action: "authentication_challenge",
		Data:   map[string]string{"token": "tkn-123"},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Fields serialize in struct-declaration order (seq, action, data); the
	// single-key data map serializes deterministically.
	want := `{"seq":1,"action":"authentication_challenge","data":{"token":"tkn-123"}}`
	if string(b) != want {
		t.Errorf("wsAuthChallenge JSON = %s, want %s", b, want)
	}

	// Round-trip: the frame decodes back to the same challenge.
	var back wsAuthChallenge
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Seq != 1 || back.Action != "authentication_challenge" || back.Data["token"] != "tkn-123" {
		t.Errorf("round-trip = %+v", back)
	}
}
