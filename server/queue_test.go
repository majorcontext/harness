package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// queueProv blocks its FIRST Stream call until release is closed (so a test
// can hold a genuine in-flight turn open while it enqueues prompts against a
// busy session), then serves scripted turns for every call after — the shape
// the prompt-queue drain tests need: one test-controlled occupant, followed
// by fully deterministic scripted turns for whatever the queue subsequently
// drains into.
type queueProv struct {
	name    string
	mu      sync.Mutex
	turns   [][]provider.Event
	call    int
	started chan struct{}
	release chan struct{}
	once    sync.Once
	// firstDone flips true the instant the blocked first call is released;
	// every Stream call afterward — including ones from other, later
	// dispatched turns — is scripted, never blocked again.
	firstDone bool
}

func (p *queueProv) Name() string { return p.name }

func (p *queueProv) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	blockThis := !p.firstDone
	p.mu.Unlock()
	if blockThis {
		return &queueBlockingStream{p: p, ctx: ctx}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.call >= len(p.turns) {
		return &scriptedStream{}, nil
	}
	ev := p.turns[p.call]
	p.call++
	return &scriptedStream{events: ev}, nil
}

type queueBlockingStream struct {
	p   *queueProv
	ctx context.Context
}

func (s *queueBlockingStream) Next() (provider.Event, error) {
	s.p.once.Do(func() { close(s.p.started) })
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.p.release:
		s.p.mu.Lock()
		s.p.firstDone = true
		s.p.mu.Unlock()
		msg := &message.Message{ID: "msg_released", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "released"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *queueBlockingStream) Close() error { return nil }

// TestQueuedPromptDispatchesOnDrain is invariant 4's dedicated test: a
// prompt queued while a session is busy is dispatched, FIFO, the instant the
// occupying turn ends — and the SSE ordering guarantee holds (the occupant's
// own idle transition is observed strictly before the dispatched prompt's
// busy).
func TestQueuedPromptDispatchesOnDrain(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("second done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started
	sse.waitFor(t, "session.status") // first's own busy

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("second prompt response = %+v, want status=queued queued=1", qr)
	}

	close(prov.release) // let the first turn finish

	// SSE ordering: the first turn's idle must precede the dispatched
	// second turn's busy.
	firstIdle := sse.waitFor(t, "session.status")
	if firstIdle.Status != "idle" {
		t.Fatalf("expected first turn's idle, got status %q", firstIdle.Status)
	}
	secondBusy := sse.waitFor(t, "session.status")
	if secondBusy.Status != "busy" {
		t.Fatalf("expected dispatched second turn's busy, got status %q", secondBusy.Status)
	}
	var asst Event
	for {
		asst = sse.waitFor(t, "message")
		if asst.Message != nil && asst.Message.Role == message.RoleAssistant {
			break
		}
	}
	if asst.Message.Parts.Text() != "second done" {
		t.Fatalf("dispatched turn text = %q, want %q", asst.Message.Parts.Text(), "second done")
	}
	secondIdle := sse.waitFor(t, "session.status")
	if secondIdle.Status != "idle" {
		t.Fatalf("expected second turn's own idle, got status %q", secondIdle.Status)
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after drain = %d, want 0", sess.Queued)
	}
}

// TestQueueDrainsFIFOAcrossMultiplePrompts extends invariant 4 to more than
// one queued prompt: three prompts queued while a session is busy must
// dispatch one turn at a time, strictly in enqueue (FIFO) order.
func TestQueueDrainsFIFOAcrossMultiplePrompts(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("r-a"), asstTurn("r-b"), asstTurn("r-c")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	for i, text := range []string{"a", "b", "c"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("prompt %q status %d: %s", text, resp.StatusCode, data)
		}
		var qr promptAsyncResponse
		if err := json.Unmarshal(data, &qr); err != nil {
			t.Fatal(err)
		}
		if qr.Status != "queued" || qr.Queued != i+1 {
			t.Fatalf("prompt %q response = %+v, want status=queued queued=%d", text, qr, i+1)
		}
	}

	close(prov.release)

	var gotOrder []string
	for len(gotOrder) < 3 {
		ev := sse.waitFor(t, "message")
		if ev.Message == nil || ev.Message.Role != message.RoleAssistant {
			continue
		}
		text := ev.Message.Parts.Text()
		if text == "released" {
			continue // the first (unrelated) turn's own assistant reply
		}
		gotOrder = append(gotOrder, text)
	}
	want := []string{"r-a", "r-b", "r-c"}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Errorf("dispatch order[%d] = %q, want %q (full order: %v)", i, gotOrder[i], want[i], gotOrder)
		}
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after full drain = %d, want 0", sess.Queued)
	}
}

// TestQueueLenExplicitOnEmptyingDequeue is the regression test for the
// queue_len omitempty ambiguity: a prompt.dequeued record that empties the
// queue (QueueLen 0) must still serialize an explicit "queue_len":0 on the
// wire — an omitempty int tag would drop the key entirely, indistinguishable
// from a dequeue record that never carried a queue_len at all. See
// server/journal.go's Event.QueueLen doc comment.
func TestQueueLenExplicitOnEmptyingDequeue(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("second done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("second prompt response = %+v, want status=queued queued=1", qr)
	}

	close(prov.release) // let the first turn finish; the drain dequeues "second", emptying the queue

	resp, data = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait for idle status %d: %s", resp.StatusCode, data)
	}

	h.srv.mu.Lock()
	var found *Event
	for i, ev := range h.srv.journal {
		if ev.SessionID == id && ev.Type == evtPromptDequeued && ev.QueueReason == "delivered" {
			found = &h.srv.journal[i]
		}
	}
	h.srv.mu.Unlock()
	if found == nil {
		t.Fatal("no prompt.dequeued(delivered) record found in the server's journal")
	}

	b, err := json.Marshal(*found)
	if err != nil {
		t.Fatalf("marshal dequeue event: %v", err)
	}
	if !strings.Contains(string(b), `"queue_len":0`) {
		t.Fatalf("dequeue event JSON = %s, want an explicit \"queue_len\":0 (queue emptied by this dequeue)", b)
	}
}

