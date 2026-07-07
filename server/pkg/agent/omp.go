package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ompBackend implements Backend by spawning the omp (Oh My Pi) CLI in
// non-interactive JSON mode (`omp -p --mode json --session-dir <dir>`) and
// parsing its event stream on stdout.
//
// omp (https://omp.sh/, github.com/can1357/oh-my-pi) is a fork of Mario
// Zechner's Pi coding agent. It keeps Pi's JSON event protocol, so the event
// vocabulary and field shapes are identical to pi.go's (the fork reuses Pi's
// AgentSession event model). The structural differences from Pi are:
//
//   - omp's `--session` is NOT Pi's `--session <file-path>`. In omp it is an
//     alias for `--resume <session-id-prefix>`; the create-empty-file-and-pass-
//     -the-path dance the Pi backend uses would be interpreted as a resume
//     prefix and break. Session identity is an opaque id emitted on stdout.
//   - In `--mode json`, the FIRST stdout line is a session header:
//     `{"type":"session","version":3,"id":"<session-id>","timestamp":...,
//     "cwd":...}`. That `id` is the resume token (passed back as
//     ResumeSessionID on the next turn). Pi emits no such header, so the
//     Pi parser would silently skip it; omp captures it (see the "session"
//     switch arm) and emits an early MessageStatus carrying the SessionID so
//     the daemon can pin the resume pointer before the run finishes.
//   - omp supports a native `--thinking <level>` flag, so the backend honours
//     ExecOptions.ThinkingLevel (the Pi backend cannot).
//
// omp also emits events Pi does not (message_start, message_end, agent_end);
// those have no matching switch arm and are ignored, which is the intended
// fall-through. The text/thinking/tool/turn_end/error/auto_retry_end arms are
// identical to Pi's because the fork kept Pi's event model verbatim.
type ompBackend struct {
	cfg Config
}

// ompSessionDir returns the directory where daemon-run omp sessions live.
// We isolate them under ~/.multica/omp-sessions (rather than omp's own
// ~/.omp/agent/sessions) so daemon-run sessions never collide with the user's
// interactive omp sessions and resume prefixes resolve deterministically.
// omp writes one `<timestamp>_<session-id>.jsonl` file per session here.
func ompSessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".multica", "omp-sessions"), nil
}

// OmpSessionDir exposes ompSessionDir to other packages in this module.
func OmpSessionDir() (string, error) {
	return ompSessionDir()
}

