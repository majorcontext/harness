package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// pausedGoalView decodes the Session JSON fields the paused-goal tests need.
type pausedGoalView struct {
	State string `json:"state"`
	Goal  *struct {
		Active      bool   `json:"active"`
		Condition   string `json:"condition"`
		Paused      bool   `json:"paused"`
		PauseReason string `json:"pause_reason"`
		Waiting     bool   `json:"waiting"`
		Retryable   bool   `json:"retryable"`
	} `json:"goal"`
}

func (h *harness) getPausedGoalView(id string) pausedGoalView {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var v pausedGoalView
	if err := json.Unmarshal(data, &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

// TestGoalPausedRestartYieldsIdleAndUsable is the "operator trap" red test
// for deliverable 2(a): a session restarted with an armed-but-unattached
// goal (journal says active, no loop running in the new process) must
// surface paused=true/pause_reason=restart, an idle COMPOSITE state (not
// busy, not goal-running — the whole point: an operator or composer must be
// able to tell this apart from a genuinely live goal), a durable goal.paused
// record explaining the transition, and — because state reads idle — the
// session must still accept an ordinary prompt (the "usable prompt path").
func TestGoalPausedRestartYieldsIdleAndUsable(t *testing.T) {
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
	sse.stop()

	before := h1.getPausedGoalView(id)
	if before.Goal == nil || !before.Goal.Active {
		t.Fatalf("before restart, goal = %+v, want active (max turns exhausted, never cleared)", before.Goal)
	}
	if before.Goal.Paused {
		t.Fatalf("before restart, goal.Paused = true, want false (loop just ran in this same process)")
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0, mutate)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	after := h2.getPausedGoalView(id)
	if after.Goal == nil || !after.Goal.Active {
		t.Fatalf("goal after restart = %+v, want active", after.Goal)
	}
	if !after.Goal.Paused {
		t.Errorf("goal.Paused after restart = false, want true")
	}
	if after.Goal.PauseReason != "restart" {
		t.Errorf("goal.PauseReason after restart = %q, want %q", after.Goal.PauseReason, "restart")
	}
	if after.State != "idle" {
		t.Errorf("state after restart = %q, want idle (not busy, not goal-running)", after.State)
	}

	// A durable goal.paused record explains the transition honestly.
	srv2.mu.Lock()
	var pausedEv *Event
	for i := range srv2.journal {
		ev := srv2.journal[i]
		if ev.SessionID == id && ev.Type == "goal.paused" {
			pausedEv = &ev
			break
		}
	}
	srv2.mu.Unlock()
	if pausedEv == nil {
		t.Fatal("no goal.paused record found in the restarted server's journal")
	}
	if pausedEv.GoalPauseReason != "restart" {
		t.Errorf("goal.paused record GoalPauseReason = %q, want %q", pausedEv.GoalPauseReason, "restart")
	}

	// GET /session/{id}/wait?until=idle must resolve immediately, not time
	// out — an idle composite state is exactly the condition it waits for.
	resp, data = h2.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
	}
	var wait struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &wait); err != nil {
		t.Fatal(err)
	}
	if wait.State != "idle" {
		t.Errorf("wait until=idle State = %q, want idle", wait.State)
	}

	// Usable prompt path: an ordinary prompt_async must not 409 just
	// because a paused goal is still nominally active.
	resp, data = h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async on a paused/idle session status = %d, want 202: %s", resp.StatusCode, data)
	}

	// The prompt above is admitted asynchronously (202): srv2 spawns a
	// runPrompt goroutine, tracked by srv2.wg, that keeps writing the session
	// journal into dir (== t.TempDir()). Drain blocks on wg.Wait until that
	// goroutine finishes, so no writer survives into t.TempDir()'s RemoveAll
	// cleanup — otherwise a journal write races the directory removal and the
	// cleanup fails with "directory not empty" under load.
	srv2.Drain(context.Background())
}

