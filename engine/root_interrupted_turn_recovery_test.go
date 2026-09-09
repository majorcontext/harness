package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// This file is the regression coverage for a live prod finding: box
// box_01m1kyfxebfyjt0tg5dwk2jb32's pod was OOMKilled at 23:09:55Z mid-turn
// on a ROOT session (ses_01m1kyhka3ewf8vcth0qbqm222, a claude-code
// delegated session). recoverInterruptedTurnLocked already existed to
// surface exactly this kind of crash — but only ever ran for a CHILD
// (adoptReloadedLocked's non-root branch); adoptRootLocked never called it
// for the root's OWN turn. The root cold-reloaded with
// hasUnfinalizedTurn() still true and nothing ever appended a marker or
// cleared it: the session sat silently wedged until a human happened to
// send a brand-new prompt roughly 18 minutes later.

// TestFinalizeTurnMarksRootTurnSettled proves the enabling half of the
// fix: finalizeTurn's own settled-marker call used to be gated to
// non-root nodes only (hasTaskParent()), on the assumption that "recovery
// is never invoked for a root" made a root's turnUnsettled value moot.
// Without lifting that gate too, making adoptRootLocked call
// recoverInterruptedTurnLocked (see the next test) would misfire on
// EVERY ordinary root reload, not only a genuinely crashed one:
// hasUnfinalizedTurn() would read true forever for any root that ever
// completes so much as one turn, since nothing would ever clear it.
//
// Red-verify: before this fix, root.hasUnfinalizedTurn() stays true here
// even though the turn Send drove finished completely normally.
func TestFinalizeTurnMarksRootTurnSettled(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", doneTurn("hi"))))

	if _, err := mgr.Send(context.Background(), root.ID, "go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if root.hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true after an ordinary, successful root turn, want false — finalizeTurn's settled-marker call must cover roots, not only children, or a root's next reload always misreads as a crash")
	}
}

// TestAdoptRootSurfacesInterruptedTurnOnRecovery is the main regression
// test for the live OOM-kill finding described above. It simulates the
// crash the same way the existing child-recovery tests do
// (TestRecoverInterruptedTurnFiresForChildCrashedMidToolLoop): manually
// append a user message, then an assistant tool-call/tool-result pair,
// directly onto the ROOT session's own durable log, without ever running
// finalizeTurn — the exact trailing shape an OOM kill mid-tool-loop
// leaves behind. Reload into a FRESH SessionManager (a fresh process
// after the restart) via AdoptRoot and assert recovery actually fired.
//
// Red-verify: before this fix, reloadedRoot.hasUnfinalizedTurn() stays
// true after AdoptRoot, no synthetic marker is ever appended to history,
// and info.Status stays StatusIdle (adoptLocked's bare default) forever
// — exactly the silent wedge Andy hit.
func TestAdoptRootSurfacesInterruptedTurnOnRecovery(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", doneTurn("resumed"))}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)

	root1.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "start the long task"}}})
	root1.append(message.Message{ID: "a1", Role: message.RoleAssistant, Parts: message.Parts{
		&message.Text{Text: "working on it"},
		toolCall("tc1", "bash", `{"command":"sleep 999"}`),
	}})
	root1.append(message.Message{ID: "t1", Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "tc1", Content: message.Parts{&message.Text{Text: "still running"}}},
	}})
	if !root1.hasUnfinalizedTurn() {
		t.Fatal("test setup: manually appended history does not end on the trailing-unfinalized shape")
	}

	// Simulate the restart: a fresh SessionManager in a fresh process,
	// reloading the same durable session id from disk.
	mgr2 := NewSessionManager(context.Background(), 0, 0)
	reloadedRoot, err := LoadSession(Config{Providers: reg, SessionDir: dir}, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reloadedRoot.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded root lost the unfinalized shape across LoadSession")
	}

	if err := mgr2.AdoptRoot(reloadedRoot); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	if reloadedRoot.hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true after AdoptRoot, want false — a root's own crashed turn must be recovered exactly like a child's")
	}

	hist := reloadedRoot.History()
	if len(hist) == 0 {
		t.Fatal("History() is empty after recovery")
	}
	last := hist[len(hist)-1]
	if !isRecoverySyntheticCloser(last) {
		t.Errorf("last history message = %+v, want the synthetic interrupted-turn closer", last)
	}
	if !strings.Contains(last.Parts.Text(), "interrupted") {
		t.Errorf("closing message text = %q, want it to mention the interruption", last.Parts.Text())
	}

	info, ok := mgr2.Info(root1.ID)
	if !ok {
		t.Fatal("root not tracked after AdoptRoot")
	}
	if info.Status != StatusFailed {
		t.Errorf("status = %q, want %q — the crashed turn has a real, if generic, outcome", info.Status, StatusFailed)
	}

	// The specific regression risk this fix introduces:
	// recoverInterruptedTurnLocked arms n.pendingForget for an ORPHANED
	// CHILD with no live ancestor (see that field's own doc comment) — a
	// genuine root reaching the identical target==nil branch must NOT be
	// treated the same way, or Reap would garbage-collect a live,
	// still-in-use root the instant it is next momentarily childless (it
	// already is: n.finalized and a terminal n.status are both now true,
	// the only two OTHER preconditions Reap checks).
	if n := mgr2.Reap(); n != 0 {
		t.Errorf("Reap() removed %d node(s) immediately after recovering a root's own crashed turn — the root must stay protected, not armed for collection", n)
	}
	if _, ok := mgr2.Session(root1.ID); !ok {
		t.Error("root no longer tracked after Reap() — recovery must not have armed pendingForget on a genuine root")
	}
}