func (b *ompBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execName := b.cfg.ExecutablePath
	if execName == "" {
		execName = "omp"
	}
	lookedUp, err := exec.LookPath(execName)
	if err != nil {
		return nil, fmt.Errorf("omp executable not found at %q: %w", execName, err)
	}

	timeout := opts.Timeout

	// omp stores sessions under an explicit --session-dir. The directory must
	// exist; omp creates one <timestamp>_<id>.jsonl file per session inside it.
	// We always pin the same dir so a ResumeSessionID (an opaque omp session
	// id) resolves deterministically regardless of the task's cwd — resume
	// resolution under an explicit --session-dir is cwd-independent (verified
	// against omp 16.x: a --resume <id-prefix> run in a different cwd than the
	// original still loads the session because --session-dir points the lookup).
	sessionDir, err := ompSessionDir()
	if err != nil {
		return nil, fmt.Errorf("omp session dir: %w", err)
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("omp session dir: %w", err)
	}

	runCtx, cancel := runContext(ctx, timeout)

	args := buildOmpArgs(prompt, sessionDir, opts, b.cfg.Logger)
	argv0, cmdArgs := chooseOmpInvocation(execName, lookedUp, args, b.cfg.Logger)

	cmd := exec.CommandContext(runCtx, argv0, cmdArgs...)
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", argv0, "args", cmdArgs)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("omp stdout pipe: %w", err)
	}
	// Attach an explicit stdin pipe and close it after Start, for the same
	// reason as the Pi backend (#2188): omp shares Pi's bun-based event loop,
	// and when cmd.Stdin is left nil under systemd the child has been observed
	// to block in its event loop awaiting stdin events instead of progressing.
	// Closing the pipe delivers an explicit EOF on a FIFO and unblocks it.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("omp stdin pipe: %w", err)
	}
	cmd.Stderr = newLogWriter(b.cfg.Logger, "[omp:stderr] ")

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		cancel()
		return nil, fmt.Errorf("start omp: %w", err)
	}
	_ = stdin.Close()

	b.cfg.Logger.Info("omp started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// Close stdout when the context is cancelled so scanner.Scan() unblocks.
	go func() {
		<-runCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		var output strings.Builder
		finalStatus := "completed"
		var finalError string
		usage := make(map[string]TokenUsage)
		// sessionID is the opaque omp session identifier parsed from the
		// leading {"type":"session","id":...} header. It is the resume token
		// returned in Result.SessionID and surfaced early via a Status message.
		var sessionID string

		scanner := bufio.NewScanner(stdout)
		// omp message_update events embed the full message partial on each
		// delta (same as Pi), so give the scanner generous headroom.
		scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
		var textBuffer strings.Builder

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var evt piStreamEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				// Malformed lines are skipped without aborting the run, so a
				// single bad line from the CLI can't kill an otherwise healthy
				// session. Mirrors the Pi backend's tolerance.
				continue
			}

			switch evt.Type {
			case "session":
				// The leading session header. Capture the id for resume and
				// surface it immediately so the daemon can pin the resume
				// pointer before the run completes.
				if evt.ID != "" {
					sessionID = evt.ID
					trySend(msgCh, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
				}

			case "agent_start":
				trySend(msgCh, Message{Type: MessageStatus, Status: "running"})

			case "turn_start":
				output.Reset()
				textBuffer.Reset()

			case "message_update":
				if evt.AssistantMessageEvent == nil {
					continue
				}
				switch evt.AssistantMessageEvent.Type {
				case "text_delta":
					if d := drainPiTextBuffer(&textBuffer, evt.AssistantMessageEvent.Delta); d != "" {
						output.WriteString(d)
						trySend(msgCh, Message{Type: MessageText, Content: d})
					}
				case "thinking_delta":
					if d := evt.AssistantMessageEvent.Delta; d != "" {
						trySend(msgCh, Message{Type: MessageThinking, Content: d})
					}
				}

			case "tool_execution_start":
				var params map[string]any
				if len(evt.Args) > 0 {
					_ = json.Unmarshal(evt.Args, &params)
				}
				trySend(msgCh, Message{
					Type:   MessageToolUse,
					Tool:   evt.ToolName,
					CallID: evt.ToolCallID,
					Input:  params,
				})

			case "tool_execution_end":
				trySend(msgCh, Message{
					Type:   MessageToolResult,
					CallID: evt.ToolCallID,
					Output: decodePiResult(evt.Result),
				})

			case "turn_end":
				if msg := decodePiMessage(evt.Message); msg != nil && msg.Usage != nil {
					model := msg.Model
					if model == "" {
						model = opts.Model
					}
					if model == "" {
						model = "unknown"
					}
					u := usage[model]
					u.InputTokens += msg.Usage.Input
					u.OutputTokens += msg.Usage.Output
					u.CacheReadTokens += msg.Usage.CacheRead
					u.CacheWriteTokens += msg.Usage.CacheWrite
					usage[model] = u
				}

			case "error":
				errText := decodePiString(evt.Message)
				trySend(msgCh, Message{Type: MessageError, Content: errText})
				if finalStatus == "completed" {
					finalStatus = "failed"
					finalError = errText
				}

			case "auto_retry_end":
				if !evt.Success && finalStatus == "completed" {
					finalStatus = "failed"
					if evt.FinalError != "" {
						finalError = evt.FinalError
					} else {
						finalError = "omp exhausted automatic retries"
					}
				}
			}
		}
		if d := flushPiTextBuffer(&textBuffer); d != "" {
			output.WriteString(d)
			trySend(msgCh, Message{Type: MessageText, Content: d})
		}

		waitErr := cmd.Wait()
		duration := time.Since(startTime)

		if runCtx.Err() == context.DeadlineExceeded {
			finalStatus = "timeout"
			finalError = fmt.Sprintf("omp timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			finalStatus = "aborted"
			finalError = "execution cancelled"
		} else if waitErr != nil && finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("omp exited with error: %v", waitErr)
		}

		b.cfg.Logger.Info("omp finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		resCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// ── Arg builder ──

// ompBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Overriding these would break the
// daemon↔omp communication protocol.
//
// omp's `--session` is an alias for `--resume`, NOT Pi's session-file flag, so
// it (and `-r`/`--resume`) is blocked alongside `--session-dir` and `--cwd`
// (both daemon-owned). `-p`/`--print` and `--mode` select the non-interactive
// JSON transport.
var ompBlockedArgs = map[string]blockedArgMode{
	"-p":           blockedStandalone, // non-interactive mode
	"--print":      blockedStandalone, // alias for -p
	"--mode":       blockedWithValue,  // "json" event stream protocol
	"--resume":     blockedWithValue,  // daemon manages resume via ResumeSessionID
	"-r":           blockedWithValue,  // alias for --resume
	"--session":    blockedWithValue,  // alias for --resume in omp (NOT Pi's session file)
	"--session-dir": blockedWithValue, // daemon pins the session storage dir
	"--cwd":        blockedWithValue,  // daemon owns the task cwd
}

// buildOmpArgs assembles the argv for a one-shot omp invocation.
//
// Flags:
//
//	-p                          non-interactive mode (prompt is positional)
//	--mode json                 emit one JSON event per line on stdout
//	--session-dir <dir>         daemon-pinned session storage (always set)
//	--resume <id>               resume an existing session (only when ResumeSessionID set)
//	--provider <name>           provider, when Model is "provider/id"
//	--model <id>                model identifier
//	--append-system-prompt <s>  extra system instructions
//	--thinking <level>          reasoning effort (only when ThinkingLevel set)
//
// Custom args appended before the positional prompt. The prompt is a
// positional argument and must be last.
//
// We do NOT pass --tools (same reasoning as Pi's #2379: omitting it lets omp
// use its full tool registry including user-installed extension tools), and we
// do NOT pass --auto-approve because -p (non-interactive print mode) already
// auto-approves tool calls — verified against omp 16.x, a -p run executes
// tools without hanging on an approval prompt.
func buildOmpArgs(prompt, sessionDir string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"-p",
		"--mode", "json",
		"--session-dir", sessionDir,
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	if opts.Model != "" {
		provider, model := splitPiModel(opts.Model)
		if provider != "" {
			args = append(args, "--provider", provider)
		}
		if model != "" {
			args = append(args, "--model", model)
		}
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--thinking", opts.ThinkingLevel)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs, ompBlockedArgs, logger)...)
	args = append(args, prompt)
	return args
}
