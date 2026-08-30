package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
	"github.com/majorcontext/harness/provider"
)

// fakeClaudeBin is the path to the compiled fakeclaude stand-in (see
// engine/testdata/fakeclaude/main.go), built once for the whole package
// run by buildFakeClaude below.
var (
	fakeClaudeBin     string
	fakeClaudeBinOnce sync.Once
	fakeClaudeBinErr  error
)

// buildFakeClaude compiles engine/testdata/fakeclaude into a temp binary a
// single time (sync.Once) for however many tests in this package need it —
// mirrors e2e/e2e_test.go's buildHarness precedent for the same reason:
// paying one `go build` up front is far cheaper and more deterministic
// than a shell-script stand-in with its own quoting/portability concerns.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "harness-fakeclaude")
		if err != nil {
			fakeClaudeBinErr = err
			return
		}
		bin := filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeclaude")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeBinErr = fmt.Errorf("go build fakeclaude: %v\n%s", err, out)
			return
		}
		fakeClaudeBin = bin
	})
	if fakeClaudeBinErr != nil {
		t.Fatalf("buildFakeClaude: %v", fakeClaudeBinErr)
	}
	return fakeClaudeBin
}

// claudeCodeTestSession builds a session whose model routes to the
// delegated backend, with binary set to the fake stand-in and mode/session
// id/log path threaded through environment variables the child inherits
// (see fakeclaude's own doc comment) — t.Setenv so each test gets its own
// isolated values without racing another test's env.
func claudeCodeTestSession(t *testing.T, mode string) (*Session, string) {
	t.Helper()
	bin := buildFakeClaude(t)
	t.Setenv("FAKE_CLAUDE_MODE", mode)
	logPath := filepath.Join(t.TempDir(), "invocations.jsonl")
	t.Setenv("FAKE_CLAUDE_LOG", logPath)
	s := NewSession(Config{
		SessionDir: t.TempDir(),
		Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode: ClaudeCodeConfig{BinaryPath: bin},
	})
	return s, logPath
}

// readInvocations parses the fakeclaude invocation log (one JSON array of
// argv per line, per FAKE_CLAUDE_LOG's own doc comment) into a slice of
// argv slices, one per `claude` child spawned so far.
func readInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading invocation log: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var out [][]string
	for {
		var argv []string
		if err := dec.Decode(&argv); err != nil {
			break
		}
		out = append(out, argv)
	}
	return out
}

