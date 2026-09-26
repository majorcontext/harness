package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestMutableSessionColdInsertPinsBeforeSweep(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarnessOpts(t, t.TempDir(), prov, 1)

	pinnedID := h.createSession("test/m1")
	id := h.createSession("test/m1") // MaxResident=1: evicts pinnedID
	h.srv.mu.Lock()
	if _, resident := h.srv.sessions[pinnedID]; resident {
		t.Fatal("test setup: pinnedID still resident, want evicted by id's creation")
	}
	h.srv.sessions[id].pins = 1 // simulate another in-flight command on id
	h.srv.mu.Unlock()

	sess, release, ok := h.srv.mutableSession(pinnedID)
	if !ok {
		t.Fatal("mutableSession(pinnedID) ok = false, want true")
	}

	h.srv.mu.Lock()
	st := h.srv.sessions[pinnedID]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatal("pinned session is not resident after mutableSession")
	}
	if st.sess != sess {
		t.Fatal("s.sessions[pinnedID].sess is not the object mutableSession returned")
	}
	if st.pins != 1 {
		t.Fatalf("s.sessions[pinnedID].pins = %d, want 1", st.pins)
	}

	release()
	h.srv.mu.Lock()
	pins := h.srv.sessions[pinnedID].pins
	h.srv.mu.Unlock()
	if pins != 0 {
		t.Fatalf("pins after release = %d, want 0", pins)
	}
}

func TestCommandTerminalWritesLandOnLiveSessionAfterEviction(t *testing.T) {
	dir := t.TempDir()
	prov := newCapturingProvider()
	h := newHarnessOpts(t, dir, prov, 1) // MaxResident=1

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
		h.createSession("test/m1") // attempt a real concurrent eviction of id
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
	h.srv.opts.MaxResident = 1 // id is now the longest-idle resident
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

func TestRacedMidTurnCompactRecordsRefused(t *testing.T) {
	prov := &queueProv{name: "test", started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	defer close(prov.release)
	sse := h.openSSE("?from=0", "")

	raced := false
	h.srv.commandDispatchRace = func() {
		if raced {
			return
		}
		raced = true
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "occupant"}},
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("occupant prompt status %d: %s", resp.StatusCode, data)
		}
		<-prov.started
	}

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/compact status %d: %s", resp.StatusCode, data)
	}

	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil || terminal.Command.Status != message.CommandRefused {
		t.Fatalf("terminal command event = %+v, want refused", terminal.Command)
	}
	want := "/compact cannot run while a turn is running; send it again after the turn ends"
	if terminal.Command.Text != want {
		t.Errorf("terminal text = %q, want %q", terminal.Command.Text, want)
	}
	if !raced {
		t.Fatal("commandDispatchRace never ran; this test proves nothing")
	}

	for _, m := range h.userMessages(id) {
		if m.Parts.Text() == "/compact" {
			t.Fatalf("a user message holds /compact: %+v", m)
		}
	}
}

func doneChildHarness(t *testing.T) (h *harness, childID string) {
	t.Helper()
	dir := t.TempDir()
	rootProv := &scriptedProvider{name: "root"}
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("child done")}}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	model := message.ModelRef{Provider: "root", Model: "m1"}
	var srv *Server
	h = multiProviderHarnessInDir(t, dir, model, func(o *Options) {
		o.NewSession = func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = model
			}
			return engine.NewSession(engine.Config{
				Providers: reg, Model: m, WorkDir: workDir, ParentSession: parentSession,
				SessionDir: dir, OnEvent: func(ev engine.Event) { srv.Publish(ev) },
			}), nil
		}
		o.LoadSession = func(id string) (*engine.Session, error) {
			return engine.LoadSession(engine.Config{
				Providers: reg, Model: model, SessionDir: dir, OnEvent: func(ev engine.Event) { srv.Publish(ev) },
			}, id)
		}
	}, rootProv, childProv)
	srv = h.srv

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	waitForLineageStatus(t, h, child.ID, "done", 2*time.Second)
	return h, child.ID
}

func TestTypedCompactOnManagedChildRecordsFailed(t *testing.T) {
	h, childID := doneChildHarness(t)

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+childID+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/compact status %d: %s", resp.StatusCode, data)
	}

	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil || terminal.Command.Status != message.CommandFailed {
		t.Fatalf("terminal command event = %+v, want failed", terminal.Command)
	}
	want := "session is a SessionManager-managed child session; use POST /session/{id}/send instead"
	if terminal.Command.Text != want {
		t.Errorf("terminal command text = %q, want %q", terminal.Command.Text, want)
	}
}

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
