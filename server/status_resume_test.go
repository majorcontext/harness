package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestInFlightBusyStatusSurvivesAClientReconnect pins the two properties a
// consumer needs to recover a RUNNING turn after it reconnects mid-turn at
// the last seq it applied. Both were the loss surface behind the pending-
// state failures reported for the removed session monitor: that page folded
// turn-open state from live events alone and dropped the record whose seq
// equalled its snapshot cursor, so a running turn rendered as a silent idle
// one until the next status record — minutes, for a long turn.
//
//   - session.status is DURABLE. It carries a journal seq and replays to a
//     client resuming from a cursor below it. GET /session/{id}/message
//     cannot stand in for that: a status record is a journal record, not a
//     message, so replay is the only way back to it. Published live-only, a
//     client that connects after the turn started never learns it is running.
//   - `from` is EXCLUSIVE. A client resumes at the seq it last applied, so
//     the record AT the cursor must not be sent again; re-sending makes
//     every resume double-apply its own boundary record.
//
// The turn is released before the reads below so the terminal idle record
// bounds them in both the passing and the failing case: a lost busy record
// fails an assertion instead of hanging on a stream that never speaks again.
func TestInFlightBusyStatusSurvivesAClientReconnect(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("")

	readTip := func() int64 {
		t.Helper()
		resp, body := h.do(http.MethodGet, "/event/tip", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /event/tip = %d: %s", resp.StatusCode, body)
		}
		var got tipJSON
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode tip: %v", err)
		}
		return got.Seq
	}

	// Everything this client has already applied, before the turn exists.
	cursor := readTip()

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	<-prov.started // the turn is genuinely in flight, and its busy record is behind us

	resumed := h.openSSE(fmt.Sprintf("?from=%d&session=%s", cursor, id), "")
	prov.releaseAll()

	var sawBusy bool
	var busySeq int64
	for {
		ev := resumed.nextEvent(t)
		if ev.Type != evtSessionStatus {
			continue
		}
		if ev.Status == "busy" && !sawBusy {
			sawBusy, busySeq = true, ev.Seq
			continue
		}
		if ev.Status == "idle" {
			break
		}
	}
	if !sawBusy {
		t.Fatalf("resuming at cursor %d never delivered the in-flight turn's session.status busy: a client reconnecting mid-turn renders a running session as idle", cursor)
	}
	if busySeq <= cursor {
		t.Fatalf("replayed busy record seq = %d, want a durable seq above the client cursor %d", busySeq, cursor)
	}

	// Resuming AT that record: the client already applied it.
	atBoundary := h.openSSE(fmt.Sprintf("?from=%d&session=%s", busySeq, id), "")
	for {
		ev := atBoundary.nextEvent(t)
		if ev.Seq == 0 {
			continue // a transient live event carries no journal seq
		}
		if ev.Seq <= busySeq {
			t.Fatalf("resuming at seq %d re-delivered %s seq %d; from is exclusive, so a client must never be sent its own cursor record again", busySeq, ev.Type, ev.Seq)
		}
		break
	}
}