func argvContains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func argvValueAfter(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// TestClaudeCodeDelegatedTurnMapsEventsAndUsage drives one full turn
// through fakeclaude's default ("normal") canned sequence and asserts
// every event->message mapping this backend documents: an assistant text
// message, a ToolCall-bearing assistant message, a ToolResult-bearing tool
// message, a final assistant text message, the right engine.Events, the
// captured Claude Code session id, and the mapped usage.
func TestClaudeCodeDelegatedTurnMapsEventsAndUsage(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")

	var events []Event
	s.cfg.OnEvent = func(ev Event) { events = append(events, ev) }
	// OnEvent is read from s.cfg at emit time (see Session.emit); the
	// field assignment above is safe pre-Prompt since nothing else
	// touches the session yet.

	msg, err := s.Prompt(context.Background(), "please run echo hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if msg == nil {
		t.Fatal("Prompt returned a nil final message")
	}
	if got := msg.Parts.Text(); got != "Done — it printed hi." {
		t.Errorf("final message text = %q", got)
	}
	if msg.Origin != message.OriginClaudeCode {
		t.Errorf("final message Origin = %q, want %q", msg.Origin, message.OriginClaudeCode)
	}

	hist := s.History()
	// user prompt, assistant("Let me check that."), assistant(tool_use),
	// tool(tool_result), assistant("Done...") = 5 messages.
	if len(hist) != 5 {
		t.Fatalf("History() len = %d, want 5: %+v", len(hist), hist)
	}
	if hist[0].Role != message.RoleUser {
		t.Errorf("hist[0].Role = %q, want user", hist[0].Role)
	}
	if hist[1].Role != message.RoleAssistant || hist[1].Parts.Text() != "Let me check that." {
		t.Errorf("hist[1] = %+v", hist[1])
	}
	tc, ok := hist[2].Parts[0].(*message.ToolCall)
	if hist[2].Role != message.RoleAssistant || !ok || tc.Name != "Bash" || tc.CallID != "toolu_1" {
		t.Errorf("hist[2] = %+v, want an assistant ToolCall(Bash, toolu_1)", hist[2])
	}
	tr, ok := hist[3].Parts[0].(*message.ToolResult)
	if hist[3].Role != message.RoleTool || !ok || tr.CallID != "toolu_1" || tr.Content.Text() != "hi\n" {
		t.Errorf("hist[3] = %+v, want a tool ToolResult(toolu_1, \"hi\\n\")", hist[3])
	}
	if hist[4].Role != message.RoleAssistant || hist[4].Parts.Text() != "Done — it printed hi." {
		t.Errorf("hist[4] = %+v", hist[4])
	}

	// Event mapping: at least one ToolStart and one ToolEnd, matched by
	// call id.
	var sawStart, sawEnd bool
	for _, ev := range events {
		if ev.Type == EventToolStart && ev.ToolCall != nil && ev.ToolCall.CallID == "toolu_1" {
			sawStart = true
		}
		if ev.Type == EventToolEnd && ev.ToolCall != nil && ev.ToolCall.CallID == "toolu_1" {
			sawEnd = true
		}
	}
	if !sawStart {
		t.Error("no EventToolStart for toolu_1")
	}
	if !sawEnd {
		t.Error("no EventToolEnd for toolu_1")
	}

	// Usage mapping (mapClaudeCodeUsage): input/output/cache read/cache
	// write map from fakeclaude's canned result usage object.
	usage := s.Usage()
	if usage.InputTokens != 101 || usage.OutputTokens != 42 || usage.CacheReadTokens != 7 || usage.CacheWriteTokens != 5 {
		t.Errorf("Usage() = %+v, want {101 42 7 5}", usage)
	}

	if s.claudeCodeSessionID() != "fake-session-1" {
		t.Errorf("claudeCodeSessionID() = %q, want fake-session-1", s.claudeCodeSessionID())
	}
}

// TestClaudeCodeSessionIDResumedAcrossTurns proves the SECOND delegated
// turn against the same session passes --resume naming the CLI session id
// captured from the FIRST turn's system/init event.
func TestClaudeCodeSessionIDResumedAcrossTurns(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")

	if _, err := s.Prompt(context.Background(), "first turn"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if _, err := s.Prompt(context.Background(), "second turn"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}

	invocations := readInvocations(t, logPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations = %d, want 2: %+v", len(invocations), invocations)
	}
	if argvContains(invocations[0], "--resume") {
		t.Errorf("first invocation argv unexpectedly carries --resume: %v", invocations[0])
	}
	resumeID, ok := argvValueAfter(invocations[1], "--resume")
	if !ok {
		t.Fatalf("second invocation argv has no --resume: %v", invocations[1])
	}
	if resumeID != "fake-session-1" {
		t.Errorf("--resume value = %q, want fake-session-1", resumeID)
	}
	// --model sonnet must also be passed through on both calls.
	for i, argv := range invocations {
		if v, ok := argvValueAfter(argv, "--model"); !ok || v != "sonnet" {
			t.Errorf("invocation %d --model = %q, ok=%v, want sonnet", i, v, ok)
		}
	}

	// The session-id record survives a reload (LoadSession folds
	// recClaudeCodeSessionID) — a third turn after a fresh load must
	// still resume the same CLI session.
	reloaded, err := LoadSession(Config{
		SessionDir: s.cfg.SessionDir,
		ClaudeCode: s.cfg.ClaudeCode,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if reloaded.claudeCodeSessionID() != "fake-session-1" {
		t.Errorf("reloaded claudeCodeSessionID() = %q, want fake-session-1", reloaded.claudeCodeSessionID())
	}
	if _, err := reloaded.Prompt(context.Background(), "third turn"); err != nil {
		t.Fatalf("third Prompt (post-reload): %v", err)
	}
	invocations = readInvocations(t, logPath)
	if len(invocations) != 3 {
		t.Fatalf("invocations after reload = %d, want 3", len(invocations))
	}
	if v, ok := argvValueAfter(invocations[2], "--resume"); !ok || v != "fake-session-1" {
		t.Errorf("post-reload --resume = %q, ok=%v, want fake-session-1", v, ok)
	}
}

// TestClaudeCodeErrorResultReturnsError proves an IsError "result" event
// surfaces as Prompt's returned error, and that Session.Usage() still
// reflects the failed attempt's own billed usage (Claude Code, like a
// native provider, bills a call whether or not it produced a usable
// outcome).
func TestClaudeCodeErrorResultReturnsError(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "error")

	msg, err := s.Prompt(context.Background(), "do something that fails")
	if err == nil {
		t.Fatal("Prompt returned no error for an is_error result")
	}
	if msg != nil {
		t.Errorf("Prompt returned a non-nil message alongside an error: %+v", msg)
	}
	usage := s.Usage()
	if usage.InputTokens != 11 || usage.OutputTokens != 3 {
		t.Errorf("Usage() = %+v, want {11 3 0 0} (the failed call's own billed usage)", usage)
	}
}

// TestClaudeCodeAbortSignalsChild proves a canceled context makes
// runClaudeCodeTurn return promptly (bounded well under fakeclaude's own
// hour-long sleep) rather than hanging until the child exits on its own —
// the "hang" mode's child installs no signal handlers, so the driver's own
// SIGINT (Go's default disposition terminates an unhandled-signal process)
// is what has to end it. See claude_code_backend.go's signal-cascade
// goroutine.
func TestClaudeCodeAbortSignalsChild(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, "this will hang")
		done <- err
	}()

	// Give fakeclaude a moment to actually start and emit its init event
	// before pulling the plug — proves the abort interrupts a GENUINELY
	// in-flight child, not one that never started.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Prompt error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Prompt did not return within 10s of cancellation — child was not interrupted")
	}
}

