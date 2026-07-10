package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestAwaitingInputSurvivesRestart is the red-first test for issue #64 item
// 1: questionState is in-memory only, never rebuilt in loadJournal, so a
// genuinely-awaiting session restarted mid-question used to report
// state != "awaiting-input" on GET /session/{id} and never satisfy
// wait?until=awaiting-input in the SECOND process — even though the
// question.asked record (and the engine session's own awaitingQuestion, via
// AwaitingQuestion) was durably there the whole time. POST /answer already
// worked across a restart (it reads the engine session directly via
// claimForPrompt); GET and /wait must too.
func TestAwaitingInputSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		askUserTurn("tc1", `{"questions":[{"question":"Which environment?","options":["staging","prod"]}]}`),
	}}

	h1 := newHarnessDir(t, dir, prov)
	id := h1.createSession("test/m1")
	sse := h1.openSSE("?from=0", "")
	resp, data := h1.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "help me deploy"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)

	// Confirm it is actually awaiting-input in process 1 before restarting.
	before := h1.getSessionQuestion(id)
	if before.State != "awaiting-input" || before.Question == nil {
		t.Fatalf("before restart, session = %+v, want awaiting-input with a pending question", before)
	}
	sse.stop()

	if err := h1.srv.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}

	// A brand-new Server over the SAME session dir, simulating a restart. It
	// never ran this prompt itself; everything it knows about the pending
	// question must come from replaying events.jsonl in loadJournal.
	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	after := h2.getSessionQuestion(id)
	if after.State != "awaiting-input" {
		t.Errorf("state after restart = %q, want awaiting-input", after.State)
	}
	if after.Question == nil || after.Question.CallID != "tc1" {
		t.Errorf("question after restart = %+v, want CallID tc1", after.Question)
	}

	// wait?until=awaiting-input must resolve immediately in the second
	// process too — the headless wait->answer loop the design doc describes
	// must not break specifically across a restart.
	resp, data = h2.do("GET", "/session/"+id+"/wait?until=awaiting-input&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=awaiting-input (restarted) status %d: %s", resp.StatusCode, data)
	}
	var wait struct {
		State    string `json:"state"`
		Question *struct {
			CallID string `json:"call_id"`
		} `json:"question"`
	}
	if err := json.Unmarshal(data, &wait); err != nil {
		t.Fatal(err)
	}
	if wait.State != "awaiting-input" {
		t.Errorf("wait response state (restarted) = %q, want awaiting-input: %s", wait.State, data)
	}
	if wait.Question == nil || wait.Question.CallID != "tc1" {
		t.Errorf("wait response question (restarted) = %+v, want CallID tc1: %s", wait.Question, data)
	}
}

// TestGoalActiveSurvivesRestart is the goal-tracker half of issue #64 item
// 1: goalState/goalMaxTurns are in-memory only, never rebuilt in
// loadJournal, so an active (never achieved/cleared) goal used to read back
// as no goal at all (Session.Goal == nil, composite state falling back to
// idle) after a restart — even though goal.set (and no later
// achieved/cleared) was durably on disk the whole time. Here the goal
// exhausts its turn budget without being met, which — like an awaiting_input
// pause — leaves goalActive true in the journal (engine/goal.go's terminal
// "max turns" case never clears the goal).
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

	before := h1.getSessionQuestion(id)
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

	after := h2.getSessionQuestion(id)
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
