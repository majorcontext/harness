package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	// claudeCodeHistoryWatermark (recClaudeCodeHistoryWatermark) survives
	// the same reload, alongside the session id, at the exact value the
	// live session recorded — a process restart must not make the
	// directive re-fire on the very next turn just because the watermark
	// reset to 0.
	wantWatermark := len(s.History())
	if got := reloaded.claudeCodeHistoryWatermarkCount(); got != wantWatermark {
		t.Errorf("reloaded claudeCodeHistoryWatermarkCount() = %d, want %d (len(s.History()) before reload)", got, wantWatermark)
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
	if argvContains(invocations[2], "--append-system-prompt") {
		t.Errorf("post-reload invocation unexpectedly carries --append-system-prompt: %v", invocations[2])
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

// TestClaudeCodeDelegatedTurnDeliversAndCommitsTaskNotification is the
// regression test for the claude-code delegated lane's own bypass of the
// task-notification delivery/commit machinery — root-caused live as an
// infinite resume loop: a settled non-blocking child's notification never
// reached the model (runClaudeCodeTurn built its CLI turn purely from
// lastUserMessageText, never calling checkoutTaskNotificationsSegment the
// way the native loop body — engine.go's runAgenticLoop, via streamTurn —
// does on every call), so the model kept answering the bare trigger
// string with "No action taken," and the notification, never committed,
// stayed pending forever, so SessionManager.finalizeTurn's
// hasPendingTaskNotifications check kept re-firing triggerResumeLocked.
//
// Proves both halves of the fix, in one real Prompt call through the fake
// CLI: (a) the delivery half — the CLI's actual STDIN contains the
// checked-out notification's rendered content, not just the bare trigger
// string, so the model can act on the child's real result; (b) the commit
// half — a turn that then SUCCEEDS clears the pending set
// (hasPendingTaskNotifications false afterward, breaking the loop) and
// durably records delivery (a recTaskNotifyDelivered record on the
// session's own log, exactly like the native path's own commit).
func TestClaudeCodeDelegatedTurnDeliversAndCommitsTaskNotification(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	s.enqueueTaskNotification(taskNotification{
		ChildID: "ses_child1",
		Agent:   "explore",
		Status:  StatusDone,
		Result:  "found the bug in foo.go",
	})

	if _, err := s.Prompt(context.Background(), taskResumeTriggerText); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	stdinBytes, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("reading captured CLI stdin: %v", err)
	}
	stdin := string(stdinBytes)
	if !strings.Contains(stdin, "ses_child1") || !strings.Contains(stdin, "found the bug in foo.go") {
		t.Fatalf("CLI stdin missing the checked-out notification's content (only the bare trigger reached the model): %s", stdin)
	}

	if s.hasPendingTaskNotifications() {
		t.Error("notification still pending after a successful delegated turn — commitTaskNotifications did not run, so the resume loop is not broken")
	}

	data, err := os.ReadFile(filepath.Join(s.cfg.SessionDir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"task.notify_delivered"`) || !strings.Contains(log, `"child_id":"ses_child1"`) {
		t.Fatalf("log missing task.notify_delivered record: %s", log)
	}
}

// TestClaudeCodeDelegatedTurnRequeuesTaskNotificationOnFailure is the
// companion failure-path proof: when the delegated turn itself errors (the
// CLI's own "error" mode here — an is_error result, see
// TestClaudeCodeErrorResultReturnsError), the notification checked out for
// that failed attempt must be REQUEUED, not lost — mirroring the native
// loop body's own requeueTaskNotifications call on its streamTurnWithRetry
// error path (engine.go's runAgenticLoop).
func TestClaudeCodeDelegatedTurnRequeuesTaskNotificationOnFailure(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "error")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	s.enqueueTaskNotification(taskNotification{
		ChildID: "ses_child1",
		Status:  StatusDone,
		Result:  "found the bug in foo.go",
	})

	if _, err := s.Prompt(context.Background(), taskResumeTriggerText); err == nil {
		t.Fatal("Prompt returned no error for an is_error result")
	}

	// The failed attempt must actually have CHECKED OUT the notification
	// (folded it into the CLI input it sent) — otherwise "still pending"
	// below would trivially hold even with no checkout/requeue wiring at
	// all, proving nothing about the requeue path this test targets.
	stdinBytes, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("reading captured CLI stdin: %v", err)
	}
	if !strings.Contains(string(stdinBytes), "ses_child1") {
		t.Fatalf("failed attempt's own CLI stdin never carried the notification (checkout never ran, so requeue is not actually exercised): %s", stdinBytes)
	}

	if !s.hasPendingTaskNotifications() {
		t.Fatal("notification lost after a failed delegated turn — it was committed or dropped instead of requeued")
	}
	seg := s.checkoutTaskNotificationsSegment()
	if !strings.Contains(seg, "ses_child1") {
		t.Errorf("requeued notification missing from a later checkout: %q", seg)
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

// TestClaudeCodeMCPConfigFileIncludesHistoryServer proves a session
// configured with ClaudeCodeConfig.HTTPBaseURL (the harness HTTP server's
// own loopback base URL — see cmd/harness's serve wiring) gets a synthetic
// "harness-history" entry in --mcp-config naming this session's own
// /session/{id}/mcp endpoint, alongside whatever servers Config.MCP itself
// configured. This is the fix for the bug this change closes: without this
// entry, a delegated turn has no way to reach get_conversation_history at
// all, so a session that switches to claude-code mid-conversation (or on
// its first-ever claude-code turn) starts blind.
func TestClaudeCodeMCPConfigFileIncludesHistoryServer(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{
		"fs": {Command: []string{"mcp-fs"}},
	})
	s := NewSession(Config{
		Model: message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		MCP:   mgr,
		ClaudeCode: ClaudeCodeConfig{
			HTTPBaseURL:   "http://127.0.0.1:4096",
			HTTPAuthToken: "run-token-123",
		},
	})

	path, cleanup, err := s.claudeCodeMCPConfigFile()
	if err != nil {
		t.Fatalf("claudeCodeMCPConfigFile: %v", err)
	}
	defer cleanup()
	if path == "" {
		t.Fatal("claudeCodeMCPConfigFile returned an empty path with HTTPBaseURL set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading mcp-config file: %v", err)
	}
	var got claudeCodeMCPConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding mcp-config file: %v", err)
	}

	// The pre-existing configured server must still be present.
	if _, ok := got.MCPServers["fs"]; !ok {
		t.Errorf(`mcp-config missing "fs" server: %+v`, got.MCPServers)
	}

	hist, ok := got.MCPServers[claudeCodeToolsServerName]
	if !ok {
		t.Fatalf("mcp-config missing %q server: %+v", claudeCodeToolsServerName, got.MCPServers)
	}
	wantURL := "http://127.0.0.1:4096/session/" + s.ID + "/mcp"
	if hist.Type != "http" || hist.URL != wantURL {
		t.Errorf("history server = %+v, want an http server naming %s", hist, wantURL)
	}
	if hist.Headers["Authorization"] != "Bearer run-token-123" {
		t.Errorf("history server Headers = %+v, want Authorization: Bearer run-token-123", hist.Headers)
	}
}

// TestClaudeCodeMCPConfigFileHistoryServerWithNoConfiguredServers proves the
// synthetic history server entry is written even when Config.MCP has no
// servers of its own configured — HTTPBaseURL alone is enough to trigger a
// --mcp-config file, unlike len(servers)==0 with no HTTPBaseURL (see
// TestClaudeCodeMCPConfigFileEmptyWithNoServers, unchanged by this).
func TestClaudeCodeMCPConfigFileHistoryServerWithNoConfiguredServers(t *testing.T) {
	s := NewSession(Config{
		Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode: ClaudeCodeConfig{HTTPBaseURL: "http://127.0.0.1:4096"},
	})
	path, cleanup, err := s.claudeCodeMCPConfigFile()
	if err != nil {
		t.Fatalf("claudeCodeMCPConfigFile: %v", err)
	}
	defer cleanup()
	if path == "" {
		t.Fatal("claudeCodeMCPConfigFile returned an empty path with HTTPBaseURL set and no other servers")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading mcp-config file: %v", err)
	}
	var got claudeCodeMCPConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding mcp-config file: %v", err)
	}
	if len(got.MCPServers) != 1 {
		t.Fatalf("mcp-config servers = %+v, want exactly the history server", got.MCPServers)
	}
	if _, ok := got.MCPServers[claudeCodeToolsServerName]; !ok {
		t.Errorf("mcp-config missing %q server: %+v", claudeCodeToolsServerName, got.MCPServers)
	}
}

// TestClaudeCodeMCPConfigFileHistoryServerNoAuthHeaderWhenTokenEmpty proves
// an unset HTTPAuthToken (e.g. an Unauthenticated loopback-only serve, per
// server.Options.Unauthenticated) omits the Authorization header entirely
// rather than sending an empty bearer value.
func TestClaudeCodeMCPConfigFileHistoryServerNoAuthHeaderWhenTokenEmpty(t *testing.T) {
	s := NewSession(Config{
		Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode: ClaudeCodeConfig{HTTPBaseURL: "http://127.0.0.1:4096"},
	})
	path, cleanup, err := s.claudeCodeMCPConfigFile()
	if err != nil {
		t.Fatalf("claudeCodeMCPConfigFile: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading mcp-config file: %v", err)
	}
	var got claudeCodeMCPConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding mcp-config file: %v", err)
	}
	if _, ok := got.MCPServers[claudeCodeToolsServerName].Headers["Authorization"]; ok {
		t.Errorf("history server Headers = %+v, want no Authorization header", got.MCPServers[claudeCodeToolsServerName].Headers)
	}
}

// TestClaudeCodeHistoryDirectiveArgs is a pure-function table test of
// claudeCodeHistoryDirectiveArgs's own watermark-based gate: the directive
// fires exactly when history holds more than `watermark` messages before
// the pending trigger message, regardless of whether a CLI session id
// happens to be recorded — see the function's own doc comment for why this
// is deliberately NOT keyed on resumeID's emptiness.
func TestClaudeCodeHistoryDirectiveArgs(t *testing.T) {
	priorHistory := []message.Message{
		{ID: "1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "earlier question"}}},
		{ID: "2", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "earlier answer"}}},
		{ID: "3", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "the pending message"}}},
	}
	onlyPending := []message.Message{
		{ID: "3", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "the pending message"}}},
	}

	tests := []struct {
		name      string
		history   []message.Message
		watermark int
		want      []string
	}{
		{"first turn with prior history and a zero watermark gets the directive", priorHistory, 0, []string{"--append-system-prompt", claudeCodeHistoryDirective}},
		{"first turn with no prior history gets nothing", onlyPending, 0, nil},
		{"watermark caught up to prior history gets nothing (consecutive claude turns)", priorHistory, 2, nil},
		{"watermark ahead of prior history is treated the same as caught up", priorHistory, 5, nil},
		{"watermark behind prior history gets the directive even though it is non-zero (switch-back)", priorHistory, 1, []string{"--append-system-prompt", claudeCodeHistoryDirective}},
		{"a non-zero watermark equal to the (empty) prior history gets nothing", onlyPending, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeCodeHistoryDirectiveArgs(tt.history, tt.watermark)
			if !slicesEqual(got, tt.want) {
				t.Errorf("claudeCodeHistoryDirectiveArgs(len=%d, watermark=%d) = %v, want %v", len(tt.history), tt.watermark, got, tt.want)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClaudeCodeHistoryDirectiveForwardedOnFirstTurnWithPriorHistory drives
// a REAL delegated turn (via fakeclaude) on a session that already carries
// prior conversation history — as if it had just switched from a native
// provider to claude-code, or was reloaded mid-conversation — and asserts
// the child's argv carries --append-system-prompt with the catch-up
// directive text.
func TestClaudeCodeHistoryDirectiveForwardedOnFirstTurnWithPriorHistory(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	s.append(message.Message{
		ID:    "msg_prior_user",
		Role:  message.RoleUser,
		Parts: message.Parts{&message.Text{Text: "earlier question"}},
	})
	s.append(message.Message{
		ID:    "msg_prior_assistant",
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: "earlier answer"}},
	})

	if _, err := s.Prompt(context.Background(), "follow up"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if len(invocations) != 1 {
		t.Fatalf("invocations = %d, want 1: %+v", len(invocations), invocations)
	}
	got, ok := argvValueAfter(invocations[0], "--append-system-prompt")
	if !ok || got != claudeCodeHistoryDirective {
		t.Errorf("--append-system-prompt = %q, ok=%v, want %q", got, ok, claudeCodeHistoryDirective)
	}
}

// TestClaudeCodeHistoryDirectiveAbsentWithNoPriorHistory proves a session's
// very first message ever (nothing precedes the pending trigger message)
// gets no directive at all — there is no prior conversation to catch up
// on. TestClaudeCodeSessionIDResumedAcrossTurns already proves the second
// turn (resumeID != "") carries no --append-system-prompt; this covers the
// first turn's own "no prior history" branch explicitly.
func TestClaudeCodeHistoryDirectiveAbsentWithNoPriorHistory(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if argvContains(invocations[0], "--append-system-prompt") {
		t.Errorf("argv unexpectedly carries --append-system-prompt on a session's first-ever message: %v", invocations[0])
	}
}

// TestClaudeCodeHistoryDirectiveAbsentOnConsecutiveClaudeTurns proves two
// back-to-back claude-code turns with no intervening history growth get
// the directive only on the first one: the second turn's own watermark
// check sees priorCount == watermark (the first turn's own watermark
// update already accounted for everything now in history, including that
// turn's own answer), not priorCount > watermark.
func TestClaudeCodeHistoryDirectiveAbsentOnConsecutiveClaudeTurns(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	s.append(message.Message{
		ID:    "msg_prior_user",
		Role:  message.RoleUser,
		Parts: message.Parts{&message.Text{Text: "earlier question"}},
	})
	s.append(message.Message{
		ID:    "msg_prior_assistant",
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: "earlier answer"}},
	})

	if _, err := s.Prompt(context.Background(), "first claude turn"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if _, err := s.Prompt(context.Background(), "second claude turn"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}

	invocations := readInvocations(t, logPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations = %d, want 2: %+v", len(invocations), invocations)
	}
	if got, ok := argvValueAfter(invocations[0], "--append-system-prompt"); !ok || got != claudeCodeHistoryDirective {
		t.Errorf("first invocation --append-system-prompt = %q, ok=%v, want the directive (prior history existed)", got, ok)
	}
	if argvContains(invocations[1], "--append-system-prompt") {
		t.Errorf("second (consecutive claude-code) invocation unexpectedly carries --append-system-prompt: %v", invocations[1])
	}
	if resumeID, ok := argvValueAfter(invocations[1], "--resume"); !ok || resumeID != "fake-session-1" {
		t.Errorf("second invocation --resume = %q, ok=%v, want fake-session-1", resumeID, ok)
	}
}

// TestClaudeCodeHistoryDirectiveRefiresAfterSwitchBackFromNative is the
// regression test for the bug this watermark mechanism fixes: switching
// from claude-code to a native provider and back must re-fire the
// directive, even though claudeCodeCLISessionID is never cleared by a
// model switch (see its own doc comment) and so --resume still names the
// STALE CLI session that never saw the intervening native turn.
func TestClaudeCodeHistoryDirectiveRefiresAfterSwitchBackFromNative(t *testing.T) {
	bin := buildFakeClaude(t)
	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	logPath := filepath.Join(t.TempDir(), "invocations.jsonl")
	t.Setenv("FAKE_CLAUDE_LOG", logPath)

	nativeProv := &scriptedProvider{name: "native", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "native answer"}),
	}}
	s := NewSession(Config{
		SessionDir: t.TempDir(),
		Providers:  provider.Registry{nativeProv.name: nativeProv},
		Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode: ClaudeCodeConfig{BinaryPath: bin},
	})

	// Turn 1: the session's genuine first-ever message — no prior history,
	// so no directive, but the turn still records a CLI session id and a
	// watermark covering this turn's own two messages (the prompt and the
	// reply).
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("first (claude-code) Prompt: %v", err)
	}

	// Switch to native and take a turn: this grows s.History() past the
	// watermark the claude-code turn just recorded, WITHOUT touching
	// claudeCodeCLISessionID at all.
	s.SetModel(message.ModelRef{Provider: "native", Model: "m1"})
	if _, err := s.Prompt(context.Background(), "native turn"); err != nil {
		t.Fatalf("native Prompt: %v", err)
	}
	if s.claudeCodeSessionID() != "fake-session-1" {
		t.Fatalf("claudeCodeSessionID() = %q after a native turn, want it left untouched at fake-session-1", s.claudeCodeSessionID())
	}

	// Switch back to claude-code: --resume must still name the stale
	// session (it was never cleared), but the directive must ALSO fire,
	// since the native turn left history ahead of the watermark.
	s.SetModel(message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"})
	if _, err := s.Prompt(context.Background(), "back to claude"); err != nil {
		t.Fatalf("second (claude-code) Prompt: %v", err)
	}

	invocations := readInvocations(t, logPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations = %d, want 2 (the native turn never spawns claude): %+v", len(invocations), invocations)
	}
	if argvContains(invocations[0], "--append-system-prompt") {
		t.Errorf("first invocation unexpectedly carries --append-system-prompt (no prior history yet): %v", invocations[0])
	}
	if resumeID, ok := argvValueAfter(invocations[1], "--resume"); !ok || resumeID != "fake-session-1" {
		t.Errorf("second invocation --resume = %q, ok=%v, want the stale, never-cleared fake-session-1", resumeID, ok)
	}
	if got, ok := argvValueAfter(invocations[1], "--append-system-prompt"); !ok || got != claudeCodeHistoryDirective {
		t.Errorf("second invocation --append-system-prompt = %q, ok=%v, want the catch-up directive (native turn grew history past the watermark)", got, ok)
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

// TestClaudeCodeForwardSubagentTextAlwaysSet proves runClaudeCodeTurn always
// sends --forward-subagent-text. The CLI defaults this off, so a subagent's
// assistant/user frames would otherwise carry no parent_tool_use_id, and a
// consumer like the boxes console would render subagent work inline instead
// of nested under its spawning Task.
func TestClaudeCodeForwardSubagentTextAlwaysSet(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if !argvContains(invocations[0], "--forward-subagent-text") {
		t.Errorf("argv missing --forward-subagent-text: %v", invocations[0])
	}
}

// TestClaudeCodeThinkingDisplayAlwaysSummarized proves runClaudeCodeTurn
// always sends --thinking-display summarized. Opus 4.7 and later default
// thinking.display to "omitted", under which the API returns a thinking
// block whose `thinking` field is empty and whose signature is the only
// content: claudeCodeAssistantMessage then stores an empty
// message.Reasoning, consumeClaudeCodeStream emits no EventReasoningDelta
// for it (its `r.Text != ""` guard), and a consumer gets a durable part it
// cannot render and cannot align against the row that streamed the turn.
// The flag is the only channel that overrides that default — the
// showThinkingSummaries setting does not reach the request.
func TestClaudeCodeThinkingDisplayAlwaysSummarized(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	got, ok := argvValueAfter(invocations[0], "--thinking-display")
	if !ok {
		t.Fatalf("argv has no --thinking-display: %v", invocations[0])
	}
	if want := "summarized"; got != want {
		t.Errorf("--thinking-display = %q, want %q", got, want)
	}
}

// TestClaudeCodeExtraArgsCannotOverrideThinkingDisplay proves a config
// cannot quietly defeat the engine-owned --thinking-display. ExtraArgs are
// appended AFTER every engine flag and the CLI keeps the LAST value of a
// repeated option, so an ExtraArgs entry would win silently and restore the
// signature-only thinking blocks the flag exists to prevent. Both wire
// forms are rejected, matching the append-prompt conflict check.
func TestClaudeCodeExtraArgsCannotOverrideThinkingDisplay(t *testing.T) {
	for _, args := range [][]string{
		{"--thinking-display", "omitted"},
		{"--thinking-display=omitted"},
		{"--thinking-display", "summarized"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			s, _ := claudeCodeTestSession(t, "normal")
			s.cfg.ClaudeCode.ExtraArgs = args
			_, err := s.Prompt(context.Background(), "hi")
			if err == nil || !strings.Contains(err.Error(), "--thinking-display") {
				t.Fatalf("Prompt error = %v, want a --thinking-display conflict", err)
			}
		})
	}
}

// TestClaudeCodeDisallowsNativeSpawnTools proves runClaudeCodeTurn always
// sends --disallowedTools naming every native Claude Code tool that spawns
// a same-family subagent (Agent, Workflow). All subagent spawning in the
// claude-code lane must go through harness's own cross-family "task" tool
// (server/mcp_history.go, #223), never the CLI's native same-family
// equivalent, so those tools are blocked at the argv level rather than
// relying on the model to prefer one path over the other.
func TestClaudeCodeDisallowsNativeSpawnTools(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	got, ok := argvValueAfter(invocations[0], "--disallowedTools")
	if !ok {
		t.Fatalf("argv has no --disallowedTools: %v", invocations[0])
	}
	if want := "Agent,Workflow"; got != want {
		t.Errorf("--disallowedTools = %q, want %q", got, want)
	}
}

// TestClaudeCodeGroupsParallelToolCallsByUpstreamID proves ONE upstream API
// response becomes ONE harness message, even when the CLI streams its
// content blocks as several envelopes with the first tool's result
// interleaved between them.
//
// A real `claude` binary sends one envelope per content block, every
// envelope repeating the response's own message.id, and it runs the first
// tool before it sends the second tool_use. Appending one message per
// envelope therefore split a single response holding two parallel tool
// calls into two adjacent assistant messages, and the upstream id was
// decoded nowhere, so nothing downstream could put them back together.
//
// The result order is the other half of the contract: a tool_result must
// never be journaled ahead of the call it answers, so the interleaved
// result is held behind the assembled message and lands after it.
func TestClaudeCodeGroupsParallelToolCallsByUpstreamID(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "parallel_tools")
	if _, err := s.Prompt(context.Background(), "run both"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	hist := s.History()
	// user prompt, assistant(reasoning + BOTH tool calls), tool(alpha),
	// tool(beta), assistant(text) = 5 — not 6, the pre-fix shape with the
	// second tool call stranded in its own message.
	if len(hist) != 5 {
		var shape []string
		for _, m := range hist {
			kinds := make([]string, 0, len(m.Parts))
			for _, p := range m.Parts {
				kinds = append(kinds, fmt.Sprintf("%T", p))
			}
			shape = append(shape, string(m.Role)+"["+strings.Join(kinds, ",")+"]")
		}
		t.Fatalf("History() len = %d, want 5: %v", len(hist), shape)
	}

	asst := hist[1]
	if asst.Role != message.RoleAssistant {
		t.Fatalf("hist[1].Role = %q, want assistant", asst.Role)
	}
	var calls []string
	for _, p := range asst.Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			calls = append(calls, tc.CallID)
		}
	}
	if want := []string{"toolu_alpha", "toolu_beta"}; !slices.Equal(calls, want) {
		t.Errorf("assembled tool calls = %v, want %v (both blocks of one response, in order)", calls, want)
	}
	if _, ok := asst.Parts[0].(*message.Reasoning); !ok {
		t.Errorf("hist[1].Parts[0] = %T, want the response's own Reasoning first", asst.Parts[0])
	}

	// Both results follow the message that carries their calls, in arrival
	// order, and the NEXT response's own id ends the group rather than
	// joining it.
	for i, want := range []string{"toolu_alpha", "toolu_beta"} {
		m := hist[2+i]
		if m.Role != message.RoleTool {
			t.Fatalf("hist[%d].Role = %q, want tool", 2+i, m.Role)
		}
		tr, ok := m.Parts[0].(*message.ToolResult)
		if !ok || tr.CallID != want {
			t.Errorf("hist[%d].Parts[0] = %+v, want a ToolResult for %q", 2+i, m.Parts[0], want)
		}
	}
	if got := hist[4].Parts.Text(); got != "done" {
		t.Errorf("hist[4] text = %q, want the separate response %q", got, "done")
	}
}

// TestClaudeCodeBufferedReasoningStreamsOnce proves a buffered thinking
// block's delta is emitted exactly once.
//
// The buffering path streams a reasoning-only envelope's delta the moment
// it arrives, so live streaming is unaffected by the buffering. The merge
// then puts that same part at the FRONT of the next envelope's message, so
// a delta loop over the whole merged slice sends the thinking text a second
// time — and a consumer that appends deltas renders it twice until the
// EventMessage that follows replaces the row.
func TestClaudeCodeBufferedReasoningStreamsOnce(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking")

	var events []Event
	s.cfg.OnEvent = func(ev Event) { events = append(events, ev) }

	if _, err := s.Prompt(context.Background(), "think about it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var reasoningDeltas int
	for _, ev := range events {
		if ev.Type == EventReasoningDelta && ev.Text == "Let me reason about this." {
			reasoningDeltas++
		}
	}
	if reasoningDeltas != 1 {
		t.Errorf("reasoning.delta for the buffered thinking block emitted %d times, want exactly 1", reasoningDeltas)
	}
}

// TestClaudeCodeGroupingRespectsResponseAndThreadBoundaries proves the two
// boundaries the grouping must not cross, both found by review of #240.
//
// A SUBAGENT tool_result arrives on a different parent_tool_use_id while a
// main-thread response is still being assembled. It answers a call the
// subagent's own earlier response already journaled, so holding it behind
// the unrelated main-thread response would reorder it for nothing — and
// journaling it while that response is still buffered would put it AHEAD
// of a message the wire sent first. The open response closes instead.
//
// A THINKING-ONLY envelope then opens the NEXT response. It carries a
// different upstream id, but the reasoning-buffer path buffers and
// continues, so the id boundary has to be checked BEFORE that branch or
// the previous response stays open across a boundary it has nothing to do
// with.
func TestClaudeCodeGroupingRespectsResponseAndThreadBoundaries(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "parallel_tools_crossing")
	if _, err := s.Prompt(context.Background(), "cross the boundaries"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	hist := s.History()
	var shape []string
	for _, m := range hist {
		kinds := make([]string, 0, len(m.Parts))
		for _, p := range m.Parts {
			kinds = append(kinds, fmt.Sprintf("%T", p))
		}
		shape = append(shape, string(m.Role)+"["+strings.Join(kinds, ",")+"]")
	}
	// user, assistant[subagent tool_use], assistant[main tool_use],
	// tool[subagent result], assistant[reasoning + text].
	if len(hist) != 5 {
		t.Fatalf("History() len = %d, want 5: %v", len(hist), shape)
	}

	// Wire order survives: the subagent's result lands AFTER the
	// main-thread message the wire sent before it, never held behind it
	// and never journaled ahead of it.
	if hist[1].ParentToolUseID != "toolu_parent" {
		t.Errorf("hist[1].ParentToolUseID = %q, want the subagent's own thread: %v", hist[1].ParentToolUseID, shape)
	}
	if hist[2].ParentToolUseID != "" {
		t.Errorf("hist[2].ParentToolUseID = %q, want the main thread: %v", hist[2].ParentToolUseID, shape)
	}
	if hist[3].Role != message.RoleTool {
		t.Fatalf("hist[3].Role = %q, want the subagent tool result after the main-thread message: %v", hist[3].Role, shape)
	}
	tr, ok := hist[3].Parts[0].(*message.ToolResult)
	if !ok || tr.CallID != "toolu_child" {
		t.Errorf("hist[3].Parts[0] = %+v, want a ToolResult for toolu_child", hist[3].Parts[0])
	}

	// The next response's thinking block closed the previous one rather
	// than joining it: its reasoning belongs to the FINAL message.
	last := hist[4]
	if last.Role != message.RoleAssistant {
		t.Fatalf("hist[4].Role = %q, want assistant: %v", last.Role, shape)
	}
	if _, ok := last.Parts[0].(*message.Reasoning); !ok {
		t.Errorf("hist[4].Parts[0] = %T, want the second response's own Reasoning", last.Parts[0])
	}
	if got := last.Parts.Text(); got != "done" {
		t.Errorf("hist[4] text = %q, want %q", got, "done")
	}
	// Each earlier response kept exactly its own single tool call.
	for _, i := range []int{1, 2} {
		if len(hist[i].Parts) != 1 {
			t.Errorf("hist[%d].Parts = %+v, want exactly that response's one tool call", i, hist[i].Parts)
		}
	}
}

// TestClaudeCodeDeltasPrecedeTheirMessage proves every delta for a turn
// segment reaches the consumer BEFORE that segment's own EventMessage.
//
// This is the native lane's contract, and a consumer's fold is written
// against it: deltas grow an open row, and EventMessage replaces that row
// in place and adopts the durable id. Emitting EventMessage first inverted
// it, and the inversion duplicated the turn on screen — a consumer with no
// open row appended the message as a finished row, then the deltas that
// followed opened a SECOND row and rebuilt the same reasoning and text
// inside it. One model response, rendered twice, verbatim. It cleared only
// when a later envelope's message happened to overwrite the stranded row,
// so a turn ending on its own text left the duplicate up until reload.
func TestClaudeCodeDeltasPrecedeTheirMessage(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking")

	var events []Event
	s.cfg.OnEvent = func(ev Event) { events = append(events, ev) }

	if _, err := s.Prompt(context.Background(), "think about it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	firstMessage := -1
	for i, ev := range events {
		if ev.Type == EventMessage {
			firstMessage = i
			break
		}
	}
	if firstMessage < 0 {
		t.Fatalf("no EventMessage emitted: %+v", events)
	}
	// Both deltas belong to the message at firstMessage (one merged
	// reasoning+text turn — see TestClaudeCodeThinkingBlockDecodesToReasoningPart),
	// so both must sit ahead of it.
	for _, want := range []struct {
		kind string
		text string
	}{
		{EventReasoningDelta, "Let me reason about this."},
		{EventTextDelta, "Here is my answer."},
	} {
		at := -1
		for i, ev := range events {
			if ev.Type == want.kind && ev.Text == want.text {
				at = i
				break
			}
		}
		if at < 0 {
			t.Errorf("no %s carrying %q: %+v", want.kind, want.text, events)
			continue
		}
		if at > firstMessage {
			t.Errorf("%s for %q emitted at %d, AFTER its own EventMessage at %d; deltas must precede the message they build",
				want.kind, want.text, at, firstMessage)
		}
	}
}

// TestClaudeCodeThinkingBlockDecodesToReasoningPart proves a "thinking"
// content block — previously silently dropped (see claudeCodeContentBlock's
// switch in claudeCodeAssistantMessage) — decodes into a message.Reasoning
// part, is appended to history AS PART OF THE SAME MESSAGE as the text that
// follows it, and is emitted as EventReasoningDelta.
//
// The real `claude` binary streams the "thinking" block and the "text"
// block that completes the same turn segment as TWO separate stream-json
// "assistant" envelopes (see fakeclaude's "thinking" mode and
// consumeClaudeCodeStream's pendingReasoning doc comment). Before the fix
// this regresses, the engine appended one message.Message per envelope,
// so a single reasoning turn persisted as two adjacent assistant
// messages — one Reasoning-only, one Text-only — which a one-bubble-
// per-message console rendered as two separate "Agent" bubbles for one
// turn. The correct shape is ONE assistant message carrying both parts,
// in emission order.
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
	// user prompt, assistant(reasoning+text merged into ONE message) = 2
	// messages — NOT 3 (the pre-fix shape: user, assistant(thinking),
	// assistant(text)).
	if len(hist) != 2 {
		t.Fatalf("History() len = %d, want 2: %+v", len(hist), hist)
	}
	asst := hist[1]
	if asst.Role != message.RoleAssistant {
		t.Fatalf("hist[1].Role = %q, want assistant", asst.Role)
	}
	if len(asst.Parts) != 2 {
		t.Fatalf("hist[1].Parts = %+v, want exactly 2 parts (Reasoning then Text)", asst.Parts)
	}
	reasoning, ok := asst.Parts[0].(*message.Reasoning)
	if !ok || reasoning.Text != "Let me reason about this." {
		t.Fatalf("hist[1].Parts[0] = %+v, want a Reasoning(%q)", asst.Parts[0], "Let me reason about this.")
	}
	if len(reasoning.ProviderData) == 0 {
		t.Error("Reasoning.ProviderData is empty, want the thinking block's signature carried through")
	}
	text, ok := asst.Parts[1].(*message.Text)
	if !ok || text.Text != "Here is my answer." {
		t.Fatalf("hist[1].Parts[1] = %+v, want a Text(%q)", asst.Parts[1], "Here is my answer.")
	}

	var sawReasoningDelta, sawTextDelta bool
	for _, ev := range events {
		if ev.Type == EventReasoningDelta && ev.Text == "Let me reason about this." {
			sawReasoningDelta = true
		}
		if ev.Type == EventTextDelta && ev.Text == "Here is my answer." {
			sawTextDelta = true
		}
	}
	if !sawReasoningDelta {
		t.Error("no EventReasoningDelta for the thinking block")
	}
	if !sawTextDelta {
		t.Error("no EventTextDelta for the text block")
	}

	// Exactly one EventMessage for the whole merged turn (not one per
	// envelope): proves the reasoning-only envelope was buffered, not
	// flushed as its own message.
	var messageEvents int
	for _, ev := range events {
		if ev.Type == EventMessage {
			messageEvents++
		}
	}
	if messageEvents != 1 {
		t.Errorf("EventMessage count = %d, want 1 (one merged message for the turn)", messageEvents)
	}
}

// TestClaudeCodeReasoningMergeDoesNotOverMerge drives fakeclaude's
// "thinking_interleaved" sequence (text, then thinking, then the text that
// completes THAT thinking block's own turn segment) and proves the
// pendingReasoning merge in consumeClaudeCodeStream attaches ONLY forward:
// the independent leading text stays its own message, and only the
// reasoning + the text immediately after it merge into one. A merge rule
// that instead swept every adjacent assistant envelope together (or
// merged backward) would either over-merge this into a single message or
// drop the leading text — this proves neither happens.
func TestClaudeCodeReasoningMergeDoesNotOverMerge(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking_interleaved")

	msg, err := s.Prompt(context.Background(), "think about it, twice")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := msg.Parts.Text(); got != "And here is the rest." {
		t.Errorf("final message text = %q", got)
	}

	hist := s.History()
	// user prompt, assistant("First, a quick note."),
	// assistant(reasoning+"And here is the rest." merged) = 3 messages.
	if len(hist) != 3 {
		t.Fatalf("History() len = %d, want 3: %+v", len(hist), hist)
	}
	if hist[1].Role != message.RoleAssistant || len(hist[1].Parts) != 1 || hist[1].Parts.Text() != "First, a quick note." {
		t.Fatalf("hist[1] = %+v, want a standalone assistant Text(%q)", hist[1], "First, a quick note.")
	}
	asst := hist[2]
	if asst.Role != message.RoleAssistant || len(asst.Parts) != 2 {
		t.Fatalf("hist[2] = %+v, want an assistant message with exactly 2 parts", asst)
	}
	reasoning, ok := asst.Parts[0].(*message.Reasoning)
	if !ok || reasoning.Text != "Now let me reason about the rest." {
		t.Fatalf("hist[2].Parts[0] = %+v, want a Reasoning(%q)", asst.Parts[0], "Now let me reason about the rest.")
	}
	text, ok := asst.Parts[1].(*message.Text)
	if !ok || text.Text != "And here is the rest." {
		t.Fatalf("hist[2].Parts[1] = %+v, want a Text(%q)", asst.Parts[1], "And here is the rest.")
	}
}

// TestClaudeCodeReasoningMergeSurvivesRateLimitEvent drives fakeclaude's
// "thinking_ratelimit_text" sequence (thinking, then a rate_limit_event,
// then the text that completes the thinking block's own turn segment) and
// proves the pre-switch flush guard in consumeClaudeCodeStream does not
// treat a content-free "rate_limit_event" as ending the turn segment. A
// guard that flushes pendingReasoning on every non-"assistant" envelope
// re-splits the turn right here — exactly on subscription/usage sessions,
// where rate_limit_events are common (see rate_limit_event's own doc
// comment: "a long-running turn can see its own limits shift mid-turn").
func TestClaudeCodeReasoningMergeSurvivesRateLimitEvent(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking_ratelimit_text")

	msg, err := s.Prompt(context.Background(), "think about it, with a rate limit event")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := msg.Parts.Text(); got != "Here is my answer after the rate-limit event." {
		t.Errorf("final message text = %q", got)
	}

	hist := s.History()
	// user prompt, assistant(reasoning+text merged into ONE message) = 2
	// messages — NOT 3 (a rate_limit_event wrongly flushing the buffer
	// between the thinking and text envelopes).
	if len(hist) != 2 {
		t.Fatalf("History() len = %d, want 2: %+v", len(hist), hist)
	}
	asst := hist[1]
	if asst.Role != message.RoleAssistant || len(asst.Parts) != 2 {
		t.Fatalf("hist[1] = %+v, want an assistant message with exactly 2 parts", asst)
	}
	reasoning, ok := asst.Parts[0].(*message.Reasoning)
	if !ok || reasoning.Text != "Reasoning across a rate-limit event." {
		t.Fatalf("hist[1].Parts[0] = %+v, want a Reasoning(%q)", asst.Parts[0], "Reasoning across a rate-limit event.")
	}
	text, ok := asst.Parts[1].(*message.Text)
	if !ok || text.Text != "Here is my answer after the rate-limit event." {
		t.Fatalf("hist[1].Parts[1] = %+v, want a Text(%q)", asst.Parts[1], "Here is my answer after the rate-limit event.")
	}

	// The rate_limit_event itself must still be mapped onto
	// SubscriptionUsage — this test does not just prove the merge
	// survives it, but that the event was genuinely processed, not
	// silently skipped.
	usage := s.SubscriptionUsage()
	if usage == nil || len(usage.Windows) == 0 {
		t.Error("SubscriptionUsage() is empty, want the rate_limit_event mapped through")
	}
}

// TestClaudeCodeReasoningFlushesStandaloneOnCrash drives fakeclaude's
// "thinking_then_crash" sequence (a thinking block immediately followed by
// a nonzero exit with no "result" event at all) and proves
// flushPendingReasoning's post-loop flush: the buffered reasoning must
// survive as a standalone assistant message rather than being silently
// dropped when consumeClaudeCodeStream's scanner loop ends via EOF/crash
// before any envelope ever completes its turn segment.
func TestClaudeCodeReasoningFlushesStandaloneOnCrash(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking_then_crash")

	_, err := s.Prompt(context.Background(), "think about it, then crash")
	if err == nil {
		t.Fatal("Prompt returned no error for a child that crashed without a result event")
	}

	hist := s.History()
	// user prompt, assistant(reasoning only, flushed standalone) = 2
	// messages. NOT 1 (the reasoning silently dropped).
	if len(hist) != 2 {
		t.Fatalf("History() len = %d, want 2: %+v", len(hist), hist)
	}
	asst := hist[1]
	if asst.Role != message.RoleAssistant || len(asst.Parts) != 1 {
		t.Fatalf("hist[1] = %+v, want a standalone assistant message with exactly 1 part", asst)
	}
	reasoning, ok := asst.Parts[0].(*message.Reasoning)
	if !ok || reasoning.Text != "Reasoning right before a crash." {
		t.Fatalf("hist[1].Parts[0] = %+v, want a Reasoning(%q)", asst.Parts[0], "Reasoning right before a crash.")
	}
}

// TestClaudeCodeReasoningFlushesStandaloneAcrossSubagentBoundary drives
// fakeclaude's "thinking_then_subagent" sequence (a top-level thinking
// block, parent_tool_use_id "", immediately followed by an assistant
// envelope on a DIFFERENT parent_tool_use_id) and proves
// flushPendingReasoning's different-parent flush: the buffered reasoning
// must flush standalone rather than merge onto content from a different
// thread.
func TestClaudeCodeReasoningFlushesStandaloneAcrossSubagentBoundary(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "thinking_then_subagent")

	msg, err := s.Prompt(context.Background(), "think, then spawn a subagent")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := msg.Parts.Text(); got != "Working inside the subagent." {
		t.Errorf("final message text = %q", got)
	}

	hist := s.History()
	// user prompt, assistant(reasoning only, parent ""),
	// assistant("Working inside the subagent.", parent "toolu_parent") = 3
	// messages — NOT a 2-message merge across the parent boundary.
	if len(hist) != 3 {
		t.Fatalf("History() len = %d, want 3: %+v", len(hist), hist)
	}
	reasoningMsg := hist[1]
	if reasoningMsg.Role != message.RoleAssistant || len(reasoningMsg.Parts) != 1 || reasoningMsg.ParentToolUseID != "" {
		t.Fatalf("hist[1] = %+v, want a standalone top-level assistant Reasoning message", reasoningMsg)
	}
	reasoning, ok := reasoningMsg.Parts[0].(*message.Reasoning)
	if !ok || reasoning.Text != "Reasoning about which subagent to spawn." {
		t.Fatalf("hist[1].Parts[0] = %+v, want a Reasoning(%q)", reasoningMsg.Parts[0], "Reasoning about which subagent to spawn.")
	}
	subagentMsg := hist[2]
	if subagentMsg.Role != message.RoleAssistant || subagentMsg.ParentToolUseID != "toolu_parent" || subagentMsg.Parts.Text() != "Working inside the subagent." {
		t.Fatalf("hist[2] = %+v, want an assistant Text on parent toolu_parent", subagentMsg)
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

// TestClaudeCodeRateLimitEventWithNoOverageOmitsOverage proves
// mapClaudeCodeRateLimit leaves SubscriptionUsage.Overage nil — omitted on
// the wire, per message.SubscriptionUsage.Overage's own doc comment —
// when a rate_limit_event's own overage fields report no overage in play
// at all (overageStatus "", isUsingOverage false, overageResetsAt 0), the
// counterpart to TestClaudeCodeRateLimitEventCapturesSubscriptionUsage
// above, whose fixture's overageStatus "allowed" is itself a real (if
// benign) overage signal and so is a case that test cannot cover.
func TestClaudeCodeRateLimitEventWithNoOverageOmitsOverage(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "rate_limit_event_no_overage")

	if _, err := s.Prompt(context.Background(), "how am I doing on quota?"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	usage := s.SubscriptionUsage()
	if usage == nil {
		t.Fatal("SubscriptionUsage() after the turn = nil, want a captured snapshot")
	}
	if usage.Overage != nil {
		t.Errorf("Overage = %+v, want nil (no overage in play)", usage.Overage)
	}
}

// TestClaudeCodeSessionCostAccumulatesAcrossTurns proves a delegated
// session's message.SubscriptionUsage.SessionCostUSD sums the `claude`
// CLI's own per-turn total_cost_usd (fakeclaude's "normal" mode reports
// 0.0123 on every turn's "result" event) across successive turns, rather
// than reporting only the latest turn's figure — see
// Session.applyClaudeCodeUsage's own doc comment for why this is a
// cumulative session total, not a last-turn snapshot.
func TestClaudeCodeSessionCostAccumulatesAcrossTurns(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")

	if _, err := s.Prompt(context.Background(), "first turn"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	usage := s.SubscriptionUsage()
	if usage == nil || usage.SessionCostUSD == nil {
		t.Fatalf("SubscriptionUsage() after first turn = %+v, want a non-nil SessionCostUSD", usage)
	}
	if got, want := *usage.SessionCostUSD, 0.0123; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("SessionCostUSD after first turn = %v, want %v", got, want)
	}

	if _, err := s.Prompt(context.Background(), "second turn"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	usage = s.SubscriptionUsage()
	if usage == nil || usage.SessionCostUSD == nil {
		t.Fatalf("SubscriptionUsage() after second turn = %+v, want a non-nil SessionCostUSD", usage)
	}
	if got, want := *usage.SessionCostUSD, 0.0246; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("SessionCostUSD after second turn = %v, want %v (0.0123 summed twice)", got, want)
	}
}

// TestClaudeCodeSessionCostNilBeforeAnyTurn proves SessionCostUSD stays
// absent (nil, per message.SubscriptionUsage.SessionCostUSD's own doc
// comment) for a session that has not completed a "claude"-lane turn in
// this process yet — the counterpart to
// TestClaudeCodeSessionCostAccumulatesAcrossTurns, which proves the
// non-nil case.
func TestClaudeCodeSessionCostNilBeforeAnyTurn(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")

	if got := s.SubscriptionUsage(); got != nil {
		t.Fatalf("SubscriptionUsage() before any turn = %+v, want nil", got)
	}
}

// TestClaudeCodeSessionCostSurvivesReload proves SessionCostUSD is durable
// (recClaudeCodeUsage now carries the per-turn cost alongside token
// usage — see persistClaudeCodeUsage) — a process restart between two
// delegated turns must not silently reset the running dollar total to
// only the post-reload turn's own cost.
func TestClaudeCodeSessionCostSurvivesReload(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")

	if _, err := s.Prompt(context.Background(), "first turn"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}

	reloaded, err := LoadSession(Config{
		SessionDir: s.cfg.SessionDir,
		ClaudeCode: s.cfg.ClaudeCode,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	usage := reloaded.SubscriptionUsage()
	if usage == nil || usage.SessionCostUSD == nil {
		t.Fatalf("reloaded SubscriptionUsage() = %+v, want a non-nil SessionCostUSD", usage)
	}
	if got, want := *usage.SessionCostUSD, 0.0123; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("reloaded SessionCostUSD = %v, want %v", got, want)
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

// TestClaudeCodeReturnsOnResultDespiteLeakedDescendantFD reproduces the
// `claude --bg` wedge that consumeClaudeCodeStream's early return on
// "result" PLUS runClaudeCodeTurn's stderr handling both have to fix: a
// delegated turn must complete as soon as the direct `claude` child prints
// its terminal "result" event, even when some OTHER process still holds
// stdout OR stderr's pipe write end open. Before the stdout fix,
// consumeClaudeCodeStream kept calling scanner.Scan() looking for EOF,
// which only arrives once EVERY holder of the write end closes it. Before
// the stderr fix, that same class of bug survived one layer down: Cmd's
// own internal copying goroutine for a plain cmd.Stderr = io.Writer target
// (this file used cmd.Stderr = &capBuffer) makes cmd.Wait() block until
// STDERR's pipe sees EOF too — so a leaked descendant that inherits fd 2
// (a real dev server commonly inherits its parent's whole stdio, not just
// fd 1) could still wedge the turn through Wait() even once stdout no
// longer could. `claude --bg`'s own detached daemon (or a further child of
// its own) is designed to keep running after the direct child exits, and
// can inherit either or both fds — that wedges the whole harness turn
// forever, unkillably, even though the turn's own result was already in
// hand.
//
// fakeclaude's "bg_leak" mode stands in for exactly that: it emits a
// normal assistant/result pair, THEN spawns a grandchild that inherits
// BOTH its stdout and stderr and sleeps for an hour (never emitting
// anything, never exiting on its own within any sane test bound) before
// the direct child itself returns. The select below is this test's hard
// timeout guard: if a regression reintroduces either EOF-wait, this test
// fails loudly as a TIMEOUT rather than hanging the suite.
func TestClaudeCodeReturnsOnResultDespiteLeakedDescendantFD(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "bg_leak")
	pidFile := filepath.Join(t.TempDir(), "leaked.pid")
	t.Setenv("FAKE_CLAUDE_LEAK_PID_FILE", pidFile)

	var metrics []TurnMetrics
	s.cfg.OnTurnMetrics = func(m TurnMetrics) { metrics = append(metrics, m) }

	type outcome struct {
		msg *message.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := s.Prompt(context.Background(), "start a background job")
		done <- outcome{msg, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Prompt: %v", res.err)
		}
		if res.msg == nil || res.msg.Parts.Text() != "Starting a background job." {
			t.Fatalf("Prompt returned %+v, want fakeclaude's own canned final message", res.msg)
		}
	case <-time.After(10 * time.Second):
		// Cleaning up here too: if this branch ever fires, the leaked
		// grandchild would otherwise outlive the test.
		killLeakedFakeClaude(t, pidFile)
		t.Fatal("Prompt did not return within 10s of the child's \"result\" event — " +
			"consumeClaudeCodeStream is waiting for stdout EOF instead of returning " +
			"on \"result\" (the claude --bg wedge this test guards against)")
	}

	// The lingering grandchild's own stdout never got read as turn stream:
	// it emits nothing at all, so any observable content past "result"
	// (there is none here) would have to come from the direct child, and
	// finalMsg/History already prove that ended cleanly at "Starting a
	// background job." — see the assertions above and below.
	usage := s.Usage()
	if usage.InputTokens != 12 || usage.OutputTokens != 5 {
		t.Errorf("Usage() = %+v, want {12 5 0 0} (the result event's own usage)", usage)
	}
	if len(metrics) != 1 {
		t.Errorf("OnTurnMetrics called %d times, want 1: %+v", len(metrics), metrics)
	}
	hist := s.History()
	if len(hist) != 2 {
		t.Fatalf("History() len = %d, want 2 (user prompt + one assistant message): %+v", len(hist), hist)
	}

	// cmd.Wait() (runClaudeCodeTurn) only returns once the DIRECT
	// fakeclaude child has fully exited — which happens after its own
	// "bg_leak" case has already spawned the grandchild and written its
	// pid to pidFile, both of which run before that case's own return
	// statement. So by the time s.Prompt() above has returned, pidFile is
	// guaranteed to exist already: no polling or sleep needed to read it.
	killLeakedFakeClaude(t, pidFile)
}

// killLeakedFakeClaude reads the pid fakeclaude's "bg_leak" mode wrote to
// pidFile and kills that process, so this test does not leave its stand-in
// background daemon running after the test exits.
func killLeakedFakeClaude(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Logf("killLeakedFakeClaude: reading %s: %v (leaked grandchild not cleaned up)", pidFile, err)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Logf("killLeakedFakeClaude: parsing pid %q: %v", data, err)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Logf("killLeakedFakeClaude: FindProcess(%d): %v", pid, err)
		return
	}
	if err := proc.Kill(); err != nil {
		t.Logf("killLeakedFakeClaude: Kill(%d): %v (may have already exited)", pid, err)
	}
}

// TestClaudeCodeQueueInjectedMidTurnViaOpenStdin is the regression test for
// the live production bug reported as "queue doesn't seem to be working in
// opus subscription sessions" (box box_01m1f4g92bfb0a3e5863hqgbpw, session
// ses_01m1f4hbpee1nvwzam39b7fwm3): a prompt enqueued via POST
// .../sessions/{id}/send while a claude-code-lane turn was busy sat
// durably queued, undelivered, for the ENTIRE remainder of that turn —
// live-reproduced sitting queued 6+ minutes with the underlying `claude`
// turn still actively running — because runClaudeCodeTurn used to write
// its ONE input line and close stdin immediately, so a prompt queued after
// that close could never reach the already-running child; only the
// server's ordinary end-of-turn tail dispatch (a NEW turn) ever delivered
// it. A native-provider session on the same box, by contrast, delivers a
// mid-turn queued prompt within seconds via drainQueuedPromptsIntoHistory
// at the next tool-call boundary.
//
// This proves the fix directly against the mechanism: runClaudeCodeTurn's
// stdin-writer pump keeps the CLI child's stdin OPEN across the whole
// turn and writes a prompt queued via EnqueuePrompt to it as a SECOND
// stream-json input line, delivered to the SAME running child, before
// that child's own terminal "result" event — not after the process exits
// and a fresh one is dispatched.
//
// fakeclaude's "queue_injection" mode (testdata/fakeclaude/main.go) emits
// a "WAITING_FOR_QUEUE" marker message and then blocks reading a SECOND
// stdin line. This test waits for that marker via OnEvent — a
// deterministic, non-sleep synchronization point: by the time the driver
// has mapped that event, the child is already blocked in its second
// read — before calling EnqueuePrompt. If the fix were absent (a driver
// that still closes stdin right after its first write), fakeclaude's
// second read would see an immediate EOF (a closed pipe never blocks) and
// report "no second message received" instead of echoing the queued
// text — so an unfixed driver fails this test on WRONG CONTENT, not a
// timeout.
func TestClaudeCodeQueueInjectedMidTurnViaOpenStdin(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "queue_injection")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	waiting := make(chan struct{})
	var waitingOnce sync.Once
	s.cfg.OnEvent = func(ev Event) {
		if ev.Type == EventMessage && ev.Message != nil && ev.Message.Parts.Text() == "WAITING_FOR_QUEUE" {
			waitingOnce.Do(func() { close(waiting) })
		}
	}

	type outcome struct {
		msg *message.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := s.Prompt(context.Background(), "start")
		done <- outcome{msg, err}
	}()

	select {
	case res := <-done:
		t.Fatalf("Prompt returned (%+v, %v) before fakeclaude ever emitted WAITING_FOR_QUEUE", res.msg, res.err)
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("fakeclaude never emitted WAITING_FOR_QUEUE within 10s")
	}

	// The turn is now mid-flight — Prompt has NOT returned, and
	// fakeclaude is blocked on its own second stdin read. Enqueue while
	// busy, exactly like the live bug's POST /session/{id}/send arriving
	// while a claude-code turn is running.
	if _, _, err := s.EnqueuePrompt("QUEUE-MARKER: please continue", ""); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	var res outcome
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Prompt did not return within 10s of EnqueuePrompt — the queued prompt was never " +
			"delivered to the running child (fakeclaude stayed blocked on its second stdin read)")
	}
	if res.err != nil {
		t.Fatalf("Prompt: %v", res.err)
	}
	if res.msg == nil || !strings.Contains(res.msg.Parts.Text(), "QUEUE-MARKER: please continue") {
		t.Fatalf("Prompt's final message = %+v, want it to echo the mid-turn queued prompt's text "+
			"(proves fakeclaude's SAME process received line two)", res.msg)
	}

	// The queue itself must be empty afterward: delivered, not stranded.
	if q := s.QueuedPrompts(); len(q) != 0 {
		t.Errorf("QueuedPrompts() after delivery = %+v, want empty", q)
	}

	// The injected prompt must be visible in the session transcript —
	// mirrors the native path's own drainQueuedPromptsIntoHistory, and is
	// what lets the console render the delivery.
	found := false
	for _, m := range s.History() {
		if m.Role == message.RoleUser && strings.Contains(m.Parts.Text(), "QUEUE-MARKER: please continue") {
			found = true
		}
	}
	if !found {
		t.Error("session history has no user message carrying the queued prompt's text — mid-turn delivery did not append into the transcript")
	}

	// The actual bytes reached the CLI's stdin as a genuine SECOND line,
	// not just harness-side bookkeeping.
	stdinBytes, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("reading captured CLI stdin: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(stdinBytes), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("CLI stdin carried %d lines, want 2 (initial turn text + the mid-turn injected prompt): %q", len(lines), string(stdinBytes))
	}
	if !strings.Contains(lines[1], "QUEUE-MARKER: please continue") {
		t.Errorf("CLI stdin's second line = %q, want it to carry the queued prompt's text", lines[1])
	}
}

// TestClaudeCodeMidTurnInjectionWriteFailureDoesNotStrandWatermark is the
// regression test for an adversarial-review finding on #231 (PR
// majorcontext/harness#231, commit 7918b6d): a mid-turn queued prompt
// whose stdin write to the running `claude` child FAILS (the child's read
// end closes right as the injection lands — a `claude --bg` turn, or any
// child racing its own exit against the wake) was silently and
// PERMANENTLY lost, contradicting the pump's own "a delay, never a loss"
// doc comment (runClaudeCodeTurn, engine/claude_code_backend.go).
//
// Root cause: the pump appends the injected block into session history
// BEFORE attempting the write (so a failed write still leaves the block
// durably in s.history — correct, honest bookkeeping), but
// runClaudeCodeTurn's end-of-turn watermark recording
// (recordClaudeCodeHistoryWatermark(len(s.History()))) ran unconditionally
// whenever the child started, counting that now-durable-but-undelivered
// message as if the CLI's own resumed session already had it. The NEXT
// claude-code turn's claudeCodeHistoryDirectiveArgs then computes
// priorCount == watermark (not priorCount > watermark), so it never fires
// the --append-system-prompt get_conversation_history re-pull that would
// have been the injected prompt's last chance to actually reach the
// model — silently dropped, though the transcript shows it as delivered.
//
// This test proves the fix: after a mid-turn injection's write fails, the
// recorded watermark must be capped BELOW the failed message's own
// position, so a LATER claude-code turn's own directive check sees
// priorCount > watermark and re-fires the history pull — the lost
// prompt's one remaining path back to the model.
func TestClaudeCodeMidTurnInjectionWriteFailureDoesNotStrandWatermark(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "queue_injection_broken_pipe")

	ready := make(chan struct{})
	var readyOnce sync.Once
	s.cfg.OnEvent = func(ev Event) {
		if ev.Type == EventMessage && ev.Message != nil && ev.Message.Parts.Text() == "STDIN_CLOSED_READY" {
			readyOnce.Do(func() { close(ready) })
		}
	}

	type outcome struct {
		msg *message.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := s.Prompt(context.Background(), "start")
		done <- outcome{msg, err}
	}()

	select {
	case res := <-done:
		t.Fatalf("Prompt returned (%+v, %v) before fakeclaude ever emitted STDIN_CLOSED_READY", res.msg, res.err)
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("fakeclaude never emitted STDIN_CLOSED_READY within 10s")
	}

	// The turn is now mid-flight with fakeclaude's own stdin read end
	// already closed. Enqueue now: the pump's injection write is
	// guaranteed to land on the closed pipe and fail.
	if _, _, err := s.EnqueuePrompt("LOST-IF-BUGGY: please handle this", ""); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	var res outcome
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("first Prompt did not return within 10s")
	}
	if res.err != nil {
		t.Fatalf("first Prompt: %v", res.err)
	}

	// The queue itself is still empty (dequeued, not stranded there) and
	// the prompt's text is still honestly present in history — this test
	// is about the WATERMARK, not about re-queueing or hiding the attempt.
	if q := s.QueuedPrompts(); len(q) != 0 {
		t.Errorf("QueuedPrompts() after the failed injection = %+v, want empty (dequeued once, not requeued)", q)
	}
	found := false
	for _, m := range s.History() {
		if m.Role == message.RoleUser && strings.Contains(m.Parts.Text(), "LOST-IF-BUGGY: please handle this") {
			found = true
		}
	}
	if !found {
		t.Error("session history has no user message carrying the failed injection's text")
	}

	// The second claude-code turn must re-fire the history-directive
	// re-pull: proof the watermark did not silently cover the lost
	// message. Without the fix, this argv carries no
	// --append-system-prompt at all (mirrors
	// TestClaudeCodeHistoryDirectiveAbsentOnConsecutiveClaudeTurns'
	// negative case) — the model's only remaining path to the queued
	// prompt.
	if _, err := s.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	invocations := readInvocations(t, logPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations = %d, want 2: %+v", len(invocations), invocations)
	}
	got, ok := argvValueAfter(invocations[1], "--append-system-prompt")
	if !ok || got != claudeCodeHistoryDirective {
		t.Fatalf("second invocation --append-system-prompt = %q, ok=%v, want the history directive %q -- "+
			"the failed mid-turn injection was silently stranded (watermark advanced past it)", got, ok, claudeCodeHistoryDirective)
	}
}

// TestClaudeCodeStopRetiresPumpBlockedInStdinWrite is the regression test
// for the second adversarial-review finding on #231 (PR
// majorcontext/harness#231, commit 7918b6d): a stop landing (the child's
// own terminal "result" event arrives, closing stopPump) while the
// stdin-writer pump is BLOCKED inside its own stdin.Write call must still
// retire the pump promptly — not wedge <-pumpDone (and so the whole
// runClaudeCodeTurn call) until ctx cancellation, the same `claude --bg`
// class of wedge the StdoutPipe/StderrPipe handling elsewhere in this file
// already exists to prevent, just one pipe over.
//
// fakeclaude's "queue_injection_blocked_write" mode never reads stdin
// again after its own first marker message, so the driver's own mid-turn
// injection write — many times larger than any real OS pipe buffer, so it
// cannot possibly complete in one buffered chunk — blocks inside the
// write(2) syscall with nothing on the other end ever draining it. The
// select below is this test's OWN hard-timeout guard: with the pre-fix
// code (stdin closed only from inside the pump's own select branches,
// never reachable while blocked in a live Write call), this test hangs
// until it times out; with the fix (the outer goroutine closes stdin
// itself, unconditionally, right after close(stopPump), before ever
// waiting on pumpDone), Go's os.File.Close interrupts the pump's blocked
// Write immediately and the turn completes normally.
func TestClaudeCodeStopRetiresPumpBlockedInStdinWrite(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "queue_injection_blocked_write")
	pidFile := filepath.Join(t.TempDir(), "leaked.pid")
	t.Setenv("FAKE_CLAUDE_LEAK_PID_FILE", pidFile)

	waiting := make(chan struct{})
	var waitingOnce sync.Once
	s.cfg.OnEvent = func(ev Event) {
		if ev.Type == EventMessage && ev.Message != nil && ev.Message.Parts.Text() == "WAITING_FOR_QUEUE" {
			waitingOnce.Do(func() { close(waiting) })
		}
	}

	type outcome struct {
		msg *message.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := s.Prompt(context.Background(), "start")
		done <- outcome{msg, err}
	}()

	select {
	case res := <-done:
		t.Fatalf("Prompt returned (%+v, %v) before fakeclaude ever emitted WAITING_FOR_QUEUE", res.msg, res.err)
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("fakeclaude never emitted WAITING_FOR_QUEUE within 10s")
	}

	// Many times larger than any real pipe buffer (typically 16-64KiB) --
	// the driver's own write of this, plus its JSON/OPERATOR-MESSAGES
	// framing overhead, cannot complete in one buffered chunk while
	// fakeclaude never reads any of it.
	huge := strings.Repeat("X", 8*1024*1024)
	if _, _, err := s.EnqueuePrompt(huge, ""); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			killLeakedFakeClaude(t, pidFile)
			t.Fatalf("Prompt: %v", res.err)
		}
	case <-time.After(15 * time.Second):
		// Cleaning up here too: if this branch ever fires, the leaked
		// grandchild would otherwise outlive the test.
		killLeakedFakeClaude(t, pidFile)
		t.Fatal("Prompt did not return within 15s of the child's \"result\" event — " +
			"the stdin-writer pump is wedged inside a blocked Write, never retired by the stop " +
			"(the claude --bg-class stdin wedge this test guards against)")
	}
	killLeakedFakeClaude(t, pidFile)
}

// TestClaudeCodeQueueInjectedMidTurnCarriesAttachments is the regression for
// a mid-turn queued prompt LOSING its attachments in the claude-code lane.
//
// A prompt that arrives while a turn is running is queued with its blobs
// (QueuedPrompt.Blobs), and the native loop's own drain delivers them:
// drainQueuedPromptsIntoHistory (engine.go) builds its appended message with
// promptParts(block, queuedBlobs(queued)), so the bytes ride as Blob parts.
// This lane's drain appended a bare Text part and wrote stdin with no blobs,
// so an image or PDF sent mid-turn to a claude-code session was silently
// dropped on both halves at once — the running child never saw it, AND the
// durable history had no record of it for the next turn's --resume to
// recover. "A delay, never a loss" did not hold for the bytes.
//
// Both halves are asserted, because either alone would have passed while the
// other still dropped the file.
func TestClaudeCodeQueueInjectedMidTurnCarriesAttachments(t *testing.T) {
	// Reuses fakeclaude's "queue_injection" mode — the one that blocks for a
	// SECOND stdin line mid-turn — because that is exactly the delivery this
	// regression is about; only what is ENQUEUED differs from the sibling
	// test above.
	s, _ := claudeCodeTestSession(t, "queue_injection")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	waiting := make(chan struct{})
	var waitingOnce sync.Once
	s.cfg.OnEvent = func(ev Event) {
		if ev.Type == EventMessage && ev.Message != nil && ev.Message.Parts.Text() == "WAITING_FOR_QUEUE" {
			waitingOnce.Do(func() { close(waiting) })
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(context.Background(), "start")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Prompt returned (%v) before fakeclaude emitted WAITING_FOR_QUEUE", err)
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("fakeclaude never emitted WAITING_FOR_QUEUE within 10s")
	}

	// A 1x1 PNG, the same shape server/prompt_parts.go admits.
	png := []byte("\x89PNG\r\n\x1a\nQUEUED-PNG-BYTES")
	if _, _, err := s.EnqueuePrompt("look at this", "", &message.Blob{
		MediaType: "image/png",
		Data:      png,
	}); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Prompt did not return within 10s of EnqueuePrompt")
	}

	// Half one: the running child actually received the bytes. The stdin
	// line must carry an image content block, not just the prompt's text.
	stdinBytes, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("reading captured CLI stdin: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(stdinBytes), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("CLI stdin carried %d lines, want 2: %q", len(lines), string(stdinBytes))
	}
	injected := lines[1]
	if !strings.Contains(injected, `"type":"image"`) {
		t.Errorf("CLI stdin's injected line carried no image block, so the queued attachment never "+
			"reached the running child: %q", injected)
	}
	if !strings.Contains(injected, base64.StdEncoding.EncodeToString(png)) {
		t.Errorf("CLI stdin's injected line did not carry the queued PNG's own bytes: %q", injected)
	}

	// Half two: the durable history records the attachment too, so the next
	// turn's --resume recovery has something to recover.
	var blobs int
	for _, m := range s.History() {
		if m.Role != message.RoleUser {
			continue
		}
		for _, p := range m.Parts {
			if b, ok := p.(*message.Blob); ok && bytes.Equal(b.Data, png) {
				blobs++
			}
		}
	}
	if blobs != 1 {
		t.Errorf("history carried %d copies of the queued PNG, want exactly 1 (the mid-turn drain's "+
			"appended message must hold it as a Blob part, exactly once)", blobs)
	}
}
