package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrNoMattermostSession reports that the chat session has no Mattermost
// channel binding — it is a Slack, Feishu, or web-only session. Callers
// surface it as an empty (not failed) read so the unified `multica chat
// history` / `multica chat thread` commands answer gracefully.
var ErrNoMattermostSession = errors.New("mattermost: session has no mattermost channel binding")

const (
	// defaultHistoryLimit is the page size used when the caller asks for none.
	defaultHistoryLimit = 20
	// maxHistoryLimit caps a single page so a pull can't dump an unbounded
	// transcript into the agent's context.
	maxHistoryLimit = 50
)

// historyQueries is the slice of generated queries the reader needs.
type historyQueries interface {
	GetChannelChatSessionBindingBySession(ctx context.Context, arg db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

// History reads a Mattermost conversation on demand — the pull side of the
// unified `multica chat history` (channel overview) and `multica chat thread
// [id]` (one thread) commands (MUL-3871). Both are scoped to the session's OWN
// channel: the channel is resolved server-side from the binding and never
// taken from the agent, so a thread id is only a within-channel locator.
// Sessions with no Mattermost binding return ErrNoMattermostSession.
type History struct {
	q         historyQueries
	decrypt   Decrypter
	logger    *slog.Logger
	newClient func(creds credentials) restAPI
}

// NewHistory builds the reader over the generated queries and the bot-token
// decrypter (box.Open at wiring time).
func NewHistory(q historyQueries, decrypt Decrypter, logger *slog.Logger) *History {
	if logger == nil {
		logger = slog.Default()
	}
	h := &History{q: q, decrypt: decrypt, logger: logger}
	h.newClient = func(c credentials) restAPI {
		return newRestClient(c.ServerURL, c.BotToken, nil)
	}
	return h
}

// mattermostTarget is the resolved per-session read context: a bot-token
// client plus the session's pinned channel and its own thread root.
type mattermostTarget struct {
	client     restAPI
	channelID  string
	threadRoot string // the session's own thread root (empty for a DM)
	botUserID  string
}

// resolve maps a chat_session to its Mattermost channel + bot client. The
// channel is server-derived here and never accepted from the caller — that is
// the security boundary for `multica chat thread <id>` (the agent supplies
// only a within-channel thread locator).
func (h *History) resolve(ctx context.Context, chatSessionID pgtype.UUID) (mattermostTarget, error) {
	binding, err := h.q.GetChannelChatSessionBindingBySession(ctx, db.GetChannelChatSessionBindingBySessionParams{
		ChatSessionID: chatSessionID,
		ChannelType:   string(TypeMattermost),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mattermostTarget{}, ErrNoMattermostSession
		}
		return mattermostTarget{}, fmt.Errorf("lookup mattermost chat binding: %w", err)
	}
	inst, err := h.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          binding.InstallationID,
		ChannelType: string(TypeMattermost),
	})
	if err != nil {
		return mattermostTarget{}, fmt.Errorf("load mattermost installation: %w", err)
	}
	if inst.Status != "active" {
		return mattermostTarget{}, ErrNoMattermostSession // revoked install: nothing to read
	}
	creds, err := decodeCredentials(inst.Config, h.decrypt)
	if err != nil {
		return mattermostTarget{}, fmt.Errorf("decode mattermost credentials: %w", err)
	}
	channelID, threadRoot := historyTarget(binding)
	return mattermostTarget{
		client:     h.newClient(creds),
		channelID:  channelID,
		threadRoot: threadRoot,
		botUserID:  creds.BotUserID,
	}, nil
}

