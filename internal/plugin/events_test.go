package plugin

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
)

// TestEventOrderingPerPlugin proves that, for any given call id, a
// tool.execute.start always arrives at a plugin before the matching
// tool.execute.end — even though the engine calls Emit for both from the
// same goroutine back-to-back and the host fans events out asynchronously.
// The event queue is sized comfortably above the total event count so the
// run is guaranteed to be drop-free (see TestEventQueueFullDropsWithoutBlocking
// for drop behavior), isolating this test to ordering alone.
func TestEventOrderingPerPlugin(t *testing.T) {
	const iterations = 2000 // meaningful count to give -race a chance to catch interleaving bugs

	got := make(chan Event, iterations*2)
	recorder := testPlugin(t, "recorder", &Hooks{
		Event: func(_ context.Context, _ *Client, events []Event) {
			for _, ev := range events {
				got <- ev
			}
		},
	})
	h := newTestHost(t, Options{EventQueueSize: iterations*2 + 8}, recorder)

	for i := 0; i < iterations; i++ {
		id := fmt.Sprintf("call-%d", i)
		h.Emit([]Event{{Type: EventToolExecuteStart, SessionID: id}})
		h.Emit([]Event{{Type: EventToolExecuteEnd, SessionID: id}})
	}

	// Block directly on the channel for the exact expected count; the
	// queue is sized so nothing can be dropped, so this can never hang
	// unless delivery is broken.
	seenStart := make(map[string]bool, iterations)
	for i := 0; i < iterations*2; i++ {
		ev := <-got
		switch ev.Type {
		case EventToolExecuteStart:
			seenStart[ev.SessionID] = true
		case EventToolExecuteEnd:
			if !seenStart[ev.SessionID] {
				t.Fatalf("tool.execute.end for %q arrived before its tool.execute.start", ev.SessionID)
			}
		default:
			t.Fatalf("unexpected event type %q", ev.Type)
		}
	}
}

// TestEventQueueFullDropsWithoutBlocking proves that Emit never blocks the
// caller, even when a plugin's connection is wedged: events queue up to a
// small bounded capacity and anything beyond that is dropped and counted,
// rather than stalling the engine goroutine that called Emit.
//
// The wedge is a connection nobody reads from, so the sender goroutine's
// very first write blocks durably — inside a synctest bubble this is
// deterministic (no sleep, no guessed deadline) and released via
// t.Cleanup so the blocked goroutine exits before the bubble ends, per
// AGENTS.md's synchronization rules.
func TestEventQueueFullDropsWithoutBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hostSide, pluginSide := net.Pipe()
		release := make(chan struct{})
		// Nothing reads pluginSide until release fires, so the sender's
		// first write on this connection blocks durably for the whole
		// burst below.
		go func() {
			<-release
			_, _ = io.Copy(io.Discard, pluginSide)
		}()

		const queueSize = 4
		spec := Spec{
			Manifest: Manifest{Name: "wedged", ProtocolVersion: ProtocolVersion, Hooks: []Hook{HookEvent}},
			dial: func() (io.ReadWriteCloser, error) {
				return hostSide, nil
			},
		}
		h := newTestHost(t, Options{EventQueueSize: queueSize}, spec)
		// Registered after newTestHost's own t.Cleanup(h.Close): cleanups
		// run LIFO, so this releases the wedge (unblocking the sender
		// goroutine stuck mid-handshake) before Host.Close tries to stop
		// the instance — otherwise Close would itself wait on the same
		// wedge.
		t.Cleanup(func() {
			close(release)
			_ = hostSide.Close()
			_ = pluginSide.Close()
		})

		// Fire far more events than the queue can hold. If Emit ever
		// blocked on a full queue this loop would hang.
		const sent = 500
		for i := 0; i < sent; i++ {
			h.Emit([]Event{{Type: EventToolExecuteStart, SessionID: fmt.Sprintf("call-%d", i)}})
		}

		dropped := h.EventsDropped("wedged")
		if dropped == 0 {
			t.Fatal("EventsDropped(\"wedged\") = 0, want > 0 for a wedged connection under burst")
		}
		if dropped > sent {
			t.Fatalf("EventsDropped = %d, cannot exceed events sent (%d)", dropped, sent)
		}
	})
}

// TestEventOrderingConcurrentEmitters exercises Emit from many goroutines at
// once (the shape of the original bug report: concurrent goroutines racing
// for the connection write mutex). Ordering per call id — which is
// established by a single goroutine doing start-then-end sequentially — must
// still hold no matter how other goroutines interleave their own emits.
func TestEventOrderingConcurrentEmitters(t *testing.T) {
	const goroutines = 200

	var mu sync.Mutex
	arrival := make(map[string][]string) // call id -> event types in arrival order
	done := make(chan struct{})
	count := 0
	const wantEvents = goroutines * 2

	recorder := testPlugin(t, "recorder", &Hooks{
		Event: func(_ context.Context, _ *Client, events []Event) {
			mu.Lock()
			defer mu.Unlock()
			for _, ev := range events {
				arrival[ev.SessionID] = append(arrival[ev.SessionID], ev.Type)
				count++
			}
			if count == wantEvents {
				close(done)
			}
		},
	})
	h := newTestHost(t, Options{EventQueueSize: wantEvents + 8}, recorder)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("call-%d", i)
			h.Emit([]Event{{Type: EventToolExecuteStart, SessionID: id}})
			h.Emit([]Event{{Type: EventToolExecuteEnd, SessionID: id}})
		}(i)
	}
	wg.Wait()

	<-done // block on the channel; queue is oversized so this cannot hang

	mu.Lock()
	defer mu.Unlock()
	for id, seq := range arrival {
		if len(seq) != 2 || seq[0] != EventToolExecuteStart || seq[1] != EventToolExecuteEnd {
			t.Errorf("call %s: arrival order = %v, want [%s %s]", id, seq, EventToolExecuteStart, EventToolExecuteEnd)
		}
	}
}

// TestEventSenderStopStartRace hammers Host.Close (which stops instances,
// nil-ing their conn) concurrently with Host.Emit (which spawns a sender
// goroutine that starts the instance and then notifies over its conn) across
// many trials. It exists to catch a specific bug: runEventSender used to
// call inst.start(ctx) and then read the instance's conn field a second
// time, unsynchronized, to call notify — a window in which a concurrent
// stop() could nil (or a future start could replace) that field out from
// under it. Under -race this must never report a data race, and the
// unsynchronized read must never nil-deref.
func TestEventSenderStopStartRace(t *testing.T) {
	const trials = 200
	for trial := 0; trial < trials; trial++ {
		recorder := testPlugin(t, "recorder", &Hooks{
			Event: func(_ context.Context, _ *Client, _ []Event) {},
		})
		h, err := NewHost(Options{EventQueueSize: 4}, recorder)
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				h.Emit([]Event{{Type: EventToolExecuteStart, SessionID: "x"}})
			}
		}()
		go func() {
			defer wg.Done()
			h.Close()
		}()
		wg.Wait()
	}
}
