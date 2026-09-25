package server

import (
	"net/http"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestHandleEndRefusesWhilePinned: a command dispatch pins id's
// sessionState (sessionState.pins) for the whole dispatch, from
// writeCommand's lookup through the terminal write, but handleEnd only
// ever checked st.running. DELETE landing in that gap removed a pinned
// entry from residency, and the route handler's next lookup cold-loaded a
// second *engine.Session over the same log — the live one stayed stuck at
// "accepted" forever. Failure mode this guards: DELETE returns 204/200
// while st.pins > 0.
func TestHandleEndRefusesWhilePinned(t *testing.T) {
	dir := t.TempDir()
	prov := newCapturingProvider()
	h := newHarnessOpts(t, dir, prov, 2)

	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	raced := false
	h.srv.commandDispatchRace = func() {
		if raced {
			return
		}
		raced = true
		resp, data := h.do("DELETE", "/session/"+id, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("DELETE while pinned status = %d: %s, want 409", resp.StatusCode, data)
		}
		h.srv.mu.Lock()
		_, resident := h.srv.sessions[id]
		h.srv.mu.Unlock()
		if !resident {
			t.Error("session evicted by a DELETE that should have been refused while pinned")
		}
	}

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/status"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil || terminal.Command.Status != message.CommandSucceeded {
		t.Fatalf("terminal command event = %+v, want succeeded", terminal.Command)
	}
	if !raced {
		t.Fatal("commandDispatchRace never ran; this test proves nothing")
	}

	resp, data = h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE after pin released status = %d: %s, want 204", resp.StatusCode, data)
	}
}
