package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// testBox returns a deterministic secretbox.Box keyed by a fixed 32-byte test
// key, so install tests can assert round-trip encryption (Seal then Open)
// without touching env config.
func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// mustUUID parses a canonical UUID literal, failing the test on a malformed
// input (test data is always valid).
func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := util.ParseUUID(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// fakeInstallQueries is a no-DB installQueries. WithTx returns itself (the tx
// is a no-op token). UpsertChannelInstallation is configurable: by default it
// returns a fresh active row built from the params; if existing is set it
// returns that row (simulating an in-place UPDATE on reconnect); if
// appIDTaken is set it reports a unique-constraint violation so persistInstall
// surfaces ErrServerBotOwnedByAnotherWorkspace. The list/get/setStatus methods
// capture their params so delegation can be asserted.
type fakeInstallQueries struct {
	existing   *db.ChannelInstallation
	appIDTaken bool
	rowID      pgtype.UUID

	upsertCalled bool
	upsertParams db.UpsertChannelInstallationParams

	listCalled bool
	listParams db.ListChannelInstallationsByWorkspaceParams
	listRows   []db.ChannelInstallation

	getCalled bool
	getParams db.GetChannelInstallationInWorkspaceParams
	getRow    db.ChannelInstallation
	getErr    error

	setStatusCalled bool
	setStatusParams db.SetChannelInstallationStatusParams
}

// WithTx returns the same fake — the fake tx is a no-op token.
func (f *fakeInstallQueries) WithTx(_ pgx.Tx) installQueries { return f }

func (f *fakeInstallQueries) UpsertChannelInstallation(_ context.Context, arg db.UpsertChannelInstallationParams) (db.ChannelInstallation, error) {
	f.upsertCalled = true
	f.upsertParams = arg
	if f.appIDTaken {
		return db.ChannelInstallation{}, &pgconn.PgError{Code: pgUniqueViolation}
	}
	id := f.rowID
	if f.existing != nil {
		id = f.existing.ID // reconnect updates the agent's existing row in place
	}
	return db.ChannelInstallation{
		ID:              id,
		WorkspaceID:     arg.WorkspaceID,
		AgentID:         arg.AgentID,
		ChannelType:     arg.ChannelType,
		Config:          arg.Config,
		InstallerUserID: arg.InstallerUserID,
		Status:          "active",
	}, nil
}

func (f *fakeInstallQueries) ListChannelInstallationsByWorkspace(_ context.Context, arg db.ListChannelInstallationsByWorkspaceParams) ([]db.ChannelInstallation, error) {
	f.listCalled = true
	f.listParams = arg
	return f.listRows, nil
}

func (f *fakeInstallQueries) GetChannelInstallationInWorkspace(_ context.Context, arg db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error) {
	f.getCalled = true
	f.getParams = arg
	return f.getRow, f.getErr
}

func (f *fakeInstallQueries) SetChannelInstallationStatus(_ context.Context, arg db.SetChannelInstallationStatusParams) error {
	f.setStatusCalled = true
	f.setStatusParams = arg
	return nil
}

// fakeTx is a no-op pgx.Tx: embedding the interface satisfies it, and the
// install paths only ever call Commit / Rollback. committed records whether the
// happy path committed vs rolled back.
type fakeTx struct {
	pgx.Tx
	committed bool
}

func (t *fakeTx) Commit(context.Context) error   { t.committed = true; return nil }
func (t *fakeTx) Rollback(context.Context) error { return nil }

type fakeTxStarter struct{ tx *fakeTx }

func (f *fakeTxStarter) Begin(context.Context) (pgx.Tx, error) { return f.tx, nil }

// mmInstallService builds an InstallService over a fake tx + the deterministic
// test box, ready to drive RegisterBYO / persistInstall against fakeQueries.
func mmInstallService(t *testing.T, q installQueries) *InstallService {
	t.Helper()
	svc, err := newInstallService(q, &fakeTxStarter{tx: &fakeTx{}}, testBox(t), nil)
	if err != nil {
		t.Fatalf("newInstallService: %v", err)
	}
	return svc
}

// mmMockOptions parameterizes the install-time Mattermost API stub.
type mmMockOptions struct {
	// authOK selects /api/v4/users/me's response: true → 200 with a bot user
	// body; false → 401 with a Mattermost error body (drives ErrInvalidBotToken).
	authOK bool
	// hits, when non-nil, is incremented once per handled request, so a test
	// can assert the server was (or was not) reached.
	hits *int
}

// mmMockServer stubs GET /api/v4/users/me — the only call RegisterBYO makes
// against the Mattermost server.
func mmMockServer(t *testing.T, opts mmMockOptions) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.hits != nil {
			*opts.hits++
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v4/users/me" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found","status_code":404}`))
			return
		}
		if !opts.authOK {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Invalid or expired token","status_code":401}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"botuserid","username":"botuser","nickname":"","first_name":"","last_name":"","is_bot":true}`))
	}))
}