// TestGoalStalledProviderBackoffSurfacesPaused is deliverable 2(b)'s wire
// test: while the retryable-backoff park machinery (engine/goal.go) waits
// out provider weather, that state must be visible as paused=true,
// pause_reason="provider-backoff" — without changing the underlying
// behavior (the loop retries and eventually succeeds exactly as before).
func TestGoalStalledProviderBackoffSurfacesPaused(t *testing.T) {
	prov := &goalProv{
		name:       "test",
		workerErrN: 1, // first attempt fails retryably, second (retried) attempt succeeds
		workerErr:  provider.MarkRetryable(errFakeOverload(), provider.RetryableOverloaded),
		worker:     [][]provider.Event{asstTurn("done")},
		eval:       [][]provider.Event{asstTurn("MET: looks complete")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}

	stalled := sse.waitFor(t, "goal.stalled")
	if !stalled.GoalPaused {
		t.Error("goal.stalled event GoalPaused = false, want true (waiting out provider weather)")
	}
	if stalled.GoalPauseReason != "provider-backoff" {
		t.Errorf("goal.stalled event GoalPauseReason = %q, want %q", stalled.GoalPauseReason, "provider-backoff")
	}

	view := h.getPausedGoalView(id)
	if view.Goal == nil || !view.Goal.Paused || view.Goal.PauseReason != "provider-backoff" {
		t.Errorf("session goal = %+v, want paused=true pause_reason=provider-backoff", view.Goal)
	}
	// Behavior is unchanged: state stays goal-running (a loop IS attached
	// and running, just waiting), never idle, during a provider-backoff
	// pause — only the restart pause forces idle.
	if view.State != "goal-running" {
		t.Errorf("state during provider-backoff pause = %q, want goal-running", view.State)
	}

	// The park machinery self-re-arms once the backoff elapses and the
	// retry succeeds: paused clears without any client action ("already
	// re-arms" per the spec).
	evs := sse.collectUntilIdle(t)
	var sawAchieved bool
	for _, ev := range evs {
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
	}
	if !sawAchieved {
		t.Fatal("expected goal.achieved after the retryable stall recovered")
	}
	after := h.getPausedGoalView(id)
	if after.Goal != nil && after.Goal.Paused {
		t.Errorf("goal.Paused after achievement = true, want false")
	}
}

// TestGoalReArmClearsRestartPause is deliverable 2(c)'s test for the
// restart half: POST /session/{id}/goal with the persisted condition
// re-arms a paused/restart goal (rather than 409ing "already active") and
// clears paused — starting a fresh loop.
func TestGoalReArmClearsRestartPause(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("try 1"), asstTurn("try 2")},
		eval:   [][]provider.Event{asstTurn("NOT MET: nope"), asstTurn("MET: now it is")},
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
	sse.stop()
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	srv2 := newServer(t, dir, prov, 0, mutate)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	before := h2.getPausedGoalView(id)
	if !before.Goal.Paused || before.Goal.PauseReason != "restart" {
		t.Fatalf("before re-arm, goal = %+v, want paused/restart", before.Goal)
	}

	// Replay only events from here on: from=0 would replay the FIRST
	// (pre-restart) turn's history, including its own session.status idle,
	// and collectUntilIdle would stop right there.
	resp, data = h2.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get seq status %d: %s", resp.StatusCode, data)
	}
	var seqView struct {
		Seq int64 `json:"seq"`
	}
	mustUnmarshal(t, data, &seqView)
	sse2 := h2.openSSE(fmt.Sprintf("?from=%d", seqView.Seq), "")
	resp, data = h2.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "impossible"})
	if resp.StatusCode != 202 {
		t.Fatalf("re-arm POST goal status %d: %s", resp.StatusCode, data)
	}
	evs := sse2.collectUntilIdle(t)
	var sawAchieved bool
	for _, ev := range evs {
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
	}
	if !sawAchieved {
		t.Fatal("expected the re-armed goal to achieve")
	}

	after := h2.getPausedGoalView(id)
	if after.Goal == nil || after.Goal.Active {
		t.Fatalf("after re-arm and achievement, goal = %+v, want inactive (achieved)", after.Goal)
	}
	if after.Goal.Paused {
		t.Errorf("goal.Paused after re-arm = true, want false")
	}
}

