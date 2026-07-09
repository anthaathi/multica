package mattermost

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// typingEmoji is the reaction name used as the "processing" indicator on the
// user's post while the agent is working — the same 👀 ("seen, on it")
// convention as the Slack adapter. The WebSocket user_typing action was
// deliberately not used: it expires after a few seconds and would need a
// refresh loop, while a reaction plugs into the existing bus lifecycle.
const typingEmoji = "eyes"

// typingIndicatorMaxAge bounds how old an inbound post may be before we skip
// the reaction, so a reconnect that replays old events does not stamp
// "processing" badges onto long-finished conversations. Mirrors Slack/Feishu.
const typingIndicatorMaxAge = 2 * time.Minute

// typingState is the post the reaction was added to. Mattermost removes a
// reaction by (post id, emoji name) alone, so no channel id is needed.
type typingState struct {
	PostID string
}

// TypingIndicatorQueries is the narrow DB surface the manager needs to resolve
// an installation's bot token when clearing a reaction. *db.Queries satisfies
// it (the same two reads the outbound reply subscriber uses).
type TypingIndicatorQueries interface {
	GetChannelChatSessionBindingBySession(ctx context.Context, arg db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

// TypingIndicatorManager owns the "processing" reaction lifecycle for inbound
// Mattermost posts: it adds a 👀 reaction when a post is ingested and removes
// it when the agent's run finishes (EventChatDone) or fails (EventTaskFailed).
//
// It mirrors slack.TypingIndicatorManager: state is held in memory keyed by
// chat_session_id, the bot token is re-resolved from the DB on clear (never
// held in the map between add and clear), and every failure is logged and
// swallowed — the indicator is best-effort and must never block or fail a real
// reply.
type TypingIndicatorManager struct {
	q       TypingIndicatorQueries
	decrypt Decrypter
	log     *slog.Logger
	newAPI  func(creds credentials) restAPI

	mu     sync.RWMutex
	states map[string][]typingState // key = chat_session_id string
}

// NewTypingIndicatorManager builds a manager over the generated queries and
// the bot-token decrypter. The REST client is constructed per call from the
// installation's decrypted credentials, exactly like the outbound sender.
func NewTypingIndicatorManager(q TypingIndicatorQueries, decrypt Decrypter, logger *slog.Logger) *TypingIndicatorManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &TypingIndicatorManager{
		q:       q,
		decrypt: decrypt,
		log:     logger,
		newAPI:  func(c credentials) restAPI { return newRestClient(c.ServerURL, c.BotToken, nil) },
		states:  make(map[string][]typingState),
	}
}

// Add reacts to the just-ingested post and records the state under the chat
// session. inst is the resolved installation row whose Config blob carries the
// encrypted bot token; createAt is the post's create_at (ms since epoch, 0 =
// unknown = treat as fresh). It is synchronous — the Router calls it in a
// detached, time-bounded goroutine. Errors are logged and swallowed.
func (m *TypingIndicatorManager) Add(ctx context.Context, inst db.ChannelInstallation, sessionID pgtype.UUID, postID string, createAt int64) {
	if postID == "" {
		return
	}
	if isPostTooOld(createAt) {
		m.log.Debug("mattermost typing indicator: post too old, skipping",
			"chat_session_id", util.UUIDToString(sessionID), "post_id", postID)
		return
	}
	creds, err := decodeCredentials(inst.Config, m.decrypt)
	if err != nil {
		m.log.Warn("mattermost typing indicator: decode credentials failed",
			"chat_session_id", util.UUIDToString(sessionID), "err", err)
		return
	}
	if err := m.newAPI(creds).AddReaction(ctx, creds.BotUserID, postID, typingEmoji); err != nil {
		m.log.Warn("mattermost typing indicator: add reaction failed",
			"chat_session_id", util.UUIDToString(sessionID), "post_id", postID, "err", err)
		return
	}
	key := util.UUIDToString(sessionID)
	m.mu.Lock()
	m.states[key] = append(m.states[key], typingState{PostID: postID})
	m.mu.Unlock()
}

// Clear removes every tracked reaction for the chat session and drops the
// state. It re-resolves the installation's bot token from the binding so no
// decrypted token is held in memory between add and clear. Individual remove
// failures are logged but do not abort the loop. Best-effort throughout.
func (m *TypingIndicatorManager) Clear(ctx context.Context, sessionID pgtype.UUID) {
	key := util.UUIDToString(sessionID)
	m.mu.Lock()
	states := m.states[key]
	delete(m.states, key)
	m.mu.Unlock()
	if len(states) == 0 {
		return
	}

	binding, err := m.q.GetChannelChatSessionBindingBySession(ctx, db.GetChannelChatSessionBindingBySessionParams{
		ChatSessionID: sessionID,
		ChannelType:   string(TypeMattermost),
	})
	if err != nil {
		// A missing binding means the session is not (or no longer) a Mattermost
		// target; nothing to clear, and not worth a warning.
		if !errors.Is(err, pgx.ErrNoRows) {
			m.log.Warn("mattermost typing indicator: lookup binding for clear failed",
				"chat_session_id", key, "err", err)
		}
		return
	}
	inst, err := m.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          binding.InstallationID,
		ChannelType: string(TypeMattermost),
	})
	if err != nil {
		m.log.Warn("mattermost typing indicator: lookup installation for clear failed",
			"chat_session_id", key, "err", err)
		return
	}
	creds, err := decodeCredentials(inst.Config, m.decrypt)
	if err != nil {
		m.log.Warn("mattermost typing indicator: decode credentials for clear failed",
			"chat_session_id", key, "err", err)
		return
	}

	api := m.newAPI(creds)
	for _, s := range states {
		if err := api.RemoveReaction(ctx, s.PostID, typingEmoji); err != nil {
			m.log.Warn("mattermost typing indicator: remove reaction failed",
				"chat_session_id", key, "post_id", s.PostID, "err", err)
		}
	}
}

