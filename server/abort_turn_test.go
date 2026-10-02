package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type stopProvider struct {
	text     string
	reply    string
	started  chan struct{}
	release  chan struct{}
	canceled atomic.Bool
	calls    atomic.Int32
}

func newStopProvider(text, reply string) *stopProvider {
	return &stopProvider{text: text, reply: reply, started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (p *stopProvider) Name() string { return "test" }

func (p *stopProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.calls.Add(1)
	return &stopStream{p: p, ctx: ctx}, nil
}

type stopStream struct {
	p       *stopProvider
	ctx     context.Context
	emitted bool
}

func (s *stopStream) Next() (provider.Event, error) {
	if !s.emitted && s.p.text != "" {
		s.emitted = true
		return provider.Event{Type: provider.EventTextDelta, Text: s.p.text, ID: "msg_partial"}, nil
	}
	s.p.started <- struct{}{}
	select {
	case <-s.p.release:
		msg := &message.Message{ID: "msg_reply", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: s.p.reply}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	case <-s.ctx.Done():
		s.p.canceled.Store(true)
		return provider.Event{}, s.ctx.Err()
	}
}

func (s *stopStream) Close() error { return nil }

func (h *harness) startTurn(sse *sseStream, id, text string) string {
	h.t.Helper()
	resp, body := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}})
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("prompt status = %d: %s", resp.StatusCode, body)
	}
	for {
		ev := sse.waitFor(h.t, "session.status")
		if ev.Status == "busy" {
			if ev.TurnID == "" {
				h.t.Fatal("busy event has no turn_id")
			}
			return ev.TurnID
		}
	}
}

func (h *harness) abortBody(id, raw string) {
	h.t.Helper()
	var resp *http.Response
	var body []byte
	if raw == "" {
		resp, body = h.do("POST", "/session/"+id+"/abort", nil)
	} else {
		resp, body = h.doRaw("POST", "/session/"+id+"/abort", raw)
	}
	if resp.StatusCode != http.StatusNoContent {
		h.t.Fatalf("abort status = %d: %s", resp.StatusCode, body)
	}
}

func journalTypes(t *testing.T, store engine.SessionStore, id string) []map[string]any {
	t.Helper()
	lines, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func tailMessageText(rec map[string]any) (id, role, text string) {
	m, _ := rec["message"].(map[string]any)
	id, _ = m["id"].(string)
	role, _ = m["role"].(string)
	parts, _ := m["parts"].([]any)
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if s, ok := pm["text"].(string); ok {
				text += s
			}
		}
	}
	return
}

