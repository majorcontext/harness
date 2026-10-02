package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const resumeWait = 30 * time.Second

func (s *sseStream) waitUntil(t *testing.T, match func(Event) bool) Event {
	t.Helper()
	deadline := time.NewTimer(resumeWait)
	defer deadline.Stop()
	for {
		select {
		case it, ok := <-s.items:
			if !ok {
				t.Fatal("sse stream closed before the awaited event")
			}
			if !it.heartbeat && match(it.ev) {
				return it.ev
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for an sse event")
		}
	}
}

func (s *sseStream) waitType(t *testing.T, typ string) Event {
	t.Helper()
	return s.waitUntil(t, func(ev Event) bool { return ev.Type == typ })
}

func interruptedSession(t *testing.T, store engine.SessionStore, text string) string {
	t.Helper()
	prov := &cancelWatchProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	owner := newFakeOwner()
	h := newOwnerServer(t, store, prov, func(o *Options) { o.SessionOwner = owner })
	id := h.createSession("")
	if resp, body := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}}); resp.StatusCode != 202 {
		t.Fatalf("prompt status = %d: %s", resp.StatusCode, body)
	}
	<-prov.started
	owner.loseOwnership(id)
	<-prov.canceled
	h.srv.wg.Wait()
	h.srv.Close()
	return id
}

func appendResumed(t *testing.T, store engine.SessionStore, id string, counts ...int) {
	t.Helper()
	for _, n := range counts {
		at, err := store.Len(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(id, at, []byte(fmt.Sprintf(`{"type":"turn.resumed","count":%d}`, n))); err != nil {
			t.Fatal(err)
		}
	}
}

func journalHas(recs []map[string]any, typ string) bool {
	for _, r := range recs {
		if r["type"] == typ {
			return true
		}
	}
	return false
}

func providerCalls(p *scriptedProvider) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call
}

func (h *harness) openSSEFromHead() *sseStream {
	h.srv.mu.Lock()
	from := h.srv.seq
	h.srv.mu.Unlock()
	return h.openSSE("", strconv.FormatInt(from, 10))
}

func resumeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), resumeWait)
	t.Cleanup(cancel)
	return ctx
}

func TestResumeSessionContinuesInterruptedTurn(t *testing.T) {
	store := engine.NewMemStore()
	id := interruptedSession(t, store, "q")

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("a")}}
	h := newOwnerServer(t, store, prov)
	sse := h.openSSEFromHead()

	if err := h.srv.ResumeSession(resumeCtx(t), id); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	busy := sse.waitUntil(t, func(ev Event) bool { return ev.Type == "session.status" && ev.Status == "busy" })
	if busy.TurnID == "" {
		t.Error("busy event has no turn_id")
	}
	resumed := sse.waitType(t, "turn.resumed")
	if resumed.ResumeCount != 1 {
		t.Errorf("turn.resumed resume_count = %d, want 1", resumed.ResumeCount)
	}
	reply := sse.waitUntil(t, func(ev Event) bool {
		return ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant
	})
	if got := msgText(reply.Message); got != "a" {
		t.Errorf("reply = %q, want %q", got, "a")
	}
	end := sse.waitType(t, "turn.end")
	if end.Outcome != "completed" || end.TurnID != busy.TurnID {
		t.Errorf("turn.end = outcome %q turn %q, want completed on %q", end.Outcome, end.TurnID, busy.TurnID)
	}
	sse.waitUntil(t, func(ev Event) bool { return ev.Type == "session.status" && ev.Status == "idle" })
	h.srv.wg.Wait()

	recs := journalTypes(t, store, id)
	n := len(recs) - 1
	for n > 0 && recs[n]["type"] != "message" {
		n--
	}
	if _, role, text := tailMessageText(recs[n]); role != "assistant" || text != "a" {
		t.Fatalf("last message record = %v, want assistant message a", recs[n])
	}
	if recs[n-1]["type"] != "turn.resumed" || recs[n-1]["count"] != float64(1) {
		t.Errorf("record before the reply = %v, want turn.resumed count 1", recs[n-1])
	}
}

func TestResumeSessionNoUnfinishedTurnIsNoOp(t *testing.T) {
	store := engine.NewMemStore()
	seed := newOwnerServer(t, store, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}})
	id := seed.createSession("")
	sse := seed.openSSE("", "")
	seed.startTurn(sse, id, "q")
	sse.waitUntil(t, func(ev Event) bool { return ev.Type == "session.status" && ev.Status == "idle" })
	seed.srv.wg.Wait()
	seed.srv.Close()

	prov := &scriptedProvider{name: "test"}
	h := newOwnerServer(t, store, prov)
	if err := h.srv.ResumeSession(resumeCtx(t), id); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	h.srv.wg.Wait()
	if n := providerCalls(prov); n != 0 {
		t.Errorf("provider received %d requests, want none", n)
	}
	if journalHas(journalTypes(t, store, id), "turn.resumed") {
		t.Error("journal holds a turn.resumed record")
	}
}

