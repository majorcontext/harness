package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// commandEventsForSession returns every durable "command" event journaled
// for id, in journal order.
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

// TestCommandEventJournaled proves a live RecordCommand call on a resident
// session flows through Publish into a durable, replayable "command" event:
// the wire shape a connected (or replaying) SSE client relies on to see a
// resolved slash command at all.
func TestCommandEventJournaled(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	h.srv.mu.Lock()
	sess := h.srv.sessions[id].sess
	h.srv.mu.Unlock()

	cmdID := engine.NewCommandID()
	if err := sess.RecordCommand(message.CommandRecord{
		ID:     cmdID,
		Line:   "/compact",
		Name:   "compact",
		Source: message.PromptSourceTyped,
		Status: message.CommandAccepted,
	}); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	ev := sse.waitFor(t, "command")
	if ev.Seq == 0 {
		t.Error("command event has zero seq, want non-zero")
	}
	if ev.Command == nil || ev.Command.ID != cmdID || ev.Command.Status != message.CommandAccepted {
		t.Fatalf("command event = %+v, want id=%q status=accepted", ev.Command, cmdID)
	}
}

// TestCommandPromptAsyncSeqPrecedesAcceptedEvent is the named-failure test
// for prompt_async's command cursor: a client that resumes GET
// /event?from=<seq> only replays events with a strictly greater seq, so a
// prompt_async response for a typed command must report a seq sampled
// BEFORE the accepted "command" event is journaled. A response that instead
// samples the journal's seq AFTER RecordCommand equals (or passes) the
// accepted event's own seq, and a client resuming from it skips that event.
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

// TestBootMarksAcceptedCommandInterrupted is the crash-recovery red test: a
// command that never reached a terminal status before its process died must
// not stay "accepted" forever, and the reload must not silently re-run it.
// Boot must mark it interrupted, journal that as a durable "command" event
// with the boot wording beside the original accepted one (the log is
// append-only), and — because a LATER boot's reconcile backfill finds the
// SAME interrupted record already journaled — never add a second one.
func TestBootMarksAcceptedCommandInterrupted(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}

	srv1 := newServer(t, dir, prov, 0)
	id := createSessionDirect(t, srv1, "test/m1")

	srv1.mu.Lock()
	sess := srv1.sessions[id].sess
	srv1.mu.Unlock()

	cmdID := engine.NewCommandID()
	if err := sess.RecordCommand(message.CommandRecord{
		ID:     cmdID,
		Line:   "/compact",
		Name:   "compact",
		Source: message.PromptSourceTyped,
		Status: message.CommandAccepted,
	}); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}

	wantText := "harness restarted before /compact finished; it will not run again"

	// The first boot's journal already carries the ORIGINAL accepted record
	// (srv1's own live Publish journaled that one before it ever closed) —
	// reconcile's backfill adds a SECOND, interrupted record beside it. It
	// never rewrites or removes the first: the log is append-only.
	srv2 := newServer(t, dir, prov, 0)
	firstBoot := commandEventsForSession(srv2, id)
	if len(firstBoot) != 2 {
		t.Fatalf("command events after first boot = %d, want 2 (accepted, then interrupted): %+v", len(firstBoot), firstBoot)
	}
	if firstBoot[0].Command == nil || firstBoot[0].Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want status=accepted", firstBoot[0].Command)
	}
	got := firstBoot[1]
	if got.Seq == 0 {
		t.Error("interrupted command event has zero seq, want non-zero")
	}
	if got.Command == nil || got.Command.ID != cmdID || got.Command.Status != message.CommandInterrupted {
		t.Fatalf("second command event = %+v, want id=%q status=interrupted", got.Command, cmdID)
	}
	if got.Command.Text != wantText {
		t.Errorf("interrupted text = %q, want %q", got.Command.Text, wantText)
	}

	reloaded, err := srv2.opts.LoadSession(id)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	cmds := reloaded.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandInterrupted {
		t.Fatalf("reloaded Commands() = %+v, want one interrupted record", cmds)
	}
	if err := srv2.Close(); err != nil {
		t.Fatalf("closing second server: %v", err)
	}

	// A third boot (second reload) must add no new interrupted event:
	// RepairInterruptedCommands finds nothing still accepted, and the
	// backfill's seen set — rebuilt by loadJournal from the command event
	// the first boot already journaled — skips the already-known
	// (id, interrupted) pair.
	srv3 := newServer(t, dir, prov, 0)
	t.Cleanup(func() {
		if err := srv3.Close(); err != nil {
			t.Errorf("closing third server: %v", err)
		}
	})
	secondBoot := commandEventsForSession(srv3, id)
	if len(secondBoot) != 2 {
		t.Fatalf("command events after second boot = %d, want 2 (no new interrupted event added): %+v", len(secondBoot), secondBoot)
	}
}
