package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/slack"
	"github.com/multica-ai/multica/server/internal/integrations/mattermost"
)

type fakeChatHistoryReader struct {
	page          channel.HistoryPage
	err           error
	overviewCalls int
	threadCalls   int
	gotSession    pgtype.UUID
	gotThreadID   string
	gotOpts       channel.HistoryOptions
}

func (f *fakeChatHistoryReader) ChannelOverview(_ context.Context, sid pgtype.UUID, opts channel.HistoryOptions) (channel.HistoryPage, error) {
	f.overviewCalls++
	f.gotSession = sid
	f.gotOpts = opts
	return f.page, f.err
}

func (f *fakeChatHistoryReader) Thread(_ context.Context, sid pgtype.UUID, threadID string, opts channel.HistoryOptions) (channel.HistoryPage, error) {
	f.threadCalls++
	f.gotSession = sid
	f.gotThreadID = threadID
	f.gotOpts = opts
	return f.page, f.err
}

// newChatHistoryTask inserts a chat task bound to a fresh chat session and
// returns the task id. With chatSession=false it inserts a non-chat task.
func newChatHistoryTask(t *testing.T, chatSession bool) string {
	t.Helper()
	agentID := createHandlerTestAgent(t, "ChatHistoryAgent", []byte("[]"))
	runtimeID := handlerTestRuntimeID(t)
	var sessionArg any
	if chatSession {
		sessionArg = createHandlerTestChatSession(t, agentID)
	}
	var taskID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, chat_session_id)
		VALUES ($1, $2, 'completed', 0, $3)
		RETURNING id
	`, agentID, runtimeID, sessionArg).Scan(&taskID); err != nil {
		t.Fatalf("insert chat history task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

// taskActorReq builds a request as the Auth middleware would leave it for a mat_
// task token: the server-set X-Actor-Source=task_token + the authoritative
// X-Task-ID. target may carry query params (e.g. "?id=70.0").
func taskActorReq(target, taskID string) *http.Request {
	req := newRequest("GET", target, nil)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	return req
}

func withSlackHistory(t *testing.T, r ChatChannelHistoryReader) {
	t.Helper()
	orig := testHandler.SlackHistory
	testHandler.SlackHistory = r
	t.Cleanup(func() { testHandler.SlackHistory = orig })
}

func TestGetChatChannelHistory_Success(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	fake := &fakeChatHistoryReader{page: channel.HistoryPage{
		ChannelType: "slack",
		Messages: []channel.HistoryMessage{
			{ID: "100", Author: "Alice", Role: channel.HistoryRoleUser, Text: "deploy thread", TS: "100", ThreadID: "100", ReplyCount: 3},
			{ID: "101", Author: "Bob", Role: channel.HistoryRoleUser, Text: "fyi", TS: "101"},
		},
		NextCursor: "100",
	}}
	withSlackHistory(t, fake)

	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, taskActorReq("/api/chat/history?limit=10", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp ChatChannelHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ChannelType != "slack" || len(resp.Messages) != 2 || resp.NextCursor != "100" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Messages[0].ThreadID != "100" || resp.Messages[0].ReplyCount != 3 {
		t.Errorf("overview did not carry thread metadata: %+v", resp.Messages[0])
	}
	if fake.overviewCalls != 1 || fake.threadCalls != 0 {
		t.Errorf("expected ChannelOverview, got overview=%d thread=%d", fake.overviewCalls, fake.threadCalls)
	}
}

func TestGetChatThread_CurrentThread(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	fake := &fakeChatHistoryReader{page: channel.HistoryPage{ChannelType: "slack", ThreadID: "50.0", Messages: []channel.HistoryMessage{{ID: "50.0", TS: "50.0", Text: "root"}}}}
	withSlackHistory(t, fake)

	w := httptest.NewRecorder()
	testHandler.GetChatThread(w, taskActorReq("/api/chat/thread", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if fake.threadCalls != 1 || fake.overviewCalls != 0 {
		t.Errorf("expected Thread, got overview=%d thread=%d", fake.overviewCalls, fake.threadCalls)
	}
	if fake.gotThreadID != "" {
		t.Errorf("current-thread read should pass empty id, got %q", fake.gotThreadID)
	}
}

func TestGetChatThread_ByID(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	fake := &fakeChatHistoryReader{page: channel.HistoryPage{ChannelType: "slack", ThreadID: "70.0"}}
	withSlackHistory(t, fake)

	w := httptest.NewRecorder()
	testHandler.GetChatThread(w, taskActorReq("/api/chat/thread?id=70.0", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if fake.gotThreadID != "70.0" {
		t.Errorf("thread id passed to reader = %q, want 70.0", fake.gotThreadID)
	}
}

func TestGetChatHistory_NoBindingReturnsNote(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	withSlackHistory(t, &fakeChatHistoryReader{err: slack.ErrNoSlackSession})

	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, taskActorReq("/api/chat/history", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp ChatChannelHistoryResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Note == "" || len(resp.Messages) != 0 {
		t.Fatalf("expected empty messages + a note, got %+v", resp)
	}
}

func TestGetChatHistory_NilReaderReturnsNote(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	withSlackHistory(t, nil)

	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, taskActorReq("/api/chat/history", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp ChatChannelHistoryResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Note == "" {
		t.Fatalf("expected a note when no reader configured, got %+v", resp)
	}
}

// TestGetChatHistory_RejectsForgedTaskID: a normal request (no server-set
// X-Actor-Source) that forges X-Task-ID — what a member could do with a JWT /
// mul_ PAT, since the Auth middleware does NOT strip a client-sent X-Task-ID —
// must be rejected, never served another session's history.
func TestGetChatHistory_RejectsForgedTaskID(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	fake := &fakeChatHistoryReader{page: channel.HistoryPage{ChannelType: "slack"}}
	withSlackHistory(t, fake)

	req := newRequest("GET", "/api/chat/history", nil)
	req.Header.Set("X-Task-ID", taskID) // forged: no X-Actor-Source=task_token
	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if fake.overviewCalls != 0 || fake.threadCalls != 0 {
		t.Fatalf("reader must not be called for a forged X-Task-ID")
	}
}

func TestGetChatHistory_MissingTaskHeader(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	// Task-token actor source but no X-Task-ID: a defensive 400.
	req := newRequest("GET", "/api/chat/history", nil)
	req.Header.Set("X-Actor-Source", "task_token")
	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestGetChatHistory_NonChatTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, false) // task with no chat_session_id
	withSlackHistory(t, &fakeChatHistoryReader{})

	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, taskActorReq("/api/chat/history", taskID))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// withMattermostHistory swaps testHandler's Mattermost reader for the duration
// of the test, restoring it on cleanup. Mirrors withSlackHistory.
func withMattermostHistory(t *testing.T, r ChatChannelHistoryReader) {
	t.Helper()
	orig := testHandler.MattermostHistory
	testHandler.MattermostHistory = r
	t.Cleanup(func() { testHandler.MattermostHistory = orig })
}

// The chat-history dispatch tries Slack first, then Mattermost (see
// chatHistoryReaders + readChannelHistory): a session Slack does not own
// (slack.ErrNoSlackSession) must fall through to the Mattermost reader instead
// of surfacing the Slack miss.
func TestChatChannelHistory_MattermostFallThrough(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	mmPage := channel.HistoryPage{
		ChannelType: "mattermost",
		Messages: []channel.HistoryMessage{
			{ID: "mm-1", Author: "Carol", Role: channel.HistoryRoleUser, Text: "hello from mattermost", TS: "1"},
		},
		NextCursor: "mm-cursor",
	}
	slackReader := &fakeChatHistoryReader{err: slack.ErrNoSlackSession}
	mmReader := &fakeChatHistoryReader{page: mmPage}
	withSlackHistory(t, slackReader)
	withMattermostHistory(t, mmReader)

	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, taskActorReq("/api/chat/history", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp ChatChannelHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ChannelType != "mattermost" || len(resp.Messages) != 1 || resp.Messages[0].Text != "hello from mattermost" {
		t.Fatalf("expected the Mattermost page to win the dispatch, got %+v", resp)
	}
	if slackReader.overviewCalls != 1 {
		t.Errorf("Slack reader must be tried first: overviewCalls = %d, want 1", slackReader.overviewCalls)
	}
	if mmReader.overviewCalls != 1 {
		t.Errorf("Mattermost reader must be the fallback: overviewCalls = %d, want 1", mmReader.overviewCalls)
	}
}

// When NEITHER platform owns the session (Slack ErrNoSlackSession AND Mattermost
// ErrNoMattermostSession), the terminal not-found case maps to the friendly
// "not connected to a chat channel" note (200), not an error — see
// respondChatHistory's isNoChannelSession branch.
func TestChatChannelHistory_BothNoSessionNote(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	withSlackHistory(t, &fakeChatHistoryReader{err: slack.ErrNoSlackSession})
	withMattermostHistory(t, &fakeChatHistoryReader{err: mattermost.ErrNoMattermostSession})

	w := httptest.NewRecorder()
	testHandler.GetChatChannelHistory(w, taskActorReq("/api/chat/history", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (note), body: %s", w.Code, w.Body.String())
	}
	var resp ChatChannelHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Note == "" || len(resp.Messages) != 0 {
		t.Fatalf("expected empty messages + a note when no platform backs the session, got %+v", resp)
	}
}

// The same Slack→Mattermost dispatch backs GetChatThread: a Slack miss on the
// thread read falls through to the Mattermost thread reader.
func TestChatThread_MattermostFallThrough(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	taskID := newChatHistoryTask(t, true)
	mmThread := channel.HistoryPage{
		ChannelType: "mattermost",
		ThreadID:    "mm-root",
		Messages:    []channel.HistoryMessage{{ID: "mm-root", Author: "Dan", Role: channel.HistoryRoleUser, Text: "thread root", TS: "9"}},
	}
	slackReader := &fakeChatHistoryReader{err: slack.ErrNoSlackSession}
	mmReader := &fakeChatHistoryReader{page: mmThread}
	withSlackHistory(t, slackReader)
	withMattermostHistory(t, mmReader)

	w := httptest.NewRecorder()
	testHandler.GetChatThread(w, taskActorReq("/api/chat/thread?id=mm-root", taskID))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp ChatChannelHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ThreadID != "mm-root" || resp.ChannelType != "mattermost" {
		t.Fatalf("expected the Mattermost thread page, got %+v", resp)
	}
	if slackReader.threadCalls != 1 || mmReader.threadCalls != 1 {
		t.Errorf("expected Slack then Mattermost thread reads (1/1), got slack=%d mm=%d", slackReader.threadCalls, mmReader.threadCalls)
	}
	if mmReader.gotThreadID != "mm-root" {
		t.Errorf("thread id passed to Mattermost reader = %q, want mm-root", mmReader.gotThreadID)
	}
}