func TestResumeSessionStoppedTurnIsNoOp(t *testing.T) {
	store := engine.NewMemStore()
	stopProv := newStopProvider("par", "never")
	seed := newOwnerServer(t, store, stopProv)
	id := seed.createSession("")
	sse := seed.openSSE("", "")
	turnID := seed.startTurn(sse, id, "q")
	<-stopProv.started
	seed.abortBody(id, `{"turn_id":"`+turnID+`"}`)
	seed.srv.wg.Wait()
	seed.srv.Close()

	prov := &scriptedProvider{name: "test"}
	h := newOwnerServer(t, store, prov)
	if err := h.srv.ResumeSession(resumeCtx(t), id); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	h.srv.wg.Wait()
	if n := providerCalls(prov); n != 0 {
		t.Errorf("provider received %d requests, want none", n)
	}
	recs := journalTypes(t, store, id)
	if journalHas(recs, "turn.resumed") {
		t.Error("journal holds a turn.resumed record")
	}
	if last := recs[len(recs)-1]["type"]; last != "turn.stopped" {
		t.Errorf("last record = %v, want turn.stopped", last)
	}
}

func TestResumeSessionCapClosesLost(t *testing.T) {
	store := engine.NewMemStore()
	id := interruptedSession(t, store, "q")
	appendResumed(t, store, id, 1, 2, 3)

	prov := &scriptedProvider{name: "test"}
	h := newOwnerServer(t, store, prov)
	sse := h.openSSEFromHead()
	if err := h.srv.ResumeSession(resumeCtx(t), id); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	end := sse.waitType(t, "turn.end")
	if end.Outcome != "lost" {
		t.Errorf("turn.end outcome = %q, want lost", end.Outcome)
	}
	h.srv.wg.Wait()
	if n := providerCalls(prov); n != 0 {
		t.Errorf("provider received %d requests, want none", n)
	}
	recs := journalTypes(t, store, id)
	var last map[string]any
	for _, r := range recs {
		if r["type"] == "message" {
			last = r
		}
	}
	_, role, text := tailMessageText(last)
	if role == "" || !bytes.Contains([]byte(text), []byte("interrupted by a process restart")) {
		t.Errorf("last message = %s %q, want the lost-to-restart marker", role, text)
	}
}

func TestResumeSessionWhileRunningIsNoOp(t *testing.T) {
	store := engine.NewMemStore()
	id := interruptedSession(t, store, "q")

	prov := newStopProvider("", "a")
	h := newOwnerServer(t, store, prov)
	if err := h.srv.ResumeSession(resumeCtx(t), id); err != nil {
		t.Fatalf("first ResumeSession: %v", err)
	}
	select {
	case <-prov.started:
	case <-time.After(resumeWait):
		t.Fatal("resumed turn never reached the provider")
	}
	if err := h.srv.ResumeSession(resumeCtx(t), id); err != nil {
		t.Fatalf("second ResumeSession: %v", err)
	}
	close(prov.release)
	h.srv.wg.Wait()

	if n := prov.calls.Load(); n != 1 {
		t.Errorf("provider saw %d requests, want 1", n)
	}
	for _, r := range journalTypes(t, store, id) {
		if r["type"] == "turn.resumed" && r["count"] != float64(1) {
			t.Errorf("record %v: the second call dispatched a resume", r)
		}
	}
}

func TestResumeSessionNotOwned(t *testing.T) {
	inner := engine.NewMemStore()
	id := interruptedSession(t, inner, "q")
	store := &loadRecordingStore{SessionStore: inner}
	owner := newFakeOwner()
	owner.err = errors.New("lease held elsewhere")
	h := newOwnerServer(t, store, &scriptedProvider{name: "test"}, func(o *Options) { o.SessionOwner = owner })
	store.mu.Lock()
	store.loadIDs = nil
	store.mu.Unlock()

	err := h.srv.ResumeSession(resumeCtx(t), id)
	if !errors.Is(err, ErrSessionNotOwned) {
		t.Fatalf("ResumeSession = %v, want ErrSessionNotOwned", err)
	}
	if store.loaded(id) {
		t.Errorf("Load(%q) ran although Acquire refused", id)
	}
	if n := owner.acquireCalls(id); n != 1 {
		t.Errorf("Acquire called %d times, want 1", n)
	}
}

func TestResumeSessionUnknown(t *testing.T) {
	h := newOwnerServer(t, engine.NewMemStore(), &scriptedProvider{name: "test"})
	err := h.srv.ResumeSession(resumeCtx(t), "ses_00000000000000000000000000")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResumeSession = %v, want an error wrapping fs.ErrNotExist", err)
	}
}