// TestWaitUntilIdleDoesNotWakeEarlyOnQueuedFollowUp is the regression test
// for a live, reproduced CI failure (TestQueueLenExplicitOnEmptyingDequeue
// above, root-caused): freeRunSlotAndEmitIdle (a completed turn's own idle
// transition) and maybeDispatchQueued (the SAME tail's immediate re-claim
// of the next queued item, if any — runPrompt, handlers.go) are two
// SEPARATE steps, not one atomic operation. In between them, the session
// genuinely reads not-running with its prompt queue still non-empty — and
// a GET /session/{id}/wait?until=idle waiter, woken by that transient idle
// event, used to observe it and return immediately, BEFORE the queue's own
// next item had even been dequeued yet. Not a data race — every access is
// correctly mutex-protected — a pure semantic gap: until=idle's own
// condition (waitConditionMet, wait.go) checked composite state alone,
// never the queue.
//
// Fixed by Server.queueDrainPending (its own doc comment, server.go):
// freeRunSlotAndEmitIdle sets it, still under the same s.mu hold that
// emits the "idle" event, and maybeDispatchQueued's own deferred
// clearQueueDrainPending resolves it once the redispatch attempt is
// settled one way or another. waitSnapshot (wait.go) folds it into the
// composite state it hands GET /session/{id}/wait, alongside st.running,
// from that same single s.mu-held read — no torn read possible. A
// boot-resumed session's queue (loadJournal never sets the flag) is
// deliberately unaffected: see
// TestWaitUntilIdleReturnsImmediatelyForBootResumedQueue (wait_test.go).
//
// Forces the exact race deterministically via two test-only seams
// (waitRegisteredRace, postIdleEmitRace — server.go) instead of relying
// on scheduling luck: waitRegisteredRace confirms the waiter is parked
// before "first" is released; postIdleEmitRace blocks runPrompt's own
// tail — after "first"'s idle transition (and queueDrainPending set) has
// already woken the waiter, before maybeDispatchQueued gets a chance to
// touch the queue or clear the flag — and, right there, calls
// waitSnapshot directly and asserts it does NOT read idle. Asserting on
// waitSnapshot itself, synchronously, in the exact window that matters,
// rather than inferring the answer from whichever wake a real waiter
// goroutine happens to process first, is what makes this deterministic:
// an earlier version of this test gated its assertion on the FIRST wake
// a real parked waiter received via a third seam (waitWakeCheckedRace,
// since removed) — but that first wake is often an earlier,
// correctly-not-met busy wake (the turn's own assistant-message journal,
// or its turn-end record), which consumed the guard before the transient
// idle wake under test ever fired. See the commit history for that
// version's own "failed 4/5" measurement against the exact pre-fix
// condition it was meant to catch.
func TestWaitUntilIdleDoesNotWakeEarlyOnQueuedFollowUp(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("second done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d: %s", resp.StatusCode, data)
	}

	registered := make(chan struct{})
	h.srv.waitRegisteredRace = func() {
		close(registered)
	}
	var calls atomic.Int32
	h.srv.postIdleEmitRace = func() {
		// Fires once per completed turn's tail — "first"'s own
		// completion (call 1), then "second"'s own completion (call 2,
		// once maybeDispatchQueued below has dispatched it). Only call
		// 1 lands in the transient window this fix targets: "first"'s
		// own idle has just been durably emitted (any waiter parked on
		// this session has already been woken, non-blocking, by
		// notifyWaitersLocked inside that same call) but
		// maybeDispatchQueued has not run yet, so the queue still shows
		// "second" pending. By call 2 the queue is genuinely empty
		// (dispatchQueueHead's own dequeue already reduced it to 0) and
		// this same state SHOULD read idle — asserting "not idle" there
		// too would be a false failure, not a guard.
		//
		// Calling waitSnapshot directly here — rather than relying on
		// the real waiter goroutine's own scheduling to have already
		// evaluated the wake by this point, which is not guaranteed —
		// red-verifies the NAMED mechanism deterministically: reverting
		// the queueDrainPending fold in waitSnapshot (wait.go) makes
		// this assertion fail on every run, not a probabilistic subset.
		if calls.Add(1) != 1 {
			return
		}
		if state, _ := h.srv.waitSnapshot(id); state == "idle" {
			t.Error("waitSnapshot reported idle while \"second\" is still queued and about to be redispatched by this same tail's own maybeDispatchQueued")
		}
	}

	type waitResult struct {
		wr  waitJSON
		err error
	}
	waitDone := make(chan waitResult, 1)
	go func() {
		resp, data := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
		if resp.StatusCode != http.StatusOK {
			waitDone <- waitResult{err: fmt.Errorf("wait status %d: %s", resp.StatusCode, data)}
			return
		}
		var wr waitJSON
		if err := json.Unmarshal(data, &wr); err != nil {
			waitDone <- waitResult{err: err}
			return
		}
		waitDone <- waitResult{wr: wr}
	}()
	<-registered // the waiter is parked, reachable by the wake "first" is about to fire

	close(prov.release) // let "first" finish; its own idle transition wakes the waiter

	// No time.After failsafe here (AGENTS.md: "No guessed deadlines...
	// let the test binary timeout catch hangs") — a genuine hang is
	// caught by `go test`'s own timeout, same as everywhere else in this
	// file.
	res := <-waitDone
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.wr.State != "idle" {
		t.Fatalf("wait returned state = %q, want idle (once genuinely settled — \"second\" must have already run to completion)", res.wr.State)
	}

	final := h.getSessionJSON(id)
	if final.Queued != 0 {
		t.Fatalf("final queued = %d, want 0 — \"second\" must have actually run, not merely been reported idle-and-abandoned", final.Queued)
	}
}

