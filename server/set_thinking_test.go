package server

import (
	"encoding/json"
	"testing"
)

// countEffortJournalRecords returns how many durable "effort" records the
// journal holds for id.
func countEffortJournalRecords(h *harness, id string) int {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	n := 0
	for _, ev := range h.srv.journal {
		if ev.SessionID == id && ev.Type == evtEffort {
			n++
		}
	}
	return n
}

// TestSetThinkingEndpointChangesAndJournalsOnce drives the real POST
// /session/{id}/thinking route: a valid swap returns 200 with the new level,
// changes the session effort, surfaces it on GET /session, and journals EXACTLY
// ONE durable "effort" record (the surplus-direction guard against a double
// emit).
func TestSetThinkingEndpointChangesAndJournalsOnce(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "high"})
	if resp.StatusCode != 200 {
		t.Fatalf("set thinking status %d: %s", resp.StatusCode, data)
	}
	var got setThinkingResponseJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Effort.String() != "high" {
		t.Fatalf("response effort = %q, want high", got.Effort)
	}

	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Effort != "high" {
		t.Fatalf("GET /session effort = %q, want high", sess.Effort)
	}

	if n := countEffortJournalRecords(h, id); n != 1 {
		t.Fatalf("durable effort records = %d, want exactly 1 (no double emit)", n)
	}
}

// TestSetThinkingEndpointAcceptsEveryLevel: each valid level round-trips.
func TestSetThinkingEndpointAcceptsEveryLevel(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high"} {
		h := newHarness(t, &scriptedProvider{name: "test"})
		id := h.createSession("test/m1")
		resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": level})
		if resp.StatusCode != 200 {
			t.Fatalf("level %q: status %d: %s", level, resp.StatusCode, data)
		}
		var got setThinkingResponseJSON
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Effort.String() != level {
			t.Fatalf("level %q: response effort = %q", level, got.Effort)
		}
	}
}

// TestSetThinkingEndpointClearsWithEmpty: an empty string clears the level to
// EffortUnset (provider default), reported as absent on GET /session.
func TestSetThinkingEndpointClearsWithEmpty(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	// Set to high, then clear with "".
	h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "high"})
	resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("clear thinking status %d: %s", resp.StatusCode, data)
	}
	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Effort != "" {
		t.Fatalf("GET /session effort = %q after clear, want empty", sess.Effort)
	}
}

// TestSetThinkingEndpointRejectsInvalidLevel: an unknown effort value is 400
// and leaves the level unchanged (no journal record).
func TestSetThinkingEndpointRejectsInvalidLevel(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "xhigh"})
	if resp.StatusCode != 400 {
		t.Fatalf("invalid-level status %d: %s, want 400", resp.StatusCode, data)
	}
	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Effort != "" {
		t.Fatalf("GET /session effort = %q after rejected swap, want empty", sess.Effort)
	}
	if n := countEffortJournalRecords(h, id); n != 0 {
		t.Fatalf("durable effort records = %d after rejected swap, want 0", n)
	}
}

// TestSetThinkingEndpointUnknownSession: an unknown session is 404.
func TestSetThinkingEndpointUnknownSession(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_01000000000000000000000000/thinking", map[string]string{"effort": "high"})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown-session status %d: %s, want 404", resp.StatusCode, data)
	}
}