// ChannelOverview returns the channel's recent top-level posts (oldest-first),
// each thread tagged with its id + reply count derived from the fetched page.
// It does NOT expand thread contents — it is the table of contents the agent
// reads to find a thread, then drills into with `multica chat thread <id>`.
// Backs `multica chat history`.
func (h *History) ChannelOverview(ctx context.Context, chatSessionID pgtype.UUID, opts channel.HistoryOptions) (channel.HistoryPage, error) {
	t, err := h.resolve(ctx, chatSessionID)
	if err != nil {
		return channel.HistoryPage{}, err
	}
	limit := clampHistoryLimit(opts.Limit)
	list, err := t.client.GetPostsForChannel(ctx, t.channelID, limit, opts.Before)
	if err != nil {
		return channel.HistoryPage{}, fmt.Errorf("read mattermost channel: %w", err)
	}
	posts := orderedPosts(list)
	// Overview shows top-level posts only; replies in the page contribute the
	// per-thread reply counts (best-effort: counts reflect the fetched window,
	// which is what the agent pages through anyway).
	replyCounts := make(map[string]int)
	latestReply := make(map[string]string)
	var roots []mmPost
	for _, p := range posts {
		if p.RootID == "" {
			roots = append(roots, p)
			continue
		}
		replyCounts[p.RootID]++
		ts := formatPostTS(p.CreateAt)
		if ts > latestReply[p.RootID] {
			latestReply[p.RootID] = ts
		}
	}
	page := h.normalizePage(ctx, t, roots, true, replyCounts, latestReply)
	// Advertise a cursor only when the platform returned a full page (more may
	// exist older than the oldest post we just returned).
	if len(posts) >= limit && len(page.Messages) > 0 {
		page.NextCursor = page.Messages[0].ID
	}
	page.ChannelType = string(TypeMattermost)
	return page, nil
}

// Thread returns one thread's posts (oldest-first). threadID empty reads the
// thread the session is in (the agent's own thread); a non-empty id reads that
// specific thread — but always within the session's pinned channel (posts from
// other channels are filtered out). A DM (no thread root) reads its linear
// conversation. Backs `multica chat thread [id]`.
func (h *History) Thread(ctx context.Context, chatSessionID pgtype.UUID, threadID string, opts channel.HistoryOptions) (channel.HistoryPage, error) {
	t, err := h.resolve(ctx, chatSessionID)
	if err != nil {
		return channel.HistoryPage{}, err
	}
	limit := clampHistoryLimit(opts.Limit)
	root := threadID
	if root == "" {
		root = t.threadRoot // the session's own thread
	}

	var posts []mmPost
	if root == "" {
		// No thread to read (a DM, or a group whose root could not be recovered):
		// fall back to the channel's linear conversation.
		list, herr := t.client.GetPostsForChannel(ctx, t.channelID, limit, opts.Before)
		if herr != nil {
			return channel.HistoryPage{}, fmt.Errorf("read mattermost thread: %w", herr)
		}
		posts = orderedPosts(list)
	} else {
		list, rerr := t.client.GetPostThread(ctx, root)
		if rerr != nil {
			return channel.HistoryPage{}, fmt.Errorf("read mattermost thread: %w", rerr)
		}
		// The channel pin is the security boundary: a thread id pointing into
		// another channel reads as empty rather than leaking its contents.
		for _, p := range orderedPosts(list) {
			if p.ChannelID == t.channelID {
				posts = append(posts, p)
			}
		}
		if len(posts) > limit {
			posts = posts[len(posts)-limit:] // most recent window, still oldest-first
		}
	}
	page := h.normalizePage(ctx, t, posts, false, nil, nil)
	page.ChannelType = string(TypeMattermost)
	page.ThreadID = root
	return page, nil
}

func clampHistoryLimit(n int) int {
	if n <= 0 {
		return defaultHistoryLimit
	}
	if n > maxHistoryLimit {
		return maxHistoryLimit
	}
	return n
}

// historyTarget recovers the real channel id and the session's own thread root
// from the binding. The channel_chat_id may be a composite "channel:threadRoot"
// isolation key, so the real channel id is read from the binding config
// (mattermostBindingConfig). The thread root is the recorded reply thread
// (last_thread_id), falling back to the composite-key suffix; empty for a DM.
func historyTarget(b db.ChannelChatSessionBinding) (channelID, threadRoot string) {
	channelID = b.ChannelChatID
	if len(b.Config) > 0 {
		var cfg mattermostBindingConfig
		if err := json.Unmarshal(b.Config, &cfg); err == nil && cfg.ChannelID != "" {
			channelID = cfg.ChannelID
		}
	}
	if b.LastThreadID.Valid && b.LastThreadID.String != "" {
		threadRoot = b.LastThreadID.String
	} else if i := strings.IndexByte(b.ChannelChatID, ':'); i >= 0 {
		threadRoot = b.ChannelChatID[i+1:]
	}
	return channelID, threadRoot
}

