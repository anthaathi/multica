package issuesync

import (
	"encoding/json"
	"testing"
	"time"
)

func TestContentHashDeterministic(t *testing.T) {
	h1 := ContentHash("title", "desc", "open", []string{"bug", "ui"}, "123")
	h2 := ContentHash("title", "desc", "open", []string{"ui", "bug"}, "123") // label order-insensitive
	if h1 != h2 {
		t.Fatalf("label order changed the hash: %s != %s", h1, h2)
	}
	// Differ on any field → different hash.
	if ContentHash("title", "desc", "open", []string{"bug"}, "123") == h1 {
		t.Fatal("expected hash to change when labels differ")
	}
	if ContentHash("title", "desc", "closed", []string{"bug", "ui"}, "123") == h1 {
		t.Fatal("expected hash to change when state differs")
	}
	if ContentHash("title", "desc", "open", []string{"bug", "ui"}, "456") == h1 {
		t.Fatal("expected hash to change when assignee differs")
	}
}

// TestEchoSuppressionByHash documents the contract the engine relies on: when
// an inbound event's content hashes to the link's last_pushed_hash, the engine
// treats it as its own write echoing back and skips the DB write. This test
// verifies the hash equality check directly (the engine branch is covered by
// the integration scenario).
func TestEchoSuppressionByHash(t *testing.T) {
	title, desc, state := "Fix bug", "details", "open"
	labels := []string{"bug"}
	assignee := "42"
	pushed := ContentHash(title, desc, state, labels, assignee)

	// Re-hash the same content the outbox would have stored → equal.
	echo := ContentHash(title, desc, state, labels, assignee)
	if pushed != echo {
		t.Fatalf("re-hash of identical content must equal the pushed hash")
	}
}

func TestGitHubIssueToExternal(t *testing.T) {
	t.Run("real_issue", func(t *testing.T) {
		raw := json.RawMessage(`{
			"id": 12345,
			"number": 42,
			"title": "Crash on start",
			"body": "steps to repro",
			"state": "open",
			"labels": [{"name":"bug"},{"name":"ui"}],
			"assignee": {"id": 99, "login": "jane", "avatar_url": "https://x/a.png"},
			"user": {"id": 7, "login": "reporter"},
			"html_url": "https://github.com/acme/widget/issues/42",
			"updated_at": "2026-01-02T03:04:05Z"
		}`)
		ext, ok := GitHubIssueToExternal(raw)
		if !ok {
			t.Fatal("expected ok for a valid issue")
		}
		if ext.ID != "12345" || ext.Key != "#42" || ext.Title != "Crash on start" {
			t.Fatalf("unexpected external issue: %+v", ext)
		}
		if ext.State != "open" || len(ext.Labels) != 2 {
			t.Fatalf("unexpected state/labels: %s %v", ext.State, ext.Labels)
		}
		if ext.Assignee == nil || ext.Assignee.AccountID != "99" {
			t.Fatalf("unexpected assignee: %+v", ext.Assignee)
		}
		if ext.WebURL != "https://github.com/acme/widget/issues/42" {
			t.Fatalf("unexpected web url: %s", ext.WebURL)
		}
		want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		if !ext.UpdatedAt.Equal(want) {
			t.Fatalf("updated_at mismatch: %v != %v", ext.UpdatedAt, want)
		}
	})

	t.Run("pull_request_rejected", func(t *testing.T) {
		raw := json.RawMessage(`{"id": 1, "number": 1, "pull_request": {"url": "x"}}`)
		if _, ok := GitHubIssueToExternal(raw); ok {
			t.Fatal("PRs must be rejected — they belong to the PR-mirror integration")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, raw := range []json.RawMessage{
			json.RawMessage(`{}`),
			json.RawMessage(`not json`),
			json.RawMessage(`{"number": 1}`),
		} {
			if _, ok := GitHubIssueToExternal(raw); ok {
				t.Fatalf("expected rejection for %s", string(raw))
			}
		}
	})
}

func TestGitHubCommentToExternal(t *testing.T) {
	raw := json.RawMessage(`{
		"id": 777,
		"body": "looks good",
		"user": {"id": 5, "login": "alice"},
		"html_url": "https://github.com/acme/widget/issues/42#issuecomment-777",
		"updated_at": "2026-03-04T05:06:07Z"
	}`)
	c, ok := GitHubCommentToExternal(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if c.ID != "777" || c.Body != "looks good" {
		t.Fatalf("unexpected: %+v", c)
	}
	if c.Author == nil || c.Author.Login != "alice" {
		t.Fatalf("unexpected author: %+v", c.Author)
	}
}

func TestGitHubContainerKey(t *testing.T) {
	if k := GitHubContainerKey("Acme", "Widget"); k != "acme/widget" {
		t.Fatalf("expected lowercased owner/name, got %s", k)
	}
}
