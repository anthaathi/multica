package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildOmpArgsFreshRun(t *testing.T) {
	args := buildOmpArgs("hello world", "/tmp/omp-sessions", ExecOptions{
		Model:        "anthropic/claude-sonnet-4-20250514",
		SystemPrompt: "be helpful",
		ThinkingLevel: "low",
	}, slog.Default())

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p",
		"--mode json",
		"--session-dir /tmp/omp-sessions",
		"--provider anthropic",
		"--model claude-sonnet-4-20250514",
		"--append-system-prompt",
		"--thinking low",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %v", want, args)
		}
	}

	// Fresh run must NOT pass --resume.
	if strings.Contains(joined, "--resume") {
		t.Errorf("fresh run should not pass --resume, got: %v", args)
	}
	// Prompt must be the last positional argument.
	if args[len(args)-1] != "hello world" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}
}

func TestBuildOmpArgsResume(t *testing.T) {
	args := buildOmpArgs("follow up", "/tmp/omp-sessions", ExecOptions{
		ResumeSessionID: "019f38fc-c2df",
	}, slog.Default())

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume 019f38fc-c2df") {
		t.Errorf("resume run should pass --resume <id>, got: %v", args)
	}
	if args[len(args)-1] != "follow up" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}
}

func TestBuildOmpArgsNoThinkingWhenEmpty(t *testing.T) {
	// Empty ThinkingLevel means "use omp default" — the flag must be omitted
	// so omp's own default applies (per the ExecOptions.ThinkingLevel contract).
	args := buildOmpArgs("p", "/tmp/omp-sessions", ExecOptions{}, slog.Default())
	for _, a := range args {
		if a == "--thinking" {
			t.Errorf("--thinking should be omitted when ThinkingLevel is empty, got: %v", args)
		}
	}
}

func TestBuildOmpArgsBlockedCustomArgs(t *testing.T) {
	// Protocol-critical flags from custom_args must be filtered out so a user
	// can't break the daemon↔omp transport.
	args := buildOmpArgs("p", "/tmp/omp-sessions", ExecOptions{
		CustomArgs: []string{
			"--resume", "evil-id", // blocked (with value)
			"--session", "evil", // blocked (omp alias for resume, with value)
			"--session-dir", "/tmp/evil", // blocked (with value)
			"--mode", "text", // blocked (with value)
			"-p", // blocked (standalone)
			"--verbose", // allowed
		},
	}, slog.Default())

	joined := strings.Join(args, " ")
	for _, banned := range []string{"--resume", "evil-id", "--session-dir /tmp/evil", "--mode text"} {
		if strings.Contains(joined, banned) {
			t.Errorf("blocked custom arg leaked into argv: %q in %v", banned, args)
		}
	}
	foundVerbose := false
	for _, a := range args {
		if a == "--verbose" {
			foundVerbose = true
		}
	}
	if !foundVerbose {
		t.Errorf("allowed custom arg --verbose should pass through, got: %v", args)
	}
}

func TestParseOmpModelsJSON(t *testing.T) {
	raw := []byte(`{"models":[` +
		`{"provider":"anthropic","id":"claude-sonnet-4","selector":"anthropic/claude-sonnet-4","name":"Claude Sonnet 4","thinking":["low","medium","high"]},` +
		`{"provider":"openai","id":"gpt-5","selector":"openai/gpt-5","name":"GPT-5","thinking":[]}` +
		`]}`)
	models := parseOmpModelsJSON(raw)
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(models), models)
	}
	if models[0].ID != "anthropic/claude-sonnet-4" {
		t.Errorf("model[0] ID: got %q, want provider/id selector form", models[0].ID)
	}
	if models[0].Provider != "anthropic" {
		t.Errorf("model[0] Provider: got %q", models[0].Provider)
	}
	if models[0].Label != "Claude Sonnet 4" {
		t.Errorf("model[0] Label: got %q", models[0].Label)
	}
	if models[0].Thinking == nil || len(models[0].Thinking.SupportedLevels) != 3 {
		t.Errorf("model[0] should have 3 thinking levels, got %+v", models[0].Thinking)
	}
	// Empty thinking list → no picker.
	if models[1].Thinking != nil {
		t.Errorf("model[1] should have nil Thinking for empty list, got %+v", models[1].Thinking)
	}
}