// TestFreeRunSlotAndEmitIdleSetsQueueDrainPendingUnconditionally is the
// regression test for a live review finding on freeRunSlotAndEmitIdle
// (handlers.go): an earlier version gated Server.queueDrainPending on a
// separate queueDepth cache (`if s.queueDepth[id] > 0`), populated by a
// SEPARATE event path (publishQueue -> emitDurableLocked) that can itself
// still be in flight relative to a concurrent enqueue — that enqueue's own
// append can land before its own cache update, right as a concurrent turn's
// own freeRunSlotAndEmitIdle reads the (still stale) cache. That
// reintroduces the exact false-idle class this whole fix exists to close,
// just triggered by an enqueue racing turn-end instead of a queue that was
// already non-empty going in.
//
// Fixed by setting queueDrainPending unconditionally, every time,
// regardless of what any cache says — maybeDispatchQueued's own deferred
// clearQueueDrainPending resolves it correctly either way (a genuinely
// empty queue just clears it again immediately, waking any waiter to
// re-observe idle). queueDepth itself was removed entirely (server.go,
// journal.go): with nothing left to read it, keeping it around would only
// be a second, now-pointless source to go stale from.
//
// Proven WITHOUT needing to force the exact lock-contention timing the live
// finding describes: an ordinary, single, never-queued turn's own
// completion is enough to distinguish the two versions. A queue that
// really is empty at turn-completion time has an ACCURATE (not stale)
// cache reading 0 under the OLD design — so the old gated code would
// correctly (for the wrong reason) never set the flag here, and a waiter
// parked in freeRunSlotAndEmitIdle's own window would see idle immediately,
// same as pre-fix. The NEW code sets it regardless, so the SAME waiter
// briefly sees not-idle in that same window before self-clearing moments
// later — this test asserts exactly that shape, red-verifying the
// "unconditional, not gated" mechanism directly rather than the specific
// race that motivated it.
func TestFreeRunSlotAndEmitIdleSetsQueueDrainPendingUnconditionally(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "solo"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	checked := make(chan struct{})
	h.srv.postIdleEmitRace = func() {
		defer close(checked)
		// The queue is genuinely, verifiably empty here — nothing was
		// ever enqueued for this session — so this is not a race window
		// at all under the OLD gated design: an accurate, non-stale
		// queueDepth[id]==0 would correctly skip setting the flag. Only
		// the NEW unconditional design sets it regardless.
		if n := h.getSessionJSON(id).Queued; n != 0 {
			t.Fatalf("queue depth = %d, want 0 (this test proves the UNCONDITIONAL set on an ordinary turn, not a real queued item)", n)
		}
		if state, _ := h.srv.waitSnapshot(id); state == "idle" {
			t.Error("waitSnapshot reported idle inside freeRunSlotAndEmitIdle's own window, before maybeDispatchQueued has resolved queueDrainPending — queueDrainPending must be set unconditionally here, not gated on any queue-depth cache")
		}
	}

	close(prov.release) // let the solo turn finish
	<-checked           // the in-window assertion above has run

	resp, data = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait status %d: %s", resp.StatusCode, data)
	}
	var wr waitJSON
	if err := json.Unmarshal(data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State != "idle" {
		t.Fatalf("wait state = %q, want idle (queueDrainPending must self-clear once maybeDispatchQueued finds nothing to dispatch)", wr.State)
	}
}

// TestQueueBeatsGoalAutoArm is invariant 5's dedicated test: when a turn ends
// with BOTH a non-empty queue and an armed goal, the queued prompt(s) must
// run first — no goal.eval/goal.achieved may appear until the queue is fully
// drained — and only then does the goal auto-arm.
func TestQueueBeatsGoalAutoArm(t *testing.T) {
	prov := &autoArmProv{
		name:    "test",
		blocked: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
		worker:  [][]provider.Event{asstTurn("queued-turn"), asstTurn("goal-turn")},
		eval:    [][]provider.Event{asstTurn("MET: done")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "queued"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queued prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("queued prompt response = %+v, want status=queued queued=1", qr)
	}

	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal while busy status %d: %s", resp.StatusCode, data)
	}
	var gr struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &gr); err != nil {
		t.Fatal(err)
	}
	if gr.Status != "armed" {
		t.Fatalf("POST goal while busy response status = %q, want armed", gr.Status)
	}

	prov.mu.Lock()
	prov.blocked = false
	prov.mu.Unlock()
	close(prov.release)

	// First batch: through the first prompt's own idle. No goal activity yet.
	firstEvs := sse.collectUntilIdle(t)
	for _, ev := range firstEvs {
		if ev.Type == "goal.eval" || ev.Type == "goal.achieved" {
			t.Fatalf("goal loop ran before the queued prompt drained: %v", firstEvs)
		}
	}

	// Second batch: the dispatched QUEUED prompt's own turn — still no goal
	// activity, proving the queue, not the armed goal, was dispatched.
	queuedEvs := sse.collectUntilIdle(t)
	var sawQueuedText bool
	for _, ev := range queuedEvs {
		if ev.Type == "goal.eval" || ev.Type == "goal.achieved" {
			t.Fatalf("goal loop ran before the queued prompt's own turn finished: %v", queuedEvs)
		}
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == "queued-turn" {
			sawQueuedText = true
		}
	}
	if !sawQueuedText {
		t.Fatalf("queued prompt's own assistant turn never arrived: %v", queuedEvs)
	}

	// Third batch: only now does the goal auto-arm and run to achievement.
	goalEvs := sse.collectUntilIdle(t)
	var sawAchieved bool
	for _, ev := range goalEvs {
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
	}
	if !sawAchieved {
		t.Fatalf("goal never achieved after the queue drained: %v", goalEvs)
	}
}

