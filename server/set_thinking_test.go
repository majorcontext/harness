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

// effortJournalRecords returns the durable "effort" records the journal holds
// for id, in journal order.
func effortJournalRecords(h *harness, id string) []Event {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	var recs []Event
	for _, ev := range h.srv.journal {
		if ev.SessionID == id && ev.Type == evtEffort {
			recs = append(recs, ev)
		}
	}
	return recs
}

// TestEffortJournalRecordAlwaysCarriesLevel drives POST /session/{id}/thinking
// through a set and a clear, then marshals every durable "effort" record the
// way it streams and journals. Each record MUST carry the "effort" key — so
// "cleared to provider default" (effort "") is never byte-indistinguishable
// from a malformed record that dropped its only field. This mirrors the
// "model" record, which never clears to empty.
//
// Named mechanism: the server Event.Effort field renders a cleared level as an
// explicit, present empty value on the wire. Red-verify by restoring
// `Effort message.Effort json:"effort,omitempty"` on Event (server/journal.go):
// the clear record then marshals with NO "effort" key and this test fails.
func TestEffortJournalRecordAlwaysCarriesLevel(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	// Set to high, then clear to the provider default with "".
	h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "high"})
	h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": ""})

	recs := effortJournalRecords(h, id)
	if len(recs) != 2 {
		t.Fatalf("durable effort records = %d, want 2 (set high, then clear)", len(recs))
	}

	// Every effort record marshals WITH the "effort" key present.
	for i, rec := range recs {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["effort"]; !ok {
			t.Fatalf("effort record %d marshaled without an \"effort\" key: %s", i, raw)
		}
	}

	// The set record carries "high"; the clear record carries an explicit "".
	type effortWire struct {
		Effort *string `json:"effort"`
	}
	var setRec, clearRec effortWire
	rawSet, _ := json.Marshal(recs[0])
	if err := json.Unmarshal(rawSet, &setRec); err != nil {
		t.Fatal(err)
	}
	rawClear, _ := json.Marshal(recs[1])
	if err := json.Unmarshal(rawClear, &clearRec); err != nil {
		t.Fatal(err)
	}
	if setRec.Effort == nil || *setRec.Effort != "high" {
		t.Fatalf("set effort record effort = %v, want \"high\"", setRec.Effort)
	}
	if clearRec.Effort == nil || *clearRec.Effort != "" {
		t.Fatalf("clear effort record effort = %v, want explicit \"\" (cleared to provider default)", clearRec.Effort)
	}
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

// TestSetThinkingEndpointOmittedFieldClears: an omitted effort field (body {})
// is treated the same as "" — a clear to the provider default, 200. The openapi
// SetThinkingRequest makes effort optional for exactly this reason.
func TestSetThinkingEndpointOmittedFieldClears(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	h.do("POST", "/session/"+id+"/thinking", map[string]string{"effort": "high"})

	resp, data := h.doRaw("POST", "/session/"+id+"/thinking", `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("omitted-effort status %d: %s, want 200 (clear)", resp.StatusCode, data)
	}
	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Effort != "" {
		t.Fatalf("GET /session effort = %q after omitted-field clear, want empty", sess.Effort)
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

// TestSetThinkingEndpointUnknownSessionWinsOverInvalidLevel: an INVALID level on
// an UNKNOWN session returns 404, not 400 — the session must resolve before the
// effort validates (mirrors handleSetModel). Named mechanism: ParseEffort runs
// AFTER session resolution. Red-verify by moving ParseEffort back above the
// resolution block; this test then returns 400.
func TestSetThinkingEndpointUnknownSessionWinsOverInvalidLevel(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_01000000000000000000000000/thinking", map[string]string{"effort": "xhigh"})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown-session + invalid-level status %d: %s, want 404 (session resolves before effort validates)", resp.StatusCode, data)
	}
}
