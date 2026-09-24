package server

import (
	"net/http"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/message"
)

// TestCommandTerminalWritesLandOnLiveSessionAfterEviction: the accepted
// record's own *engine.Session object can be evicted from residency in the
// gap between writeCommand's own lookup and the route handler's independent
// one (the object is not running, so it is an ordinary LRU eviction
// candidate). Failure mode this guards: the terminal record lands on the
// stale, now-orphaned object instead of whatever *engine.Session the
// server currently holds resident for id — invisible to a live reader,
// which keeps seeing the command stuck "accepted" forever.
func TestCommandTerminalWritesLandOnLiveSessionAfterEviction(t *testing.T) {
	dir := t.TempDir()
	prov := newCapturingProvider()
	// MaxResident=1: any second resident session evicts the first idle one.
	h := newHarnessOpts(t, dir, prov, 1)

	id := h.createSession("test/m1")

	raced := false
	h.srv.commandDispatchRace = func() {
		if raced {
			return
		}
		raced = true
		// Force a real concurrent eviction of id's own (idle, non-running)
		// resident object right here, in the gap this seam exists to open
		// — see commandDispatchRace's own doc comment (server.go).
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

	// The session object the server CURRENTLY holds resident for id — the
	// freshly cold-loaded one the eviction above forced, not the one
	// writeCommand originally resolved — must show the terminal status.
	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatal("session is not resident after dispatch completed")
	}
	cmds := st.sess.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandSucceeded {
		t.Fatalf("live resident session's Commands() = %+v, want one succeeded command", cmds)
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
