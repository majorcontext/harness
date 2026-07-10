package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// This file tests the fix for a "grandchild holds the output pipe" hang
// (see the issue this closes): cmd.Stdout/Stderr being a non-*os.File
// writer forces os/exec to run its internal pipe copy in a goroutine, and
// Wait() blocks on that goroutine seeing pipe EOF — not on the direct `sh`
// child exiting. A backgrounded grandchild that inherited the write end of
// that pipe keeps it open indefinitely, so both a BashTimeout and an
// engine-level abort (context cancellation) are powerless: they can kill
// `sh`, but Wait() still waits forever for EOF that will never come.
//
// These tests exercise bashTool with real `sh` subprocesses — deliberately:
// the subprocess machinery itself (pipes, process groups, cmd.Cancel,
// cmd.WaitDelay) is exactly what's under test, the sanctioned exception to
// "never spawn real subprocess fixtures" in AGENTS.md. Because real OS
// processes run in real wall-clock time, none of this can run inside a
// testing/synctest bubble (fake time does not govern subprocess scheduling);
// deadline-bound polling loops (never a raw synchronization time.Sleep) are
// used instead, matching the e2e cross-process-wait convention.

// setBashWaitDelay overrides the package-level bashWaitDelay for the
// duration of a test, restoring the original in t.Cleanup. Keeping it small
// keeps these tests' wall-clock cost small without touching the value used
// in production.
func setBashWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := bashWaitDelay
	bashWaitDelay = d
	t.Cleanup(func() { bashWaitDelay = orig })
}

// pgidAlive reports whether any process in the given process group still
// exists, via the kill(pgid, 0) existence probe (signal 0 sends nothing).
func pgidAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// waitForFileContent polls (deadline-bound; this observes real, out-of-
// process state across an OS process boundary, so no in-process channel
// can substitute) for path to contain non-empty content, returning it. Safe
// to call from a non-test goroutine (unlike *testing.T.Fatalf).
func waitForFileContent(path string, deadline time.Time) (string, bool) {
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b), true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", false
}

// mustWaitForFileContent is the *testing.T-failing wrapper of
// waitForFileContent for use on the test's own goroutine.
func mustWaitForFileContent(t *testing.T, path string) string {
	t.Helper()
	content, ok := waitForFileContent(path, time.Now().Add(3*time.Second))
	if !ok {
		t.Fatalf("file %s did not appear in time", path)
	}
	return content
}

// waitForGroupDeath polls (deadline-bound, same cross-process rationale as
// waitForFileContent) until every process in pgid has exited, failing the
// test if the deadline passes first. This is what proves the fix's
// process-group kill actually reaps a backgrounded grandchild instead of
// orphaning it holding a dead pipe.
func waitForGroupDeath(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pgidAlive(pgid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process group %d still alive after deadline: orphaned grandchild (process-group kill did not work)", pgid)
}

func parsePGID(t *testing.T, s string) int {
	t.Helper()
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("bad pgid %q: %v", s, err)
	}
	return n
}

// TestBashTimeoutKillsWholeProcessGroup is the red-first test for the
// timeout half of the issue: a command backgrounds one child (holding the
// output pipe open) and blocks in the foreground on another. Before the fix,
// exec.CommandContext's default Cancel only kills the direct `sh` child, and
// with no WaitDelay set, Wait() blocks on pipe EOF that the still-alive
// backgrounded child will never produce — so the tool call would hang past
// the 60s sleeps, not return near the ~250ms BashTimeout used here.
func TestBashTimeoutKillsWholeProcessGroup(t *testing.T) {
	setBashWaitDelay(t, 150*time.Millisecond)
	s := NewSession(Config{
		Providers:   provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:       message.ModelRef{Provider: "test", Model: "m1"},
		BashTimeout: 250 * time.Millisecond,
	})

	dir := t.TempDir()
	pgidFile := filepath.Join(dir, "pgid")
	// Background one sleep (the pipe-holding grandchild) *before* recording
	// the group's pgid, then block in the foreground on a second sleep so
	// the tool call is still in flight when BashTimeout fires.
	cmd := fmt.Sprintf(`sleep 60 & printf '%%s' "$$" > %q; sleep 60`, pgidFile)
	args, _ := json.Marshal(map[string]string{"command": cmd})

	tool := s.tools["bash"]
	start := time.Now()
	_, err := tool.Run(context.Background(), s, args)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bash tool took %v to return, want close to the 250ms BashTimeout, not the 60s sleeps", elapsed)
	}

	pgid := parsePGID(t, mustWaitForFileContent(t, pgidFile))
	waitForGroupDeath(t, pgid)
}

