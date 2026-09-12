// Tests for finalizeTurnFrom's and Reap's orphaned-queue drain: a
// depth>0 subagent is one-shot (see finalizeTurnFrom's own doc comment)
// and never gets another turn once terminal, so any prompt still in its
// queue at that point — or still there when Reap collects it — will
// never be delivered by anything in this package. Before this fix,
// QueuedPrompts() read nonzero forever; promptQueueFold resurrected the
// same undelivered prompt.queued record on every reload.
package engine

import (
	"context"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// TestFinalizeTurnDrainsOrphanedQueueOnExplicitCancel is the RED-VERIFY
// regression: canceling a running child with a message already queued
// used to leave that message queued forever — CancelDescendant sets
// StatusCanceled synchronously, which skips finalizeTurnFrom's re-drive
// gate (n.status != StatusCanceled), and nothing else ever drives
// another turn on a depth>0 node to pick it up. Also proves the drain is
// durable, not memory-only: promptQueueFold must net a fresh LoadSession's
// queue to zero, or a cold reload resurrects the very record this fix
// exists to stop resurrecting.
func TestFinalizeTurnDrainsOrphanedQueueOnExplicitCancel(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: release}
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started

	queued, err := mgr.SendToDescendant(root.ID, childID, "left behind")
	if err != nil || !queued {
		t.Fatalf("SendToDescendant: queued=%v err=%v, want queued=true err=nil", queued, err)
	}
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session: child not found")
	}

	if _, err := mgr.CancelDescendant(root.ID, childID); err != nil {
		t.Fatalf("CancelDescendant: %v", err)
	}
	waitForFinalized(t, mgr, childID, time.Second)

	if pending := child.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after a canceled child settled = %+v, want empty: a terminal subagent's queue is orphaned forever", pending)
	}

	reloaded, err := LoadSession(Config{SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if pending := reloaded.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after reload = %+v, want empty: the dequeue must be journaled, not memory-only", pending)
	}
}

// TestReapDrainsPreexistingOrphanedQueue covers Site 2: a queue that
// landed on an already-terminal, already-drained session — the "adopted
// mid-terminal from disk" case finalizeTurnFrom's own drain never runs
// for, since no turn on that id will ever finalize again — must still be
// drained, durably, before Reap deletes the node.
func TestReapDrainsPreexistingOrphanedQueue(t *testing.T) {
	dir := t.TempDir()
	cfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("done")))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session: child not found")
	}
	// Bypasses SessionManager entirely — the durable analogue of a
	// message that landed on this exact id from a prior process, after
	// its own finalizeTurnFrom drain already ran.
	if _, _, err := child.EnqueuePrompt("too late", "", PromptProvenance{}); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	if pending := child.QueuedPrompts(); len(pending) != 1 {
		t.Fatalf("QueuedPrompts before Reap = %+v, want 1 (test setup)", pending)
	}

	waitForReap(t, mgr, 1, time.Second, "settled child never became reapable")

	if pending := child.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after Reap = %+v, want empty", pending)
	}
	reloaded, err := LoadSession(Config{SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if pending := reloaded.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after reload = %+v, want empty: Reap's drain must be durable", pending)
	}
}

// TestFinalizeTurnRootQueueSurvivesOrphanCleanup is the regression guard:
// a root (depth 0) never settles terminal and its queue drives its own
// future idle dispatch, so this cleanup must never touch it.
func TestFinalizeTurnRootQueueSurvivesOrphanCleanup(t *testing.T) {
	release := make(chan struct{})
	rootProv := &signaledBlockingProvider{name: "root", started: make(chan struct{}), release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", rootProv))

	// Mirrors cmd/harness's bare-mode bracket (runCmd, main.go): the
	// caller's own scheduler holds the run slot across Prompt and
	// reports completion via ReportTurnEnd.
	mgr.ReportTurnStart(root)
	turnDone := make(chan struct{})
	var msg *message.Message
	var promptErr error
	go func() {
		defer close(turnDone)
		msg, promptErr = root.Prompt(context.Background(), "go")
	}()
	<-rootProv.started

	queued, err := mgr.SendOrQueue(context.Background(), root.ID, "left behind", "", PromptProvenance{})
	if err != nil {
		t.Fatalf("SendOrQueue: %v", err)
	}
	if !queued {
		t.Fatal("SendOrQueue on a running root: queued = false, want true")
	}

	close(release)
	<-turnDone
	if resume := mgr.ReportTurnEnd(root.ID, msg, promptErr); resume != nil {
		go resume()
	}
	waitForStatus(t, mgr, root.ID, StatusIdle, time.Second)

	if pending := root.QueuedPrompts(); len(pending) != 1 {
		t.Fatalf("root QueuedPrompts after its own turn settled = %+v, want the queued message preserved for idle dispatch", pending)
	}
}
