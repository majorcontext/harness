// Tests for SessionManager.SendOrQueue and SetChildTurnObserver — the
// single-owner send path server/session_tree.go's unified session.send
// endpoint uses for a managed child, added so a child gets the SAME
// admission behavior a root already has (queue on busy, never a bare
// refusal) without ever creating a second *engine.Session over the
// child's own on-disk log. See docs/design/2026-09-session-send-unification.md.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestSendOrQueueRunningChildQueuesInsteadOfRefusing is the named-failure
// regression this feature exists to fix: before SendOrQueue, a busy
// child had no queue at all (SessionManager.Send's reserveSendLocked
// refuses any Running target with ErrSessionBusy — see CanSend's own
// doc comment), so server/session_tree.go's handleSessionSend answered
// a busy child with 409 and dropped the caller's text. SendOrQueue must
// instead queue it — queued=true, no error — and deliver it once the
// current turn ends, exactly like SendToDescendant's own running-target
// branch (which this reuses), but reachable with no ancestor/caller id
// at all.
func TestSendOrQueueRunningChildQueuesInsteadOfRefusing(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	childProv := &blockFirstThenScriptedProvider{
		name:    "child",
		release: release,
		started: started,
		turns:   [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "second done"})},
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started // the child's first turn is genuinely in flight

	queued, err := mgr.SendOrQueue(context.Background(), childID, "please also cover Y", "")
	if err != nil {
		t.Fatalf("SendOrQueue on a running child: err = %v, want nil", err)
	}
	if !queued {
		t.Error("SendOrQueue on a running child: queued = false, want true")
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if len(childProv.requests) != 2 {
		t.Fatalf("child provider requests = %d, want 2 (the queued text must launch a second turn, not be dropped)", len(childProv.requests))
	}
	second := childProv.requests[1]
	lastText := second.Messages[len(second.Messages)-1].Parts.Text()
	if lastText != "please also cover Y" {
		t.Errorf("second turn's trailing message = %q, want the queued text delivered verbatim", lastText)
	}
}

// TestSendOrQueueSettledChildLaunchesFreshTurnAsynchronously proves the
// settled (done) path behaves like Send — a genuinely new, separately
// blockable turn — and that SendOrQueue itself returns immediately
// (queued=false) rather than blocking the caller for the turn's
// duration, matching Send/Spawn's own non-blocking contract.
func TestSendOrQueueSettledChildLaunchesFreshTurnAsynchronously(t *testing.T) {
	release := make(chan struct{})
	childProv := &blockAfterFirstProvider{name: "child", release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	queued, err := mgr.SendOrQueue(context.Background(), childID, "please redo this", "")
	if err != nil {
		t.Fatalf("SendOrQueue on a done child: err = %v, want nil", err)
	}
	if queued {
		t.Error("SendOrQueue on a done child: queued = true, want false (a fresh turn, not a queue append)")
	}

	// SendOrQueue must not itself block: the node should already be
	// (or shortly become) Running again from the re-run turn, and this
	// call must return before that turn's own release below.
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)
	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

// TestSendOrQueueThreadsBlobsThroughRunningChildQueue proves item 4 of
// the unification (blobs through the one path) for the queue branch: a
// blob attached to a message enqueued against a RUNNING child survives
// the queue (QueuedPrompt.Blobs) and reaches the eventual turn's
// PromptWithOrigin call, appended as its own message.Blob part — not
// silently dropped, which drainQueueAndPrompt's old text-only signature
// would have done.
func TestSendOrQueueThreadsBlobsThroughRunningChildQueue(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	childProv := &blockFirstThenScriptedProvider{
		name:    "child",
		release: release,
		started: started,
		turns:   [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "second done"})},
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started

	blob := &message.Blob{MediaType: "image/png", Data: []byte("fake-png-bytes")}
	queued, err := mgr.SendOrQueue(context.Background(), childID, "see attached", "", blob)
	if err != nil {
		t.Fatalf("SendOrQueue: %v", err)
	}
	if !queued {
		t.Fatal("SendOrQueue on a running child: queued = false, want true")
	}

	qp := mgr.nodes[childID].session.QueuedPrompts()
	if len(qp) != 1 || len(qp[0].Blobs) != 1 || qp[0].Blobs[0] != blob {
		t.Fatalf("QueuedPrompts() = %+v, want one entry carrying the blob", qp)
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(childID): not found")
	}
	history := child.History()
	last := history[len(history)-1-1] // trailing assistant message is last; the user message precedes it
	// Find the delivered user message carrying the blob, searching from
	// the end: the exact index depends on how many messages the turn
	// itself appended.
	var found *message.Blob
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != message.RoleUser {
			continue
		}
		for _, p := range history[i].Parts {
			if b, ok := p.(*message.Blob); ok {
				found = b
				break
			}
		}
		if found != nil {
			break
		}
	}
	_ = last
	if found == nil {
		t.Fatal("no message.Blob part found in child history; the queued blob was dropped")
	}
	if found.MediaType != "image/png" || string(found.Data) != "fake-png-bytes" {
		t.Errorf("delivered blob = %+v, want the exact queued blob", found)
	}
}

