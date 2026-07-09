package mattermost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// cfgJSON marshals v to a json.RawMessage for config-blob tests. It is local to
// this file (cfg-prefixed) so it cannot collide with helpers owned by other
// _test.go files in the package.
func cfgJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return b
}

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{"https with subpath", "https://chat.example.com/mm", "https://chat.example.com/mm", nil},
		{"trailing slash stripped", "https://chat.example.com/", "https://chat.example.com", nil},
		{"multiple trailing slashes stripped", "https://chat.example.com/mm//", "https://chat.example.com/mm", nil},
		{"uppercase host lowercased", "https://CHAT.Example.COM/MM", "https://chat.example.com/MM", nil},
		{"subpath preserved", "https://chat.example.com/team/path", "https://chat.example.com/team/path", nil},
		{"surrounding whitespace trimmed", "  https://chat.example.com  ", "https://chat.example.com", nil},
		{"query dropped", "https://chat.example.com/mm?x=1", "https://chat.example.com/mm", nil},
		{"fragment dropped", "https://chat.example.com/mm#anchor", "https://chat.example.com/mm", nil},
		{"userinfo dropped", "https://user:pass@chat.example.com/mm", "https://chat.example.com/mm", nil},
		// NOTE: the source doc comment claims "a default port is dropped
		// implicitly by the URL parse", but Go's net/url does NOT drop default
		// ports. We assert the REAL behavior (port preserved) here.
		{"explicit default port preserved", "https://chat.example.com:443/mm", "https://chat.example.com:443/mm", nil},
		{"non-http scheme rejected", "ftp://chat.example.com", "", ErrInvalidServerURL},
		{"tcp scheme rejected", "tcp://chat.example.com:5432", "", ErrInvalidServerURL},
		{"bare host no scheme rejected", "chat.example.com", "", ErrInvalidServerURL},
		{"empty rejected", "", "", ErrInvalidServerURL},
		{"whitespace only rejected", "   ", "", ErrInvalidServerURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeServerURL(tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("normalizeServerURL(%q) err = %v, want %v", tc.in, err, tc.err)
				}
				if got != "" {
					t.Errorf("normalizeServerURL(%q) = %q, want empty on error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeServerURL(%q) unexpected err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("normalizeServerURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRoutingAppID(t *testing.T) {
	got := routingAppID("https://chat.example.com", "bot123")
	want := "https://chat.example.com#bot123"
	if got != want {
		t.Errorf("routingAppID = %q, want %q", got, want)
	}
	// With a subpath server URL the composite still embeds it verbatim.
	got = routingAppID("https://chat.example.com/mm", "bot123")
	if got != "https://chat.example.com/mm#bot123" {
		t.Errorf("routingAppID with subpath = %q", got)
	}
}

func TestDecodeCredentials(t *testing.T) {
	tokenB64 := base64.StdEncoding.EncodeToString([]byte("tok-123"))

	t.Run("identity decrypter returns expected creds", func(t *testing.T) {
		raw := cfgJSON(t, map[string]string{
			"server_url":          "https://chat.example.com",
			"bot_user_id":         "u1",
			"bot_username":        "bot",
			"bot_token_encrypted": tokenB64,
		})
		creds, err := decodeCredentials(raw, func(b []byte) ([]byte, error) { return b, nil })
		if err != nil {
			t.Fatalf("decodeCredentials: %v", err)
		}
		if creds.ServerURL != "https://chat.example.com" {
			t.Errorf("ServerURL = %q", creds.ServerURL)
		}
		if creds.BotUserID != "u1" {
			t.Errorf("BotUserID = %q", creds.BotUserID)
		}
		if creds.BotUsername != "bot" {
			t.Errorf("BotUsername = %q", creds.BotUsername)
		}
		if creds.BotToken != "tok-123" {
			t.Errorf("BotToken = %q, want tok-123", creds.BotToken)
		}
	})

	t.Run("nil decrypter treats base64 bytes as plaintext", func(t *testing.T) {
		raw := cfgJSON(t, map[string]string{
			"server_url":          "https://chat.example.com",
			"bot_token_encrypted": tokenB64,
		})
		creds, err := decodeCredentials(raw, nil)
		if err != nil {
			t.Fatalf("decodeCredentials nil decrypter: %v", err)
		}
		if creds.BotToken != "tok-123" {
			t.Errorf("BotToken = %q, want tok-123 (decoded plaintext)", creds.BotToken)
		}
	})

	t.Run("decrypter error propagates", func(t *testing.T) {
		raw := cfgJSON(t, map[string]string{
			"server_url":          "https://chat.example.com",
			"bot_token_encrypted": tokenB64,
		})
		sentinel := errors.New("kms down")
		_, err := decodeCredentials(raw, func(b []byte) ([]byte, error) { return nil, sentinel })
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want wrapping %v", err, sentinel)
		}
	})

	t.Run("empty config errors", func(t *testing.T) {
		if _, err := decodeCredentials(nil, nil); err == nil {
			t.Error("empty config should error")
		}
	})

	t.Run("missing server_url errors", func(t *testing.T) {
		raw := cfgJSON(t, map[string]string{
			"bot_token_encrypted": tokenB64,
		})
		if _, err := decodeCredentials(raw, nil); err == nil {
			t.Error("missing server_url should error")
		}
	})
}

func TestDecryptToken(t *testing.T) {
	t.Run("base64 round trip", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString([]byte("secret-token"))
		got, err := decryptToken(enc, nil)
		if err != nil {
			t.Fatalf("decryptToken: %v", err)
		}
		if got != "secret-token" {
			t.Errorf("= %q, want secret-token", got)
		}
	})

	t.Run("MIME newline-wrapped decodes the same", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString([]byte("secret-token"))
		// Wrap the base64 with newlines every 4 chars + a leading tab/space,
		// like PostgreSQL's encode(...,'base64') MIME output.
		var wrapped strings.Builder
		for i, r := range enc {
			if i > 0 && i%4 == 0 {
				wrapped.WriteRune('\n')
			}
			wrapped.WriteRune(r)
		}
		wrapped.WriteRune('\n')
		got, err := decryptToken(wrapped.String(), nil)
		if err != nil {
			t.Fatalf("decryptToken wrapped: %v", err)
		}
		if got != "secret-token" {
			t.Errorf("= %q, want secret-token (whitespace stripped)", got)
		}
	})

	t.Run("empty string yields empty token", func(t *testing.T) {
		got, err := decryptToken("", nil)
		if err != nil {
			t.Errorf("unexpected err = %v", err)
		}
		if got != "" {
			t.Errorf("= %q, want empty", got)
		}
	})
}

