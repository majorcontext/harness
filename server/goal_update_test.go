package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
)

// goalSummaryView mirrors the subset of Session JSON's goal field the tests
// in this file assert on (see TestGoalAchievedJournaled for the established
// decode idiom).
type goalSummaryView struct {
	Goal *struct {
		Condition string `json:"condition"`
		Active    bool   `json:"active"`
	} `json:"goal"`
}

func (h *harness) getGoalSummary(id string) goalSummaryView {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var v goalSummaryView
	if err := json.Unmarshal(data, &v); err != nil {
		h.t.Fatalf("decoding session JSON: %v (%s)", err, data)
	}
	return v
}

// TestGoalUpdatedFoldsLive is the live-path counterpart to
// TestGoalUpdatedFoldRebuild below: it drives a real engine session under
// the server (goalProv's blockWorker keeps the worker turn in flight, so the
// goal loop stays active for the duration of the test — the same idiom
// TestGoalBusyRejectsPromptAndGoal and TestTurnEndSuppressedOnGoalClearedInFlight
// use), calls Session.UpdateGoal directly (Task 5's POST /goal update path
// does not exist yet — see the plan's Task 4 scope note), and asserts:
// publishGoal folded the new condition into Session JSON's goal summary,
// a durable goal.updated SSE event carrying the new condition reached the
// client, and the same record landed in the server's durable journal.
func TestGoalUpdatedFoldsLive(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "first condition"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // worker turn now in flight; the goal loop is active

	before := h.getGoalSummary(id)
	if before.Goal == nil || before.Goal.Condition != "first condition" {
		t.Fatalf("goal before update = %+v, want condition %q", before.Goal, "first condition")
	}

	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatalf("session %s not resident", id)
	}
	if err := st.sess.UpdateGoal("second condition"); err != nil {
		t.Fatalf("UpdateGoal: %v", err)
	}

	updated := sse.waitFor(t, "goal.updated")
	if updated.Seq == 0 {
		t.Error("goal.updated event has no seq (must be durable)")
	}
	if updated.GoalCondition != "second condition" {
		t.Errorf("goal.updated GoalCondition = %q, want %q", updated.GoalCondition, "second condition")
	}

	// publishGoal folded the new condition into Session JSON, touching
	// nothing else: the goal must still read active (no fake pause/retry
	// transition — see the plan's Task 4 scope note).
	after := h.getGoalSummary(id)
	if after.Goal == nil {
		t.Fatalf("goal missing after update")
	}
	if after.Goal.Condition != "second condition" {
		t.Errorf("goal.Condition after update = %q, want %q", after.Goal.Condition, "second condition")
	}
	if !after.Goal.Active {
		t.Errorf("goal.Active after update = false, want true (an update must not fake a state transition)")
	}

	// The durable record landed in the server's own journal (not just the
	// live SSE fanout) — same idiom as TestGoalStalledJournaledAndActive.
	h.srv.mu.Lock()
	var journaled *Event
	for i := range h.srv.journal {
		ev := h.srv.journal[i]
		if ev.SessionID == id && ev.Type == "goal.updated" {
			journaled = &ev
			break
		}
	}
	h.srv.mu.Unlock()
	if journaled == nil {
		t.Fatal("goal.updated not found in the server journal")
	}
	if journaled.GoalCondition != "second condition" {
		t.Errorf("journaled goal.updated condition = %q, want %q", journaled.GoalCondition, "second condition")
	}

	// Release the blocked worker turn so the goal loop's goroutine unwinds
	// before the test ends.
	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE goal = %d, want 204", resp.StatusCode)
	}
}

// TestGoalUpdatedFoldRebuild is the rebuild-from-disk counterpart: it writes
// a journal containing goal.set then goal.updated (via the real engine
// methods, same as TestGoalPausedRestartYieldsIdleAndUsable's restart
// idiom), closes the server, opens a fresh one over the same directory, and
// asserts foldGoalRecordLocked (loadJournal's replay path) reports the
// UPDATED condition in GoalSummary — not the original goal.set condition.
func TestGoalUpdatedFoldRebuild(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
	}
	mutate := func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
	}
	srv1 := newServer(t, dir, prov, 0, mutate)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	resp, data := h1.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "first condition"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // worker turn in flight

	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatalf("session %s not resident", id)
	}
	if err := st.sess.UpdateGoal("second condition"); err != nil {
		t.Fatalf("UpdateGoal: %v", err)
	}

	// Confirm the durable record landed before tearing this process down.
	srv1.mu.Lock()
	var sawUpdated bool
	for _, ev := range srv1.journal {
		if ev.SessionID == id && ev.Type == "goal.updated" && ev.GoalCondition == "second condition" {
			sawUpdated = true
		}
	}
	srv1.mu.Unlock()
	if !sawUpdated {
		t.Fatal("goal.updated with the new condition not found in the first process's journal")
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0, mutate)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	rebuilt := h2.getGoalSummary(id)
	if rebuilt.Goal == nil {
		t.Fatalf("goal missing after rebuild")
	}
	if rebuilt.Goal.Condition != "second condition" {
		t.Errorf("goal.Condition after rebuild = %q, want %q (the updated condition, not the original goal.set)", rebuilt.Goal.Condition, "second condition")
	}
	if !rebuilt.Goal.Active {
		t.Errorf("goal.Active after rebuild = false, want true")
	}
}