// TestQueuedDispatchAfterGoalLoopEnds is the runGoal-tail hook's dedicated
// test: a prompt enqueued while a goal loop's worker turn is genuinely in
// flight — after that turn's own boundary drain already ran, so the
// engine's per-turn injection never sees it — must still be dispatched once
// the loop terminates (goal achieved), via maybeDispatchQueued's new call at
// runGoal's tail.
func TestQueuedDispatchAfterGoalLoopEnds(t *testing.T) {
	prov := &autoArmProv{
		name:    "test",
		blocked: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
		eval:    [][]provider.Event{asstTurn("MET: done")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // the goal loop's own worker turn is in flight

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "queued"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queued prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("queued prompt response = %+v, want status=queued queued=1", qr)
	}

	// A scripted turn for the eventually-dispatched queued prompt.
	prov.mu.Lock()
	prov.worker = append(prov.worker, asstTurn("queued-done"))
	prov.blocked = false
	prov.mu.Unlock()
	close(prov.release)

	goalEvs := sse.collectUntilIdle(t)
	var sawAchieved bool
	for _, ev := range goalEvs {
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
	}
	if !sawAchieved {
		t.Fatalf("goal loop events = %v, want goal.achieved", goalEvs)
	}

	dispatchEvs := sse.collectUntilIdle(t)
	var sawBusy, sawText bool
	for _, ev := range dispatchEvs {
		if ev.Type == evtSessionStatus && ev.Status == "busy" {
			sawBusy = true
		}
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == "queued-done" {
			sawText = true
		}
	}
	if !sawBusy || !sawText {
		t.Fatalf("dispatch events after goal ended = %v, want a busy transition and %q", dispatchEvs, "queued-done")
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after dispatch = %d, want 0", sess.Queued)
	}
}

// TestQueueRestartRefoldNoAutoDispatch is invariant 8's dedicated test: a
// prompt enqueued in one process must survive a restart (surfaced as a
// count on GET /session, engine.Session.QueuedPrompts's own replay fold —
// see queue.go/store.go), and nothing may dispatch it on its own — the same
// "boot never auto-dispatches" rule already established for goals
// (pauseArmedGoalsAtBoot) — until the next natural drain trigger.
func TestQueueRestartRefoldNoAutoDispatch(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	srv1 := newServer(t, dir, prov, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")

	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, err := st.sess.EnqueuePrompt("queued before restart"); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	sess := h2.getSessionJSON(id)
	if sess.Queued != 1 {
		t.Fatalf("queued after restart = %d, want 1", sess.Queued)
	}
	if sess.State != "idle" {
		t.Fatalf("state after restart = %q, want idle (nothing dispatches on its own)", sess.State)
	}
	if sess.LastTurn != nil {
		t.Errorf("last_turn after restart with no drain trigger = %+v, want nil (nothing ran)", sess.LastTurn)
	}
}

// TestDeleteQueueClearsDurably is invariant 10's dedicated test:
// DELETE /session/{id}/queue drains every pending prompt (journaling
// prompt.dequeued reason="cleared" for each), is idempotent on an empty
// queue, and leaves a genuinely running turn completely untouched.
func TestDeleteQueueClearsDurably(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	t.Cleanup(prov.releaseAll)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("second prompt response = %+v, want status=queued queued=1", qr)
	}

	resp, _ = h.do("DELETE", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE queue status %d, want 204", resp.StatusCode)
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Fatalf("queued after DELETE = %d, want 0", sess.Queued)
	}
	if sess.State != "busy" {
		t.Fatalf("state after DELETE = %q, want busy (the running first turn is untouched)", sess.State)
	}

	// Idempotent on an already-empty queue.
	resp, _ = h.do("DELETE", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second DELETE queue status %d, want 204", resp.StatusCode)
	}

	// Durable: a prompt.dequeued(cleared) record landed on the server's
	// journal (see publishQueue) — the wire evidence an orchestrator
	// tailing /event would see, not just an in-memory reset.
	h.srv.mu.Lock()
	var found bool
	for _, ev := range h.srv.journal {
		if ev.SessionID == id && ev.Type == evtPromptDequeued && ev.QueueReason == "cleared" {
			found = true
		}
	}
	h.srv.mu.Unlock()
	if !found {
		t.Fatal("no prompt.dequeued(cleared) record found in the server's journal")
	}

	// Unknown session is 404.
	resp, _ = h.do("DELETE", "/session/ses_0000000000000000/queue", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE queue for unknown session status %d, want 404", resp.StatusCode)
	}
}

// TestPromptQueueRaceWithFreedSlot forces maybeDispatchQueued's losing-race
// path deterministically (mirrors TestAutoArmRaceWithIncomingPrompt): when
// the just-freed run slot is claimed by a concurrent incoming prompt_async
// before maybeDispatchQueued's own claim lands, maybeDispatchQueued must
// return cleanly rather than double-dispatch — and the queued prompt is
// never stranded.
//
// The racer's OWN claim wins the race (proving maybeDispatchQueued's later
// claim attempt loses and returns cleanly), but per the global-FIFO fix
// (Gap 1: handlePrompt's claim-success path enqueues-then-dispatches-head
// whenever the queue is non-empty) the racer does NOT get to run its own
// text just because it won the claim: the prompt already queued ahead of it
// must go first. So the racer's own request enqueues "racer" behind the
// existing "queued" entry, then dispatches the queue's HEAD (the original
// "queued" prompt, not its own text) into the slot it just claimed — its own
// response is "queued", not "started". The queued prompt's own tail then
// drains "racer" next, uncontested. End state: both run, strictly FIFO
// (queued, then racer), queue ends empty.
//
// One thing about this scenario is inherently racy — genuinely unspecified
// by any documented invariant, not a bug — and this test must not assert an
// exact outcome for it: the racer's own prompt_async response reads
// len(st.sess.QueuedPrompts()) AFTER the just-dequeued "queued" turn's
// goroutine has already been spawned (`go s.runPrompt(...)`, a non-blocking
// statement). That spawned goroutine can race ahead far enough to dequeue
// (and even finish) "racer" too before this response line ever runs, so the
// reported depth can legitimately read 0 as well as 1 — the same class of
// race TestQueueClearRaceDuringIdleDispatchIsNotAnError already documents
// for a sibling code path. Asserting exactly 1 here flakes on nothing but
// scheduling speed.
//
// What IS a real, load-bearing invariant — enforced by
// Server.freeRunSlotAndEmitIdle's single locked critical section — is the
// one collectUntilIdle's own doc comment names: "session.status busy for
// [a] dispatched turn always arrives strictly after [the freeing turn's]
// idle." Before that fix, runPrompt's tail reset st.running=false and
// journaled the idle event as two SEPARATE s.mu critical sections; a
// concurrent claimForPrompt (this test's own racer, retried from
// maybeDispatchQueued on the ORIGINAL "first" turn's still-unwinding
// goroutine) could slip into that gap, observe running==false, and win the
// next dispatch BEFORE the freeing turn's own idle had been journaled. The
// freeing turn's idle would then land AFTER the entire next turn it was
// supposed to precede — collapsing collectUntilIdle's per-turn boundary, so
// the SECOND collectUntilIdle call below (which should stop at "queued"'s
// own idle) instead over-reads straight through "racer"'s whole turn too,
// leaving the THIRD call nothing but a stray, mis-ordered idle. This test
// asserts that boundary directly: queuedEvs must contain "queued"'s own
// message and must NOT already contain "racer"'s (proving the two turns'
// idle/busy boundaries never collapsed), and racerEvs — read strictly
// after — must contain "racer"'s message (proving it was never stranded
// either).
func TestPromptQueueRaceWithFreedSlot(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("queued done"), asstTurn("racer done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "queued"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queued prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("queued prompt response = %+v, want status=queued queued=1", qr)
	}

	var raced bool
	h.srv.queueDispatchRace = func() {
		if raced {
			return
		}
		raced = true
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "racer"}},
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("racer prompt status %d: %s", resp.StatusCode, data)
		}
		var rr promptAsyncResponse
		if err := json.Unmarshal(data, &rr); err != nil {
			t.Fatal(err)
		}
		// Status is deterministic: handlePrompt's idle-with-queue branch
		// always dequeues the current HEAD ("queued", enqueued well before
		// "racer" existed) into the slot this request just claimed, so this
		// request's own text can never be the one dispatched — see the
		// doc comment above for why Queued's exact depth is NOT asserted.
		if rr.Status != "queued" {
			t.Fatalf("racer prompt response = %+v, want status=queued (it wins the freed slot, but the already-queued prompt still goes first — global FIFO)", rr)
		}
		if rr.Queued > 1 {
			t.Fatalf("racer prompt response = %+v, want queued depth at most 1", rr)
		}
	}

	close(prov.release) // first turn finishes; its tail's maybeDispatchQueued call fires the seam above

	firstEvs := sse.collectUntilIdle(t)
	_ = firstEvs // just drains through the first turn's own idle

	hasAssistantText := func(evs []Event, want string) bool {
		for _, ev := range evs {
			if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == want {
				return true
			}
		}
		return false
	}

	// The already-QUEUED prompt's own turn ran first — dispatched into the
	// slot the racer's request claimed — proving global FIFO held even
	// though the racer won the claim race and maybeDispatchQueued's own
	// later claim attempt lost and returned cleanly rather than
	// double-dispatching. Critically, this read must stop exactly at
	// "queued"'s own idle: if freeRunSlotAndEmitIdle's atomicity ever
	// regresses, "racer"'s entire turn (busy, message, idle) collapses into
	// THIS read too — the regression this test exists to catch (see the
	// doc comment above).
	queuedEvs := sse.collectUntilIdle(t)
	if !hasAssistantText(queuedEvs, "queued done") {
		t.Fatalf("queued prompt events = %v, want %q", queuedEvs, "queued done")
	}
	if hasAssistantText(queuedEvs, "racer done") {
		t.Fatalf("queued prompt events = %v, want to NOT already contain %q -- \"queued\"'s own idle must be journaled before \"racer\"'s turn starts, not after it finishes (freeRunSlotAndEmitIdle's atomicity)", queuedEvs, "racer done")
	}

	// Never stranded: the queued prompt's own tail drains "racer" next,
	// uncontested. Reading strictly after queuedEvs's own idle boundary (the
	// assertion above) is what makes this a real, independent check rather
	// than something the previous read could have smuggled in.
	racerEvs := sse.collectUntilIdle(t)
	if !hasAssistantText(racerEvs, "racer done") {
		t.Fatalf("racer turn events = %v, want %q", racerEvs, "racer done")
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after both turns dispatched = %d, want 0", sess.Queued)
	}
}

