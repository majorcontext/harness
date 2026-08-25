package engine

import (
	"context"
	"testing"
	"time"
)

// TestFinalizeTurnReDriveLeavesQueueOnCtxOnlyCancel is the regression test
// for a review finding: finalizeTurn's queued-message re-drive gated only
// on n.status != StatusCanceled, and StatusCanceled is set ONLY by
// cancelOneNodeLocked/cancelSubtreeLocked (task cancel, AbortTurn). A
// cascade cancel of the manager's own base ctx — process shutdown — cancels
// n.ctx and leaves n.status at StatusRunning.
//
// On that path the re-drive popped a queued prompt and journaled it
// prompt.dequeued("delivered") while drainQueueAndPrompt's own ctx guard
// made sure nothing ran, and the resume's own finalizeTurn call re-entered
// the gate and popped the next one — draining the whole queue as delivered.
// A later reload folds those records out, so the prompts are lost. That is
// the opposite of the task-cancel contract, where a canceled child's queue
// stays queued and inert until the node is Reaped.
//
// The queue must survive a ctx-only cancel exactly as it survives a
// status cancel.
func TestFinalizeTurnReDriveLeavesQueueOnCtxOnlyCancel(t *testing.T) {
	baseCtx, cancelBase := context.WithCancel(context.Background())
	t.Cleanup(cancelBase)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: release}
	mgr := NewSessionManager(baseCtx, 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

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

	pending := child.QueuedPrompts()
	if len(pending) != 2 || pending[0].Text != "message A" || pending[1].Text != "message B" {
		t.Fatalf("QueuedPrompts after a ctx-only cancel = %+v, want both messages left queued: a re-drive journaled them delivered without running them, so a reload loses them", pending)
	}
}