// mmNormalize canonicalizes a server URL exactly like the adapter, so tests
// can build the expected routing key without duplicating the rules.
func mmNormalize(t *testing.T, raw string) string {
	t.Helper()
	got, err := normalizeServerURL(raw)
	if err != nil {
		t.Fatalf("normalizeServerURL(%q): %v", raw, err)
	}
	return got
}

// mmBYOParams builds a happy-path RegisterBYOParams against serverURL.
func mmBYOParams(t *testing.T, ws, agent, serverURL string) RegisterBYOParams {
	t.Helper()
	return RegisterBYOParams{
		WorkspaceID: mustUUID(t, ws),
		AgentID:     mustUUID(t, agent),
		InitiatorID: mustUUID(t, "33333333-3333-3333-3333-333333333333"),
		ServerURL:   serverURL,
		BotToken:    "secret-bot-token",
	}
}

func TestRegisterBYO_PersistsEncryptedTokenKeyedByServerBotID(t *testing.T) {
	srv := mmMockServer(t, mmMockOptions{authOK: true})
	defer srv.Close()

	q := &fakeInstallQueries{rowID: mustUUID(t, "44444444-4444-4444-4444-444444444444")}
	svc := mmInstallService(t, q)

	row, err := svc.RegisterBYO(context.Background(), mmBYOParams(t,
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		srv.URL,
	))
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}
	if row.ID != q.rowID {
		t.Errorf("row id = %v, want %v", row.ID, q.rowID)
	}
	if !q.upsertCalled || q.upsertParams.ChannelType != string(TypeMattermost) {
		t.Fatalf("upsert not called for mattermost: %+v", q.upsertParams)
	}

	var cfg installConfig
	if err := json.Unmarshal(q.upsertParams.Config, &cfg); err != nil {
		t.Fatalf("decode upserted config: %v", err)
	}
	server := mmNormalize(t, srv.URL)
	wantAppID := server + "#botuserid"
	if cfg.AppID != wantAppID {
		t.Errorf("config app_id = %q, want %q", cfg.AppID, wantAppID)
	}
	// team_id repurposes the identity-reuse slot with the server URL.
	if cfg.TeamID != server {
		t.Errorf("config team_id = %q, want server URL %q", cfg.TeamID, server)
	}
	if cfg.ServerURL != server {
		t.Errorf("config server_url = %q, want %q", cfg.ServerURL, server)
	}
	if cfg.BotUserID != "botuserid" || cfg.BotUsername != "botuser" {
		t.Errorf("config bot = %q/%q, want botuserid/botuser", cfg.BotUserID, cfg.BotUsername)
	}
	// Token stored encrypted (never plaintext) and round-trips through the
	// test box.
	if cfg.BotTokenEncrypted == "" {
		t.Fatalf("bot token not stored: %+v", cfg)
	}
	if strings.Contains(cfg.BotTokenEncrypted, "secret-bot-token") {
		t.Error("bot token must be stored encrypted, not plaintext")
	}
	dec, err := decryptToken(cfg.BotTokenEncrypted, svc.box.Open)
	if err != nil || dec != "secret-bot-token" {
		t.Errorf("decrypted bot token = %q, %v; want secret-bot-token", dec, err)
	}
}

func TestRegisterBYO_EmptyToken_RejectedWithoutHTTP(t *testing.T) {
	var hits int
	srv := mmMockServer(t, mmMockOptions{authOK: true, hits: &hits})
	defer srv.Close()

	q := &fakeInstallQueries{}
	svc := mmInstallService(t, q)

	for _, tok := range []string{"", "   ", "\t\n"} {
		hits = 0
		p := mmBYOParams(t,
			"11111111-1111-1111-1111-111111111111",
			"22222222-2222-2222-2222-222222222222",
			srv.URL,
		)
		p.BotToken = tok
		_, err := svc.RegisterBYO(context.Background(), p)
		if !errors.Is(err, ErrInvalidBotToken) {
			t.Errorf("token %q = %v, want ErrInvalidBotToken", tok, err)
		}
		if hits != 0 {
			t.Errorf("token %q reached the server (%d hits); an empty token must be rejected pre-flight", tok, hits)
		}
	}
	if q.upsertCalled {
		t.Error("an empty token must be rejected before the upsert")
	}
}