// orderCaptureProv records the last user-message text of every Stream call
// (the prompt actually delivered to a turn) in call order, and replies with
// a scripted, uniquely-identifiable text per call — so a test can verify
// dispatch order two independent ways: what the provider actually received,
// and what the SSE stream's assistant messages report back.
type orderCaptureProv struct {
	name string
	mu   sync.Mutex

	order   []string
	replies []string
	call    int
	// calls, when non-nil, receives a value at the START of every Stream
	// call, before this provider does anything else — a channel-based
	// synchronization point a test can block on to know a specific call
	// has begun, instead of a guessed time.Sleep (AGENTS.md's testing
	// rule: "No raw time.Sleep for synchronization — ever"). Must be
	// buffered with enough capacity for every call a test expects, since
	// nothing ever drains it if a test chooses not to receive from it —
	// nil is a safe no-op (every send is skipped) for callers that don't
	// need this.
	calls chan struct{}
}

func (p *orderCaptureProv) Name() string { return p.name }

func (p *orderCaptureProv) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls != nil {
		p.calls <- struct{}{}
	}
	var text string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == message.RoleUser {
			text = req.Messages[i].Parts.Text()
			break
		}
	}
	p.order = append(p.order, text)
	reply := fmt.Sprintf("done-%d", p.call)
	if p.call < len(p.replies) {
		reply = p.replies[p.call]
	}
	p.call++
	return &scriptedStream{events: asstTurn(reply)}, nil
}

