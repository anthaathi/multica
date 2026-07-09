package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// This file is the minimal hand-rolled Mattermost events WebSocket client
// (/api/v4/websocket). The protocol is plain JSON: the client dials, sends an
// authentication_challenge frame carrying the bot token, and then receives
// {event, data, broadcast, seq} envelopes. There is no ACK/redelivery — a
// message posted while the socket is down is lost (same property as the
// Feishu connector); the dedup layer protects against replays after
// reconnect, not against gaps.

const (
	// wsWriteTimeout bounds a single control/auth frame write.
	wsWriteTimeout = 10 * time.Second
	// wsPingInterval is how often the client pings so a dead TCP path is
	// detected instead of blocking the read loop forever.
	wsPingInterval = 30 * time.Second
	// wsReadTimeout is the read deadline refreshed by any inbound traffic
	// (frames or pongs). It must exceed wsPingInterval.
	wsReadTimeout = 90 * time.Second
)

// wsEvent is one inbound event envelope. Data values are heterogeneous
// (strings, JSON-encoded strings, numbers), so they stay raw until the event
// type is known.
type wsEvent struct {
	Event     string                     `json:"event"`
	Data      map[string]json.RawMessage `json:"data"`
	Broadcast json.RawMessage            `json:"broadcast"`
	Seq       int64                      `json:"seq"`
}

// postedEventData is the decoded payload of a "posted" event. Post and
// Mentions arrive as JSON-encoded STRINGS inside the envelope (Mattermost
// double-encodes them), so decoding is a two-step unmarshal — see
// decodePostedEvent.
type postedEventData struct {
	Post        mmPost
	ChannelType string   // "D" direct, "G" group DM, "O" public, "P" private
	Mentions    []string // user ids the server's mention parser matched
	SenderName  string   // "@username" of the poster (display convenience)
}

// decodePostedEvent unpacks a "posted" envelope. The post (and the optional
// mentions array) are JSON documents encoded AS STRINGS in the event data —
// the double-unmarshal here is protocol, not defensiveness.
func decodePostedEvent(e wsEvent) (postedEventData, error) {
	var out postedEventData
	rawPost, ok := e.Data["post"]
	if !ok {
		return out, errors.New("mattermost: posted event has no post field")
	}
	var postJSON string
	if err := json.Unmarshal(rawPost, &postJSON); err != nil {
		return out, fmt.Errorf("mattermost: posted event post is not a string: %w", err)
	}
	if err := json.Unmarshal([]byte(postJSON), &out.Post); err != nil {
		return out, fmt.Errorf("mattermost: decode posted event post: %w", err)
	}
	if raw, ok := e.Data["channel_type"]; ok {
		_ = json.Unmarshal(raw, &out.ChannelType)
	}
	if raw, ok := e.Data["mentions"]; ok {
		var mentionsJSON string
		if json.Unmarshal(raw, &mentionsJSON) == nil && mentionsJSON != "" {
			_ = json.Unmarshal([]byte(mentionsJSON), &out.Mentions)
		}
	}
	if raw, ok := e.Data["sender_name"]; ok {
		_ = json.Unmarshal(raw, &out.SenderName)
	}
	return out, nil
}

// wsAuthChallenge is the first frame the client sends: it authenticates the
// connection with the bot token (Mattermost also accepts the token as an
// Authorization header at dial time; the challenge frame is the documented
// client behavior and works across proxy setups that strip headers).
type wsAuthChallenge struct {
	Seq    int64             `json:"seq"`
	Action string            `json:"action"`
	Data   map[string]string `json:"data"`
}

// websocketURL derives the WebSocket endpoint from the normalized server URL
// (http -> ws scheme swap, path-preserving for subpath deployments).
func websocketURL(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", ErrInvalidServerURL
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", ErrInvalidServerURL
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v4/websocket"
	return u.String(), nil
}

// wsConn is one live, authenticated events connection.
type wsConn struct {
	conn *websocket.Conn
}

// dialWS dials and authenticates the events WebSocket. The Authorization
// header AND the challenge frame both carry the token — belt and braces, and
// it means the "hello" event arrives already authenticated either way.
func dialWS(ctx context.Context, serverURL, token string) (*wsConn, error) {
	endpoint, err := websocketURL(serverURL)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("mattermost: dial websocket: %w", err)
	}
	c := &wsConn{conn: conn}
	if err := c.writeJSON(wsAuthChallenge{
		Seq:    1,
		Action: "authentication_challenge",
		Data:   map[string]string{"token": token},
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mattermost: websocket auth: %w", err)
	}
	// Any inbound traffic (event frames, pongs) refreshes the read deadline.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})
	return c, nil
}

func (c *wsConn) writeJSON(v any) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.conn.WriteJSON(v)
}

func (c *wsConn) close() { _ = c.conn.Close() }

// run pumps the connection until ctx is cancelled or the link drops: a ping
// goroutine keeps the path alive while the read loop decodes each envelope and
// hands EVENT frames (frames with a non-empty event field; auth/status replies
// have none) to onEvent. A non-nil onEvent error tears the connection down and
// propagates — the supervisor treats it as "this attempt failed" and
// reconnects with backoff.
func (c *wsConn) run(ctx context.Context, onEvent func(ctx context.Context, e wsEvent) error) error {
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
				if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()
	// Unblock the read loop when ctx ends: gorilla reads have no ctx, so the
	// close is what interrupts ReadMessage.
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stop()
	defer func() { _ = c.conn.Close(); <-pingDone }()

	for {
		if err := c.conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
			return fmt.Errorf("mattermost: set read deadline: %w", err)
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mattermost: websocket read: %w", err)
		}
		var e wsEvent
		if err := json.Unmarshal(data, &e); err != nil {
			// A malformed frame is logged upstream via the handler error path only
			// when it matters; an undecodable envelope is skipped, not fatal.
			continue
		}
		if e.Event == "" {
			continue // status/auth reply frame, not an event
		}
		if err := onEvent(ctx, e); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