func TestRegisterBYO_AuthRejected_ErrInvalidBotToken(t *testing.T) {
	srv := mmMockServer(t, mmMockOptions{authOK: false}) // /users/me → 401
	defer srv.Close()

	q := &fakeInstallQueries{}
	svc := mmInstallService(t, q)

	_, err := svc.RegisterBYO(context.Background(), mmBYOParams(t,
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		srv.URL,
	))
	if !errors.Is(err, ErrInvalidBotToken) {
		t.Fatalf("401 from /users/me = %v, want ErrInvalidBotToken", err)
	}
	if q.upsertCalled {
		t.Error("a rejected token must not persist an installation")
	}
}

func TestRegisterBYO_AlreadyConnected_Rejected(t *testing.T) {
	srv := mmMockServer(t, mmMockOptions{authOK: true})
	defer srv.Close()
	// The pasted bot is already connected to another agent / workspace, so the
	// (channel_type, app_id) routing index rejects the upsert (unique
	// violation). We must refuse, not steal it.
	q := &fakeInstallQueries{
		rowID:      mustUUID(t, "44444444-4444-4444-4444-444444444444"),
		appIDTaken: true,
	}
	svc := mmInstallService(t, q)

	_, err := svc.RegisterBYO(context.Background(), mmBYOParams(t,
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		srv.URL,
	))
	if !errors.Is(err, ErrServerBotOwnedByAnotherWorkspace) {
		t.Fatalf("already-connected bot = %v, want ErrServerBotOwnedByAnotherWorkspace", err)
	}
}

func TestRegisterBYO_ReconnectSameAgent_UpdatesRowInPlace(t *testing.T) {
	srv := mmMockServer(t, mmMockOptions{authOK: true})
	defer srv.Close()
	// The agent already has a Mattermost row (e.g. a previously-disconnected
	// bot). Re-connecting it — even with a NEW bot — must UPDATE that same row
	// in place (keyed by workspace+agent), not error on the (workspace, agent,
	// channel) unique. The fake returns the existing row id on the upsert.
	existingID := mustUUID(t, "55555555-5555-5555-5555-555555555555")
	q := &fakeInstallQueries{
		rowID: mustUUID(t, "44444444-4444-4444-4444-444444444444"),
		existing: &db.ChannelInstallation{
			ID:          existingID,
			WorkspaceID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
			AgentID:     mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		},
	}
	svc := mmInstallService(t, q)

	row, err := svc.RegisterBYO(context.Background(), mmBYOParams(t,
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		srv.URL,
	))
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}
	if row.ID != existingID {
		t.Errorf("reconnect should reuse the agent's existing row %v, got %v", existingID, row.ID)
	}
}

func TestPersistInstall_HappyPath_Commits(t *testing.T) {
	tx := &fakeTx{}
	q := &fakeInstallQueries{rowID: mustUUID(t, "44444444-4444-4444-4444-444444444444")}
	svc, err := newInstallService(q, &fakeTxStarter{tx: tx}, testBox(t), nil)
	if err != nil {
		t.Fatalf("newInstallService: %v", err)
	}
	row, err := svc.persistInstall(context.Background(), installPersist{
		wsID:        mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		agentID:     mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		installerID: mustUUID(t, "33333333-3333-3333-3333-333333333333"),
		configJSON:  []byte(`{"app_id":"https://mm.example#bot1"}`),
	})
	if err != nil {
		t.Fatalf("persistInstall: %v", err)
	}
	if !q.upsertCalled {
		t.Fatal("UpsertChannelInstallation must be called")
	}
	if q.upsertParams.ChannelType != string(TypeMattermost) {
		t.Errorf("channel_type = %q, want %q", q.upsertParams.ChannelType, TypeMattermost)
	}
	if !tx.committed {
		t.Error("happy path must commit the tx")
	}
	if row.ID != q.rowID {
		t.Errorf("row id = %v, want %v", row.ID, q.rowID)
	}
}

func TestPersistInstall_AlreadyConnected_RollsBack(t *testing.T) {
	tx := &fakeTx{}
	q := &fakeInstallQueries{appIDTaken: true}
	svc, err := newInstallService(q, &fakeTxStarter{tx: tx}, testBox(t), nil)
	if err != nil {
		t.Fatalf("newInstallService: %v", err)
	}
	_, err = svc.persistInstall(context.Background(), installPersist{
		wsID:    mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		agentID: mustUUID(t, "22222222-2222-2222-2222-222222222222"),
	})
	if !errors.Is(err, ErrServerBotOwnedByAnotherWorkspace) {
		t.Fatalf("persistInstall = %v, want ErrServerBotOwnedByAnotherWorkspace", err)
	}
	if tx.committed {
		t.Error("a rejected upsert must not commit")
	}
}