// TestIdlePromptWithQueueGoesFIFO is the fix for Gap 1: a prompt arriving at
// an IDLE session whose durable queue is already non-empty (here, refolded
// from a restart) must not jump the FIFO line. handlePrompt's claim-success
// path must enqueue the incoming prompt behind the existing two, then
// dispatch the queue's HEAD into the slot it just claimed — never the
// incoming prompt itself, unless it happens to also be the head (the
// queue-was-actually-empty degenerate case, exercised elsewhere).
func TestIdlePromptWithQueueGoesFIFO(t *testing.T) {
	dir := t.TempDir()
	prov := &orderCaptureProv{name: "test", replies: []string{"r1", "r2", "r3"}}
	srv1 := newServer(t, dir, prov, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")

	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, err := st.sess.EnqueuePrompt("q1"); err != nil {
		t.Fatalf("EnqueuePrompt q1: %v", err)
	}
	if _, err := st.sess.EnqueuePrompt("q2"); err != nil {
		t.Fatalf("EnqueuePrompt q2: %v", err)
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	sess := h2.getSessionJSON(id)
	if sess.Queued != 2 {
		t.Fatalf("queued after restart = %d, want 2", sess.Queued)
	}
	if sess.State != "idle" {
		t.Fatalf("state after restart = %q, want idle", sess.State)
	}

	sse := h2.openSSE("?from=0", "")

	resp, data := h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "third"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 2 {
		t.Fatalf("response = %+v, want status=queued queued=2 (FIFO: the two restart-refolded prompts still ahead of this one)", qr)
	}

	// All three turns must run, in FIFO order (q1, q2, third), draining one
	// at a time.
	var gotOrder []string
	want := []string{"r1", "r2", "r3"}
	for len(gotOrder) < 3 {
		ev := sse.waitFor(t, "message")
		if ev.Message == nil || ev.Message.Role != message.RoleAssistant {
			continue
		}
		gotOrder = append(gotOrder, ev.Message.Parts.Text())
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Errorf("dispatch order[%d] = %q, want %q (full order: %v)", i, gotOrder[i], want[i], gotOrder)
		}
	}

	prov.mu.Lock()
	gotPrompts := append([]string(nil), prov.order...)
	prov.mu.Unlock()
	wantPrompts := []string{"q1", "q2", "third"}
	if len(gotPrompts) != len(wantPrompts) {
		t.Fatalf("provider-observed prompt order = %v, want %v", gotPrompts, wantPrompts)
	}
	for i := range wantPrompts {
		if gotPrompts[i] != wantPrompts[i] {
			t.Errorf("provider-observed prompt order[%d] = %q, want %q (full order: %v)", i, gotPrompts[i], wantPrompts[i], gotPrompts)
		}
	}

	finalSess := h2.getSessionJSON(id)
	if finalSess.Queued != 0 {
		t.Errorf("queued after full drain = %d, want 0", finalSess.Queued)
	}
}

// TestIdlePromptWithQueueDispatchDoesNotRaceQueuedCountInResponse is the
// regression test for a live, intermittent CI failure
// (TestIdlePromptWithQueueGoesFIFO itself, seen twice: once during the
// subagent-sessions PR series, once on main — both times as a fast, 0.02s
// assertion mismatch, never a timeout, and never caught despite 5,700+
// combined local -race iterations across every GOMAXPROCS/concurrency/GC-
// pressure combination tried).
//
// Root cause: dispatchQueueHead spawns the dispatched head's own runPrompt
// turn in a goroutine and returns immediately — nothing waits for it. With
// a fast enough provider (a scripted test double, or simply an unlucky
// real one), that goroutine can run the ENTIRE turn to completion — and,
// via its own tail's maybeDispatchQueued, dispatch (and itself complete)
// the NEXT queued item too — before the calling goroutine's own next
// statement ever runs. Every caller used to compute its response's queued
// depth by re-reading QueuedPrompts() AFTER dispatchQueueHead returned,
// racing that goroutine. Not a data race — every access is correctly
// mutex-protected, which is exactly why -race never caught it — a pure
// ordering assumption ("the item I just dispatched is still running, so
// the queue behind it is untouched") nothing in the code actually
// enforced.
//
// Forces the race deterministically via dispatchQueueHeadRace (test-only
// seam, server.go) instead of relying on scheduling luck: the hook blocks
// on orderCaptureProv's own calls channel until the dispatched turn's
// Stream call AND its own chained dispatch's Stream call have both
// actually started — channel-based, not a guessed time.Sleep (AGENTS.md's
// testing rule: "No raw time.Sleep for synchronization — ever"). By the
// time a dispatched item's own Stream call starts, dispatchQueueHead has
// already dequeued it (dequeue always precedes the goroutine spawn that
// eventually calls Stream) — sufficient to reproduce the exact window an
// unlucky real scheduling decision opened in CI, deterministically, on
// every single run. Asserts the response's own Queued count is unaffected
// by that race window having been forced open, proving the fix
// (dispatchQueueHead's own remaining return value, snapshotted atomically
// with the dequeue itself, before the goroutine is ever spawned) closes it
// structurally rather than merely making it rarer.
func TestIdlePromptWithQueueDispatchDoesNotRaceQueuedCountInResponse(t *testing.T) {
	dir := t.TempDir()
	calls := make(chan struct{}, 3)
	prov := &orderCaptureProv{name: "test", replies: []string{"r1", "r2", "r3"}, calls: calls}
	srv1 := newServer(t, dir, prov, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")

	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, err := st.sess.EnqueuePrompt("q1"); err != nil {
		t.Fatalf("EnqueuePrompt q1: %v", err)
	}
	if _, err := st.sess.EnqueuePrompt("q2"); err != nil {
		t.Fatalf("EnqueuePrompt q2: %v", err)
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0)
	var raceOnce sync.Once
	srv2.dispatchQueueHeadRace = func() {
		// dispatchQueueHead is called three times in this test's own
		// flow (this POST's own dispatch of q1, then q1's own tail
		// chain-dispatching q2, then possibly q2's own tail
		// chain-dispatching "third") — only the FIRST call is this
		// POST handler's own, the one whose response the assertion
		// below checks; block only there.
		raceOnce.Do(func() {
			<-calls // q1's own Stream call has started (already dequeued)
			<-calls // q2's own Stream call has started (already dequeued)
		})
	}
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	sess := h2.getSessionJSON(id)
	if sess.Queued != 2 {
		t.Fatalf("queued after restart = %d, want 2", sess.Queued)
	}

	resp, data := h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "third"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 2 {
		t.Fatalf("response = %+v, want status=queued queued=2 (the two restart-refolded prompts still ahead of this one, regardless of how far the dispatched turn raced ahead before this response was built)", qr)
	}
}

// TestQueuedArrivalDoesNotRetargetSessionModel is the regression test for
// the SetModel leak: handlePrompt's claim-success path used to call SetModel
// on a per-request model override BEFORE checking whether the queue was
// non-empty, so an idle-with-queue arrival retargeted the SESSION's model
// even though a DIFFERENT, already-queued head is what actually gets
// dispatched into the run slot -- contradicting the documented "a per-request
// model override is silently dropped when the prompt is queued" rule (see
// AGENTS.md's Prompt queue section and enqueueOrDispatch's identical rule for
// the same-session-busy branch).
//
// The override here names a provider that is NOT registered
// ("bogus/doesnotexist"): if the leak were still present, the dispatched
// head's own turn would run under the retargeted (bogus) model and fail to
// resolve a provider, surfacing as a turn error -- a strong, deterministic
// signal that the override bled into the wrong turn, not just a same-value
// coincidence.
func TestQueuedArrivalDoesNotRetargetSessionModel(t *testing.T) {
	dir := t.TempDir()
	prov := &orderCaptureProv{name: "test", replies: []string{"r1", "r2"}}
	srv1 := newServer(t, dir, prov, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")

	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, err := st.sess.EnqueuePrompt("q1"); err != nil {
		t.Fatalf("EnqueuePrompt q1: %v", err)
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	// Restart: idle, one prompt already queued -- the same idle-with-queue
	// shape TestIdlePromptWithQueueGoesFIFO exercises.
	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	before := h2.getSessionJSON(id)
	if before.Queued != 1 || before.Model.String() != "test/m1" {
		t.Fatalf("before override: queued=%d model=%q, want queued=1 model=test/m1", before.Queued, before.Model.String())
	}

	resp, data := h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "third"}},
		"model": "bogus/doesnotexist",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("response = %+v, want status=queued queued=1 (q1 is the dispatched head, not this arrival)", qr)
	}

	// The money assertion: the session's own model must be untouched by an
	// override on a request whose prompt got queued, not dispatched.
	afterResp := h2.getSessionJSON(id)
	if afterResp.Model.String() != "test/m1" {
		t.Fatalf("session model after queued-path override = %q, want unchanged test/m1", afterResp.Model.String())
	}

	// Let the dispatched head (q1) run to completion: it must succeed under
	// the ORIGINAL model, never the dropped override.
	resp, data = h2.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait for idle status %d: %s", resp.StatusCode, data)
	}

	final := h2.getSessionJSON(id)
	if final.Model.String() != "test/m1" {
		t.Fatalf("final session model = %q, want unchanged test/m1", final.Model.String())
	}
	if final.LastTurn == nil || final.LastTurn.Outcome != "completed" {
		t.Fatalf("dispatched head's turn outcome = %+v, want completed (a bled-in bogus model would fail to resolve a provider)", final.LastTurn)
	}
}

