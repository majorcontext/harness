package server

import (
	"encoding/json"
	"testing"
)

// countModelJournalRecords returns how many durable "model" records the journal
// holds for id. It reads under srv.mu, like the other journal-inspecting tests.
func countModelJournalRecords(h *harness, id string) int {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	n := 0
	for _, ev := range h.srv.journal {
		if ev.SessionID == id && ev.Type == evtModel {
			n++
		}
	}
	return n
}

// TestSetModelEndpointChangesAndJournalsOnce drives the real POST
// /session/{id}/model route: a valid swap returns 200 with the new model,
// actually changes the session model, and journals EXACTLY ONE durable "model"
// record. The exactly-one assertion is the surplus-direction guard for the
// double-emit removed from handlePrompt: SetModel's single EventModelChanged is
// the one journal path, so a second explicit emit would show as two records.
func TestSetModelEndpointChangesAndJournalsOnce(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/model", map[string]string{"model": "test/m2"})
	if resp.StatusCode != 200 {
		t.Fatalf("set model status %d: %s", resp.StatusCode, data)
	}
	var got setModelResponseJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model.String() != "test/m2" {
		t.Fatalf("response model = %q, want test/m2", got.Model.String())
	}

	// The live session reports the new model on GET /session.
	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Model != "test/m2" {
		t.Fatalf("GET /session model = %q, want test/m2", sess.Model)
	}

	if n := countModelJournalRecords(h, id); n != 1 {
		t.Fatalf("durable model records = %d, want exactly 1 (no double emit)", n)
	}
}

// TestSetModelEndpointRejectsUnknownProvider: a swap to an unconfigured
// provider is 400 and leaves the model unchanged (no journal record).
func TestSetModelEndpointRejectsUnknownProvider(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/model", map[string]string{"model": "ghost/x"})
	if resp.StatusCode != 400 {
		t.Fatalf("set unknown-provider status %d: %s, want 400", resp.StatusCode, data)
	}

	_, sdata := h.do("GET", "/session/"+id, nil)
	var sess struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(sdata, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Model != "test/m1" {
		t.Fatalf("GET /session model = %q after rejected swap, want unchanged test/m1", sess.Model)
	}
	if n := countModelJournalRecords(h, id); n != 0 {
		t.Fatalf("durable model records = %d after rejected swap, want 0", n)
	}
}

// TestSetModelEndpointRejectsEmptyModel: a missing/empty model is 400.
func TestSetModelEndpointRejectsEmptyModel(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.doRaw("POST", "/session/"+id+"/model", `{}`)
	if resp.StatusCode != 400 {
		t.Fatalf("set empty-model status %d: %s, want 400", resp.StatusCode, data)
	}
}

// TestSetModelEndpointUnknownSession: an unknown session is 404.
func TestSetModelEndpointUnknownSession(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	resp, data := h.do("POST", "/session/ses_01000000000000000000000000/model", map[string]string{"model": "test/m2"})
	if resp.StatusCode != 404 {
		t.Fatalf("set unknown-session status %d: %s, want 404", resp.StatusCode, data)
	}
}
