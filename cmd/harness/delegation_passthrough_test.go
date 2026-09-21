package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeClaudeBinForHarness is the compiled engine/testdata/fakeclaude
// stand-in (see engine/claude_code_backend_test.go's buildFakeClaude for
// the original, and server/claude_code_model_switch_test.go's own copy for
// precedent), rebuilt here because it is a package-private helper of the
// engine test binary and cmd/harness needs its own copy to drive a REAL
// delegated `harness run`, not just an engine.Session in isolation.
var (
	fakeClaudeBinForHarness     string
	fakeClaudeBinForHarnessOnce sync.Once
	fakeClaudeBinForHarnessErr  error
)

func buildFakeClaudeForHarness(t *testing.T) string {
	t.Helper()
	fakeClaudeBinForHarnessOnce.Do(func() {
		dir, err := os.MkdirTemp("", "harness-fakeclaude-cmd")
		if err != nil {
			fakeClaudeBinForHarnessErr = err
			return
		}
		bin := filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", bin, "../../engine/testdata/fakeclaude")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeBinForHarnessErr = fmt.Errorf("go build fakeclaude: %v\n%s", err, out)
			return
		}
		fakeClaudeBinForHarness = bin
	})
	if fakeClaudeBinForHarnessErr != nil {
		t.Fatalf("buildFakeClaudeForHarness: %v", fakeClaudeBinForHarnessErr)
	}
	return fakeClaudeBinForHarness
}

// writeDelegatedConfig writes a HARNESS_CONFIG file whose default model
// routes to the claude-code delegated backend, with binary_path set to
// bin (the fakeclaude stand-in), and points HARNESS_CONFIG at it.
//
// snapshot_every_records is explicitly 0 (off): Session.Prompt defers
// snapshotOnIdle unconditionally (engine/engine.go), which starts an
// UNJOINED background goroutine (engine/snapshot.go's startSnapshotLocked)
// the instant any record has been written — cmd/harness has no seam to
// wait for it. Left at its product default, that write races this test's
// own t.TempDir() cleanup and intermittently fails it with "directory not
// empty" (reproduced under -race; see the delegation-passthrough report
// for the isolated repro). Disabling it removes the race outright without
// touching engine/ or cmd/harness production code for a test-only
// concern.
func writeDelegatedConfig(t *testing.T, dir, bin string) {
	t.Helper()
	configPath := filepath.Join(dir, "config.json")
	body := `{"model": "claude-code/sonnet", "snapshot_every_records": 0, "providers": {"claude-code": {"type": "claude-code-cli", "binary_path": ` + fmt.Sprintf("%q", bin) + `}}}`
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", configPath)
}

// TestRunCmdUnknownCommandNonDelegatedStillErrors red-verifies that the
// delegation exception does not widen the general case: on a session that
// does NOT route to the Claude Code CLI (here, the default anthropic
// model — no claude-code provider configured at all), an unknown /name
// must still refuse exactly as before, and no turn may reach the model.
// The empty session directory is the proof that dispatch never happened,
// not just that runCmd returned an error — mirrors
// TestRunCmdRefusedCommandNeverLoadsConfig's own use of that signal.
func TestRunCmdUnknownCommandNonDelegatedStillErrors(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	sessDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_CONFIG", "")
	t.Setenv("HARNESS_SESSION_DIR", sessDir)

	var runErr error
	captureStdout(t, func() {
		runErr = runCmd([]string{"-p", "/nope"})
	})
	if runErr == nil {
		t.Fatal("runCmd returned nil for an unknown command on a non-delegated session")
	}
	if !strings.Contains(runErr.Error(), `unknown command "nope"`) {
		t.Errorf("error = %q, want it to name the unknown command", runErr)
	}
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		t.Fatalf("reading session dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("session dir has %d entries after a refused unknown command, want 0 (no turn should have started)", len(entries))
	}
}

// TestRunCmdUnknownCommandDelegatedReachesPromptUnchanged proves the fix:
// on a session delegated to the Claude Code CLI, an unknown /name is sent
// to the CLI as an ordinary prompt, with the ORIGINAL line — "/cost", not
// "cost" (Resolve set no Text for an unknown command) and not "//cost"
// (the escape a caller might otherwise reach for). The fakeclaude
// stand-in records its own stdin verbatim (FAKE_CLAUDE_STDIN_LOG), so this
// inspects the exact bytes the delegated CLI child actually received.
func TestRunCmdUnknownCommandDelegatedReachesPromptUnchanged(t *testing.T) {
	bin := buildFakeClaudeForHarness(t)
	workDir := t.TempDir()
	home := t.TempDir()
	sessDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessDir)
	writeDelegatedConfig(t, workDir, bin)

	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	var runErr error
	captureStdout(t, func() {
		runErr = runCmd([]string{"-p", "/cost"})
	})
	if runErr != nil {
		t.Fatalf("runCmd: %v", runErr)
	}

	stdinBytes, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("reading captured CLI stdin: %v", err)
	}
	stdin := string(stdinBytes)
	if !strings.Contains(stdin, `"content":"/cost"`) {
		t.Errorf("CLI stdin = %q, want it to contain the original line %q unchanged", stdin, "/cost")
	}
	if strings.Contains(stdin, `"content":"cost"`) {
		t.Error("CLI stdin sent the command name with its leading slash stripped")
	}
	if strings.Contains(stdin, `"content":"//cost"`) {
		t.Error("CLI stdin sent the escaped literal //cost instead of the original /cost")
	}
}

// TestRunCmdSurplusArgOnDelegatedSessionStillRefuses is the regression
// this fix is most likely to introduce: harness owns /clear (KindFrontend,
// no Args), so a surplus-argument line must keep refusing even on a
// delegated session — delegation rescues only an UNKNOWN name, never a
// harness command harness itself refuses. The absent fakeclaude
// invocation log is the proof the CLI child was never even spawned.
func TestRunCmdSurplusArgOnDelegatedSessionStillRefuses(t *testing.T) {
	bin := buildFakeClaudeForHarness(t)
	workDir := t.TempDir()
	home := t.TempDir()
	sessDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessDir)
	writeDelegatedConfig(t, workDir, bin)

	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	invocationLog := filepath.Join(t.TempDir(), "invocations.jsonl")
	t.Setenv("FAKE_CLAUDE_LOG", invocationLog)

	var runErr error
	captureStdout(t, func() {
		runErr = runCmd([]string{"-p", "/clear extra"})
	})
	if runErr == nil {
		t.Fatal("runCmd returned nil for /clear with a surplus argument on a delegated session")
	}
	if !strings.Contains(runErr.Error(), "takes no arguments") {
		t.Errorf("error = %q, want it to say /clear takes no arguments", runErr)
	}
	if _, err := os.Stat(invocationLog); err == nil {
		t.Error("fakeclaude invocation log exists — the CLI child was spawned for a command harness itself must refuse")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat invocation log: %v", err)
	}
}