// compactProv splits behavior by tool presence: an ordinary (tool-bearing)
// prompt turn is served immediately from worker, scripted; the tool-less
// compaction summarization call (see engine/compact.go's
// runCompactionSummary, which sends no Tools) blocks until release is
// closed, giving a test a deterministic window in which a compact call is
// genuinely in flight.
type compactProv struct {
	name string
	mu   sync.Mutex

	worker [][]provider.Event
	wi     int

	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *compactProv) Name() string { return p.name }

func (p *compactProv) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 {
		return &compactBlockingStream{p: p, ctx: ctx}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// compactBlockingStream backs the compaction summarization call.
// runCompactionSummary's own Next loop (unlike an ordinary turn's streamTurn,
// which returns immediately on EventDone) keeps calling Next until it sees
// io.EOF, so this must report EventDone exactly once, then EOF forever after
// — otherwise a closed release channel keeps winning the select on every
// subsequent call and the summarization loop never terminates.
type compactBlockingStream struct {
	p    *compactProv
	ctx  context.Context
	sent bool
}

func (s *compactBlockingStream) Next() (provider.Event, error) {
	if s.sent {
		return provider.Event{}, io.EOF
	}
	s.p.once.Do(func() { close(s.p.started) })
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.p.release:
		s.sent = true
		msg := &message.Message{ID: "msg_summary", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "SUMMARY"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *compactBlockingStream) Close() error { return nil }

// TestCompactTailDispatchesQueue is the fix for Gap 2: a prompt queued while
// a compact call is genuinely in flight must be dispatched the instant
// compact's own tail releases the run slot — handleCompact's tail must call
// maybeDispatchQueued, exactly like runPrompt's and runGoal's tails already
// do.
func TestCompactTailDispatchesQueue(t *testing.T) {
	prov := &compactProv{
		name:    "test",
		worker:  [][]provider.Event{asstTurn("go1-done"), asstTurn("go2-done")},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")

	sse := h.openSSE("?from=0", "")

	compactDone := make(chan struct{})
	var compactResp *http.Response
	var compactData []byte
	go func() {
		compactResp, compactData = h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
		close(compactDone)
	}()
	<-prov.started // the compaction summarization call is genuinely in flight

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "queued"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queued prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("queued prompt response = %+v, want status=queued queued=1", qr)
	}

	prov.mu.Lock()
	prov.worker = append(prov.worker, asstTurn("queued-done"))
	prov.mu.Unlock()
	close(prov.release)

	<-compactDone
	if compactResp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", compactResp.StatusCode, compactData)
	}

	var gotText bool
	for !gotText {
		ev := sse.waitFor(t, "message")
		if ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == "queued-done" {
			gotText = true
		}
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after dispatch = %d, want 0", sess.Queued)
	}
}

// compactGoalProv extends compactProv's tool-presence split with a third
// case: a tool-less call whose MaxTokens is the goal evaluator's constant
// (256, engine/goal.go) is the evaluator, distinguished from the tool-less
// compaction summarization call (MaxTokens 1024, engine/compact.go's
// compactionMaxTokens) by that value.
type compactGoalProv struct {
	name string
	mu   sync.Mutex

	worker [][]provider.Event
	wi     int
	eval   [][]provider.Event
	ei     int

	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *compactGoalProv) Name() string { return p.name }

func (p *compactGoalProv) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) > 0 {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.wi >= len(p.worker) {
			return &scriptedStream{}, nil
		}
		ev := p.worker[p.wi]
		p.wi++
		return &scriptedStream{events: ev}, nil
	}
	if req.MaxTokens == 256 { // goal evaluator, see engine/goal.go
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	// Compaction summarization call: block until release.
	return &compactGoalBlockingStream{p: p, ctx: ctx}, nil
}