// Register subscribes the manager to the task-lifecycle events that end a run
// so the reaction is cleared on both success and failure. The outbound reply
// subscriber only handles EventChatDone, so this is the only path that removes
// the reaction when a run fails. Call once at boot against a fresh bus;
// register it before the outbound subscriber so the reaction clears ahead of
// the reply on EventChatDone (bus delivery is synchronous, in subscription
// order).
func (m *TypingIndicatorManager) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventChatDone, m.handleEvent)
	bus.Subscribe(protocol.EventTaskFailed, m.handleEvent)
}

func (m *TypingIndicatorManager) handleEvent(e events.Event) {
	sessionID, ok := chatSessionIDFromEvent(e)
	if !ok {
		// Issue / autopilot tasks carry no chat_session — nothing to clear.
		return
	}
	// Bus delivery is synchronous; bound the reaction calls so a stuck HTTP
	// request cannot wedge the publish call site.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.Clear(ctx, sessionID)
}

// chatSessionIDFromEvent recovers the chat session id from a task-lifecycle
// event. EventChatDone sets it on the envelope; EventTaskFailed carries it
// only in the broadcast payload map (chat tasks only), so both are checked.
func chatSessionIDFromEvent(e events.Event) (pgtype.UUID, bool) {
	if e.ChatSessionID != "" {
		if id, err := util.ParseUUID(e.ChatSessionID); err == nil && id.Valid {
			return id, true
		}
	}
	if m, ok := e.Payload.(map[string]any); ok {
		if s, _ := m["chat_session_id"].(string); s != "" {
			if id, err := util.ParseUUID(s); err == nil && id.Valid {
				return id, true
			}
		}
	}
	return pgtype.UUID{}, false
}

// isPostTooOld reports whether a post's create_at (ms since epoch) is older
// than typingIndicatorMaxAge. A zero/negative value is treated as fresh (not
// skipped) — we would rather over-react than drop a real message.
func isPostTooOld(createAt int64) bool {
	if createAt <= 0 {
		return false
	}
	return time.Since(time.UnixMilli(createAt)) > typingIndicatorMaxAge
}