// TestAdoptRootRecoveredSessionAcceptsNextPromptNatively proves the
// recovered root is left USABLE, not merely marked — the native-provider
// half of the brief's "the session is usable for the next prompt after
// recovery" requirement. Drives an ordinary next turn through
// SessionManager.Send after recovery and confirms it completes normally.
func TestAdoptRootRecoveredSessionAcceptsNextPromptNatively(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", doneTurn("resumed"))}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)
	root1.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "start"}}})
	root1.append(message.Message{ID: "a1", Role: message.RoleAssistant, Parts: message.Parts{
		&message.Text{Text: "working"},
		toolCall("tc1", "bash", `{"command":"sleep 999"}`),
	}})
	root1.append(message.Message{ID: "t1", Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "tc1", Content: message.Parts{&message.Text{Text: "still running"}}},
	}})

	mgr2 := NewSessionManager(context.Background(), 0, 0)
	reloadedRoot, err := LoadSession(Config{Providers: reg, SessionDir: dir}, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if err := mgr2.AdoptRoot(reloadedRoot); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	msg, err := mgr2.Send(context.Background(), root1.ID, "please continue")
	if err != nil {
		t.Fatalf("Send after recovery: %v", err)
	}
	if msg == nil || msg.Parts.Text() != "resumed" {
		t.Errorf("Send after recovery returned %+v, want the scripted \"resumed\" completion", msg)
	}
	if reloadedRoot.hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true after the post-recovery turn completed, want false")
	}
	info, ok := mgr2.Info(root1.ID)
	if !ok || info.Status != StatusIdle {
		t.Errorf("Info() after the post-recovery turn = %+v, ok=%v, want StatusIdle", info, ok)
	}
}

// TestClaudeCodeRootRecoveredAfterCrashStillResumesDelegatedSession is the
// delegated-backend half of the same usability requirement: the CLI's own
// on-disk session (named by Session.claudeCodeSessionID(), durable
// independent of harness's own bookkeeping — see claude_code_backend.go's
// package doc) must still be resumed correctly via --resume after a
// crashed turn was recovered — the synthetic closing message recovery
// appends lives only in harness's OWN s.history, never in the CLI's own
// transcript, so it must not disturb --resume at all.
//
// Mirrors TestClaudeCodeSessionIDResumedAcrossTurns's own reload
// assertions, with one crash-and-recover step inserted between the first
// real delegated turn and the reload.
func TestClaudeCodeRootRecoveredAfterCrashStillResumesDelegatedSession(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")

	if _, err := s.Prompt(context.Background(), "first turn"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if s.claudeCodeSessionID() != "fake-session-1" {
		t.Fatalf("claudeCodeSessionID() = %q after first turn, want fake-session-1", s.claudeCodeSessionID())
	}

	// Simulate the OOM kill: a second delegated turn starts (a user
	// message, plus the assistant/tool shape a mid-tool-loop crash
	// leaves) but the process dies before the CLI's own "result" event
	// (and harness's own finalizeTurn) ever lands.
	s.append(message.Message{ID: "u2", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "second turn"}}})
	s.append(message.Message{ID: "a2", Role: message.RoleAssistant, Parts: message.Parts{
		&message.Text{Text: "still working"},
		toolCall("tc2", "bash", `{"command":"sleep 999"}`),
	}})
	s.append(message.Message{ID: "t2", Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "tc2", Content: message.Parts{&message.Text{Text: "still running"}}},
	}})
	if !s.hasUnfinalizedTurn() {
		t.Fatal("test setup: manually appended history does not end on the trailing-unfinalized shape")
	}

	reloaded, err := LoadSession(Config{
		SessionDir: s.cfg.SessionDir,
		ClaudeCode: s.cfg.ClaudeCode,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reloaded.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded session lost the unfinalized shape across LoadSession")
	}
	if reloaded.claudeCodeSessionID() != "fake-session-1" {
		t.Fatalf("reloaded claudeCodeSessionID() = %q, want fake-session-1", reloaded.claudeCodeSessionID())
	}

	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.AdoptRoot(reloaded); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if reloaded.hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true after AdoptRoot, want false")
	}
	// AdoptRoot must not have touched the CLI's own session id — the
	// synthetic closer recovery appends lives only in s.history, never in
	// the durable claudeCodeCLISessionID field --resume reads.
	if reloaded.claudeCodeSessionID() != "fake-session-1" {
		t.Errorf("claudeCodeSessionID() after AdoptRoot = %q, want it untouched (fake-session-1)", reloaded.claudeCodeSessionID())
	}

	if _, err := reloaded.Prompt(context.Background(), "third turn"); err != nil {
		t.Fatalf("third Prompt (post-recovery): %v", err)
	}
	invocations := readInvocations(t, logPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations after recovery = %d, want 2 (first turn, then the post-recovery turn)", len(invocations))
	}
	if v, ok := argvValueAfter(invocations[1], "--resume"); !ok || v != "fake-session-1" {
		t.Errorf("post-recovery --resume = %q, ok=%v, want fake-session-1 — the synthetic closing message must not have disturbed the CLI's own resumed session", v, ok)
	}
}