func TestStripWhitespace(t *testing.T) {
	if got := stripWhitespace("ab c\td\ne\r\nf"); got != "abcdef" {
		t.Errorf("stripWhitespace = %q, want abcdef", got)
	}
	if got := stripWhitespace("  a\tb  "); got != "ab" {
		t.Errorf("stripWhitespace spaces/tabs = %q, want ab", got)
	}
	if got := stripWhitespace(""); got != "" {
		t.Errorf("stripWhitespace(empty) = %q, want empty", got)
	}
	if got := stripWhitespace("nochange"); got != "nochange" {
		t.Errorf("stripWhitespace(no ws) = %q", got)
	}
}

func TestPublicConfig(t *testing.T) {
	// PublicConfig only surfaces the display-safe, non-secret fields
	// (ServerURL/BotUserID/BotUsername). The encrypted bot token and the
	// routing/reuse keys (app_id, team_id) are deliberately NOT exposed.
	t.Run("extracts display fields", func(t *testing.T) {
		raw := cfgJSON(t, map[string]string{
			"app_id":              "https://chat.example.com#u1",
			"server_url":          "https://chat.example.com",
			"team_id":             "https://chat.example.com",
			"bot_user_id":         "u1",
			"bot_username":        "bot",
			"bot_token_encrypted": "eGV5Cg==",
		})
		pc := DecodePublicConfig(raw)
		if pc.ServerURL != "https://chat.example.com" {
			t.Errorf("ServerURL = %q", pc.ServerURL)
		}
		if pc.BotUserID != "u1" {
			t.Errorf("BotUserID = %q", pc.BotUserID)
		}
		if pc.BotUsername != "bot" {
			t.Errorf("BotUsername = %q", pc.BotUsername)
		}
	})

	t.Run("malformed blob yields zero value", func(t *testing.T) {
		pc := DecodePublicConfig(json.RawMessage("{not json"))
		if pc != (PublicConfig{}) {
			t.Errorf("malformed: got %+v, want zero PublicConfig", pc)
		}
	})

	t.Run("empty blob yields zero value", func(t *testing.T) {
		pc := DecodePublicConfig(nil)
		if pc != (PublicConfig{}) {
			t.Errorf("empty: got %+v, want zero PublicConfig", pc)
		}
	})
}

func TestInstallServerURL(t *testing.T) {
	t.Run("reads server_url slot", func(t *testing.T) {
		raw := cfgJSON(t, map[string]string{
			"server_url": "https://chat.example.com",
		})
		if got := installServerURL(raw); got != "https://chat.example.com" {
			t.Errorf("= %q, want https://chat.example.com", got)
		}
	})

	t.Run("empty blob returns empty", func(t *testing.T) {
		if got := installServerURL(nil); got != "" {
			t.Errorf("= %q, want empty", got)
		}
	})

	t.Run("undecodable blob returns empty", func(t *testing.T) {
		if got := installServerURL(json.RawMessage("{{bad")); got != "" {
			t.Errorf("= %q, want empty", got)
		}
	})
}

func TestTypeMattermost(t *testing.T) {
	if TypeMattermost != channel.Type("mattermost") {
		t.Errorf("TypeMattermost = %q, want \"mattermost\"", TypeMattermost)
	}
}
