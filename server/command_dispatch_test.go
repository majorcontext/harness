package server

import (
	"net/http"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/message"
)

// TestCommandTerminalWritesLandOnLiveSessionAfterEviction: id's own
// *engine.Session is pinned for the whole dispatch, from writeCommand's
// lookup through the terminal write, so a concurrent eviction sweep in the
// gap between them (commandDispatchRace) must skip it instead of unloading
// it. Failure mode this guards: the terminal record lands on a second,
// freshly cold-loaded object for id instead of the one writeCommand's own
// accepted record landed on — invisible to a live reader, which keeps
// seeing the command stuck "accepted" forever.
func TestCommandTerminalWritesLandOnLiveSessionAfterEviction(t *testing.T) {
	dir := t.TempDir()
	prov := newCapturingProvider()
	// MaxResident=1: any second resident session would evict the first idle
	// one, if id's own entry were not pinned.
	h := newHarnessOpts(t, dir, prov, 1)

	id := h.createSession("test/m1")
	h.srv.mu.Lock()
	original := h.srv.sessions[id].sess
	h.srv.mu.Unlock()

	raced := false
	h.srv.commandDispatchRace = func() {
		if raced {
			return
		}
		raced = true
		// Attempt a real concurrent eviction of id's own resident object
		// right here, in the gap this seam exists to open — see
		// commandDispatchRace's own doc comment (server.go). The pin held
		// across this whole dispatch must make it a no-op for id.
		h.createSession("test/m1")
	}

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/thinking high"}},
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

	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatal("session is not resident after dispatch completed")
	}
	if st.sess != original {
		t.Fatal("a second *engine.Session was created for id during dispatch, want the pin to keep the original resident")
	}
	if st.pins != 0 {
		t.Fatalf("pins = %d after dispatch completed, want 0 (released)", st.pins)
	}
	cmds := st.sess.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandSucceeded || cmds[0].CreatedAt.IsZero() {
		t.Fatalf("live resident session's Commands() = %+v, want one succeeded command with a non-zero created_at", cmds)
	}
}

// TestMutableSessionPinReleasedAfterCommand: the pin mutableSession takes for
// a dispatched command's session is released once the command reaches its
// terminal write, so the session becomes an ordinary eviction candidate
// again. Failure: a leaked pin permanently exempts a session from
// MaxResident eviction.
func TestMutableSessionPinReleasedAfterCommand(t *testing.T) {
	dir := t.TempDir()
	prov := newCapturingProvider()
	h := newHarnessOpts(t, dir, prov, 2)

	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/status"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	sse.waitFor(t, "command") // accepted
	sse.waitFor(t, "command") // succeeded (terminal)

	h.srv.mu.Lock()
	pins := h.srv.sessions[id].pins
	h.srv.opts.MaxResident = 1
	// id is now the longest-idle resident; a pin left behind would exempt
	// it from this sweep. Two more sessions push the count past the cap.
	h.srv.mu.Unlock()
	if pins != 0 {
		t.Fatalf("pins = %d after command finished, want 0", pins)
	}

	h.createSession("test/m1")
	h.srv.mu.Lock()
	_, resident := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if resident {
		t.Fatal("id still resident after MaxResident pressure, want evicted (pin was released)")
	}
}

// TestRunCommandHandlerPanicRecordsFailed: a serveOpHandlers panic must not
// crash the process — runCommand runs off the request goroutine, so
// net/http's own per-request recover never reaches it. Failure: the
// goroutine's panic propagates unrecovered and takes the whole process down
// instead of leaving one command "failed".
func TestRunCommandHandlerPanicRecordsFailed(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	orig := serveOpHandlers[command.OpStatus]
	serveOpHandlers[command.OpStatus] = func(*Server, http.ResponseWriter, *http.Request) {
		panic("boom")
	}
	t.Cleanup(func() { serveOpHandlers[command.OpStatus] = orig })

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
	if terminal.Command == nil || terminal.Command.Status != message.CommandFailed {
		t.Fatalf("terminal command event = %+v, want failed", terminal.Command)
	}
	want := "/status failed: internal error"
	if terminal.Command.Text != want {
		t.Errorf("terminal text = %q, want %q", terminal.Command.Text, want)
	}
}