// orderedPosts flattens a {order, posts} list into oldest-first posts. The
// order array is newest-first post ids; ids missing from the posts map are
// skipped.
func orderedPosts(list mmPostList) []mmPost {
	out := make([]mmPost, 0, len(list.Order))
	for _, id := range list.Order {
		if p, ok := list.Posts[id]; ok {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreateAt < out[j].CreateAt })
	return out
}

// normalizePage turns raw posts into a normalized, oldest-first page: it
// resolves display names in one batch, labels senders, maps roles, and — on an
// overview — tags thread heads with their id + reply count so the agent can
// drill in with `multica chat thread <id>`.
func (h *History) normalizePage(ctx context.Context, t mattermostTarget, posts []mmPost, overview bool, replyCounts map[string]int, latestReply map[string]string) channel.HistoryPage {
	names := h.resolveUserNames(ctx, t.client, posts, t.botUserID)
	labeler := newHistoryLabeler(names)

	out := make([]channel.HistoryMessage, 0, len(posts))
	for _, p := range posts {
		text := strings.TrimSpace(p.Message)
		if text == "" || p.Type != "" {
			continue // system post or empty body: no readable content
		}
		own := p.UserID != "" && p.UserID == t.botUserID
		role := channel.HistoryRoleUser
		if own {
			role = channel.HistoryRoleAssistant
		}
		hm := channel.HistoryMessage{
			ID:       p.ID,
			Author:   labeler.label(p, own),
			AuthorID: p.UserID,
			Role:     role,
			Text:     text,
			TS:       formatPostTS(p.CreateAt),
		}
		if overview {
			if n := replyCounts[p.ID]; n > 0 {
				hm.ThreadID = p.ID
				hm.ReplyCount = n
				hm.LatestReply = latestReply[p.ID]
			}
		}
		out = append(out, hm)
	}
	return channel.HistoryPage{Messages: out}
}

// formatPostTS renders a create_at (ms since epoch) as a fixed-width decimal
// string so pages sort lexicographically, matching the HistoryMessage.TS
// contract.
func formatPostTS(createAt int64) string {
	if createAt < 0 {
		createAt = 0
	}
	return fmt.Sprintf("%013d", createAt)
}

// resolveUserNames batch-resolves human senders' display names, best-effort. A
// failure yields a nil map so the labeler falls back to positional "User N"
// rather than blocking the read.
func (h *History) resolveUserNames(ctx context.Context, client restAPI, posts []mmPost, botUserID string) map[string]string {
	seen := make(map[string]bool)
	ids := make([]string, 0, len(posts))
	for _, p := range posts {
		u := p.UserID
		if u == "" || u == botUserID || seen[u] {
			continue
		}
		seen[u] = true
		ids = append(ids, u)
	}
	if len(ids) == 0 {
		return nil
	}
	users, err := client.GetUsersByIDs(ctx, ids)
	if err != nil {
		h.logger.WarnContext(ctx, "mattermost history: user name resolution failed", "ids", len(ids), "error", err)
		return nil
	}
	names := make(map[string]string, len(users))
	for _, u := range users {
		if name := displayName(u); name != "" {
			names[u.ID] = name
		}
	}
	return names
}

// historyLabeler assigns stable, human-readable labels within one page: this
// bot is "Bot"; a resolved human gets their real name; an unresolved sender
// falls back to positional "User N".
type historyLabeler struct {
	names map[string]string
	seen  map[string]string
	n     int
}

func newHistoryLabeler(names map[string]string) *historyLabeler {
	return &historyLabeler{names: names, seen: make(map[string]string)}
}

func (l *historyLabeler) label(p mmPost, own bool) string {
	if own {
		return "Bot"
	}
	key := p.UserID
	if lbl, ok := l.seen[key]; ok {
		return lbl
	}
	var lbl string
	if name := l.names[p.UserID]; name != "" {
		lbl = name
	} else {
		l.n++
		lbl = fmt.Sprintf("User %d", l.n)
	}
	l.seen[key] = lbl
	return lbl
}
