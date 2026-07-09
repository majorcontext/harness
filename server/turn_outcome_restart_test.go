package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/provider"
)

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

// TestLastTurnSurvivesRestart is the red-first regression test for PR #55
// review finding (2): reconcile()/loadJournal() already replays events.jsonl
// on boot to rebuild the seen-message index, but did nothing with turn.end
// records — so last_turn, a field whose entire purpose is to answer "did the
// last turn finish cleanly" for an orchestrator that reconnects after this
// process restarts, was silently lost across exactly the restart it needs to
// survive. A second Server built over the same session dir must still
// report the prior process's last_turn.
func TestLastTurnSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("first process")}}

	h1 := newHarnessDir(t, dir, prov)
	id := h1.createSession("test/m1")
	resp, data := h1.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	resp, data = h1.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
	}

	// Confirm last_turn is actually set in process 1 before restarting —
	// otherwise a "survives restart" assertion afterward would be vacuous.
	resp, data = h1.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var before struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	mustUnmarshal(t, data, &before)
	if before.LastTurn == nil || before.LastTurn.Outcome != "completed" {
		t.Fatalf("before restart, last_turn = %+v, want {completed}", before.LastTurn)
	}

	if err := h1.srv.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}

	// A brand-new Server over the SAME session dir — simulating a process
	// restart. It never ran this prompt itself; everything it knows about
	// this session's last turn must come from replaying events.jsonl.
	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	resp, data = h2.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session (restarted) status %d: %s", resp.StatusCode, data)
	}
	var after struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	mustUnmarshal(t, data, &after)
	if after.LastTurn == nil {
		t.Fatalf("last_turn did not survive restart: %s", data)
	}
	if after.LastTurn.Outcome != "completed" || after.LastTurn.Error != "" {
		t.Errorf("last_turn after restart = %+v, want {completed, \"\"}", *after.LastTurn)
	}

	// Also reachable through /session/status.
	resp, data = h2.do("GET", "/session/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET status (restarted) status %d: %s", resp.StatusCode, data)
	}
	var statuses map[string]struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	mustUnmarshal(t, data, &statuses)
	entry, ok := statuses[id]
	if !ok || entry.LastTurn == nil || entry.LastTurn.Outcome != "completed" {
		t.Errorf("status entry after restart = %+v, want last_turn.outcome completed", entry)
	}
}