// compactGoalBlockingStream has the same one-EventDone-then-EOF shape as
// compactBlockingStream (see its doc comment) — required for the same
// reason: runCompactionSummary's Next loop only stops on io.EOF.
type compactGoalBlockingStream struct {
	p    *compactGoalProv
	ctx  context.Context
	sent bool
}

func (s *compactGoalBlockingStream) Next() (provider.Event, error) {
	if s.sent {
		return provider.Event{}, io.EOF
	}
	s.p.once.Do(func() { close(s.p.started) })
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.p.release:
		s.sent = true
		msg := &message.Message{ID: "msg_summary", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "SUMMARY"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *compactGoalBlockingStream) Close() error { return nil }

// TestCompactTailAutoArmsGoal is Gap 2's other half: a goal armed while a
// compact call is genuinely in flight (POST /goal's "armed" 202, same as an
// ordinary busy prompt) must auto-arm the instant compact's tail releases the
// run slot — handleCompact's tail must call maybeAutoArmGoal, same
// precedence as runPrompt's tail (queue first, then goal auto-arm).
func TestCompactTailAutoArmsGoal(t *testing.T) {
	prov := &compactGoalProv{
		name:    "test",
		worker:  [][]provider.Event{asstTurn("go1-done"), asstTurn("go2-done"), asstTurn("goal-turn")},
		eval:    [][]provider.Event{asstTurn("MET: done")},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")

	sse := h.openSSE("?from=0", "")

	compactDone := make(chan struct{})
	var compactResp *http.Response
	var compactData []byte
	go func() {
		compactResp, compactData = h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 1})
		close(compactDone)
	}()
	<-prov.started // the compaction summarization call is genuinely in flight

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal while compact busy status %d: %s", resp.StatusCode, data)
	}
	var gr struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &gr); err != nil {
		t.Fatal(err)
	}
	if gr.Status != "armed" {
		t.Fatalf("POST goal while compact busy response status = %q, want armed", gr.Status)
	}

	close(prov.release)

	<-compactDone
	if compactResp.StatusCode != http.StatusOK {
		t.Fatalf("compact status %d: %s", compactResp.StatusCode, compactData)
	}

	sse.waitFor(t, "goal.achieved")

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued = %d, want 0", sess.Queued)
	}
}