func TestAbortNamesActiveTurn(t *testing.T) {
	store := engine.NewMemStore()
	prov := newStopProvider("par", "never")
	h := newOwnerServer(t, store, prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	turnID := h.startTurn(sse, id, "hi")
	<-prov.started

	h.abortBody(id, `{"turn_id":"`+turnID+`"}`)
	h.srv.wg.Wait()

	recs := journalTypes(t, store, id)
	n := len(recs)
	msgID, role, text := tailMessageText(recs[n-2])
	if recs[n-2]["type"] != "message" || role != "assistant" || text != "par" {
		t.Fatalf("record before the marker = %v, want the assistant message with text par", recs[n-2])
	}
	if recs[n-1]["type"] != "turn.stopped" || recs[n-1]["message_id"] != msgID {
		t.Fatalf("last record = %v, want turn.stopped naming %s", recs[n-1], msgID)
	}

	ev := sse.waitFor(t, "session.aborted")
	if ev.TurnID != turnID || ev.MessageID != msgID {
		t.Errorf("session.aborted turn_id=%q message_id=%q, want %q %q", ev.TurnID, ev.MessageID, turnID, msgID)
	}
}

func TestAbortWrongTurnIsNoOp(t *testing.T) {
	prov := newStopProvider("", "scripted")
	h := newOwnerServer(t, engine.NewMemStore(), prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	h.startTurn(sse, id, "hi")
	<-prov.started

	h.abortBody(id, `{"turn_id":"turn_other"}`)
	close(prov.release)
	h.srv.wg.Wait()

	if prov.canceled.Load() {
		t.Error("provider context was canceled by an abort naming another turn")
	}
	end := sse.waitFor(t, "turn.end")
	if end.Outcome != "completed" {
		t.Errorf("turn.end outcome = %q, want completed", end.Outcome)
	}
}

func TestAbortWithoutBodyStillAborts(t *testing.T) {
	prov := newStopProvider("", "never")
	h := newOwnerServer(t, engine.NewMemStore(), prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	turnID := h.startTurn(sse, id, "hi")
	<-prov.started

	h.abortBody(id, "")
	h.srv.wg.Wait()

	if ev := sse.waitFor(t, "session.aborted"); ev.TurnID != turnID {
		t.Errorf("session.aborted turn_id = %q, want %q", ev.TurnID, turnID)
	}
}

func TestLateAbortDoesNotStopNewerTurn(t *testing.T) {
	prov := newStopProvider("", "scripted")
	h := newOwnerServer(t, engine.NewMemStore(), prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	first := h.startTurn(sse, id, "one")
	<-prov.started
	prov.release <- struct{}{}
	sse.waitFor(t, "turn.end")
	sse.collectUntilIdle(t)

	second := h.startTurn(sse, id, "two")
	<-prov.started
	if first == second {
		t.Fatal("two turns share one turn_id")
	}
	h.abortBody(id, `{"turn_id":"`+first+`"}`)
	if prov.canceled.Load() {
		t.Fatal("late abort canceled the newer turn")
	}
	prov.release <- struct{}{}
	h.srv.wg.Wait()
	if end := sse.waitFor(t, "turn.end"); end.Outcome != "completed" || end.TurnID != second {
		t.Errorf("turn.end = %+v, want completed for %s", end, second)
	}
}

func TestStoppedTurnNeverResumes(t *testing.T) {
	store := engine.NewMemStore()
	prov := newStopProvider("par", "never")
	h := newOwnerServer(t, store, prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	turnID := h.startTurn(sse, id, "hi")
	<-prov.started
	h.abortBody(id, `{"turn_id":"`+turnID+`"}`)
	h.srv.wg.Wait()

	reloaded, err := engine.LoadSession(engine.Config{
		Providers:      provider.Registry{"test": prov},
		Model:          message.ModelRef{Provider: "test", Model: "m1"},
		SessionStore:   store,
		MaxTurnResumes: 3,
	}, id)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ResumableTurn() {
		t.Error("ResumableTurn() = true after a stop, want false")
	}
	if !reloaded.TurnStopped() {
		t.Error("TurnStopped() = false after a stop, want true")
	}
}

func TestStopBeforeAnyTextWritesMarkerWithoutMessageID(t *testing.T) {
	store := engine.NewMemStore()
	prov := newStopProvider("", "never")
	h := newOwnerServer(t, store, prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	turnID := h.startTurn(sse, id, "hi")
	<-prov.started
	h.abortBody(id, `{"turn_id":"`+turnID+`"}`)
	h.srv.wg.Wait()

	recs := journalTypes(t, store, id)
	last := recs[len(recs)-1]
	if last["type"] != "turn.stopped" {
		t.Fatalf("last record = %v, want turn.stopped", last)
	}
	if _, has := last["message_id"]; has {
		t.Errorf("turn.stopped carries message_id %v, want none", last["message_id"])
	}
	if ev := sse.waitFor(t, "session.aborted"); ev.MessageID != "" {
		t.Errorf("session.aborted message_id = %q, want empty", ev.MessageID)
	}
}

func TestOwnershipLossDoesNotWriteStopped(t *testing.T) {
	store := engine.NewMemStore()
	prov := newStopProvider("par", "never")
	owner := newFakeOwner()
	h := newOwnerServer(t, store, prov, func(o *Options) { o.SessionOwner = owner })
	id := h.createSession("")
	sse := h.openSSE("", "")
	h.startTurn(sse, id, "hi")
	<-prov.started
	owner.loseOwnership(id)
	h.srv.wg.Wait()

	for _, r := range journalTypes(t, store, id) {
		if r["type"] == "turn.stopped" {
			t.Fatalf("ownership loss wrote %v", r)
		}
	}
}

type toolStopProvider struct {
	toolStarted chan struct{}
	calls       atomic.Int32
}

func (p *toolStopProvider) Name() string { return "test" }

func (p *toolStopProvider) tools() []engine.Tool {
	return []engine.Tool{{
		Def: provider.ToolDef{Name: "hang", Description: "hang", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, _ *engine.Session, _ json.RawMessage) (message.Parts, error) {
			close(p.toolStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}}
}

func (p *toolStopProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	if p.calls.Add(1) > 1 {
		return nil, context.Canceled
	}
	call := &message.ToolCall{CallID: "call_1", Name: "hang", Arguments: json.RawMessage(`{}`)}
	msg := &message.Message{ID: "msg_call", Role: message.RoleAssistant, Parts: message.Parts{call}}
	return &toolStopStream{ev: provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopToolUse}}, nil
}

type toolStopStream struct {
	ev   provider.Event
	done bool
}

func (s *toolStopStream) Next() (provider.Event, error) {
	if s.done {
		return provider.Event{}, context.Canceled
	}
	s.done = true
	return s.ev, nil
}

func (s *toolStopStream) Close() error { return nil }

func TestStopDuringToolCall(t *testing.T) {
	store := engine.NewMemStore()
	prov := &toolStopProvider{toolStarted: make(chan struct{})}
	h := newOwnerServer(t, store, prov)
	id := h.createSession("")
	sse := h.openSSE("", "")
	turnID := h.startTurn(sse, id, "hi")
	<-prov.toolStarted
	h.abortBody(id, `{"turn_id":"`+turnID+`"}`)
	h.srv.wg.Wait()

	recs := journalTypes(t, store, id)
	n := len(recs)
	if recs[n-1]["type"] != "turn.stopped" {
		t.Fatalf("last record = %v, want turn.stopped", recs[n-1])
	}
	_, role, _ := tailMessageText(recs[n-2])
	m, _ := recs[n-2]["message"].(map[string]any)
	parts, _ := m["parts"].([]any)
	if role != "tool" || len(parts) != 1 {
		t.Fatalf("record before the marker = %v, want one tool result message", recs[n-2])
	}
	if got := parts[0].(map[string]any)["call_id"]; got != "call_1" {
		t.Errorf("tool result call_id = %v, want call_1", got)
	}
}