// TestSendOrQueueUnknownSessionIsError mirrors CanSend/Send's own
// ErrUnknownSession contract: SendOrQueue must not create anything for
// an id this manager does not track.
func TestSendOrQueueUnknownSessionIsError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	_, err := mgr.SendOrQueue(context.Background(), "nope", "hi", "")
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err = %v, want ErrUnknownSession", err)
	}
}

// TestSendOrQueueRejectsCanceledTarget mirrors
// TestSendToDescendantRejectsCanceledTarget: a canceled child's queue
// must never be looked at again by anyone (see drainQueueAndPrompt's own
// doc comment) — SendOrQueue must refuse synchronously, not append.
func TestSendOrQueueRejectsCanceledTarget(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("blocker"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	if err := mgr.Cancel(childID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, time.Second)

	if _, err := mgr.SendOrQueue(context.Background(), childID, "hi", ""); !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("err = %v, want ErrSessionCanceled", err)
	}
}

// TestSendOrQueueConcurrentCallsAgainstSameRunningChildNeverCorrupt is
// the concurrency-safety proof the design's "non-negotiable" section
// requires: N concurrent SendOrQueue calls against the SAME running
// child must serialize through the single resident *engine.Session
// (n.session, under s.mu) rather than each cold-loading or otherwise
// touching a second Session object — every call must succeed, and every
// one of the N distinct texts must be delivered EXACTLY once once the
// child fully drains, with none lost or duplicated. Run with -race.
func TestSendOrQueueConcurrentCallsAgainstSameRunningChildNeverCorrupt(t *testing.T) {
	const n = 20
	release := make(chan struct{})
	started := make(chan struct{})
	// One scripted turn per queued message PLUS the first ("go") turn:
	// drainQueueAndPrompt drives every one of the n concurrently-queued
	// messages as its own separate Prompt call once the child's first
	// turn releases.
	turns := make([][]provider.Event, n)
	for i := range turns {
		turns[i] = asstTurn(provider.StopEndTurn, &message.Text{Text: fmt.Sprintf("done-%d", i)})
	}
	childProv := &blockFirstThenScriptedProvider{name: "child", release: release, started: started, turns: turns}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started

	var wg sync.WaitGroup
	errs := make([]error, n)
	queueds := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q, err := mgr.SendOrQueue(context.Background(), childID, fmt.Sprintf("msg-%d", i), "")
			errs[i] = err
			queueds[i] = q
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: err = %v, want nil", i, err)
		}
		if !queueds[i] {
			t.Errorf("call %d: queued = false, want true (child still running)", i)
		}
	}

	qp := mgr.nodes[childID].session.QueuedPrompts()
	if len(qp) != n {
		t.Fatalf("QueuedPrompts() len = %d, want %d — every concurrent call must be counted exactly once", len(qp), n)
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, 2*time.Second)

	if len(childProv.requests) != n+1 {
		t.Fatalf("child provider requests = %d, want %d (1 initial + %d queued, none lost or duplicated)", len(childProv.requests), n+1, n)
	}
	delivered := make(map[string]int, n)
	for _, req := range childProv.requests[1:] {
		text := req.Messages[len(req.Messages)-1].Parts.Text()
		delivered[text]++
	}
	if len(delivered) != n {
		t.Fatalf("delivered %d distinct messages, want %d — delivered set: %v", len(delivered), n, delivered)
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("msg-%d", i)
		if delivered[want] != 1 {
			t.Errorf("delivered[%q] = %d, want exactly 1", want, delivered[want])
		}
	}
}

