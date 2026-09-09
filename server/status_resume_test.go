package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestBusyStatusReachesAClientConnectingMidTurn pins what a consumer needs
// to recover a RUNNING turn when it connects after that turn started:
// session.status is durable, so replay from a cursor below it still
// delivers it.
//
// This was the loss surface behind the pending-state failures reported for
// the removed session monitor. That page folded turn-open state from live
// events alone and dropped the record whose seq equalled its snapshot
// cursor, so a running turn rendered as a silent idle one. No snapshot can
// stand in for the record: GET /session/{id}/message returns messages, and
// a status record is a journal record, not a message. Published live-only
// it would be unrecoverable, and every consumer that connects mid-turn
// (tools/hub, tools/inspector, an orchestrator resuming its tail) would
// show the session as idle until the next status record — minutes, for a
// long turn.
//
// The exclusive `from` boundary that pairs with this is already pinned by
// TestReplayFromSeq and TestLastEventIDHeader; this covers only the half
// they do not, a client whose connection starts mid-turn.
//
// Both reads below are bounded by the journal tip, taken once the turn has
// ended through the same wait seam production uses, so a regression that
// loses the record fails an assertion instead of hanging on a stream that
// never speaks again.
func TestBusyStatusReachesAClientConnectingMidTurn(t *testing.T) {
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
	<-prov.started // the turn is in flight, and its busy record is already behind us

	resumed := h.openSSE(fmt.Sprintf("?from=%d&session=%s", cursor, id), "")

	prov.releaseAll()
	h.waitIdle(id)
	tip := readTip()
	if tip <= cursor {
		t.Fatalf("journal tip %d did not advance past the pre-turn cursor %d; the turn journaled nothing", tip, cursor)
	}

	var sawBusy bool
	var busySeq int64
	for {
		ev := resumed.nextEvent(t)
		if ev.Seq == 0 {
			continue // a transient live event carries no journal seq
		}
		if ev.Type == evtSessionStatus && ev.Status == "busy" && !sawBusy {
			sawBusy, busySeq = true, ev.Seq
		}
		// The turn's own terminal record, or anything at the tip: either way
		// the journal window this client asked for has been delivered.
		if ev.Seq >= tip || (ev.Type == evtSessionStatus && ev.Status == "idle") {
			break
		}
	}
	if !sawBusy {
		t.Fatalf("connecting at cursor %d never delivered the in-flight turn's session.status busy: a client that joins mid-turn renders a running session as idle", cursor)
	}
	if busySeq <= cursor {
		t.Fatalf("busy record seq = %d, want a durable seq above the client cursor %d", busySeq, cursor)
	}
}