// TestClaudeCodeModelRefSelection proves selection is purely a function of
// the session's model ref: a claude-code ref dispatches to the delegated
// backend (never touching the registered native provider at all), and an
// ordinary ref keeps running the native provider-call path untouched —
// the same session type, same Prompt call, branching only on
// claudeCodeDelegated().
func TestClaudeCodeModelRefSelection(t *testing.T) {
	t.Run("claude-code ref bypasses the native provider entirely", func(t *testing.T) {
		bin := buildFakeClaude(t)
		t.Setenv("FAKE_CLAUDE_MODE", "normal")
		t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))
		prov := &scriptedProvider{name: "native-should-not-be-called"}
		s := NewSession(Config{
			SessionDir: t.TempDir(),
			Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
			ClaudeCode: ClaudeCodeConfig{BinaryPath: bin},
			Providers:  provider.Registry{prov.name: prov},
		})
		if !s.claudeCodeDelegated() {
			t.Fatal("claudeCodeDelegated() = false for a claude-code model ref")
		}
		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if prov.call != 0 {
			t.Errorf("native provider Stream called %d times, want 0", prov.call)
		}
	})

	t.Run("a normal ref never touches the delegated backend", func(t *testing.T) {
		prov := &scriptedProvider{name: "native", turns: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "hi from native"}),
		}}
		s := NewSession(Config{
			SessionDir: t.TempDir(),
			Model:      message.ModelRef{Provider: "native", Model: "m1"},
			Providers:  provider.Registry{prov.name: prov},
			// Deliberately no ClaudeCode.BinaryPath: if this session were
			// ever (incorrectly) dispatched to the delegated backend, the
			// exec would fail loudly (no such binary) rather than
			// silently succeeding, making this a meaningful negative
			// check.
		})
		if s.claudeCodeDelegated() {
			t.Fatal("claudeCodeDelegated() = true for an ordinary provider ref")
		}
		msg, err := s.Prompt(context.Background(), "hello")
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if msg.Parts.Text() != "hi from native" {
			t.Errorf("final text = %q, want the native provider's own reply", msg.Parts.Text())
		}
		if prov.call != 1 {
			t.Errorf("native provider Stream called %d times, want 1", prov.call)
		}
	})
}

// TestClaudeCodeContextWindowSatisfiesRequireContextWindow proves the
// modelmeta entry this backend relies on: a session whose model names
// ClaudeCodeProviderFamily must not be refused at create time even when
// Config.RequireContextWindow is set (the default — see
// config.ContextWindowRequiredValue) — see modelmeta.ContextWindow's own
// claudeCodeProvider case.
func TestClaudeCodeContextWindowSatisfiesRequireContextWindow(t *testing.T) {
	s := NewSession(Config{
		Model:                message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		RequireContextWindow: true,
	})
	if err := s.ContextWindowErr(); err != nil {
		t.Errorf("ContextWindowErr() = %v, want nil for a claude-code model ref", err)
	}
}

// TestClaudeCodeProviderFamilyMatchesModelmeta proves
// ClaudeCodeProviderFamily and modelmeta's own (unexported) duplicate of
// that string — see modelmeta.ContextWindow's claudeCodeProvider case —
// have not drifted apart: a claude-code model ref must resolve a KNOWN
// context window, or RequireContextWindow refuses session create for
// every real deployment (see modelmeta.go's own doc comment on why the
// string is duplicated there rather than imported).
func TestClaudeCodeProviderFamilyMatchesModelmeta(t *testing.T) {
	ref := message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"}
	if _, ok := modelmeta.ContextWindow(ref); !ok {
		t.Errorf("modelmeta.ContextWindow(%s) reported unknown — modelmeta's claudeCodeProvider constant has drifted from engine.ClaudeCodeProviderFamily (%q)", ref, ClaudeCodeProviderFamily)
	}
}

// TestClaudeCodeDefaultBinaryPath proves newSession defaults an unset
// ClaudeCodeConfig.BinaryPath to "claude" — config.Provider.BinaryPath's
// own doc comment promises this.
func TestClaudeCodeDefaultBinaryPath(t *testing.T) {
	s := NewSession(Config{Model: message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"}})
	if s.cfg.ClaudeCode.BinaryPath != defaultClaudeCodeBinaryPath {
		t.Errorf("ClaudeCode.BinaryPath = %q, want %q", s.cfg.ClaudeCode.BinaryPath, defaultClaudeCodeBinaryPath)
	}
}