// TestChildTurnObserverFiresOnceOnSuccessfulChildTurn proves item 5:
// a CHILD's completed turn (Spawn-driven) fires the ChildTurnObserver
// hook exactly like a root's turn.end/session.status pair would, which
// server/handlers.go's onChildTurnEnd wiring turns into the SAME wire
// events a root's turn produces.
func TestChildTurnObserverFiresOnceOnSuccessfulChildTurn(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("child said hi"))))

	type call struct {
		id       string
		text     string
		err      error
		canceled bool
	}
	calls := make(chan call, 4)
	mgr.SetChildTurnObserver(func(id string, msg *message.Message, err error, canceled bool) {
		text := ""
		if msg != nil {
			text = msg.Parts.Text()
		}
		calls <- call{id: id, text: text, err: err, canceled: canceled}
	})

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case c := <-calls:
		if c.id != childID {
			t.Errorf("observer id = %q, want %q", c.id, childID)
		}
		if c.err != nil {
			t.Errorf("observer err = %v, want nil", c.err)
		}
		if c.canceled {
			t.Error("observer canceled = true, want false")
		}
		if c.text != "child said hi" {
			t.Errorf("observer msg text = %q, want %q", c.text, "child said hi")
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnObserver never fired")
	}

	select {
	case c := <-calls:
		t.Fatalf("observer fired a second time unexpectedly: %+v", c)
	default:
	}
}

// TestChildTurnObserverNotFiredForRootTurn proves the observer is
// scoped to CHILDREN only (n.parentID != ""): a root driven directly
// through Send (bare-engine usage, no ExternalRunner) must never fire
// it — server/handlers.go's own runPrompt/recordTurnEnd already covers
// a root's turn.end, and firing this hook too would double-emit.
func TestChildTurnObserverNotFiredForRootTurn(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", doneTurn("root said hi"))))

	fired := make(chan struct{}, 1)
	mgr.SetChildTurnObserver(func(string, *message.Message, error, bool) {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	if _, err := mgr.Send(context.Background(), root.ID, "go"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-fired:
		t.Fatal("ChildTurnObserver fired for a ROOT turn; must be scoped to children only")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestChildTurnObserverReportsCanceled proves a canceled child reports
// canceled=true — matching a root's session.aborted (not turn.end)
// treatment, see server/handlers.go's runPrompt context.Canceled case.
func TestChildTurnObserverReportsCanceled(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	childProv := &blockFirstThenScriptedProvider{name: "child", release: release, started: started}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	calls := make(chan bool, 1)
	mgr.SetChildTurnObserver(func(_ string, _ *message.Message, _ error, canceled bool) {
		calls <- canceled
	})

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started

	if _, err := mgr.CancelDescendant(root.ID, childID); err != nil {
		t.Fatalf("CancelDescendant: %v", err)
	}
	close(release)

	select {
	case canceled := <-calls:
		if !canceled {
			t.Error("observer canceled = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnObserver never fired for a canceled child")
	}
}
