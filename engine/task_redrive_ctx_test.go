package engine

import (
	"context"
	"testing"
	"time"
)

// TestFinalizeTurnCtxOnlyCancelDrainsQueueAsOrphaned is the regression
// test for a review finding: finalizeTurn's queued-message re-drive
// gated only on n.status != StatusCanceled, and StatusCanceled is set
// ONLY by cancelOneNodeLocked/cancelSubtreeLocked (task cancel,
// AbortTurn). A cascade cancel of the manager's own base ctx — process
// shutdown — cancels n.ctx and leaves n.status at StatusRunning.
//
// On that path the re-drive used to pop a queued prompt and journal it
// prompt.dequeued("delivered") while drainQueueAndPrompt's own ctx guard
// made sure nothing ran, and the resume's own finalizeTurn call
// re-entered the gate and popped the next one — draining the whole
// queue as delivered even though neither message ever ran. finalizeTurn
// now skips the re-drive on this path (ctx.Err() != nil) and instead
// drains the queue itself, once, at the terminal switch — journaled
// dequeued("orphaned"), never "delivered": the messages did not run,
// and nothing records that they did.
func TestFinalizeTurnCtxOnlyCancelDrainsQueueAsOrphaned(t *testing.T) {
	baseCtx, cancelBase := context.WithCancel(context.Background())
	t.Cleanup(cancelBase)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: release}
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)
	var reasons []string
	cfg.OnEvent = func(ev Event) {
		if ev.Type == EventPromptDequeued {
			reasons = append(reasons, ev.QueueReason)
		}
	}
	mgr := NewSessionManager(baseCtx, 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // the child's turn is genuinely in flight

	for _, text := range []string{"message A", "message B"} {
		queued, err := mgr.SendToDescendant(root.ID, childID, text)
		if err != nil || !queued {
			t.Fatalf("SendToDescendant %q: queued=%v err=%v, want queued=true err=nil", text, queued, err)
		}
	}

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session before shutdown: not found")
	}

	// Process shutdown: the base ctx cascade-cancels every node's ctx and
	// touches no status at all.
	cancelBase()

	// Wait for the child's own turn goroutine to finish unwinding, which
	// is exactly when its finalizeTurn call has run: a node becomes
	// Reap-eligible only once finalizeTurn flips finalized (see
	// sessionNode.finalized's doc comment, session_manager.go). Reached
	// through the Changed seam, with no sampling. The Session handle above
	// keeps the queue readable after the sweep removes the node.
	waitForReap(t, mgr, 1, time.Second, "child never became reapable after the base ctx was canceled")

	if pending := child.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after a ctx-only cancel settled = %+v, want empty: an orphaned queue must be drained, not left stuck forever", pending)
	}
	if len(reasons) != 2 || reasons[0] != "orphaned" || reasons[1] != "orphaned" {
		t.Fatalf("prompt.dequeued reasons = %v, want [orphaned orphaned]: neither message ran, so neither may be journaled \"delivered\"", reasons)
	}
}
