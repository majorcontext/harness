package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// restartGoalView decodes the Session JSON fields TestGoalActiveSurvivesRestart
// needs.
type restartGoalView struct {
	State string `json:"state"`
	Goal  *struct {
		Active bool `json:"active"`
	} `json:"goal"`
}

func (h *harness) getSessionGoal(id string) restartGoalView {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var v restartGoalView
	if err := json.Unmarshal(data, &v); err != nil {
		h.t.Fatal(err)
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
func TestGoalActiveSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("try 1")},
		eval:   [][]provider.Event{asstTurn("NOT MET: nope")},
	}
	mutate := func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
	}
	srv1 := newServer(t, dir, prov, 0, mutate)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	sse := h1.openSSE("?from=0", "")
	resp, data := h1.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "impossible", "max_turns": 1})
	if resp.StatusCode != 202 {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)

	before := h1.getSessionGoal(id)
	if before.Goal == nil || !before.Goal.Active {
		t.Fatalf("before restart, goal = %+v, want active (max turns exhausted, never cleared)", before.Goal)
	}
	sse.stop()

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0, mutate)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	after := h2.getSessionGoal(id)
	if after.Goal == nil || !after.Goal.Active {
		t.Errorf("goal after restart = %+v, want active", after.Goal)
	}
	if after.State != "goal-running" {
		t.Errorf("state after restart = %q, want goal-running", after.State)
	}

	// A tiny positive timeout (rather than the 30s default) keeps this fast
	// while still proving goal-done is NOT met — the goal survived restart
	// as active, so the wait must time out rather than resolve immediately.
	resp, data = h2.do("GET", "/session/"+id+"/wait?until=goal-done&timeout_s=1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=goal-done status %d: %s", resp.StatusCode, data)
	}
	var wait struct {
		Goal *struct {
			Active bool `json:"active"`
		} `json:"goal"`
	}
	if err := json.Unmarshal(data, &wait); err != nil {
		t.Fatal(err)
	}
	if wait.Goal == nil || !wait.Goal.Active {
		t.Errorf("wait response goal (restarted) = %+v, want still active", wait.Goal)
	}
}
