package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/provider"
)

// contextOverflowErr builds the classified error a provider adapter returns
// for issue #62's incident (reused here rather than depending on the
// engine/provider test packages).
func contextOverflowErr() *provider.Error {
	return &provider.Error{
		Kind:         provider.ErrKindContextOverflow,
		Raw:          "anthropic: prompt is too long: 205102 tokens > 200000 maximum (invalid_request_error, HTTP 400)",
		PromptTokens: 205102,
		TokenLimit:   200000,
	}
}

// TestTurnEndOnPromptContextOverflowIsDistinctOutcome is the plain-prompt
// half of issue #62's classification requirement: a context-overflow
// failure must not just be another "error" outcome indistinguishable from a
// transcode bug or a dropped connection — it gets its own turn.end outcome
// ("context_exhausted") so a poller can react to it specifically (e.g.
// rotate the session) without string-matching last_turn.error.
func TestTurnEndOnPromptContextOverflowIsDistinctOutcome(t *testing.T) {
	prov := &errThenOKProvider{name: "test", err: contextOverflowErr()}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "too much history"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}

	end := sse.waitFor(t, "turn.end")
	if end.Outcome != outcomeContextExhausted {
		t.Errorf("turn.end outcome = %q, want %q", end.Outcome, outcomeContextExhausted)
	}
	wantErr := "context exhausted: prompt 205102 tokens > limit 200000"
	if end.Error != wantErr {
		t.Errorf("turn.end error = %q, want %q", end.Error, wantErr)
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
	if sess.LastTurn == nil || sess.LastTurn.Outcome != outcomeContextExhausted {
		t.Fatalf("last_turn = %+v, want outcome %q", sess.LastTurn, outcomeContextExhausted)
	}
	if sess.LastTurn.Error != wantErr {
		t.Errorf("Session.last_turn.error = %q, want %q", sess.LastTurn.Error, wantErr)
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
	if !ok || entry.LastTurn == nil || entry.LastTurn.Outcome != outcomeContextExhausted {
		t.Errorf("status entry for %s = %+v, want last_turn.outcome %q", id, entry, outcomeContextExhausted)
	}
}

// TestTurnEndOnGoalContextOverflowIsDistinctOutcomeAndFailsFast is the
// goal-loop half: a context-overflow worker-turn failure must fail fast (one
// worker call, no retries — see engine/goal.go) AND still surface its own
// turn.end outcome, distinct from both "error" (an ordinary permanent
// failure) and "max_turns_exceeded".
func TestTurnEndOnGoalContextOverflowIsDistinctOutcomeAndFailsFast(t *testing.T) {
	prov := &goalProv{
		name:       "test",
		workerErrN: 100, // every attempt would fail if retried; fail-fast means only 1 is ever made
		workerErr:  contextOverflowErr(),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "do the thing"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}

	evs := sse.collectUntilIdle(t)

	var end *Event
	var stalled int
	for i := range evs {
		switch evs[i].Type {
		case "turn.end":
			end = &evs[i]
		case "goal.stalled":
			stalled++
		}
	}
	if end == nil {
		t.Fatal("no turn.end event observed")
	}
	if end.Outcome != outcomeContextExhausted {
		t.Errorf("turn.end outcome = %q, want %q", end.Outcome, outcomeContextExhausted)
	}
	wantErr := "context exhausted: prompt 205102 tokens > limit 200000"
	if end.Error != wantErr {
		t.Errorf("turn.end error = %q, want %q", end.Error, wantErr)
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want exactly 1 (fail fast, no retry)", stalled)
	}
	if prov.workerErrHit != 1 {
		t.Errorf("worker calls that hit the injected error = %d, want exactly 1", prov.workerErrHit)
	}
}