func TestPersistInstall_ReconnectSameAgent_UpdatesInPlace(t *testing.T) {
	// When the upsert is an UPDATE-in-place it must not error — the conflict
	// target is NOT the row key, so a reconnect reuses the agent's row.
	existingID := mustUUID(t, "55555555-5555-5555-5555-555555555555")
	q := &fakeInstallQueries{
		rowID: mustUUID(t, "44444444-4444-4444-4444-444444444444"),
		existing: &db.ChannelInstallation{
			ID:          existingID,
			WorkspaceID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
			AgentID:     mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		},
	}
	svc := mmInstallService(t, q)
	row, err := svc.persistInstall(context.Background(), installPersist{
		wsID:    mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		agentID: mustUUID(t, "22222222-2222-2222-2222-222222222222"),
	})
	if err != nil {
		t.Fatalf("persistInstall: %v", err)
	}
	if row.ID != existingID {
		t.Errorf("reconnect should reuse existing row %v, got %v", existingID, row.ID)
	}
}

func TestListByWorkspace_DelegatesWithMattermostType(t *testing.T) {
	wantRow := db.ChannelInstallation{ID: mustUUID(t, "44444444-4444-4444-4444-444444444444")}
	q := &fakeInstallQueries{listRows: []db.ChannelInstallation{wantRow}}
	svc := mmInstallService(t, q)
	ws := mustUUID(t, "11111111-1111-1111-1111-111111111111")

	rows, err := svc.ListByWorkspace(context.Background(), ws)
	if err != nil {
		t.Fatalf("ListByWorkspace: %v", err)
	}
	if !q.listCalled {
		t.Fatal("ListChannelInstallationsByWorkspace must be called")
	}
	if q.listParams.WorkspaceID != ws {
		t.Errorf("workspace = %v, want %v", q.listParams.WorkspaceID, ws)
	}
	if q.listParams.ChannelType != string(TypeMattermost) {
		t.Errorf("channel_type = %q, want %q", q.listParams.ChannelType, TypeMattermost)
	}
	if len(rows) != 1 || rows[0].ID != wantRow.ID {
		t.Errorf("rows = %+v, want [%v]", rows, wantRow.ID)
	}
}

func TestGetInWorkspace_DelegatesAndMapsNotFound(t *testing.T) {
	ws := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	id := mustUUID(t, "44444444-4444-4444-4444-444444444444")

	t.Run("found", func(t *testing.T) {
		row := db.ChannelInstallation{ID: id, WorkspaceID: ws, ChannelType: string(TypeMattermost)}
		q := &fakeInstallQueries{getRow: row}
		svc := mmInstallService(t, q)
		got, err := svc.GetInWorkspace(context.Background(), id, ws)
		if err != nil {
			t.Fatalf("GetInWorkspace: %v", err)
		}
		if !q.getCalled {
			t.Fatal("GetChannelInstallationInWorkspace must be called")
		}
		if q.getParams.ID != id || q.getParams.WorkspaceID != ws {
			t.Errorf("get params = id %v ws %v, want %v / %v", q.getParams.ID, q.getParams.WorkspaceID, id, ws)
		}
		if q.getParams.ChannelType != string(TypeMattermost) {
			t.Errorf("channel_type = %q, want %q", q.getParams.ChannelType, TypeMattermost)
		}
		if got.ID != id {
			t.Errorf("got id %v, want %v", got.ID, id)
		}
	})

	// No row in this workspace → ErrInstallationNotFound (never leak pgx's
	// sentinel up to the handler).
	t.Run("not found", func(t *testing.T) {
		q := &fakeInstallQueries{getErr: pgx.ErrNoRows}
		svc := mmInstallService(t, q)
		_, err := svc.GetInWorkspace(context.Background(), id, ws)
		if !errors.Is(err, ErrInstallationNotFound) {
			t.Fatalf("GetInWorkspace not-found = %v, want ErrInstallationNotFound", err)
		}
	})
}

func TestRevoke_DelegatesWithRevokedStatus(t *testing.T) {
	q := &fakeInstallQueries{}
	svc := mmInstallService(t, q)
	id := mustUUID(t, "44444444-4444-4444-4444-444444444444")

	if err := svc.Revoke(context.Background(), id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !q.setStatusCalled {
		t.Fatal("SetChannelInstallationStatus must be called")
	}
	if q.setStatusParams.ID != id {
		t.Errorf("id = %v, want %v", q.setStatusParams.ID, id)
	}
	if q.setStatusParams.Status != "revoked" {
		t.Errorf("status = %q, want revoked", q.setStatusParams.Status)
	}
}
