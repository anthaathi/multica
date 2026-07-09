package mattermost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrInvalidBotToken is returned by RegisterBYO when the pasted token is
// rejected by the Mattermost server (401 from /api/v4/users/me). The handler
// maps it to 400 so the dialog can show a precise hint instead of a generic
// failure.
var ErrInvalidBotToken = errors.New("mattermost: the bot token was rejected by the server")

// RegisterBYOParams are the inputs for a bring-your-own-bot install: the agent
// this bot represents, who is installing, and the server URL + token the user
// pasted from their own Mattermost bot account.
type RegisterBYOParams struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InitiatorID pgtype.UUID
	ServerURL   string // https://mattermost.example.com — the self-hosted server
	BotToken    string // the bot account's access token (REST + events WebSocket)
}

// RegisterBYO installs a user-supplied ("bring your own") Mattermost bot for
// an agent. The user creates a bot account in their Mattermost System Console
// (Integrations → Bot Accounts) and pastes the server URL + the bot's access
// token. There is NO OAuth exchange: we validate the token live via
// GET /api/v4/users/me (which also yields the bot's user id + username),
// encrypt the token at rest, and persist the installation.
//
// The stored config carries app_id = "<normalized server URL>#<bot user id>"
// for inbound routing and team_id = the server URL for cross-bot identity
// reuse (see installConfig). persistInstall keys the row by (workspace, agent)
// and refuses the pair if that bot is already connected to another
// agent/workspace. The dedicated events WebSocket that consumes the stored
// token lives in mattermost_channel.go; this method only persists the
// installation.
func (s *InstallService) RegisterBYO(ctx context.Context, p RegisterBYOParams) (db.ChannelInstallation, error) {
	serverURL, err := normalizeServerURL(p.ServerURL)
	if err != nil {
		return db.ChannelInstallation{}, err
	}
	botToken := strings.TrimSpace(p.BotToken)
	if botToken == "" {
		return db.ChannelInstallation{}, ErrInvalidBotToken
	}

	// Validate the token live and learn the bot's identity. /users/me
	// authenticates with the token and returns the bot's OWN user id (the
	// inbound loop guard + reaction identity) and username (the @-mention
	// fallback the inbound translation strips).
	me, err := newRestClient(serverURL, botToken, s.httpClient).GetMe(ctx)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
			return db.ChannelInstallation{}, ErrInvalidBotToken
		}
		return db.ChannelInstallation{}, fmt.Errorf("mattermost users/me: %w", err)
	}
	if me.ID == "" {
		return db.ChannelInstallation{}, errors.New("mattermost users/me: response missing user id")
	}

	sealed, err := s.box.Seal([]byte(botToken))
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encrypt mattermost bot token: %w", err)
	}
	cfgJSON, err := json.Marshal(installConfig{
		AppID:     routingAppID(serverURL, me.ID),
		ServerURL: serverURL,
		// team_id repurposes the generic identity-reuse slot with the server URL
		// — one account link per server, not per bot (see installConfig).
		TeamID:            serverURL,
		BotUserID:         me.ID,
		BotUsername:       me.Username,
		BotTokenEncrypted: base64.StdEncoding.EncodeToString(sealed),
	})
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode mattermost installation config: %w", err)
	}

	// Persist one bot per agent (the row is keyed by workspace + agent). The
	// stored config carries the routing app_id; persistInstall refuses the pair
	// if that bot is already connected to another agent/workspace.
	return s.persistInstall(ctx, installPersist{
		wsID:        p.WorkspaceID,
		agentID:     p.AgentID,
		installerID: p.InitiatorID,
		configJSON:  cfgJSON,
	})
}
