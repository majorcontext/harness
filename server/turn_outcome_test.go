package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// lastTurnJSONForTest mirrors the openapi last_turn shape, used to decode
// both GET /session/{id}.last_turn and GET /session/status entries'
// last_turn in these tests.
type lastTurnJSONForTest struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// TestTurnEndOnPromptCompletionExposedAsLastTurn is the "idle because done"
// half of the primitive: a prompt turn that finishes cleanly journals a
// durable turn.end record with outcome "completed" and no error, reaches the
// SSE stream, and is surfaced as Session.last_turn — a poller watching only
// GET /session/{id} must be able to tell "idle, and the last turn actually
// finished" without inferring anything from message part shapes.
func TestTurnEndOnPromptCompletionExposedAsLastTurn(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("all good")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}

	end := sse.waitFor(t, "turn.end")
	if end.Seq == 0 {
		t.Errorf("turn.end has no seq (must be durable)")
	}
	if end.Outcome != "completed" {
		t.Errorf("turn.end outcome = %q, want completed", end.Outcome)
	}
	if end.Error != "" {
		t.Errorf("turn.end error = %q, want empty on completion", end.Error)
	}

	idle := sse.waitFor(t, "session.status")
	for idle.Status != "idle" {
		idle = sse.waitFor(t, "session.status")
	}
	if idle.Seq <= end.Seq {
		t.Errorf("idle seq %d not after turn.end seq %d", idle.Seq, end.Seq)
	}

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.LastTurn == nil {
		t.Fatalf("session JSON missing last_turn: %s", data)
	}
	if sess.LastTurn.Outcome != "completed" || sess.LastTurn.Error != "" {
		t.Errorf("last_turn = %+v, want {completed, \"\"}", *sess.LastTurn)
	}

	resp, data = h.do("GET", "/session/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET status status %d: %s", resp.StatusCode, data)
	}
	var statuses map[string]struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	if err := json.Unmarshal(data, &statuses); err != nil {
		t.Fatal(err)
	}
	entry, ok := statuses[id]
	if !ok || entry.LastTurn == nil || entry.LastTurn.Outcome != "completed" {
		t.Errorf("status entry for %s = %+v, want last_turn.outcome completed", id, entry)
	}
}

// TestTurnEndOnPromptFailureIsSanitizedAndSurfaced is the "idle because the
// turn died" half of the primitive: three plain-prompt turns died mid-stream
// today (final assistant message reasoning-only, no text, no tool call) and
// every monitor had to infer death from message part shapes. turn.end with
// outcome "error" must make that unnecessary — and the carried error string
// must never leak credentials from a wrapped provider/HTTP error.
func TestTurnEndOnPromptFailureIsSanitizedAndSurfaced(t *testing.T) {
	prov := &errThenOKProvider{name: "test"}
	prov.err = errors.New(`provider request failed: Authorization: Bearer sk-live-abcdef123456`)
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "boom"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}

	end := sse.waitFor(t, "turn.end")
	if end.Seq == 0 {
		t.Errorf("turn.end has no seq (must be durable)")
	}
	if end.Outcome != "error" {
		t.Errorf("turn.end outcome = %q, want error", end.Outcome)
	}
	if end.Error == "" {
		t.Errorf("turn.end missing error detail")
	}
	if strings.Contains(end.Error, "sk-live-abcdef123456") {
		t.Errorf("turn.end error leaked a credential: %q", end.Error)
	}

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.LastTurn == nil || sess.LastTurn.Outcome != "error" {
		t.Fatalf("last_turn = %+v, want outcome error", sess.LastTurn)
	}
	if strings.Contains(sess.LastTurn.Error, "sk-live-abcdef123456") {
		t.Errorf("Session.last_turn.error leaked a credential: %q", sess.LastTurn.Error)
	}
}

// TestTurnEndOnGoalWorkerFailureParksWithError exercises the goal-loop half
// of the primitive: a permanently failing worker turn exhausts its retries
// and EXIT-PARKS the goal (superseding this test's original
// clear-based contract — see engine/goal.go's "Round 7" doc section and
// docs/plans/2026-07-21-goal-worker-park.md) — that lands a distinct
// turn.end{outcome: worker_parked} record, not the generic "error" a plain
// prompt's worker-turn death still records, and — unlike a clear — the goal
// itself stays fully active, ready to resume on the next ordinary activity.
func TestTurnEndOnGoalWorkerFailureParksWithError(t *testing.T) {
	// Runs under synctest so the deterministic worker retry backoff
	// (goalRetryDelay: 1s+4s between the 3 attempts) costs no real wall-clock
	// time; drives the server in-process via handleGoal + the journal (no
	// httptest.Server, whose real listener a bubble forbids) exactly like
	// TestGoalWorkerParkSurfacesPausedWorkerFailure.
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:       "test",
			worker:     [][]provider.Event{},
			eval:       [][]provider.Event{},
			workerErrN: 100, // every attempt fails, exhausting retries
		}
		srv := newServer(t, dir, prov, 0, func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		})
		id := createSessionDirect(t, srv, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"do the thing"}`))
		greq.SetPathValue("id", id)
		srv.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		srv.wg.Wait() // the parked loop (and its fake-time backoff) finishes

		srv.mu.Lock()
		var turnEndOutcomeGot, turnEndError string
		var sawTurnEnd bool
		for _, ev := range srv.journal {
			if ev.SessionID == id && ev.Type == evtTurnEnd && !sawTurnEnd {
				sawTurnEnd = true
				turnEndOutcomeGot = ev.Outcome
				turnEndError = ev.Error
			}
		}
		srv.mu.Unlock()
		if !sawTurnEnd {
			t.Fatal("no turn.end event in journal")
		}
		if turnEndOutcomeGot != outcomeWorkerParked {
			t.Errorf("turn.end outcome = %q, want %q", turnEndOutcomeGot, outcomeWorkerParked)
		}
		if turnEndError == "" {
			t.Errorf("turn.end missing error detail")
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/session/"+id, nil)
		req.SetPathValue("id", id)
		srv.handleGet(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET session status %d: %s", rec.Code, rec.Body)
		}
		var sess struct {
			LastTurn *lastTurnJSONForTest `json:"last_turn"`
			Goal     *struct {
				Active bool `json:"active"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
			t.Fatal(err)
		}
		if sess.LastTurn == nil || sess.LastTurn.Outcome != outcomeWorkerParked {
			t.Fatalf("last_turn = %+v, want outcome %q", sess.LastTurn, outcomeWorkerParked)
		}
		if sess.Goal == nil || !sess.Goal.Active {
			t.Fatalf("goal = %+v, want active (a park must never clear the goal)", sess.Goal)
		}
	})
}
