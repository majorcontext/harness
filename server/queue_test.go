package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
		if rr.Status != "queued" || rr.Queued != 1 {
			t.Fatalf("racer prompt response = %+v, want status=queued queued=1 (it wins the freed slot, but the already-queued prompt still goes first — global FIFO)", rr)
		}
	}

	close(prov.release) // first turn finishes; its tail's maybeDispatchQueued call fires the seam above

	firstEvs := sse.collectUntilIdle(t)
	_ = firstEvs // just drains through the first turn's own idle

	// The already-QUEUED prompt's own turn ran first — dispatched into the
	// slot the racer's request claimed — proving global FIFO held even
	// though the racer won the claim race and maybeDispatchQueued's own
	// later claim attempt lost and returned cleanly rather than
	// double-dispatching.
	queuedEvs := sse.collectUntilIdle(t)
	var sawQueuedText bool
	for _, ev := range queuedEvs {
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == "queued done" {
			sawQueuedText = true
		}
	}
	if !sawQueuedText {
		t.Fatalf("queued prompt events = %v, want %q", queuedEvs, "queued done")
	}

	// Never stranded: the queued prompt's own tail drains "racer" next,
	// uncontested.
	racerEvs := sse.collectUntilIdle(t)
	var sawRacerText bool
	for _, ev := range racerEvs {
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == "racer done" {
			sawRacerText = true
		}
	}
	if !sawRacerText {
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
}

func (p *orderCaptureProv) Name() string { return p.name }

func (p *orderCaptureProv) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
