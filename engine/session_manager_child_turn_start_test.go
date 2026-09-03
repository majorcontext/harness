// Tests for SessionManager.SetChildTurnStartObserver — the mirror-image
// hook to ChildTurnObserver (session_manager_send_or_queue_test.go): a
// root emits a "busy" wire event the instant its own turn is admitted
// (see server/handlers.go/session_tree.go's several
// `emitDurable(Event{Type: evtSessionStatus, Status: "busy"})` call
// sites, one at every place a root's turn is dispatched), but a CHILD
// emitted no equivalent signal at all before this — only the terminal
// ChildTurnObserver existed, leaving a console with no way to see a
// child go busy from the event stream, only by polling session.info.
// See docs/design/session-send-unification.md's "Child turn-lifecycle
// events" section for the full reasoning.
package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestChildTurnStartObserverFiresBeforeEndObserver proves the two hooks
// bracket ONE child turn in the right order and both name the same id —
// the shape a root's busy-then-idle pair already has (a root's
// session.status:busy always precedes its own turn.end/session.status:
// idle for the same turn). Spawn is the path under test: the initial
// turn of a freshly spawned child.
func TestChildTurnStartObserverFiresBeforeEndObserver(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	childProv := &blockFirstThenScriptedProvider{name: "child", release: release, started: started}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	type event struct {
		kind string // "start" or "end"
		id   string
	}
	events := make(chan event, 4)
	mgr.SetChildTurnStartObserver(func(id string) {
		events <- event{kind: "start", id: id}
	})
	mgr.SetChildTurnObserver(func(id string, _ *message.Message, _ error, _ bool) {
		events <- event{kind: "end", id: id}
	})

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// The start observer must fire before the turn is even allowed to
	// finish — assert it arrives BEFORE releasing the blocked provider,
	// proving it fires at admission, not at settle.
	select {
	case ev := <-events:
		if ev.kind != "start" || ev.id != childID {
			t.Fatalf("first observed event = %+v, want {start %s}", ev, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnStartObserver never fired before the turn completed")
	}

	close(release)

	select {
	case ev := <-events:
		if ev.kind != "end" || ev.id != childID {
			t.Fatalf("second observed event = %+v, want {end %s}", ev, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnObserver (end) never fired")
	}
}

// TestChildTurnStartObserverFiresOnSendOrQueueSettledRelaunch proves the
// start observer fires again for a FOLLOW-UP turn (SendOrQueue's
// settled-target reserve-and-relaunch path), not just a child's very
// first, Spawn-driven turn.
func TestChildTurnStartObserverFiresOnSendOrQueueSettledRelaunch(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("first"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	starts := make(chan string, 2)
	mgr.SetChildTurnStartObserver(func(id string) { starts <- id })

	// The relaunch turn's own outcome is irrelevant to this test (only
	// whether the START observer fires): "child"'s scriptedProvider has
	// just one scripted turn, so this second call runs out of script
	// and errors (io.ErrUnexpectedEOF) -- still a genuine reserved,
	// STARTED turn from SendOrQueue's own point of view.
	queued, err := mgr.SendOrQueue(context.Background(), childID, "again", "")
	if err != nil {
		t.Fatalf("SendOrQueue: %v", err)
	}
	if queued {
		t.Fatal("SendOrQueue on a done child: queued = true, want false")
	}

	select {
	case id := <-starts:
		if id != childID {
			t.Fatalf("start observer id = %q, want %q", id, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnStartObserver never fired for the relaunch")
	}
}

// TestChildTurnStartObserverNotFiredForRootTurn mirrors
// TestChildTurnObserverNotFiredForRootTurn: the start hook is scoped to
// CHILDREN only (n.depth > 0 inside reserveSendLocked) — a root driven
// directly through Send (bare-engine usage, no ExternalRunner) must
// never fire it, since this server's own root admission path
// (claimForPrompt/dispatchQueueHead) already emits the root's busy
// event itself.
func TestChildTurnStartObserverNotFiredForRootTurn(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", doneTurn("root said hi"))))

	fired := make(chan struct{}, 1)
	mgr.SetChildTurnStartObserver(func(string) {
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
		t.Fatal("ChildTurnStartObserver fired for a ROOT turn; must be scoped to children only")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestChildTurnStartObserverFiresOnceForAWholeReservedRun proves the
// start observer stays 1:1 with the END observer even when a busy
// child's queue drains a SECOND provider turn internally
// (drainQueueAndPrompt, SendOrQueue's running-target branch): a start
// fires once for the whole reserved run (Spawn's initial dispatch), a
// queued follow-up delivered while already running does NOT fire a
// second, spurious start, and the end observer fires exactly once when
// the WHOLE run (both the initial turn and the drained follow-up)
// finally settles. This mirrors ChildTurnObserver's own existing
// once-per-settle contract (session_manager_send_or_queue_test.go) —
// SessionManager treats a child's initial turn plus everything
// drainQueueAndPrompt drains behind it as ONE continuous reserved run,
// not one turn per drained item, so the start/end pair brackets that
// SAME unit on both sides rather than going out of sync with each
// other.
func TestChildTurnStartObserverFiresOnceForAWholeReservedRun(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	childProv := &blockFirstThenScriptedProvider{
		name: "child", release: release, started: started,
		turns: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "second done"})},
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	starts := make(chan string, 4)
	ends := make(chan string, 4)
	mgr.SetChildTurnStartObserver(func(id string) { starts <- id })
	mgr.SetChildTurnObserver(func(id string, _ *message.Message, _ error, _ bool) { ends <- id })

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started

	select {
	case id := <-starts:
		if id != childID {
			t.Fatalf("start id = %q, want %q", id, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnStartObserver never fired for the initial turn")
	}

	queued, err := mgr.SendOrQueue(context.Background(), childID, "follow up", "")
	if err != nil {
		t.Fatalf("SendOrQueue: %v", err)
	}
	if !queued {
		t.Fatal("SendOrQueue on a running child: queued = false, want true")
	}

	// No second start yet: the child is still running its ORIGINAL
	// reserved turn (the follow-up merely queued behind it) — assert
	// this BEFORE releasing, so a wrongly-early second start can't hide
	// behind the eventual settle below.
	select {
	case id := <-starts:
		t.Fatalf("a second start fired while the child was still running its first reserved turn: %q", id)
	default:
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	select {
	case id := <-ends:
		if id != childID {
			t.Fatalf("end id = %q, want %q", id, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("ChildTurnObserver (end) never fired once the whole run settled")
	}

	select {
	case id := <-starts:
		t.Fatalf("a second start fired for the queue-drained follow-up turn: %q — start must stay 1:1 with end, not one-per-drained-item", id)
	default:
	}
}

// TestChildTurnStartAndEndObserversConcurrentAcrossManyChildren is the
// concurrency-safety proof for the new hook: several children, each
// spawned and driven to settle CONCURRENTLY, must each report EXACTLY
// one start and one end, correctly paired by id — proving the
// deferPersist-queued observer calls (guarded by the same m.mu this
// package already serializes every other node mutation through) never
// cross-contaminate between children or race under -race.
func TestChildTurnStartAndEndObserversConcurrentAcrossManyChildren(t *testing.T) {
	const n = 10
	// Every provider this test needs is registered UP FRONT, before any
	// Spawn call — see managedConfig's own doc comment: Config.Providers
	// is a plain map inherited by reference into every child's Config,
	// so mutating it concurrently AFTER spawning would itself race,
	// independent of anything this test means to exercise.
	providers := make([]provider.Provider, 0, n+1)
	providers = append(providers, scriptedTurns("root", nil))
	for i := 0; i < n; i++ {
		providers = append(providers, scriptedTurns(fmt.Sprintf("child%d", i), doneTurn(fmt.Sprintf("done-%d", i))))
	}
	mgr := NewSessionManager(context.Background(), 0, n)
	root := mgr.NewRoot(managedConfig("root", providers...))

	type mu struct {
		starts map[string]int
		ends   map[string]int
	}
	var m mu
	m.starts = make(map[string]int)
	m.ends = make(map[string]int)
	var lock sync.Mutex
	mgr.SetChildTurnStartObserver(func(id string) {
		lock.Lock()
		m.starts[id]++
		lock.Unlock()
	})
	mgr.SetChildTurnObserver(func(id string, _ *message.Message, _ error, _ bool) {
		lock.Lock()
		m.ends[id]++
		lock.Unlock()
	})

	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			childID, err := mgr.Spawn(SpawnOptions{
				ParentID: root.ID, Prompt: "go",
				Model: modelFor(fmt.Sprintf("child%d", i)), AgentType: AgentGeneralPurpose,
			})
			if err != nil {
				t.Errorf("Spawn %d: %v", i, err)
				return
			}
			ids[i] = childID
			waitForStatus(t, mgr, childID, StatusDone, 2*time.Second)
		}(i)
	}
	wg.Wait()

	lock.Lock()
	defer lock.Unlock()
	for i, id := range ids {
		if id == "" {
			continue // Spawn failed and already reported above
		}
		if m.starts[id] != 1 {
			t.Errorf("child %d (%s): %d starts, want 1", i, id, m.starts[id])
		}
		if m.ends[id] != 1 {
			t.Errorf("child %d (%s): %d ends, want 1", i, id, m.ends[id])
		}
	}
}
