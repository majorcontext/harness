package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
)

func commandEventsForSession(srv *Server, id string) []Event {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var out []Event
	for _, ev := range srv.journal {
		if ev.Type == evtCommand && ev.SessionID == id {
			out = append(out, ev)
		}
	}
	return out
}

func TestCommandPromptAsyncSeqPrecedesAcceptedEvent(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/status"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	var body promptAsyncResponse
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode prompt_async response: %v (%s)", err, data)
	}
	if body.Status != "command" {
		t.Fatalf("response status = %q, want command", body.Status)
	}

	events := commandEventsForSession(h.srv, id)
	if len(events) == 0 {
		t.Fatal("no command events journaled")
	}
	accepted := events[0]
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	if body.Seq >= accepted.Seq {
		t.Fatalf("prompt_async seq = %d, want strictly less than accepted event seq %d", body.Seq, accepted.Seq)
	}
}