// TestBashFastExitBackgroundedChildReturnsPromptly is the red-first test for
// the "everyday agent starts a dev server" case named in the issue: `sh`
// itself exits almost immediately, but a backgrounded grandchild still holds
// the write end of the output pipe. Before the fix (no WaitDelay), Wait()
// blocks on that pipe's EOF regardless of `sh` having already exited
// successfully — so the tool call would hang until the 60s sleep exits, not
// return promptly with "started" already captured.
//
// Semantics decision (documented here and at bashWaitDelay): this is
// WaitDelay-bounded, not immediate-on-child-exit. Once `sh` exits, Wait()
// starts the WaitDelay clock and gives the pipe up to bashWaitDelay to
// close on its own (the common case: no grandchild, or one that closes fds
// promptly) before force-closing it and returning exec.ErrWaitDelay, which
// the tool treats as success with a note, not a protocol failure — the
// command's own output was fully captured.
func TestBashFastExitBackgroundedChildReturnsPromptly(t *testing.T) {
	setBashWaitDelay(t, 150*time.Millisecond)
	s := NewSession(Config{
		Providers:   provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:       message.ModelRef{Provider: "test", Model: "m1"},
		BashTimeout: 30 * time.Second, // must not be why this returns quickly
	})

	dir := t.TempDir()
	pgidFile := filepath.Join(dir, "pgid")
	cmd := fmt.Sprintf(`sleep 60 & printf '%%s' "$$" > %q; echo started`, pgidFile)
	args, _ := json.Marshal(map[string]string{"command": cmd})

	tool := s.tools["bash"]
	start := time.Now()
	out, err := tool.Run(context.Background(), s, args)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("err = %v, want nil: sh exited 0, only a backgrounded grandchild held the pipe", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bash tool took %v to return, want close to the %v WaitDelay, not the 60s backgrounded sleep", elapsed, bashWaitDelay)
	}
	text := out.Text()
	if !strings.Contains(text, "started") {
		t.Fatalf("output = %q, want it to contain sh's captured \"started\"", text)
	}

	pgid := parsePGID(t, mustWaitForFileContent(t, pgidFile))
	// The backgrounded "dev server" is deliberately left running by this
	// path (that's the point of the semantics decision above) — reap it in
	// cleanup so the test doesn't leak a real process.
	t.Cleanup(func() { syscall.Kill(-pgid, syscall.SIGKILL) })
}

// TestBashAbortUnblocksInFlightTurn is the red-first test for the abort half
// of the issue: engine-level context cancellation (POST /session/{id}/abort
// in production) during a pipe-held bash call must unblock the in-flight
// turn promptly. Before the fix, cancellation kills only the direct `sh`
// child; the backgrounded grandchild keeps the pipe open and Wait() (with no
// WaitDelay) blocks forever — so abort would be powerless, exactly the
// production incident in the issue.
func TestBashAbortUnblocksInFlightTurn(t *testing.T) {
	setBashWaitDelay(t, 150*time.Millisecond)

	dir := t.TempDir()
	pgidFile := filepath.Join(dir, "pgid")
	cmd := fmt.Sprintf(`sleep 60 & printf '%%s' "$$" > %q; sleep 60`, pgidFile)

	argsJSON, _ := json.Marshal(cmd)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", fmt.Sprintf(`{"command":%s}`, argsJSON))),
	}}
	s := NewSession(Config{
		Providers:   provider.Registry{"test": prov},
		Model:       message.ModelRef{Provider: "test", Model: "m1"},
		BashTimeout: 30 * time.Second, // must not be why abort works
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cross-process synchronization: wait for the fixture script to
		// confirm its backgrounded grandchild has actually forked (by
		// writing its own pgid) before aborting, so the abort genuinely
		// races a live pipe-holder rather than a command that never started.
		waitForFileContent(pgidFile, time.Now().Add(3*time.Second))
		cancel()
	}()

	start := time.Now()
	_, err := s.Prompt(ctx, "run it")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Prompt err = nil, want the aborted turn to surface an error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Prompt took %v to return after abort, want it unblocked promptly (bounded by WaitDelay, not the 60s sleeps)", elapsed)
	}

	pgid := parsePGID(t, mustWaitForFileContent(t, pgidFile))
	waitForGroupDeath(t, pgid)
}
