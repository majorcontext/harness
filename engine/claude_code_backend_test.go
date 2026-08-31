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
	"strings"
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

// TestClaudeCodeMCPConfigFileWritesConfiguredServers proves
// Session.claudeCodeMCPConfigFile translates a session's configured MCP
// servers (a stdio server and an HTTP server) into the CLI's own
// --mcp-config JSON shape, and that its cleanup func actually removes the
// file.
func TestClaudeCodeMCPConfigFileWritesConfiguredServers(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"fs":     {Command: []string{"mcp-fs", "--root", "/work"}, Env: []string{"FOO=bar"}},
		"remote": {URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer tok"}},
	})
	s := NewSession(Config{
		Model: message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		MCP:   mgr,
	})

	path, cleanup, err := s.claudeCodeMCPConfigFile()
	if err != nil {
		t.Fatalf("claudeCodeMCPConfigFile: %v", err)
	}
	if path == "" {
		t.Fatal("claudeCodeMCPConfigFile returned an empty path for a session with configured MCP servers")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading mcp-config file: %v", err)
	}
	var got claudeCodeMCPConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding mcp-config file: %v", err)
	}

	fs, ok := got.MCPServers["fs"]
	if !ok {
		t.Fatalf(`mcp-config missing "fs" server: %+v`, got.MCPServers)
	}
	if fs.Type != "" || fs.Command != "mcp-fs" || len(fs.Args) != 2 || fs.Args[0] != "--root" || fs.Args[1] != "/work" {
		t.Errorf("fs server = %+v, want a stdio server: command mcp-fs, args [--root /work]", fs)
	}
	if fs.Env["FOO"] != "bar" {
		t.Errorf("fs server Env = %+v, want FOO=bar", fs.Env)
	}

	remote, ok := got.MCPServers["remote"]
	if !ok {
		t.Fatalf(`mcp-config missing "remote" server: %+v`, got.MCPServers)
	}
	if remote.Type != "http" || remote.URL != "https://example.com/mcp" || remote.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("remote server = %+v, want an http server naming example.com with its Authorization header", remote)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("mcp-config file %q still exists after cleanup", path)
	}
}

