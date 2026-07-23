package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
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

// TestGoalPostWhileGoalRunningUpdatesInPlace is invariant 7's dedicated test:
// POST /goal while a goal loop is actively running updates the condition in
// place — 200 "updated", no second loop (no second goal.set, no second
// run-slot claim/session.status busy beyond the original POST's).
func TestGoalPostWhileGoalRunningUpdatesInPlace(t *testing.T) {
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

	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "second condition"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST goal while running status %d, want 200: %s", resp.StatusCode, data)
	}
	var posted struct {
		Status string `json:"status"`
		Seq    int64  `json:"seq"`
	}
	if err := json.Unmarshal(data, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Status != "updated" {
		t.Errorf("response status = %q, want %q", posted.Status, "updated")
	}
	if posted.Seq == 0 {
		t.Error("response seq = 0, want a real durable sequence number")
	}

	after := h.getGoalSummary(id)
	if after.Goal == nil || after.Goal.Condition != "second condition" {
		t.Fatalf("goal after update-while-running = %+v, want condition %q", after.Goal, "second condition")
	}

	// Release the blocked worker turn so the goal loop's goroutine unwinds
	// before the test ends.
	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE goal = %d, want 204", resp.StatusCode)
	}
	evs := sse.collectUntilIdle(t)

	var setCount, updatedCount, busyCount int
	for _, ev := range evs {
		switch {
		case ev.Type == "goal.set":
			setCount++
		case ev.Type == "goal.updated":
			updatedCount++
		case ev.Type == evtSessionStatus && ev.Status == "busy":
			busyCount++
		}
	}
	if setCount != 1 {
		t.Errorf("goal.set count = %d, want 1 (no second loop registered)", setCount)
	}
	if updatedCount != 1 {
		t.Errorf("goal.updated count = %d, want 1", updatedCount)
	}
	if busyCount != 1 {
		t.Errorf("session.status busy count = %d, want 1 (the update-in-place POST must not claim the run slot a second time)", busyCount)
	}
}

