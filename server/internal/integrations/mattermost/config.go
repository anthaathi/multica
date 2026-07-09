// Package mattermost is the Mattermost integration for the channel-agnostic
// engine. It uses the bring-your-own-bot (BYO) model like Slack (MUL-3666):
// the workspace admin creates a bot account in their Mattermost System Console
// and pastes the server URL + the bot's access token into Multica. Each
// channel_installation gets its OWN WebSocket connection to that server
// (mattermost_channel.go), supervised per-installation by the engine like
// Slack and Feishu.
//
// Installations are keyed and routed by config->>'app_id' =
// "<normalized server URL>#<bot user id>": bot user ids are only unique per
// Mattermost server, so the composite is what satisfies the global
// (channel_type, app_id) unique routing index. Each installation's own
// connection stamps that app_id into the raw inbound envelope, so routing is
// deterministic (no event-field parsing). The inbound translation (posted
// event -> channel.InboundMessage) lives in inbound.go; the outbound reply
// path (POST /api/v4/posts with root_id threading) lives in sender.go.
// Mattermost renders standard Markdown natively, so unlike Slack there is no
// markup conversion step.
package mattermost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// TypeMattermost is the channel discriminator for the Mattermost adapter. It
// is defined here (not in the channel core package) on purpose: registering a
// new platform must not require editing the core, so the Type value lives with
// its adapter (same as slack.TypeSlack).
const TypeMattermost channel.Type = "mattermost"

// installConfig is the JSON shape stored in channel_installation.config for a
// Mattermost installation. The cross-platform columns stay flat; everything
// Mattermost-specific lives in this opaque blob (the documented config
// boundary).
//
// app_id holds "<normalized server URL>#<bot user id>" — the per-installation
// routing key the generic GetChannelInstallationByAppID query and the
// (channel_type, app_id) unique index resolve inbound events by.
//
// team_id deliberately REPURPOSES the generic identity-reuse slot
// (FindReusableChannelUserBinding keys on ci.config->>'team_id') to hold the
// normalized server URL: a Mattermost user id is stable across every bot on
// the same server, so one account link serves all of them (the MUL-3911
// semantics Slack gets from its real team id). If Mattermost *teams* ever
// need first-class treatment, renaming inside this opaque blob is a
// config-only change.
//
// bot_token_encrypted is stored as base64-encoded secretbox ciphertext, never
// plaintext (mirroring Slack). Mattermost needs only the ONE token: it
// authenticates both the REST API and the events WebSocket.
type installConfig struct {
	AppID             string `json:"app_id"`
	ServerURL         string `json:"server_url"`
	TeamID            string `json:"team_id,omitempty"`
	BotUserID         string `json:"bot_user_id,omitempty"`
	BotUsername       string `json:"bot_username,omitempty"`
	BotTokenEncrypted string `json:"bot_token_encrypted"`
}

// credentials is the decoded, decrypted form the outbound sender runs on. The
// installation IDENTITY (workspace / agent / installer) is deliberately
// absent: it is resolved per message by the Router's InstallationResolver,
// exactly as the Slack adapter does.
type credentials struct {
	ServerURL   string
	BotUserID   string
	BotUsername string
	BotToken    string
}

// Decrypter turns stored ciphertext into plaintext. The wiring injects a
// secretbox-backed implementation; tests inject an identity decrypter (or nil,
// which treats the stored bytes as plaintext).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials parses the per-installation config blob and decrypts the
// stored token. It is the single place the Mattermost config JSON is
// interpreted.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("mattermost: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode mattermost installation config: %w", err)
	}
	if cfg.ServerURL == "" {
		return credentials{}, errors.New("mattermost: installation config missing server_url")
	}
	botToken, err := decryptToken(cfg.BotTokenEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt bot token: %w", err)
	}
	return credentials{
		ServerURL:   cfg.ServerURL,
		BotUserID:   cfg.BotUserID,
		BotUsername: cfg.BotUsername,
		BotToken:    botToken,
	}, nil
}

// PublicConfig is the non-secret subset of an installation config, safe to
// surface on the management API (the encrypted bot token is never included).
type PublicConfig struct {
	ServerURL   string
	BotUserID   string
	BotUsername string
}

// DecodePublicConfig extracts the display-safe fields from a stored config
// blob. A decode miss yields a zero-value PublicConfig rather than an error:
// the management list should still render the row's identity columns.
func DecodePublicConfig(raw json.RawMessage) PublicConfig {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	return PublicConfig{ServerURL: cfg.ServerURL, BotUserID: cfg.BotUserID, BotUsername: cfg.BotUsername}
}

// installServerURL reads the normalized server URL out of a stored
// installation config (the identity-reuse scope), or "" if undecodable.
func installServerURL(installConfigJSON json.RawMessage) string {
	var cfg installConfig
	_ = json.Unmarshal(installConfigJSON, &cfg)
	return cfg.ServerURL
}

// ErrInvalidServerURL is returned when a pasted server URL is not an absolute
// http(s) URL. The handler maps it to 400 so the dialog can show a precise
// hint.
var ErrInvalidServerURL = errors.New("mattermost: server URL must be an absolute http(s) URL")

// normalizeServerURL canonicalizes a pasted Mattermost server URL so the SAME
// server always yields the SAME routing/reuse key: scheme + host (and any
// explicit port) are lowercased, trailing slashes are stripped, and
// query/fragment/userinfo are rejected by truncation. The port is PRESERVED,
// not normalized away — Go's url.Parse does not drop default ports, so
// https://host:443 and https://host are distinct keys; that is fine because a
// given Mattermost server consistently advertises one address. The path is
// preserved because Mattermost can be served under a subpath.
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrInvalidServerURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), nil
}

// routingAppID builds the config->>'app_id' routing key. Bot user ids are only
// unique per Mattermost server, so the key embeds the server URL to satisfy
// the global (channel_type, app_id) unique index.
func routingAppID(serverURL, botUserID string) string {
	return serverURL + "#" + botUserID
}

// decryptToken base64-decodes the stored ciphertext (tolerating the MIME
// newline wrapping PostgreSQL's encode(...,'base64') emits) and runs it
// through the injected Decrypter. An empty stored value decodes to an empty
// token; a nil Decrypter treats the decoded bytes as plaintext (test
// convenience). Duplicated from the Slack adapter (package-private there),
// like the Lark adapter does.
func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// stripWhitespace removes ASCII whitespace so a MIME-wrapped base64 string
// (newlines every 64 chars) and an unwrapped one decode identically.
func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
