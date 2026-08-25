package engine

import (
	"context"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// TestReportTurnEndDoesNotReDriveQueuedPrompt is the regression test for a
// review finding: finalizeTurn's queued-message re-drive gated on
// n.parentID != "", which cannot tell an IN-PACKAGE child turn (Spawn's or
// Send's own goroutine, where the returned resume is the only thing that
// will ever continue the session) from an EXTERNALLY scheduled one
// (ReportTurnEnd, where the server holds the run slot and its own
// maybeDispatchQueued tail drains the queue).
//
// A depth>0 node CAN be resident and externally driven: claimForPrompt
// (server/handlers.go) LoadSessions any id with no depth guard, so a
// former child reached through POST /session/{id}/prompt_async or /goal
// runs that way. On that path the re-drive did one of two wrong things:
//
//   - queue [A]: it popped A, journaled it dequeued("delivered"), and
//     returned a resume that runs s.Prompt with NO run slot held, so a
//     concurrent POST /prompt could claim the freed slot and call Prompt
//     on the same session at the same time.
//   - queue [A, B]: it popped A, and the server's own maybeDispatchQueued
//     then dispatched B and never fired the resume, so A was recorded
//     delivered and never ran.
//
// An externally scheduled turn must therefore leave the queue completely
// alone and return no resume. This test drives the two-item shape, which
// catches both: the queue must still hold BOTH prompts.
func TestReportTurnEndDoesNotReDriveQueuedPrompt(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("child done"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session: child not found")
	}
	for _, text := range []string{"message A", "message B"} {
		if _, err := child.EnqueuePrompt(text); err != nil {
			t.Fatalf("EnqueuePrompt %q: %v", text, err)
		}
	}

	// The external scheduler's own turn on this resident node: the server
	// holds the run slot across both calls and drains the queue itself
	// afterwards.
	mgr.ReportTurnStart(child)
	msg := &message.Message{ID: "msg_ext", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "external turn done"}}}
	mgr.ReportTurnEnd(childID, msg, nil)

	// The queue, not the returned resume, is what this test asserts on.
	// ReportTurnEnd legitimately returns a resume on this path for an
	// unrelated reason — waking an ancestor whose notification this
	// child's completion just delivered (finalizeTurn's ancestor-delivery
	// branch, which the server has always fired) — so a nil check would
	// assert the wrong mechanism. An untouched queue is exactly the
	// property both halves of the finding need: with nothing popped there
	// is no re-drive resume to run Prompt without a slot, and nothing
	// journaled delivered for the server's own tail to skip.
	pending := child.QueuedPrompts()
	if len(pending) != 2 || pending[0].Text != "message A" || pending[1].Text != "message B" {
		t.Fatalf("QueuedPrompts after ReportTurnEnd = %+v, want both messages untouched: the re-drive popped one and journaled it delivered on a path that never runs it", pending)
	}
}