// autoArmProv serves a plain (tool-bearing) prompt that blocks until
// released, then scripted worker/evaluator turns for the goal loop that
// starts afterward. It is the fixture TestGoalPostWhilePromptBusyArmsThen
// AutoStarts, TestAutoArmRaceWithIncomingPrompt, and
// TestDeleteGoalClearsArmedNoLoop need: something occupying the run slot
// with an ORDINARY prompt (not a goal loop), so a POST /goal made while it is
// busy hits handleGoalBusy's "not active" branch rather than its
// running-goal-loop branch (that case is goalProv's blockWorker, used
// elsewhere).
//
// Unlike goalProv's blockWorker (a static, permanent block), autoArmProv's
// block is a mutable flag: the test flips it off before releasing, so
// worker turns AFTER the release (i.e., the auto-armed goal loop's own
// turns) run scripted instead of blocking forever.
type autoArmProv struct {
	name    string
	mu      sync.Mutex
	worker  [][]provider.Event
	eval    [][]provider.Event
	wi, ei  int
	blocked bool

	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func (p *autoArmProv) Name() string { return p.name }

func (p *autoArmProv) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 { // evaluator (tool-less)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	p.mu.Lock()
	blocked := p.blocked
	p.mu.Unlock()
	if blocked {
		return &autoArmBlockingStream{ctx: ctx, p: p}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

type autoArmBlockingStream struct {
	ctx context.Context
	p   *autoArmProv
}

func (s *autoArmBlockingStream) Next() (provider.Event, error) {
	s.p.startedOnce.Do(func() { close(s.p.started) })
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.p.release:
		msg := &message.Message{ID: "msg_released", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "released"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *autoArmBlockingStream) Close() error { return nil }

// TestGoalPostWhilePromptBusyArmsThenAutoStarts is invariant 8's dedicated
// test: POST /goal while a PLAIN PROMPT is busy (no goal active) is armed
// (202 "armed") rather than rejected; once the prompt finishes, the server
// auto-arms — the prompt's own session.status idle is observed BEFORE the
// goal's session.status busy — and the loop runs to achievement without any
// further client action.
func TestGoalPostWhilePromptBusyArmsThenAutoStarts(t *testing.T) {
	prov := &autoArmProv{
		name:    "test",
		blocked: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
		worker:  [][]provider.Event{asstTurn("working"), asstTurn("done")},
		eval:    [][]provider.Event{asstTurn("NOT MET: needs more"), asstTurn("MET: looks complete")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // the plain prompt's worker turn is now in flight, occupying the run slot

	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal while prompt busy status %d, want 202: %s", resp.StatusCode, data)
	}
	var posted struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Status != "armed" {
		t.Fatalf("POST goal while prompt busy response status = %q, want %q", posted.Status, "armed")
	}

	// The goal is armed (RegisterGoal already ran, synchronously, inside the
	// handler above) but not yet running: Session JSON must say so. The
	// composite state already reads goal-running here even though no loop
	// has started yet — compositeState's documented precedence rule is that
	// an active goal wins over the momentary running/busy flag REGARDLESS
	// (the one exception, restart-pause, does not apply to a freshly armed
	// goal); this is not a distinct "armed" presentation, just the ordinary
	// "a goal is active" one.
	view := h.getPausedGoalView(id)
	if view.Goal == nil || !view.Goal.Active {
		t.Fatalf("goal after arming = %+v, want active", view.Goal)
	}
	if view.State != "goal-running" {
		t.Errorf("state while the goal is armed = %q, want goal-running (compositeState: active goal wins over momentary busy/idle)", view.State)
	}

	// Let the goal loop's own worker/evaluator turns run scripted instead of
	// blocking, then release the blocked prompt turn.
	prov.mu.Lock()
	prov.blocked = false
	prov.mu.Unlock()
	close(prov.release)

	// First batch: through the prompt's own completion (its session.status
	// idle) — goal.set already arrived earlier (synchronous RegisterGoal).
	promptEvs := sse.collectUntilIdle(t)
	var sawGoalSet, sawPromptIdle bool
	for _, ev := range promptEvs {
		if ev.Type == "goal.set" {
			sawGoalSet = true
		}
		if ev.Type == evtSessionStatus && ev.Status == "idle" {
			sawPromptIdle = true
		}
	}
	if !sawGoalSet {
		t.Errorf("events through the prompt's idle = %v, want a goal.set", promptEvs)
	}
	if !sawPromptIdle {
		t.Fatalf("events through the prompt's completion = %v, want a session.status idle", promptEvs)
	}

	// Second batch: the auto-armed loop's own busy/eval/achieved/idle —
	// arriving strictly AFTER the prompt's idle above (invariant 8).
	loopEvs := sse.collectUntilIdle(t)
	var sawGoalBusy, sawAchieved bool
	for _, ev := range loopEvs {
		if ev.Type == evtSessionStatus && ev.Status == "busy" {
			sawGoalBusy = true
		}
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
	}
	if !sawGoalBusy {
		t.Errorf("events after the prompt's idle = %v, want a session.status busy (the auto-armed goal starting)", loopEvs)
	}
	if !sawAchieved {
		t.Fatalf("events after the prompt's idle = %v, want the auto-armed loop to run to a goal.achieved", loopEvs)
	}
}

// TestDeleteGoalDuringArmedPromptLeavesPromptRunning is the PR #77 review's
// Finding 1: the 202 "armed" path (handleGoalBusy's register-and-arm branch)
// creates an active goal while a PLAIN PROMPT still holds the run slot -- in
// that window sessionState.cancel belongs to the prompt (claimForPrompt set
// it for the prompt's own claim; no goal loop has started yet). DELETE /goal
// must clear the goal without cancelling that prompt's context -- cancelling
// here would abort the very turn that (typically) armed the goal via the
// `goal` session tool.
//
// This drives a prompt to blocked, arms a goal via POST /goal (202 "armed"),
// deletes it (204, goal.cleared journaled), then releases the prompt and
// asserts it runs to a NORMAL completion -- no session.error, no abort -- and
// that no goal loop ever starts (maybeAutoArmGoal's tail check must see
// ClearGoal already ran and no-op).
func TestDeleteGoalDuringArmedPromptLeavesPromptRunning(t *testing.T) {
	prov := &autoArmProv{
		name:    "test",
		blocked: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // the plain prompt's worker turn is now in flight, occupying the run slot

	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal while prompt busy status %d, want 202: %s", resp.StatusCode, data)
	}
	var posted struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Status != "armed" {
		t.Fatalf("POST goal while prompt busy response status = %q, want %q", posted.Status, "armed")
	}

	resp, data = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE armed goal status %d, want 204: %s", resp.StatusCode, data)
	}

	// Drain the SSE stream through the goal.cleared record before releasing
	// the prompt below, so the post-release assertions only look at events
	// that arrive AFTER the clear (a "goal.set" from the arming step above is
	// expected in this earlier batch and must not be confused with a second
	// loop starting after release).
	clearedEv := sse.waitFor(t, "goal.cleared")
	if clearedEv.Seq == 0 {
		t.Error("goal.cleared event has no seq (must be durable)")
	}

	after := h.getGoalSummary(id)
	if after.Goal != nil && after.Goal.Active {
		t.Fatalf("goal after DELETE = %+v, want inactive", after.Goal)
	}

	// Now release the prompt's blocked worker turn: it must run to a normal
	// completion. If DELETE had cancelled the prompt's context (the bug this
	// test guards against), releasing here would race the cancellation and
	// the prompt would observe ctx.Err() instead of completing.
	close(prov.release)

	promptEvs := sse.collectUntilIdle(t)
	var sawIdle bool
	for _, ev := range promptEvs {
		if ev.Type == evtSessionError {
			t.Fatalf("prompt turn errored after DELETE /goal (want it to complete undisturbed): %+v", ev)
		}
		if ev.Type == evtSessionAborted {
			t.Fatalf("prompt turn aborted after DELETE /goal (want it to complete undisturbed): %+v", ev)
		}
		if ev.Type == evtSessionStatus && ev.Status == "idle" {
			sawIdle = true
		}
		if ev.Type == "goal.set" || ev.Type == "goal.achieved" || (ev.Type == evtSessionStatus && ev.Status == "busy") {
			t.Errorf("events after DELETE+release = %v, want no goal loop activity (goal was cleared before the prompt finished)", promptEvs)
		}
	}
	if !sawIdle {
		t.Fatalf("events after releasing the prompt = %v, want a session.status idle (normal completion)", promptEvs)
	}
}

// TestClaimForPromptResetsStaleGoalLoop is a white-box, direct test of
// claimForPrompt's own defense-in-depth reset (server/handlers.go, PR review
// finding): every OTHER path that flips goalLoop back to false already runs
// at some occupant's tail (runPrompt's, runGoal's, handleCompact's,
// handleGoal's two rollback branches -- see the goalLoop field's doc comment
// in server.go), so the flag's correctness has always depended on every one
// of those tails running. This test proves the independent, cheaper
// invariant claimForPrompt itself now provides: even if EVERY tail were
// skipped (simulated here by forcing goalLoop = true directly on an idle,
// resident sessionState with no tail ever having reset it), the next
// successful claim resets it to false unconditionally -- so a stale true can
// never survive into a new occupancy no matter what came before.
func TestClaimForPromptResetsStaleGoalLoop(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	if st == nil {
		h.srv.mu.Unlock()
		t.Fatal("session not resident after createSession")
	}
	// Force the stale state directly, bypassing every tail that would
	// ordinarily reset it: idle (not running), but goalLoop left true, as if
	// some prior occupant's tail-reset never ran.
	st.running = false
	st.cancel = nil
	st.goalLoop = true
	h.srv.mu.Unlock()

	claimed, ctx, _, code, _ := h.srv.claimForPrompt(id)
	if code != 0 {
		t.Fatalf("claimForPrompt code = %d, want 0 (success)", code)
	}
	if claimed.goalLoop {
		t.Error("goalLoop = true after claimForPrompt, want false: the claim site must reset it independent of any tail")
	}

	// Release the claim so the harness's cleanup (Drain) doesn't hang on a
	// permanently-running session.
	h.srv.mu.Lock()
	claimed.running = false
	claimed.cancel = nil
	h.srv.mu.Unlock()
	if ctx.Err() != nil {
		t.Fatalf("claimed ctx already done: %v", ctx.Err())
	}
}

// TestGoalToolSetAutoArmsAfterPrompt is the headline user story from
// docs/plans/2026-07-19-goal-self-adjust.md: a prompt whose scripted tool
// call invokes the `goal` session tool's `set` action (registering a goal
// mid-turn, in-process, no HTTP round-trip); once that prompt finishes, the
// goal auto-arms and runs to achievement — with no POST /goal at all.
// Requires GoalTool wired on the server's session config (see newServer's
// mkCfg: GoalTool is enabled whenever an evaluator is configured, mirroring
// production's cmd/harness wiring).
func TestGoalToolSetAutoArmsAfterPrompt(t *testing.T) {
	prov := &goalProv{
		name: "test",
		worker: [][]provider.Event{
			{
				{
					Type: provider.EventDone,
					Message: &message.Message{
						ID:   "m_settool",
						Role: message.RoleAssistant,
						Parts: message.Parts{
							&message.ToolCall{CallID: "call_1", Name: "goal", Arguments: json.RawMessage(`{"action":"set","condition":"self-set condition"}`)},
						},
					},
					StopReason: provider.StopToolUse,
				},
			},
			asstTurn("prompt turn done"),
			asstTurn("goal loop turn"),
		},
		eval: [][]provider.Event{asstTurn("MET: self-set condition satisfied")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "set your own goal"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}

	promptEvs := sse.collectUntilIdle(t)
	var sawSet bool
	for _, ev := range promptEvs {
		if ev.Type == "goal.set" {
			sawSet = true
			if ev.GoalCondition != "self-set condition" {
				t.Errorf("goal.set condition = %q, want %q", ev.GoalCondition, "self-set condition")
			}
		}
	}
	if !sawSet {
		t.Fatalf("events through the prompt's idle = %v, want a goal.set from the tool call mid-turn", promptEvs)
	}

	loopEvs := sse.collectUntilIdle(t)
	var sawBusy, sawAchieved bool
	for _, ev := range loopEvs {
		if ev.Type == evtSessionStatus && ev.Status == "busy" {
			sawBusy = true
		}
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
	}
	if !sawBusy {
		t.Errorf("events after the prompt's idle = %v, want a session.status busy (the auto-armed goal starting)", loopEvs)
	}
	if !sawAchieved {
		t.Fatalf("events after the prompt's idle = %v, want the self-set goal to run to achievement", loopEvs)
	}

	after := h.getGoalSummary(id)
	if after.Goal == nil || after.Goal.Active {
		t.Fatalf("final goal state = %+v, want inactive (achieved)", after.Goal)
	}
}

// TestAutoArmRaceWithIncomingPrompt is invariant 9: maybeAutoArmGoal goes
// through claimForPrompt exactly like any other caller, so a racing incoming
// prompt_async is resolved by the run-slot mutex the same way any other
// claimForPrompt race is — no deadlock, no double loop, and whichever side
// wins is legal. This forces the race deterministically (via the test-only
// autoArmRace seam) rather than relying on an unobserved goroutine-scheduling
// coin flip: the racer's own claim is made to land BEFORE auto-arm's, so
// auto-arm always loses here — proving the losing side returns cleanly
// rather than deadlocking or starting a second loop. The goal is not
// stranded: the racer's own prompt finishes and its OWN runPrompt tail calls
// maybeAutoArmGoal again, this time uncontested, and the loop runs to
// achievement.
func TestAutoArmRaceWithIncomingPrompt(t *testing.T) {
	prov := &autoArmProv{
		name:    "test",
		blocked: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
		worker: [][]provider.Event{
			asstTurn("racer done"),     // the racing prompt_async's own turn
			asstTurn("goal loop turn"), // the eventually-spawned goal loop's worker turn
		},
		eval: [][]provider.Event{asstTurn("MET: done")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("initial prompt_async status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal while prompt busy status %d: %s", resp.StatusCode, data)
	}

	racerDone := make(chan *http.Response, 1)
	racerClaimed := make(chan struct{})
	var once sync.Once
	h.srv.autoArmRace = func() {
		once.Do(func() {
			go func() {
				r, _ := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
					"parts": []map[string]string{{"type": "text", "text": "racer"}},
				})
				close(racerClaimed)
				racerDone <- r
			}()
			// Force a real, deterministic ordering (the racer wins) instead
			// of an unobserved coin flip: block auto-arm's own
			// claimForPrompt call (below, after this hook returns) until the
			// racer has actually claimed the slot. handlePrompt's
			// claimForPrompt call (or its durable-enqueue fallback) always
			// runs synchronously, BEFORE the HTTP response is written — so
			// by the time h.do returns above, the racer has unconditionally
			// already gone first, and closing racerClaimed right after is a
			// signal that can never be missed. An earlier version of this
			// hook instead polled st.running in a runtime.Gosched() loop:
			// that is racing an edge (the racer's claim) with a level check
			// under a lock, and if the racer's entire turn — claim, run,
			// release — completes between two polls (observed under
			// GOMAXPROCS=1 with -race, where this goroutine can dominate
			// scheduling), the poll loop spins forever waiting for a
			// transition that already happened and reverted, hanging the
			// test. Waiting on a channel closed by the one event we
			// actually care about removes the poll entirely.
			<-racerClaimed
		})
	}

	prov.mu.Lock()
	prov.blocked = false
	prov.mu.Unlock()
	close(prov.release)

	// Through the original prompt's own idle (auto-arm fires here, loses the
	// forced race, and returns without starting a loop).
	sse.collectUntilIdle(t)

	racerResp := <-racerDone
	if racerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("racer prompt_async status = %d, want 202 (won the race)", racerResp.StatusCode)
	}

	// Through the racer's own idle (its own runPrompt tail's auto-arm call
	// this time succeeds, uncontested).
	sse.collectUntilIdle(t)

	// Through the goal loop's own busy/eval/achieved/idle.
	loopEvs := sse.collectUntilIdle(t)
	var setCount, achievedCount int
	for _, ev := range loopEvs {
		if ev.Type == "goal.set" {
			setCount++
		}
		if ev.Type == "goal.achieved" {
			achievedCount++
		}
	}
	if achievedCount != 1 {
		t.Errorf("goal.achieved count = %d, want 1 (no double loop)", achievedCount)
	}
	if setCount != 0 {
		t.Errorf("goal.set count in the final batch = %d, want 0 (the goal was registered once already, no re-registration)", setCount)
	}

	after := h.getGoalSummary(id)
	if after.Goal == nil || after.Goal.Active {
		t.Fatalf("final goal state = %+v, want inactive (achieved)", after.Goal)
	}
}

// TestDeleteGoalClearsArmedNoLoop is invariant 11: a goal that is active
// (RegisterGoal has run) but has never had a loop attached in this process —
// so sessionState.cancel is nil, exactly like a boot-time restart-paused
// goal (see TestGoalPausedRestartYieldsIdleAndUsable) — must still be
// clearable via DELETE /goal, exercising handleGoalDelete's nil-cancel
// branch rather than leaving the goal stranded active forever.
func TestDeleteGoalClearsArmedNoLoop(t *testing.T) {
	prov := &goalProv{name: "test"}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	// Register directly on the resident session (bypassing POST /goal's
	// run-slot claim entirely): the session became resident at creation
	// (handleCreate), so sessionState.cancel is still its zero value (nil)
	// and running is still false — exactly the "armed, no loop attached"
	// state.
	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatalf("session %s not resident", id)
	}
	if err := st.sess.RegisterGoal("cond"); err != nil {
		t.Fatalf("RegisterGoal: %v", err)
	}

	h.srv.mu.Lock()
	cancelNil := st.cancel == nil
	running := st.running
	h.srv.mu.Unlock()
	if !cancelNil || running {
		t.Fatalf("expected an armed goal with no attached loop (cancel nil, running false); got cancel-nil=%v running=%v", cancelNil, running)
	}

	view := h.getPausedGoalView(id)
	if view.Goal == nil || !view.Goal.Active {
		t.Fatalf("armed goal = %+v, want active", view.Goal)
	}

	resp, _ := h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE armed goal status %d, want 204", resp.StatusCode)
	}

	after := h.getPausedGoalView(id)
	if after.Goal != nil && after.Goal.Active {
		t.Errorf("goal after DELETE = %+v, want inactive", after.Goal)
	}
}