// TestGoalReArmDifferentConditionUpdatesAndResumes replaces
// TestGoalReArmMismatchedConditionRejected: Task 5 changes handleGoal's
// paused-re-arm branch so a DIFFERENT condition than the one currently
// active (paused or not) updates the goal in place and resumes the loop
// with it, instead of rejecting with 409 — see handleGoal's doc comment.
// This proves the NEW condition (not the original persisted goal.set one)
// drives the resumed loop through to achievement, that a goal.updated
// record lands, and that the response reports status "started".
func TestGoalReArmDifferentConditionUpdatesAndResumes(t *testing.T) {
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
	sse.stop()
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	prov2 := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("try 2")},
		eval:   [][]provider.Event{asstTurn("MET: now it is")},
	}
	srv2 := newServer(t, dir, prov2, 0, func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov2.Name(), Model: "eval"}
	})
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	// Replay only events from here on (see TestGoalReArmClearsRestartPause's
	// comment for why: from=0 would replay the first, pre-restart turn's
	// history and collectUntilIdle would stop at its idle).
	resp, data = h2.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get seq status %d: %s", resp.StatusCode, data)
	}
	var seqView struct {
		Seq int64 `json:"seq"`
	}
	mustUnmarshal(t, data, &seqView)
	sse2 := h2.openSSE(fmt.Sprintf("?from=%d", seqView.Seq), "")

	resp, data = h2.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "a different goal entirely"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("re-arm with a different condition status = %d, want 202: %s", resp.StatusCode, data)
	}
	var posted struct {
		Status string `json:"status"`
	}
	mustUnmarshal(t, data, &posted)
	if posted.Status != "started" {
		t.Errorf("re-arm response status = %q, want %q", posted.Status, "started")
	}

	evs := sse2.collectUntilIdle(t)
	var sawUpdated, sawAchieved bool
	for _, ev := range evs {
		switch ev.Type {
		case "goal.updated":
			sawUpdated = true
			if ev.GoalCondition != "a different goal entirely" {
				t.Errorf("goal.updated condition = %q, want %q", ev.GoalCondition, "a different goal entirely")
			}
		case "goal.achieved":
			sawAchieved = true
		}
	}
	if !sawUpdated {
		t.Errorf("goal events after re-arm = %v, want a goal.updated", evs)
	}
	if !sawAchieved {
		t.Fatal("expected the re-armed goal (with the new condition) to achieve")
	}

	after := h2.getPausedGoalView(id)
	if after.Goal == nil || after.Goal.Active {
		t.Fatalf("after re-arm and achievement, goal = %+v, want inactive (achieved)", after.Goal)
	}
	if after.Goal.Condition != "a different goal entirely" {
		t.Errorf("final goal condition = %q, want %q", after.Goal.Condition, "a different goal entirely")
	}
}

// errFakeOverload avoids importing errors just for this one sentinel — kept
// separate so goal_paused_test.go has no import beyond what it already
// needs.
func errFakeOverload() error { return fakeOverloadErr{} }

type fakeOverloadErr struct{}

func (fakeOverloadErr) Error() string { return "test: fake overloaded_error" }

// TestGoalReArmAfterRetryableStallRestartNotBackoffPaused encodes the PR#70
// review finding: a goal whose LAST journal record before the box died was
// goal.stalled(retryable=true, waiting=true) restores those fold fields at
// boot, with pausedRestart layered on top. Re-arm cleared only
// pausedRestart, so pauseView's provider-backoff case fired and a client
// polling right after the 202 saw paused=true/"provider-backoff" on a
// freshly re-armed, genuinely-running goal. The re-arm path must reset the
// stall fields exactly like the fresh-goal (evtGoalSet) fold does.
func TestGoalReArmAfterRetryableStallRestartNotBackoffPaused(t *testing.T) {
	dir := t.TempDir()
	// Every worker attempt fails retryably: the loop parks (goal.stalled
	// with waiting=true) and the server dies parked.
	prov1 := &goalProv{
		name:       "test",
		workerErrN: 1000,
		workerErr:  provider.MarkRetryable(errFakeOverload(), provider.RetryableOverloaded),
		eval:       [][]provider.Event{},
	}
	mutate := func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov1.Name(), Model: "eval"}
	}
	srv1 := newServer(t, dir, prov1, 0, mutate)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	sse := h1.openSSE("?from=0", "")
	resp, data := h1.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	stalled := sse.waitFor(t, "goal.stalled")
	if !stalled.GoalWaiting {
		t.Fatalf("goal.stalled GoalWaiting = false, want true (must die parked)")
	}
	sse.stop()
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	// Restart: the journal tail is goal.stalled(retryable, waiting). The
	// second provider BLOCKS the worker turn so nothing (no goal.eval) can
	// reset the stale fold fields before we read the view.
	prov2 := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
	}
	srv2 := newServer(t, dir, prov2, 0, func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov2.Name(), Model: "eval"}
	})
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	before := h2.getPausedGoalView(id)
	if before.Goal == nil || !before.Goal.Paused || before.Goal.PauseReason != "restart" {
		t.Fatalf("before re-arm, goal = %+v, want paused/restart (restart wins over stale backoff fields)", before.Goal)
	}

	resp, data = h2.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("re-arm POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov2.started // the re-armed loop is live, worker mid-"stream", no eval yet

	after := h2.getPausedGoalView(id)
	if after.Goal == nil {
		t.Fatal("no goal on session after re-arm")
	}
	if after.Goal.Paused {
		t.Errorf("immediately after re-arm 202: goal paused=true pause_reason=%q, want paused=false (stale stall fields must reset like the evtGoalSet fold)", after.Goal.PauseReason)
	}

	// Unblock the loop by clearing the goal; the blocked stream ends via
	// prompt-context cancel.
	resp, _ = h2.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear goal status %d", resp.StatusCode)
	}
	_, _ = h2.do("POST", "/session/"+id+"/abort", nil)
}
