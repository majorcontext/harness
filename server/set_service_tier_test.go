package server

import (
	"encoding/json"
	"testing"
)

// countServiceTierJournalRecords returns how many durable "service_tier"
// records the journal holds for id. Mirrors countEffortJournalRecords
// (set_thinking_test.go).
func countServiceTierJournalRecords(h *harness, id string) int {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	n := 0
	for _, ev := range h.srv.journal {
		if ev.SessionID == id && ev.Type == evtServiceTier {
			n++
		}
	}
	return n
}

// TestSetServiceTierEndpointChangesAndJournalsOnce drives the real POST
// /session/{id}/service-tier route: a swap returns 200 with the new value,
// changes the session service tier, surfaces it on GET /session, and
// journals EXACTLY ONE durable "service_tier" record (the surplus-direction
// guard against a double emit). Mirrors
// TestSetThinkingEndpointChangesAndJournalsOnce.
func TestSetServiceTierEndpointChangesAndJournalsOnce(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/service-tier", map[string]string{"service_tier": "fast"})
	if resp.StatusCode != 200 {
		t.Fatalf("set service tier status %d: %s", resp.StatusCode, data)
	}
	var got setServiceTierResponseJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ServiceTier != "fast" {
		t.Fatalf("response service_tier = %q, want fast", got.ServiceTier)
	}

	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.ServiceTier != "fast" {
		t.Fatalf("GET /session service_tier = %q, want fast", sess.ServiceTier)
	}

	if n := countServiceTierJournalRecords(h, id); n != 1 {
		t.Fatalf("durable service_tier records = %d, want exactly 1 (no double emit)", n)
	}
}

// TestSetServiceTierEndpointClearsWithEmpty: an empty string clears the
// value, reported as absent on GET /session. Mirrors
// TestSetThinkingEndpointClearsWithEmpty.
func TestSetServiceTierEndpointClearsWithEmpty(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	h.do("POST", "/session/"+id+"/service-tier", map[string]string{"service_tier": "fast"})
	resp, data := h.do("POST", "/session/"+id+"/service-tier", map[string]string{"service_tier": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("clear service tier status %d: %s", resp.StatusCode, data)
	}
	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.ServiceTier != "" {
		t.Fatalf("GET /session service_tier = %q after clear, want empty", sess.ServiceTier)
	}
}

// TestSetServiceTierEndpointOmittedFieldClears: an omitted service_tier
// field (body {}) is treated the same as "" — a clear, 200. Mirrors
// TestSetThinkingEndpointOmittedFieldClears.
func TestSetServiceTierEndpointOmittedFieldClears(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	h.do("POST", "/session/"+id+"/service-tier", map[string]string{"service_tier": "fast"})

	resp, data := h.doRaw("POST", "/session/"+id+"/service-tier", `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("omitted-service_tier status %d: %s, want 200 (clear)", resp.StatusCode, data)
	}
	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.ServiceTier != "" {
		t.Fatalf("GET /session service_tier = %q after omitted-field clear, want empty", sess.ServiceTier)
	}
}

// TestSetServiceTierEndpointAcceptsArbitraryValue: harness does NOT validate
// which tiers exist — boxes owns that gating table, exactly as it gates
// effort levels — so any non-empty string round-trips verbatim, with no 400
// for an "unknown" value.
func TestSetServiceTierEndpointAcceptsArbitraryValue(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")
	resp, data := h.do("POST", "/session/"+id+"/service-tier", map[string]string{"service_tier": "whatever-boxes-sends"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s, want 200 (harness does not validate tiers)", resp.StatusCode, data)
	}
	var got setServiceTierResponseJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ServiceTier != "whatever-boxes-sends" {
		t.Fatalf("response service_tier = %q, want verbatim passthrough", got.ServiceTier)
	}
}

// TestSetServiceTierEndpointUnknownSession: an unknown session is 404.
// Mirrors TestSetThinkingEndpointUnknownSession.
func TestSetServiceTierEndpointUnknownSession(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session/ses_01000000000000000000000000/service-tier", map[string]string{"service_tier": "fast"})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown-session status %d: %s, want 404", resp.StatusCode, data)
	}
}
