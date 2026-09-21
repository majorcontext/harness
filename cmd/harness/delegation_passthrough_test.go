package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/engine"
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

// TestRunCmdUnknownCommandFreshNativeBuildsNoSession is the regression test
// for the finding pinned to f6da901's deferral: a FRESH run (neither
// -resume nor -continue) has no persisted session to disagree with the
// configured model, so a native default must refuse an unknown /name
// immediately, before resolveSession ever builds and prewarms a session for
// a run that was always going to be refused (5a5105b's original guard).
//
// The refusal error alone cannot prove that: it is identical in the broken
// version (defer past resolveSession unconditionally, then refuse in the
// dispatch switch once s.ClaudeCodeDelegated() is false) and the fixed one
// (refuse before resolveSession runs at all). So this configures a plugin
// naming a binary that does not exist: buildPluginHost's manifest probe
// (cmd/harness/plugins.go, via plugin.ResolveExecutable) fails loudly and
// SYNCHRONOUSLY the moment it runs, and it sits textually between the
// fresh-run delegation check and resolveSession in runCmd. If the deferral
// ever widens back to "always defer," runCmd reaches buildPluginHost and
// returns ITS error instead of the unknown-command one — a different,
// checkable failure the plain error-message assertion by itself would miss.
func TestRunCmdUnknownCommandFreshNativeBuildsNoSession(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	sessDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessDir)

	configPath := filepath.Join(workDir, "config.json")
	body := `{"plugins": [{"name": "canary", "command": ["no-such-plugin-binary-xyz"]}]}`
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", configPath)

	var runErr error
	captureStdout(t, func() {
		runErr = runCmd([]string{"-p", "/nope"})
	})
	if runErr == nil {
		t.Fatal("runCmd returned nil for an unknown command on a fresh, native session")
	}
	if !strings.Contains(runErr.Error(), `unknown command "nope"`) {
		t.Errorf(`error = %q, want the unknown-command refusal — got a different error, which means runCmd built a session (and its plugin host) before refusing`, runErr)
	}
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

// TestRunCmdUnknownCommandResumedDelegatedReachesPromptUnchanged names the
// failure the other three tests in this file cannot: a RESUMED session
// whose persisted model is claude-code, resumed under a config whose
// DEFAULT model is native, must still route an unknown /name (here /cost)
// to the CLI. runCmd's dispatch checks s.ClaudeCodeDelegated() — the
// LOADED session's current model (engine.LoadSession: "the last model
// record wins; Config.Model otherwise") — strictly after resolveSession
// returns s, precisely so this case works. Reading the CONFIGURED model ref
// instead (the "obvious" simplification, decided before resolveSession
// even runs) would see the native default here and refuse /cost with
// *command.UnknownCommandError, never reaching Session.Prompt. See
// runCmd's own comment just above its unknownCmd/resolveSession ordering
// for why the check is deferred.
//
// -resume and -continue converge on the same load path: resolveSession
// (main.go) picks `id` differently (the -r value directly, or the most
// recent entry from engine.ListSessions for -c) but both then call the
// identical engine.LoadSession(cfg, id) — the one place a persisted model
// record is restored. One test against -resume exercises that shared path;
// a second against -continue would only re-prove the id-selection branch,
// not this test's mechanism.
func TestRunCmdUnknownCommandResumedDelegatedReachesPromptUnchanged(t *testing.T) {
	bin := buildFakeClaudeForHarness(t)
	workDir := t.TempDir()
	home := t.TempDir()
	sessDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessDir)

	// First run: config whose DEFAULT model is claude-code, so the fresh
	// session it creates persists a claude-code model record. An ordinary
	// prompt (not a slash command) is enough to commit that turn to disk.
	writeDelegatedConfig(t, workDir, bin)
	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", filepath.Join(t.TempDir(), "seed-stdin.log"))
	var seedErr error
	captureStdout(t, func() {
		seedErr = runCmd([]string{"-p", "hello"})
	})
	if seedErr != nil {
		t.Fatalf("seeding delegated session: %v", seedErr)
	}
	infos, err := engine.ListSessions(sessDir)
	if err != nil {
		t.Fatalf("engine.ListSessions: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("engine.ListSessions returned %d sessions, want 1", len(infos))
	}
	id := infos[0].ID

	// Second run: a DIFFERENT config, same claude-code provider entry (so
	// the restored model is still routable) but no top-level "model" —
	// resolves to config.DefaultModel, a native ref. This is what
	// distinguishes "delegation read from the loaded session" from
	// "delegation read from config": if the latter drove the check, this
	// run would see a native default and refuse.
	nativeConfigPath := filepath.Join(workDir, "native-default-config.json")
	nativeConfigBody := `{"snapshot_every_records": 0, "providers": {"claude-code": {"type": "claude-code-cli", "binary_path": ` + fmt.Sprintf("%q", bin) + `}}}`
	if err := os.WriteFile(nativeConfigPath, []byte(nativeConfigBody), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", nativeConfigPath)

	stdinLog := filepath.Join(t.TempDir(), "resume-stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	var runErr error
	captureStdout(t, func() {
		runErr = runCmd([]string{"-p", "/cost", "-resume", id})
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
}