// TestClaudeCodeMCPConfigFileEmptyWithNoServers proves a session with no
// configured MCP servers (nil Config.MCP, the default) gets no
// --mcp-config file at all — MCP passthrough is opt-in, never a hard
// requirement for a delegated turn.
func TestClaudeCodeMCPConfigFileEmptyWithNoServers(t *testing.T) {
	s := NewSession(Config{Model: message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"}})
	path, cleanup, err := s.claudeCodeMCPConfigFile()
	if err != nil {
		t.Fatalf("claudeCodeMCPConfigFile: %v", err)
	}
	defer cleanup()
	if path != "" {
		t.Errorf("claudeCodeMCPConfigFile path = %q, want empty for a session with no configured MCP servers", path)
	}
}

// TestClaudeCodeMCPConfigForwardedToChildArgv proves runClaudeCodeTurn
// actually appends --mcp-config (naming a real, readable file) and
// --strict-mcp-config to the child's argv when the session has configured
// MCP servers.
func TestClaudeCodeMCPConfigForwardedToChildArgv(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	s.cfg.MCP = NewMCPManager(map[string]MCPServerConfig{
		"fs": {Command: []string{"mcp-fs"}},
	})
	// s.cfg.MCP is read fresh by claudeCodeMCPConfigFile at turn time (see
	// claudeCodeTestSession's OnEvent precedent above): safe to set
	// directly here, pre-Prompt, before anything else touches the session.

	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if len(invocations) != 1 {
		t.Fatalf("invocations = %d, want 1: %+v", len(invocations), invocations)
	}
	path, ok := argvValueAfter(invocations[0], "--mcp-config")
	if !ok || path == "" {
		t.Fatalf("argv has no non-empty --mcp-config value: %v", invocations[0])
	}
	if !argvContains(invocations[0], "--strict-mcp-config") {
		t.Errorf("argv missing --strict-mcp-config: %v", invocations[0])
	}
}

// TestClaudeCodeMCPConfigCredentialsNeverInChildArgv locks in the reason
// claudeCodeMCPConfigFile writes a temp FILE rather than passing an inline
// --mcp-config JSON string: a server's Headers/Env can carry real
// credential material, and argv is visible to any other process on the
// box via /proc or ps. This configures a server with a bearer-token header
// and a secret-bearing env entry, drives one real turn, and asserts the
// secret value never appears in ANY element of the child's own argv — see
// TestClaudeCodeMCPConfigFileWritesConfiguredServers for the companion
// assertion that the same secret DOES reach the server correctly, via the
// (by-then-removed) config file's own JSON content.
func TestClaudeCodeMCPConfigCredentialsNeverInChildArgv(t *testing.T) {
	const secret = "sk-super-secret-token-do-not-leak"
	s, logPath := claudeCodeTestSession(t, "normal")
	s.cfg.MCP = NewMCPManager(map[string]MCPServerConfig{
		"remote": {URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer " + secret}},
		"fs":     {Command: []string{"mcp-fs"}, Env: []string{"MCP_FS_TOKEN=" + secret}},
	})

	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if len(invocations) != 1 {
		t.Fatalf("invocations = %d, want 1: %+v", len(invocations), invocations)
	}
	for _, arg := range invocations[0] {
		if strings.Contains(arg, secret) {
			t.Fatalf("child argv leaked the MCP credential (%q): %v", secret, invocations[0])
		}
	}
	if _, ok := argvValueAfter(invocations[0], "--mcp-config"); !ok {
		t.Fatalf("argv has no --mcp-config at all: %v", invocations[0])
	}
}

// TestClaudeCodeEffortForwardedToChildArgv proves runClaudeCodeTurn reads
// s.Effort() at turn time and forwards it as --effort, mapped through
// claudeCodeEffortArg exactly as that function's own doc comment promises.
func TestClaudeCodeEffortForwardedToChildArgv(t *testing.T) {
	tests := []struct {
		name   string
		effort message.Effort
		want   string
	}{
		{"off maps to the CLI floor", message.EffortOff, "low"},
		{"minimal maps to the CLI floor", message.EffortMinimal, "low"},
		{"low maps to low", message.EffortLow, "low"},
		{"medium maps to medium", message.EffortMedium, "medium"},
		{"high maps to high", message.EffortHigh, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logPath := claudeCodeTestSession(t, "normal")
			s.SetEffort(tt.effort)
			if _, err := s.Prompt(context.Background(), "hi"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			invocations := readInvocations(t, logPath)
			got, ok := argvValueAfter(invocations[0], "--effort")
			if !ok {
				t.Fatalf("argv has no --effort: %v", invocations[0])
			}
			if got != tt.want {
				t.Errorf("--effort = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClaudeCodeEffortUnsetOmitsFlag proves a session that never called
// SetEffort (message.EffortUnset, the zero value) sends no --effort flag at
// all, mirroring how an unset provider.Request.Effort sends no reasoning
// control to a native provider.
func TestClaudeCodeEffortUnsetOmitsFlag(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if argvContains(invocations[0], "--effort") {
		t.Errorf("argv unexpectedly carries --effort for EffortUnset: %v", invocations[0])
	}
}

// TestClaudeCodeThinkingBlockDecodesToReasoningPart proves a "thinking"
// content block — previously silently dropped (see claudeCodeContentBlock's
// switch in claudeCodeAssistantMessage) — decodes into a message.Reasoning
// part, is appended to history, and is emitted as EventReasoningDelta.
func TestClaudeCodeThinkingBlockDecodesToReasoningPart(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking")

	var events []Event
	s.cfg.OnEvent = func(ev Event) { events = append(events, ev) }

	msg, err := s.Prompt(context.Background(), "think about it")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := msg.Parts.Text(); got != "Here is my answer." {
		t.Errorf("final message text = %q", got)
	}

	hist := s.History()
	// user prompt, assistant(thinking), assistant(text) = 3 messages.
	if len(hist) != 3 {
		t.Fatalf("History() len = %d, want 3: %+v", len(hist), hist)
	}
	reasoning, ok := hist[1].Parts[0].(*message.Reasoning)
	if hist[1].Role != message.RoleAssistant || !ok || reasoning.Text != "Let me reason about this." {
		t.Fatalf("hist[1] = %+v, want an assistant Reasoning(%q)", hist[1], "Let me reason about this.")
	}
	if len(reasoning.ProviderData) == 0 {
		t.Error("Reasoning.ProviderData is empty, want the thinking block's signature carried through")
	}

	var sawReasoningDelta bool
	for _, ev := range events {
		if ev.Type == EventReasoningDelta && ev.Text == "Let me reason about this." {
			sawReasoningDelta = true
		}
	}
	if !sawReasoningDelta {
		t.Error("no EventReasoningDelta for the thinking block")
	}
}

// TestClaudeCodeParentToolUseIDCarriedOntoMessage proves the envelope's own
// parent_tool_use_id (null at top level, set to the spawning tool_use id
// inside a subagent's own turn) rides onto Message.ParentToolUseID for
// both an "assistant" and a "user" (tool_result) event.
func TestClaudeCodeParentToolUseIDCarriedOntoMessage(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "subagent")

	if _, err := s.Prompt(context.Background(), "spawn a subagent"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	hist := s.History()
	// user, assistant(tool_use Task, top-level), assistant(text, nested),
	// tool(tool_result, nested), assistant(text, top-level) = 5 messages.
	if len(hist) != 5 {
		t.Fatalf("History() len = %d, want 5: %+v", len(hist), hist)
	}
	if got := hist[1].ParentToolUseID; got != "" {
		t.Errorf("hist[1] (top-level tool_use) ParentToolUseID = %q, want empty", got)
	}
	if got := hist[2].ParentToolUseID; got != "toolu_parent" {
		t.Errorf("hist[2] (nested assistant text) ParentToolUseID = %q, want toolu_parent", got)
	}
	if got := hist[3].ParentToolUseID; got != "toolu_parent" {
		t.Errorf("hist[3] (nested tool result) ParentToolUseID = %q, want toolu_parent", got)
	}
	if got := hist[4].ParentToolUseID; got != "" {
		t.Errorf("hist[4] (final top-level text) ParentToolUseID = %q, want empty", got)
	}
}

// TestClaudeCodeTurnMetricsEmittedForDelegatedTurn proves runClaudeCodeTurn
// emits exactly one OnTurnMetrics record per delegated turn, built from the
// "result" event's own ttft_ms/duration_ms and the usage
// applyClaudeCodeUsage already maps — engine.go's native streamTurn emits
// one per completed turn too (see its own EventDone case), and a delegated
// turn getting none at all was one of this backend's v1 gaps.
func TestClaudeCodeTurnMetricsEmittedForDelegatedTurn(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")

	var metrics []TurnMetrics
	s.cfg.OnTurnMetrics = func(m TurnMetrics) { metrics = append(metrics, m) }

	if _, err := s.Prompt(context.Background(), "please run echo hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(metrics) != 1 {
		t.Fatalf("OnTurnMetrics called %d times, want 1: %+v", len(metrics), metrics)
	}
	m := metrics[0]
	if m.SessionID != s.ID {
		t.Errorf("SessionID = %q, want %q", m.SessionID, s.ID)
	}
	if m.TTFTMillis != 50 {
		t.Errorf("TTFTMillis = %d, want 50 (fakeclaude's own canned ttft_ms)", m.TTFTMillis)
	}
	if m.StreamMillis != 350 {
		t.Errorf("StreamMillis = %d, want 350 (duration_ms 400 - ttft_ms 50)", m.StreamMillis)
	}
	if m.InputTokens != 101 || m.OutputTokens != 42 {
		t.Errorf("TurnMetrics usage = {input:%d output:%d}, want {101 42} (fakeclaude's own canned usage)", m.InputTokens, m.OutputTokens)
	}
}

// TestClaudeCodeRateLimitEventCapturesSubscriptionUsage drives a turn
// through fakeclaude's "rate_limit_event" mode (a rate_limit_event ahead of
// the turn's final assistant text — the CLI's own documented ordering, see
// this file's package doc) and asserts Session.SubscriptionUsage() —
// exactly what buildSession (server/handlers.go) reads for GET /session's
// subscription_usage field — carries the mapped provider, windows, and
// overage.
func TestClaudeCodeRateLimitEventCapturesSubscriptionUsage(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "rate_limit_event")

	if got := s.SubscriptionUsage(); got != nil {
		t.Fatalf("SubscriptionUsage() before any turn = %+v, want nil", got)
	}

	if _, err := s.Prompt(context.Background(), "how am I doing on quota?"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	usage := s.SubscriptionUsage()
	if usage == nil {
		t.Fatal("SubscriptionUsage() after the turn = nil, want a captured snapshot")
	}
	if usage.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", usage.Provider)
	}
	if usage.Plan != "" {
		t.Errorf("Plan = %q, want \"\" (not cheaply available from rate_limit_event)", usage.Plan)
	}
	if usage.CapturedAt == 0 {
		t.Error("CapturedAt = 0, want a stamped Unix timestamp")
	}
	if len(usage.Windows) != 2 {
		t.Fatalf("Windows = %+v, want 2 entries", usage.Windows)
	}
	// Sorted by key (mapClaudeCodeRateLimit): "five_hour" before "seven_day".
	fh, sd := usage.Windows[0], usage.Windows[1]
	if fh.Key != "five_hour" || fh.Label != "5-hour" || fh.UsedPercent != 2 || fh.ResetsAt != 1788785267 {
		t.Errorf("Windows[0] = %+v, want {five_hour 5-hour 2 1788785267}", fh)
	}
	if sd.Key != "seven_day" || sd.Label != "Weekly" || sd.UsedPercent != 13 || sd.ResetsAt != 1789200000 {
		t.Errorf("Windows[1] = %+v, want {seven_day Weekly 13 1789200000}", sd)
	}
	if usage.Overage == nil {
		t.Fatal("Overage = nil, want a mapped overage object")
	}
	if usage.Overage.InUse || usage.Overage.Status != "allowed" || usage.Overage.ResetsAt != 1789000000 {
		t.Errorf("Overage = %+v, want {false allowed 1789000000}", usage.Overage)
	}
}

// TestClaudeCodeRetryableClassification proves a "result" event this file
// can actually name as transient provider weather (a rate-limit signal) is
// wrapped provider.RetryableError, while a genuinely deterministic result
// (max turns reached) is not — goal.go's promptTurnWithRetry uses exactly
// this distinction (provider.AsRetryable) to decide whether to back off and
// retry or fail fast.
func TestClaudeCodeRetryableClassification(t *testing.T) {
	t.Run("a rate-limit result is retryable", func(t *testing.T) {
		s, _ := claudeCodeTestSession(t, "rate_limit_error")
		_, err := s.Prompt(context.Background(), "do something")
		if err == nil {
			t.Fatal("Prompt returned no error for an is_error result")
		}
		class, ok := provider.AsRetryable(err)
		if !ok {
			t.Fatalf("provider.AsRetryable(%v) = false, want a retryable classification", err)
		}
		if class != provider.RetryableRateLimited {
			t.Errorf("class = %q, want %q", class, provider.RetryableRateLimited)
		}
	})
	t.Run("a deterministic max-turns result is not retryable", func(t *testing.T) {
		s, _ := claudeCodeTestSession(t, "deterministic_error")
		_, err := s.Prompt(context.Background(), "do something")
		if err == nil {
			t.Fatal("Prompt returned no error for an is_error result")
		}
		if _, ok := provider.AsRetryable(err); ok {
			t.Errorf("provider.AsRetryable(%v) = true, want false for a deterministic max-turns failure", err)
		}
	})
}

// TestClaudeCodeChildCrashWithoutResultIsRetryable proves a child that
// emitted at least one "system" event (so a session genuinely started) and
// THEN exits nonzero WITHOUT ever emitting a clean "result" event (a
// crash, an OOM kill — non-deterministic child-process weather, not a
// domain-level failure the CLI itself reported) is wrapped
// provider.RetryableError, via runClaudeCodeTurn's own waitErr branch
// rather than claudeCodeRetryableClass. See
// TestClaudeCodeChildExitBeforeAnySystemEventIsNotRetryable for the
// opposite case this same branch must get right.
func TestClaudeCodeChildCrashWithoutResultIsRetryable(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "crash")
	_, err := s.Prompt(context.Background(), "do something")
	if err == nil {
		t.Fatal("Prompt returned no error for a child that exited without a result event")
	}
	class, ok := provider.AsRetryable(err)
	if !ok {
		t.Fatalf("provider.AsRetryable(%v) = false, want retryable for a non-deterministic child crash", err)
	}
	if class != provider.RetryableServerError {
		t.Errorf("class = %q, want %q", class, provider.RetryableServerError)
	}
}

// TestClaudeCodeChildExitBeforeAnySystemEventIsNotRetryable proves a child
// that exits nonzero WITHOUT ever emitting even a "system" event — the
// deterministic-startup-failure shape (an unknown flag on an older
// `claude` build, a malformed --mcp-config command, an invalid --model
// value) — is NOT wrapped provider.RetryableError. Marking a deterministic
// startup failure retryable would have a PursueGoal loop burn its entire
// retryable budget with backoff before parking, delaying the surfacing of
// a config error no amount of waiting will fix. Contrast
// TestClaudeCodeChildCrashWithoutResultIsRetryable, whose child DOES get
// as far as "system" before dying.
func TestClaudeCodeChildExitBeforeAnySystemEventIsNotRetryable(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "crash_before_init")
	_, err := s.Prompt(context.Background(), "do something")
	if err == nil {
		t.Fatal("Prompt returned no error for a child that exited before any system event")
	}
	if _, ok := provider.AsRetryable(err); ok {
		t.Errorf("provider.AsRetryable(%v) = true, want false for a child that never started", err)
	}
}

// TestClaudeCodeTurnResult table-drives claudeCodeTurnResult directly —
// the exact precedence between a caller abort, a classified result error,
// a process-exit error (started vs. not), and a benign input-write race —
// without needing to force any of these interleavings out of a real child
// process. The last two cases are this test's whole reason to exist: they
// lock in the "input-write EPIPE + have-result -> ignore; input-write
// error + no-result -> real error" rule a real subprocess race can only
// exercise probabilistically (see TestClaudeCodeSucceedsDespiteInputWriteBrokenPipe
// below for that best-effort integration-level companion).
func TestClaudeCodeTurnResult(t *testing.T) {
	ctxCanceled := context.Canceled
	turnErr := errors.New("boom: turn error")
	waitErr := errors.New("exit status 1")
	inputErr := errors.New("engine: claude-code: writing turn input: write |1: broken pipe")
	okMsg := &message.Message{ID: "msg_ok"}

	tests := []struct {
		name        string
		outcome     claudeCodeTurnOutcome
		wantMsg     *message.Message
		wantErrIs   error  // set when the returned error must be exactly (or wrap) this value
		wantErrText string // set when the returned error is freshly constructed; substring to require instead
		wantRetry   bool
	}{
		{
			name:      "a caller abort wins over everything else",
			outcome:   claudeCodeTurnOutcome{ctxErr: ctxCanceled, turnErr: turnErr, waitErr: waitErr, inputErr: inputErr, finalMsg: okMsg},
			wantErrIs: ctxCanceled,
		},
		{
			name:      "a classified result error returns as-is",
			outcome:   claudeCodeTurnOutcome{turnErr: turnErr},
			wantErrIs: turnErr,
		},
		{
			name:        "a process exit after the child started is retryable",
			outcome:     claudeCodeTurnOutcome{waitErr: waitErr, started: true},
			wantErrText: waitErr.Error(),
			wantRetry:   true,
		},
		{
			name:        "a process exit before the child ever started is NOT retryable",
			outcome:     claudeCodeTurnOutcome{waitErr: waitErr, started: false},
			wantErrText: waitErr.Error(),
		},
		{
			name:        "no result and no input error is the generic no-assistant-message error",
			outcome:     claudeCodeTurnOutcome{},
			wantErrText: "turn ended with no assistant message",
		},
		{
			name:      "no result AND an input-write error surfaces the input-write error",
			outcome:   claudeCodeTurnOutcome{inputErr: inputErr},
			wantErrIs: inputErr,
		},
		{
			name:    "a usable result suppresses a benign input-write error",
			outcome: claudeCodeTurnOutcome{inputErr: inputErr, finalMsg: okMsg},
			wantMsg: okMsg,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := claudeCodeTurnResult(tt.outcome)
			if msg != tt.wantMsg {
				t.Errorf("msg = %v, want %v", msg, tt.wantMsg)
			}
			if tt.wantErrIs == nil && tt.wantErrText == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want an error")
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Errorf("err = %q, want one wrapping %q", err, tt.wantErrIs)
			}
			if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantErrText)
			}
			if _, ok := provider.AsRetryable(err); ok != tt.wantRetry {
				t.Errorf("provider.AsRetryable(%v) = %v, want %v", err, ok, tt.wantRetry)
			}
		})
	}
}

// TestClaudeCodeSucceedsDespiteInputWriteBrokenPipe proves runClaudeCodeTurn
// tolerates a broken-pipe/closed-pipe error writing (or closing) the
// turn's stdin input when the child has ALREADY produced — or is about
// to produce — a complete, valid result: a fast/trivial turn's child can
// legitimately finish and exit, closing its own end of the pipe, before
// this call finishes writing/closing its side. This reproduces a real CI
// failure (go test -race caught it; a non-race run is fast enough that
// the write usually wins the race instead) where a delegated turn failed
// with "writing turn input: write |1: broken pipe" even though the child
// had already produced a perfectly good result.
//
// fakeclaude's "fast_no_drain" mode reliably wins this race deliberately
// (see its own doc comment): it closes its own stdin immediately, before
// doing anything else, and runs at native speed since buildFakeClaude
// compiles it without -race, while harness's own -race-instrumented write
// path is comparatively slow. The turn must still succeed end to end:
// final message present, no error, and (since the fix's whole point is
// that a benign inputErr never surfaces once the turn has a usable
// result) OnTurnMetrics still fires exactly once.
func TestClaudeCodeSucceedsDespiteInputWriteBrokenPipe(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "fast_no_drain")

	var metrics []TurnMetrics
	s.cfg.OnTurnMetrics = func(m TurnMetrics) { metrics = append(metrics, m) }

	msg, err := s.Prompt(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if msg == nil || msg.Parts.Text() != "Done before you finished writing." {
		t.Fatalf("Prompt returned %+v, want fakeclaude's own canned final message", msg)
	}
	if len(metrics) != 1 {
		t.Errorf("OnTurnMetrics called %d times, want 1: %+v", len(metrics), metrics)
	}
}
