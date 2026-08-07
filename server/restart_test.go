package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// restartGoalView decodes the Session JSON fields TestGoalActiveSurvivesRestart
// needs.
type restartGoalView struct {
	State string `json:"state"`
	Goal  *struct {
		Active      bool   `json:"active"`
		Paused      bool   `json:"paused"`
		PauseReason string `json:"pause_reason"`
	} `json:"goal"`
}

// getRestartGoalView drives GET /session/{id} directly (no HTTP listener, so it
// works inside a synctest bubble) and decodes the goal-summary fields.
func getRestartGoalView(t *testing.T, srv *Server, id string) restartGoalView {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/session/"+id, nil)
	req.SetPathValue("id", id)
	srv.handleGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session status %d: %s", rec.Code, rec.Body)
	}
	var v restartGoalView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestGoalActiveSurvivesRestart is the goal-tracker half of issue #64 item
// 1: goalState is in-memory only, never rebuilt in loadJournal, so an
// active (never achieved/cleared) goal used to read back as no goal at all
// (Session.Goal == nil, composite state falling back to idle) after a
// restart — even though goal.set (and no later achieved/cleared) was
// durably on disk the whole time. Here the goal exhausts its turn budget
// without being met, which leaves goalActive true in the journal
// (engine/goal.go's terminal "max turns" case never clears the goal).
//
// The composite state assertion below intentionally differs from this
// test's original version: it used to assert "goal-running" after restart,
// which is exactly the operator trap deliverable 2 (see
// docs/design/fleet-model.md and server/goal_paused_test.go) closes — an
// active goal restored with no loop attached is not progressing, and
// "goal-running" forever is indistinguishable from a live goal. It now
// reads paused=true/pause_reason="restart" and idle, and prompt_async
// remains usable (see TestGoalPausedRestartYieldsIdleAndUsable for the
// dedicated coverage).
func TestGoalActiveSurvivesRestart(t *testing.T) {
	// Runs on fake time inside a synctest bubble: the goal loop and the
	// tiny GET /wait?timeout_s=1 poll below both cost zero real wall-clock
	// time. Handlers are driven directly with httptest recorders (no real
	// listener — a bubble forbids real network I/O) exactly like
	// TestGoalEvaluatorExhaustedTerminalOutcome. The restart is a SECOND
	// newServer over the SAME dir (ADOPT); the first server's goal loop is
	// settled via srv1.wg.Wait() before the second is constructed.
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:   "test",
			worker: [][]provider.Event{asstTurn("try 1")},
			eval:   [][]provider.Event{asstTurn("NOT MET: nope")},
		}
		mutate := func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		}
		srv1 := newServer(t, dir, prov, 0, mutate)
		id := createSessionDirect(t, srv1, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"impossible","max_turns":1}`))
		greq.SetPathValue("id", id)
		srv1.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		srv1.wg.Wait() // the goal loop (max turns exhausted, never cleared) finishes

		before := getRestartGoalView(t, srv1, id)
		if before.Goal == nil || !before.Goal.Active {
			t.Fatalf("before restart, goal = %+v, want active (max turns exhausted, never cleared)", before.Goal)
		}

		if err := srv1.Close(); err != nil {
			t.Fatalf("closing first server: %v", err)
		}

		srv2 := newServer(t, dir, prov, 0, mutate)
		// Close srv2 before the bubble ends: harmless today (New spawns no
		// goroutine), but if it ever does, a leaked goroutine would surface as
		// an obvious close rather than an opaque synctest deadlock.
		defer srv2.Close()

		after := getRestartGoalView(t, srv2, id)
		if after.Goal == nil || !after.Goal.Active {
			t.Errorf("goal after restart = %+v, want active", after.Goal)
		}
		if after.Goal == nil || !after.Goal.Paused || after.Goal.PauseReason != "restart" {
			t.Errorf("goal after restart = %+v, want paused=true pause_reason=restart", after.Goal)
		}
		if after.State != "idle" {
			t.Errorf("state after restart = %q, want idle (paused, not goal-running)", after.State)
		}

		// A tiny positive timeout (rather than the 30s default) keeps this fast
		// while still proving goal-done is NOT met — the goal survived restart
		// as active, so the wait must time out rather than resolve immediately.
		// Under fake time the 1s timer fires instantly once the handler is the
		// only (durably blocked) goroutine in the bubble.
		wrec := httptest.NewRecorder()
		wreq := httptest.NewRequest("GET", "/session/"+id+"/wait?until=goal-done&timeout_s=1", nil)
		wreq.SetPathValue("id", id)
		srv2.handleWait(wrec, wreq)
		if wrec.Code != http.StatusOK {
			t.Fatalf("wait until=goal-done status %d: %s", wrec.Code, wrec.Body)
		}
		var wait struct {
			Goal *struct {
				Active bool `json:"active"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(wrec.Body.Bytes(), &wait); err != nil {
			t.Fatal(err)
		}
		if wait.Goal == nil || !wait.Goal.Active {
			t.Errorf("wait response goal (restarted) = %+v, want still active", wait.Goal)
		}
	})
}
