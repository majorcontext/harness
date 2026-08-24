package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// toolCallTurn is asstTurn's tool-call counterpart: one assistant message
// carrying a single tool call, stopped for tool use.
func toolCallTurn(callID, name, args string) []provider.Event {
	msg := &message.Message{
		ID:   message.ProviderCallID("m", callID, 1),
		Role: message.RoleAssistant,
		Parts: message.Parts{
			&message.ToolCall{CallID: callID, Name: name, Arguments: json.RawMessage(args)},
		},
	}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopToolUse}}
}

// newGatedHarness is newServer's counterpart for tests that need a real,
// test-controlled blocking TOOL (not a blocking provider Stream call): it
// registers a "gate" tool that closes entered the instant it starts
// executing and then parks on release, letting a test hold a genuine
// in-flight tool call open long enough to enqueue a prompt against it and
// observe the mid-turn drain this file's test exercises.
func newGatedHarness(t *testing.T, dir string, prov provider.Provider, entered, release chan struct{}) *harness {
	t.Helper()
	const token = "secret-run-token"
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	var srv *Server
	gate := engine.Tool{
		Def: provider.ToolDef{
			Name:        "gate",
			Description: "test-only tool that blocks until released",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Run: func(ctx context.Context, s *engine.Session, args json.RawMessage) (message.Parts, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return message.Parts{&message.Text{Text: "gate done"}}, nil
		},
	}
	mkCfg := func(m message.ModelRef) engine.Config {
		if m.IsZero() {
			m = model
		}
		return engine.Config{
			Providers:  provider.Registry{prov.Name(): prov},
			Model:      m,
			SessionDir: dir,
			OnEvent:    func(ev engine.Event) { srv.Publish(ev) },
			Tools:      []engine.Tool{gate},
		}
	}
	opts := Options{
		SessionDir:        dir,
		RunToken:          token,
		Version:           "9.9.9",
		HeartbeatInterval: 20 * time.Millisecond,
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parentSession
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	}
	var err error
	srv, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

// TestQueueDropsMidTurnAtToolCallBoundary extends the prompt-queue SSE
// contract (see TestQueuedPromptDispatchesOnDrain) to the tool-call-boundary
// injection amendment: a prompt queued while a session's in-flight turn is
// blocked INSIDE a tool call must drop from GET /session's queued count, and
// surface a prompt.dequeued(injected) SSE event, well before that turn ever
// ends — not only at the turn's tail.
func TestQueueDropsMidTurnAtToolCallBoundary(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		toolCallTurn("tc1", "gate", `{}`),
		asstTurn("final"),
	}}
	h := newGatedHarness(t, dir, prov, entered, release)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "please run gate"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	sse.waitFor(t, engine.EventToolStart)
	<-entered // the gate tool call is genuinely executing now

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "mid-turn steer"}},
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

	close(release) // let the gate tool return, landing the tool result

	// The mid-turn drain fires the instant that tool-result message is
	// appended — strictly BEFORE the turn's second provider call (which
	// produces "final" below) or its own idle transition. Observing the
	// dequeue SSE event and GET /session's already-empty queued count here,
	// ahead of waiting for "final"/idle, is exactly the tool-call-boundary
	// claim under test: delivery does not wait for the whole turn to end.
	dequeue := sse.waitFor(t, engine.EventPromptDequeued)
	if dequeue.QueueReason != "injected" {
		t.Fatalf("dequeue reason = %q, want injected", dequeue.QueueReason)
	}
	if dequeue.QueueLen == nil || *dequeue.QueueLen != 0 {
		t.Fatalf("dequeue queue_len = %v, want 0", dequeue.QueueLen)
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Fatalf("GET /session queued = %d, want 0 (dropped mid-turn)", sess.Queued)
	}

	var asst Event
	for {
		asst = sse.waitFor(t, "message")
		if asst.Message != nil && asst.Message.Role == message.RoleAssistant && asst.Message.Parts.Text() == "final" {
			break
		}
	}
	idle := sse.waitFor(t, "session.status")
	if idle.Status != "idle" {
		t.Fatalf("expected the turn's own idle after finishing, got status %q", idle.Status)
	}
}