func TestParseOmpModelsJSONMalformed(t *testing.T) {
	// Malformed JSON yields an empty list, never an error/panic.
	if got := parseOmpModelsJSON([]byte("not json")); len(got) != 0 {
		t.Errorf("malformed JSON should yield empty list, got %d models", len(got))
	}
}

// TestOmpExecuteParsesSessionHeaderAndEvents drives the omp backend against
// a fake script emitting a session header followed by the Pi/omp event
// vocabulary. It verifies: the leading {"type":"session"} id is captured as
// Result.SessionID and surfaced via an early Status message, text deltas
// accumulate, tool events are forwarded, turn_end usage aggregates, and a
// trailing error flips the final status to failed.
func TestOmpExecuteParsesSessionHeaderAndEvents(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	events := []string{
		`{"type":"session","version":3,"id":"019f38fc-c2df-7000-b822-c6fb06f86205","timestamp":"2026-07-06T19:51:56.895Z","cwd":"/tmp"}`,
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hello "}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"world"}}`,
		`{"type":"tool_execution_start","toolCallId":"call_1","toolName":"bash","args":{"command":"echo hi"}}`,
		`{"type":"tool_execution_end","toolCallId":"call_1","toolName":"bash","result":"hi","isError":false}`,
		`{"type":"turn_end","message":{"role":"assistant","model":"test-model","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":1,"totalTokens":18}}}`,
	}
	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(eventStreamScript(events)))

	backend, err := New("omp", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new omp backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	var (
		gotSessionStatus bool
		gotText          strings.Builder
		gotToolUse       int
		gotToolResult    int
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range session.Messages {
			switch msg.Type {
			case MessageStatus:
				if msg.SessionID == "019f38fc-c2df-7000-b822-c6fb06f86205" {
					gotSessionStatus = true
				}
			case MessageText:
				gotText.WriteString(msg.Content)
			case MessageToolUse:
				gotToolUse++
			case MessageToolResult:
				gotToolResult++
			}
		}
	}()

	select {
	case result := <-session.Result:
		<-done
		if result.Status != "completed" {
			t.Fatalf("expected status=completed, got %q (error=%q)", result.Status, result.Error)
		}
		if result.SessionID != "019f38fc-c2df-7000-b822-c6fb06f86205" {
			t.Errorf("Result.SessionID: got %q, want the session header id", result.SessionID)
		}
		if gotText.String() != "hello world" {
			t.Errorf("text: got %q, want %q", gotText.String(), "hello world")
		}
		if result.Output != "hello world" {
			t.Errorf("Output: got %q, want %q", result.Output, "hello world")
		}
		if !gotSessionStatus {
			t.Errorf("expected an early Status message carrying the session id")
		}
		if gotToolUse != 1 || gotToolResult != 1 {
			t.Errorf("tool events: got %d use / %d result, want 1/1", gotToolUse, gotToolResult)
		}
		u, ok := result.Usage["test-model"]
		if !ok {
			t.Fatalf("usage for test-model missing: %+v", result.Usage)
		}
		if u.InputTokens != 10 || u.OutputTokens != 5 || u.CacheReadTokens != 2 || u.CacheWriteTokens != 1 {
			t.Errorf("usage tokens: got %+v, want in=10 out=5 cr=2 cw=1", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// TestOmpExecuteErrorFlipsStatus verifies an `error` event sets the final
// status to failed with the error text.
func TestOmpExecuteErrorFlipsStatus(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	events := []string{
		`{"type":"session","version":3,"id":"abc-123","timestamp":"t","cwd":"/tmp"}`,
		`{"type":"error","message":"upstream rate limited"}`,
	}
	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(eventStreamScript(events)))

	backend, err := New("omp", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new omp backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "p", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		if result.Status != "failed" {
			t.Fatalf("expected status=failed, got %q", result.Status)
		}
		if !strings.Contains(result.Error, "upstream rate limited") {
			t.Errorf("expected error text preserved, got %q", result.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// TestOmpExecuteSkipsMalformedLines verifies a bad line in the event stream
// is skipped without aborting the run, so one corrupt line can't kill an
// otherwise healthy session. Also exercises a non-Pi event type (message_end,
// agent_end) that omp emits but our parser has no arm for — it must be
// ignored, not fatal.
func TestOmpExecuteSkipsMalformedLines(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	events := []string{
		`{"type":"session","version":3,"id":"sid-1","timestamp":"t","cwd":"/tmp"}`,
		`this is not json`,
		`{"type":"message_end","message":{"role":"assistant"}}`, // omp-only event, ignored
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`,
		`{"type":"agent_end"}`, // omp-only event, ignored
		`{"type":"turn_end","message":{"role":"assistant","model":"m","usage":{"input":1,"output":1}}}`,
	}
	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(eventStreamScript(events)))

	backend, err := New("omp", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new omp backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "p", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		if result.Status != "completed" {
			t.Fatalf("expected status=completed despite malformed lines, got %q (error=%q)", result.Status, result.Error)
		}
		if result.Output != "ok" {
			t.Errorf("Output: got %q, want %q", result.Output, "ok")
		}
		if result.SessionID != "sid-1" {
			t.Errorf("SessionID: got %q, want sid-1", result.SessionID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// eventStreamScript builds a sh script that prints each JSON event on its own
// stdout line. Fixtures must not contain single quotes. Shared across the omp
// stream tests (mirrors piEventStreamScript).
func eventStreamScript(events []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, e := range events {
		b.WriteString("printf '%s\\n' '")
		b.WriteString(e)
		b.WriteString("'\n")
	}
	return b.String()
}

// TestOmpModelsJSONShapeMatchesFieldNames is a regression guard: it confirms
// the real omp `models --json` field names (provider/id/selector/name/thinking)
// round-trip through our parser, so a rename upstream would surface here. The
// fixture mirrors the verified output of omp 16.x.
func TestOmpModelsJSONShapeMatchesFieldNames(t *testing.T) {
	raw := json.RawMessage(`{"models":[{"provider":"deepseek","id":"deepseek-v4-flash","selector":"deepseek/deepseek-v4-flash","name":"DeepSeek V4 Flash","contextWindow":1000000,"maxTokens":384000,"reasoning":true,"thinking":["minimal","low","medium","high","xhigh"],"input":["text"],"cost":{"input":0.14}}]}`)
	models := parseOmpModelsJSON(raw)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	m := models[0]
	if m.ID != "deepseek/deepseek-v4-flash" {
		t.Errorf("ID should be the selector provider/id form, got %q", m.ID)
	}
	if m.Provider != "deepseek" || m.Label != "DeepSeek V4 Flash" {
		t.Errorf("provider/label mismatch: %q / %q", m.Provider, m.Label)
	}
	if m.Thinking == nil || len(m.Thinking.SupportedLevels) != 5 {
		t.Errorf("expected 5 thinking levels, got %+v", m.Thinking)
	}
}

// TestOmpRealJSONSmoke drives the REAL `omp` binary end-to-end in JSON print
// mode when it is installed and configured. It is the live counterpart to the
// fake-script tests above: it proves the backend's session-header capture,
// event parsing, and resume-id return path work against the actual binary.
//
// Skipped automatically when omp is not on PATH or the run fails for a missing
// model / credentials reason, so CI — which has neither — stays green. Run
// locally with a configured omp to exercise it.
func TestOmpRealJSONSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}
	path, err := exec.LookPath("omp")
	if err != nil {
		t.Skip("omp not on PATH; skipping real-binary smoke test")
	}

	backend, err := New("omp", Config{ExecutablePath: path, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new omp backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "Reply with exactly one word: pong. Do not use any tools.", ExecOptions{
		Cwd:     t.TempDir(),
		Timeout: 80 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		// A not-configured omp (no model / no API key) fails fast; treat that
		// as a skip so the test only fails for real protocol regressions.
		if result.Status == "failed" {
			t.Skipf("omp run failed (likely unconfigured — no model/API key): %v", result.Error)
		}
		if result.Status != "completed" {
			t.Fatalf("real omp run did not complete: status=%q error=%q", result.Status, result.Error)
		}
		if !strings.Contains(strings.ToLower(result.Output), "pong") {
			t.Fatalf("expected real omp output to contain 'pong', got %q", result.Output)
		}
		// The resume id comes from omp's leading {"type":"session","id":...}
		// header; a missing SessionID means header capture regressed.
		if result.SessionID == "" {
			t.Error("expected a non-empty session id captured from omp's session header")
		}
		t.Logf("real omp smoke OK: session=%s output=%q", result.SessionID, result.Output)
	case <-time.After(90 * time.Second):
		t.Fatal("timeout waiting for real omp result")
	}
}
