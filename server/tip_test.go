package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEventTipReportsJournalTip(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	readTip := func() int64 {
		t.Helper()
		resp, body := h.do(http.MethodGet, "/event/tip", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /event/tip status = %d, want 200 (body %s)", resp.StatusCode, body)
		}
		var got struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode tip: %v", err)
		}
		return got.Seq
	}

	before := readTip()

	seq := h.srv.emitDurable(Event{Type: evtSessionStatus, SessionID: "ses_tip", Status: "busy"})

	if got := readTip(); got != seq {
		t.Fatalf("tip after emit = %d, want the emitted seq %d", got, seq)
	}
	if seq <= before {
		t.Fatalf("emitted seq %d did not advance past the tip before it (%d)", seq, before)
	}
}
