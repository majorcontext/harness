// Tests for Task 2 of the goal worker-failure park work (Round 7): the
// server-side outcome mapping, pause-presentation fold, and resume-on-
// activity behavior for engine/goal.go's exit-parked worker turns (Task 1,
// commit 1ffb48a). See docs/plans/2026-07-21-goal-worker-park.md's
// "Invariants" list — invariant 1's server half, invariant 2's server half,
// invariant 4, invariant 5, and invariant 7 (already-covered operator paths)
// are this file's.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// goalWorkerRetriesForTest mirrors engine's unexported goalWorkerRetries (2):
// this package cannot reference it directly, and re-deriving it from a magic
// number in each test would risk silent drift if the engine constant ever
// changes. workerErrN must be exactly goalWorkerRetriesForTest+1 (the
// initial attempt plus that many retries) to exhaust the DETERMINISTIC tier
// and exit-park — see engine/goal.go's promptTurnWithRetry and
// TestPursueGoalWorkerFailsPermanentlyParksGoal, whose engine-level
// equivalent this constant mirrors (goalEvalFailureLimitForTest, this
// package's own analogous mirror, sets the same precedent).
const goalWorkerRetriesForTest = 2

// permanentWorkerErr is a plain, non-retryable error standing in for a
// deterministic-tier failure (e.g. the production incident's OpenRouter
// 404s) — never provider.MarkRetryable, which would route into the
// separately-budgeted retryable tier instead (goalRetryableMaxAttempts,
// far too many attempts for an ordinary test).
func permanentWorkerErr() error { return errors.New("permanent failure: 404 not found") }

// TestTurnEndOutcomeWorkerParked is invariant 1's outcome-mapping headline:
// turnEndOutcome must map a worker-parked sentinel (engine.IsGoalWorkerParked)
// to the distinct "worker_parked" outcome, not the generic "error"
// every other failure has always recorded. engine.IsGoalWorkerParked has no
// public constructor (see engine/goal.go's goalWorkerParkedError), so the
// only way to obtain a genuine one is to actually exhaust a goal loop's
// deterministic retry budget via PursueGoal — run under synctest so the
// real goalRetryDelay backoff (1s+4s between the 3 attempts) costs nothing.
//
// Red-verified: with turnEndOutcome's engine.IsGoalWorkerParked check
// removed (i.e. against the pre-Task-2 code), this test fails — outcome
// reads "error" instead of "worker_parked".
func TestTurnEndOutcomeWorkerParked(t *testing.T) {
	dir := t.TempDir()
	var err error
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:       "test",
			workerErrN: goalWorkerRetriesForTest + 1,
			workerErr:  permanentWorkerErr(),
		}
		cfg := engine.Config{
			Providers:  provider.Registry{prov.Name(): prov},
			Model:      message.ModelRef{Provider: prov.Name(), Model: "m1"},
			SessionDir: dir,
		}
		s := engine.NewSession(cfg)
		_, err = s.PursueGoal(context.Background(), "cond", engine.GoalOptions{
			Evaluator: message.ModelRef{Provider: prov.Name(), Model: "eval"},
		})
	})
	if err == nil || !engine.IsGoalWorkerParked(err) {
		t.Fatalf("PursueGoal error = %v, want a worker-parked sentinel (IsGoalWorkerParked)", err)
	}
	if got := turnEndOutcome(err); got != outcomeWorkerParked {
		t.Errorf("turnEndOutcome(worker-parked err) = %q, want %q", got, outcomeWorkerParked)
	}
}

// TestForcesIdlePauseIncludesWorkerFailure is a focused unit test on the
// renamed/extended helper (formerly isRestartPaused): "restart" and
// "worker_failure" (Round 7) must both force compositeState to idle;
// "provider-backoff" must not (its loop is genuinely alive, merely waiting
// — see TestGoalStalledProviderBackoffSurfacesPaused).
func TestForcesIdlePauseIncludesWorkerFailure(t *testing.T) {
	cases := []struct {
		name string
		goal *goalJSON
		want bool
	}{
		{"nil goal", nil, false},
		{"not paused", &goalJSON{Paused: false}, false},
		{"restart", &goalJSON{Paused: true, PauseReason: pauseReasonRestart}, true},
		{"worker_failure", &goalJSON{Paused: true, PauseReason: pauseReasonWorkerFailure}, true},
		{"provider-backoff", &goalJSON{Paused: true, PauseReason: pauseReasonProviderBackoff}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := forcesIdlePause(c.goal); got != c.want {
				t.Errorf("forcesIdlePause(%+v) = %v, want %v", c.goal, got, c.want)
			}
		})
	}
}

// TestGoalTrackerPauseViewPrecedence locks in pauseView's three-way
// precedence (Task 2): restart > worker_failure > provider-backoff.
func TestGoalTrackerPauseViewPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		g          *goalTracker
		wantPaused bool
		wantReason string
	}{
		{
			name:       "restart wins over worker-failure and backoff",
			g:          &goalTracker{active: true, pausedRestart: true, pausedWorker: true, retryable: true, waiting: true},
			wantPaused: true, wantReason: pauseReasonRestart,
		},
		{
			name:       "worker-failure wins over backoff",
			g:          &goalTracker{active: true, pausedWorker: true, retryable: true, waiting: true},
			wantPaused: true, wantReason: pauseReasonWorkerFailure,
		},
		{
			name:       "provider-backoff alone",
			g:          &goalTracker{active: true, retryable: true, waiting: true},
			wantPaused: true, wantReason: pauseReasonProviderBackoff,
		},
		{
			name:       "nothing paused",
			g:          &goalTracker{active: true},
			wantPaused: false, wantReason: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paused, reason := c.g.pauseView()
			if paused != c.wantPaused || reason != c.wantReason {
				t.Errorf("pauseView() = (%v, %q), want (%v, %q)", paused, reason, c.wantPaused, c.wantReason)
			}
		})
	}
}

// TestGoalParkedFoldLockstepBetweenLiveAndReplay proves publishGoal (the
// live path) and foldGoalRecordLocked (the boot-replay path) leave a
// goal.parked-driven goalTracker in the IDENTICAL shape — the "both folds
// lockstep" contract every other goal.* record already keeps (see
// foldGoalRecordLocked's doc comment). Exercises the field mapping directly
// (GoalAttempts, the engine's plural field for a park's TOTAL attempt
// count, reused into the wire event's singular GoalAttempt) rather than
// going through a full HTTP+restart cycle, which pauseArmedGoalsAtBoot
// would otherwise mask (a restart always ALSO sets pausedRestart, which
// outranks pausedWorker in pauseView — see
// TestGoalWorkerParkPauseSurvivesRestartAsRestartReason for that end-to-end
// precedence proof).
func TestGoalParkedFoldLockstepBetweenLiveAndReplay(t *testing.T) {
	liveSrv := &Server{goalState: map[string]*goalTracker{"s1": {active: true}}}
	ev := engine.Event{
		Type:          engine.EventGoalParked,
		SessionID:     "s1",
		GoalReason:    "worker turn failed repeatedly and did not recover",
		GoalTurn:      2,
		GoalAttempts:  goalWorkerRetriesForTest + 1,
		GoalRetryable: false,
	}
	liveSrv.publishGoal(ev)
	live := liveSrv.goalState["s1"]
	if live == nil {
		t.Fatal("no goalTracker after publishGoal(goal.parked)")
	}
	if !live.pausedWorker {
		t.Error("pausedWorker not set by the live fold")
	}
	if live.attempt != goalWorkerRetriesForTest+1 {
		t.Errorf("live fold attempt = %d, want %d (reused from GoalAttempts)", live.attempt, goalWorkerRetriesForTest+1)
	}
	if paused, reason := live.pauseView(); !paused || reason != pauseReasonWorkerFailure {
		t.Errorf("live pauseView() = (%v, %q), want (true, %q)", paused, reason, pauseReasonWorkerFailure)
	}

	if len(liveSrv.journal) != 1 {
		t.Fatalf("journal len = %d, want 1", len(liveSrv.journal))
	}
	wire := liveSrv.journal[0]
	if wire.Type != evtGoalParked {
		t.Fatalf("journaled event type = %q, want %q", wire.Type, evtGoalParked)
	}
	if !wire.GoalPaused || wire.GoalPauseReason != pauseReasonWorkerFailure {
		t.Errorf("journaled goal.parked event GoalPaused/GoalPauseReason = %v/%q, want true/%q", wire.GoalPaused, wire.GoalPauseReason, pauseReasonWorkerFailure)
	}

	replaySrv := &Server{goalState: map[string]*goalTracker{"s1": {active: true}}}
	replaySrv.foldGoalRecordLocked(wire)
	replay := replaySrv.goalState["s1"]
	if replay == nil {
		t.Fatal("no goalTracker after foldGoalRecordLocked(goal.parked)")
	}
	if *live != *replay {
		t.Errorf("live fold = %+v, replay fold = %+v, want identical (lockstep)", *live, *replay)
	}
}

// TestGoalParkedRoutedThroughPublish proves engine.EventGoalParked is in
// Publish's routing allowlist (not silently dropped, the failure mode a new
// event type risks if the switch in Publish is forgotten).
func TestGoalParkedRoutedThroughPublish(t *testing.T) {
	srv := &Server{goalState: map[string]*goalTracker{}}
	srv.Publish(engine.Event{
		Type:         engine.EventGoalParked,
		SessionID:    "s1",
		GoalReason:   "worker turn failed repeatedly and did not recover",
		GoalAttempts: goalWorkerRetriesForTest + 1,
	})
	g := srv.goalState["s1"]
	if g == nil || !g.pausedWorker {
		t.Fatalf("goalState after Publish(goal.parked) = %+v, want pausedWorker=true", g)
	}
}

// TestGoalWorkerParkSurfacesPausedWorkerFailure is invariant 1's server half
// plus invariant 5's live half: deterministic-tier exhaustion (3 failed
// attempts, non-retryable) must journal goal.parked, keep the goal ACTIVE,
// and surface paused=true/pause_reason="worker_failure"/state="idle" —
// mirroring TestGoalEvaluatorExhaustedTerminalOutcome's event-order pin but
// for the park terminal, which — unlike the evaluator-exhausted terminal —
// does NOT clear the goal.
func TestGoalWorkerParkSurfacesPausedWorkerFailure(t *testing.T) {
	// Runs under synctest so the deterministic retry backoff (goalRetryDelay:
	// 1s+4s) costs no real wall-clock time; drives the server in-process via
	// handleGoal + the journal (no httptest.Server, whose real listener a
	// bubble forbids) exactly like TestGoalEvaluatorExhaustedTerminalOutcome.
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:       "test",
			workerErrN: goalWorkerRetriesForTest + 1,
			workerErr:  permanentWorkerErr(),
		}
		srv := newServer(t, dir, prov, 0, func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		})
		id := createSessionDirect(t, srv, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"cond"}`))
		greq.SetPathValue("id", id)
		srv.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		srv.wg.Wait() // the parked loop (and its fake-time backoff) finishes

		srv.mu.Lock()
		var evs []Event
		for _, ev := range srv.journal {
			if ev.SessionID == id {
				evs = append(evs, ev)
			}
		}
		srv.mu.Unlock()

		idx := map[string]int{}
		for i, ev := range evs {
			switch ev.Type {
			case "goal.parked":
				if _, ok := idx["goal.parked"]; !ok {
					idx["goal.parked"] = i
				}
				if !ev.GoalPaused || ev.GoalPauseReason != pauseReasonWorkerFailure {
					t.Errorf("goal.parked event GoalPaused/GoalPauseReason = %v/%q, want true/%q", ev.GoalPaused, ev.GoalPauseReason, pauseReasonWorkerFailure)
				}
				if ev.GoalReason == "" {
					t.Error("goal.parked event missing GoalReason")
				}
				if want := goalWorkerRetriesForTest + 1; ev.GoalAttempt != want {
					t.Errorf("goal.parked event GoalAttempt = %d, want %d", ev.GoalAttempt, want)
				}
				if ev.GoalRetryable {
					t.Error("goal.parked event GoalRetryable = true, want false (deterministic tier)")
				}
			case evtSessionError:
				if _, ok := idx[evtSessionError]; !ok {
					idx[evtSessionError] = i
				}
			case evtTurnEnd:
				if _, ok := idx[evtTurnEnd]; !ok {
					idx[evtTurnEnd] = i
					if ev.Outcome != outcomeWorkerParked {
						t.Errorf("turn.end outcome = %q, want %q", ev.Outcome, outcomeWorkerParked)
					}
					if ev.Error == "" {
						t.Error("turn.end missing sanitized error detail")
					}
				}
			case evtSessionStatus:
				if ev.Status == "idle" {
					if _, ok := idx["idle"]; !ok {
						idx["idle"] = i
					}
				}
			case "goal.cleared":
				t.Error("goal.cleared emitted — a worker-turn park must never clear the goal")
			}
		}
		for _, want := range []string{"goal.parked", evtSessionError, evtTurnEnd, "idle"} {
			if _, ok := idx[want]; !ok {
				t.Fatalf("missing expected event %q in stream: %+v", want, evs)
			}
		}
		if !(idx["goal.parked"] < idx[evtSessionError] && idx[evtSessionError] < idx[evtTurnEnd] && idx[evtTurnEnd] < idx["idle"]) {
			t.Errorf("event order = goal.parked:%d session.error:%d turn.end:%d idle:%d, want strictly increasing",
				idx["goal.parked"], idx[evtSessionError], idx[evtTurnEnd], idx["idle"])
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/session/"+id, nil)
		req.SetPathValue("id", id)
		srv.handleGet(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET session status %d: %s", rec.Code, rec.Body)
		}
		var view struct {
			State string `json:"state"`
			Goal  *struct {
				Active      bool   `json:"active"`
				Paused      bool   `json:"paused"`
				PauseReason string `json:"pause_reason"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Goal == nil || !view.Goal.Active {
			t.Fatalf("goal after park = %+v, want active (park never clears)", view.Goal)
		}
		if !view.Goal.Paused || view.Goal.PauseReason != "worker_failure" {
			t.Errorf("goal paused/pause_reason after park = %v/%q, want true/worker_failure", view.Goal.Paused, view.Goal.PauseReason)
		}
		if view.State != "idle" {
			t.Errorf("state after park = %q, want idle (worker_failure forces idle like restart)", view.State)
		}
	})
}

// TestGoalWorkerParkFreesRunSlotForQueuedPrompt is invariant 2's server
// half: unlike the OLD in-loop park (GitHub issue #61's continue, which
// pinned the run slot for the whole outage — see engine/goal.go's Round 7
// supersession doc), an exit-parked goal loop frees the run slot, so a
// prompt queued while it was retrying dispatches as a NORMAL turn once the
// park happens — "delivered", not "injected" — mirroring
// TestQueuedDispatchAfterGoalLoopEnds's shape for the achieved-goal case.
func TestGoalWorkerParkFreesRunSlotForQueuedPrompt(t *testing.T) {
	// Runs under synctest so the deterministic worker retry backoff
	// (goalRetryDelay: 1s+4s) costs no real wall-clock time; drives the server
	// in-process via handleGoal/handlePrompt + the journal (no httptest.Server,
	// whose real listener a bubble forbids) exactly like
	// TestGoalWorkerParkSurfacesPausedWorkerFailure. srv.wg.Wait covers the
	// whole chain — the park frees the slot, the queued prompt dispatches, and
	// that prompt's own tail auto-arms the goal to achievement — because every
	// tail's dispatch/auto-arm does its wg.Add before the previous goroutine's
	// deferred wg.Done (see handlers.go's wg.Add-before-wg.Done ordering note).
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:       "test",
			workerErrN: goalWorkerRetriesForTest + 1,
			workerErr:  permanentWorkerErr(),
			// worker[0] serves the dispatched queued prompt; worker[1] serves
			// the fresh loop that prompt's own tail auto-arms (see below) — its
			// eval script lets that loop achieve immediately instead of
			// cascading into a second park with an empty script.
			worker: [][]provider.Event{asstTurn("queued-done"), asstTurn("resumed-goal-turn")},
			eval:   [][]provider.Event{asstTurn("MET: done")},
		}
		srv := newServer(t, dir, prov, 0, func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		})
		id := createSessionDirect(t, srv, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"cond"}`))
		greq.SetPathValue("id", id)
		srv.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		// Let the goal loop claim the run slot, run its turn-boundary queue
		// drain (empty), fail its first worker attempt, and settle into the
		// retry backoff BEFORE the prompt is queued. Real HTTP timing gave the
		// original test this ordering for free; without it the prompt can race
		// ahead of the turn-boundary drain and be delivered "injected" (mid-goal
		// turn) rather than "delivered" (a normal turn after the park frees the
		// slot), the invariant under test.
		synctest.Wait()

		prec := httptest.NewRecorder()
		preq := httptest.NewRequest("POST", "/session/"+id+"/prompt_async", strings.NewReader(`{"parts":[{"type":"text","text":"queued"}]}`))
		preq.SetPathValue("id", id)
		srv.handlePrompt(prec, preq)
		if prec.Code != http.StatusAccepted {
			t.Fatalf("queued prompt status %d: %s", prec.Code, prec.Body)
		}
		var qr promptAsyncResponse
		if err := json.Unmarshal(prec.Body.Bytes(), &qr); err != nil {
			t.Fatal(err)
		}
		if qr.Status != "queued" || qr.Queued != 1 {
			t.Fatalf("queued prompt response = %+v, want status=queued queued=1", qr)
		}

		srv.wg.Wait() // park -> dispatch queued prompt -> the prompt's tail auto-arms the goal to achievement

		srv.mu.Lock()
		var evs []Event
		for _, ev := range srv.journal {
			if ev.SessionID == id {
				evs = append(evs, ev)
			}
		}
		srv.mu.Unlock()

		idxParked, idxDelivered, idxAchieved := -1, -1, -1
		var sawBusyAfterPark, sawText, sawDelivered bool
		for i, ev := range evs {
			switch {
			case ev.Type == "goal.parked":
				if idxParked == -1 {
					idxParked = i
				}
			case ev.Type == "prompt.dequeued":
				// The QUEUED prompt, dispatched into the just-freed run slot as
				// a normal turn: "delivered", never a mid-turn "injected".
				if ev.QueueReason != "delivered" {
					t.Errorf("queued prompt dequeue reason = %q, want %q (a normal turn, not a mid-turn injection)", ev.QueueReason, "delivered")
				}
				if idxDelivered == -1 {
					idxDelivered = i
				}
				sawDelivered = true
			case ev.Type == "goal.achieved":
				if idxAchieved == -1 {
					idxAchieved = i
				}
			case ev.Type == evtSessionStatus && ev.Status == "busy":
				if idxParked != -1 {
					sawBusyAfterPark = true
				}
			case ev.Type == "message" && ev.Message != nil && ev.Message.Role == message.RoleAssistant && ev.Message.Parts.Text() == "queued-done":
				sawText = true
			}
		}
		if idxParked == -1 {
			t.Fatalf("goal loop events = %+v, want goal.parked", evs)
		}
		if !sawDelivered {
			t.Fatal("no prompt.dequeued event for the queued prompt")
		}
		if !sawBusyAfterPark || !sawText {
			t.Fatalf("dispatch events after park = %+v, want a busy transition and %q", evs, "queued-done")
		}
		// The dispatched queued prompt is an ordinary runPrompt turn, so ITS
		// OWN tail also calls maybeAutoArmGoal (every runPrompt tail does,
		// queued dispatch included — see runPrompt's doc comment) — the goal is
		// still active, so a fresh loop starts immediately and (against the
		// now-scripted-to-succeed provider) achieves.
		// TestGoalWorkerParkResumesOnNextPromptActivity is this file's
		// dedicated, more detailed proof of that resume path.
		if idxAchieved == -1 {
			t.Fatalf("goal events after the queued prompt's own auto-arm = %+v, want goal.achieved", evs)
		}
		if !(idxParked < idxDelivered && idxDelivered < idxAchieved) {
			t.Errorf("event order = goal.parked:%d prompt.dequeued(delivered):%d goal.achieved:%d, want strictly increasing", idxParked, idxDelivered, idxAchieved)
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/session/"+id, nil)
		req.SetPathValue("id", id)
		srv.handleGet(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET session status %d: %s", rec.Code, rec.Body)
		}
		var view struct {
			Queued   int `json:"queued"`
			LastTurn *struct {
				Outcome string `json:"outcome"`
			} `json:"last_turn"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Queued != 0 {
			t.Errorf("queued after dispatch = %d, want 0", view.Queued)
		}
		if view.LastTurn == nil || view.LastTurn.Outcome != "completed" {
			t.Errorf("dispatched queued prompt's last_turn = %+v, want outcome completed", view.LastTurn)
		}
	})
}

// TestGoalWorkerParkResumesOnNextPromptActivity is invariant 4: after a
// worker-park, a plain prompt completing auto-arms the still-active goal
// (maybeAutoArmGoal, the pre-existing activity-driven resume mechanism —
// see AGENTS.md's "Resume needs zero new machinery" design note), the
// paused/worker_failure presentation resets, and a now-healthy provider
// lets the goal achieve. It also proves the anti-churn property: with an
// EMPTY queue, nothing re-arms the goal immediately at park time — runGoal's
// tail deliberately never calls maybeAutoArmGoal (see its doc comment) —
// only the NEXT ordinary prompt does.
func TestGoalWorkerParkResumesOnNextPromptActivity(t *testing.T) {
	// Runs under synctest so the deterministic worker retry backoff costs no
	// real wall-clock time; two phases (park, then a resume prompt) driven
	// in-process via handleGoal/handlePrompt + the journal. The park exits its
	// own loop (runGoal's tail never auto-arms with an empty queue), so
	// srv.wg.Wait after phase 1 settles at the paused state, then the resume
	// prompt's chain (plain turn -> auto-armed goal loop -> achievement) is a
	// second srv.wg.Wait.
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:       "test",
			workerErrN: goalWorkerRetriesForTest + 1,
			workerErr:  permanentWorkerErr(),
			worker:     [][]provider.Event{asstTurn("plain prompt turn"), asstTurn("goal turn")},
			eval:       [][]provider.Event{asstTurn("MET: now it is")},
		}
		srv := newServer(t, dir, prov, 0, func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		})
		id := createSessionDirect(t, srv, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"cond"}`))
		greq.SetPathValue("id", id)
		srv.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		srv.wg.Wait() // the park's own turn ends and the loop exits (empty queue: no auto-rearm)

		midRec := httptest.NewRecorder()
		midReq := httptest.NewRequest("GET", "/session/"+id, nil)
		midReq.SetPathValue("id", id)
		srv.handleGet(midRec, midReq)
		var mid struct {
			State string `json:"state"`
			Goal  *struct {
				Active      bool   `json:"active"`
				Paused      bool   `json:"paused"`
				PauseReason string `json:"pause_reason"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(midRec.Body.Bytes(), &mid); err != nil {
			t.Fatal(err)
		}
		if mid.Goal == nil || !mid.Goal.Active || !mid.Goal.Paused || mid.Goal.PauseReason != "worker_failure" {
			t.Fatalf("goal right after park = %+v, want active/paused/worker_failure", mid.Goal)
		}
		if mid.State != "idle" {
			t.Errorf("state right after park = %q, want idle", mid.State)
		}

		// Anti-churn: nothing has happened since the park's own idle — an empty
		// queue must never trigger an immediate re-arm (runGoal's tail calls
		// ONLY maybeDispatchQueued, never maybeAutoArmGoal). Record the phase-1
		// event count so the resume prompt's events can be sliced off cleanly
		// below.
		srv.mu.Lock()
		var lastEv Event
		phase1Count := 0
		for _, ev := range srv.journal {
			if ev.SessionID == id {
				lastEv = ev
				phase1Count++
			}
		}
		srv.mu.Unlock()
		if lastEv.Type != evtSessionStatus || lastEv.Status != "idle" {
			t.Fatalf("last journaled event before the resume prompt = %+v, want the park's own idle (anti-churn: no immediate re-arm with an empty queue)", lastEv)
		}

		// A plain prompt is the activity that resumes the goal.
		prec := httptest.NewRecorder()
		preq := httptest.NewRequest("POST", "/session/"+id+"/prompt_async", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))
		preq.SetPathValue("id", id)
		srv.handlePrompt(prec, preq)
		if prec.Code != http.StatusAccepted {
			t.Fatalf("resume prompt status %d: %s", prec.Code, prec.Body)
		}
		var pr promptAsyncResponse
		if err := json.Unmarshal(prec.Body.Bytes(), &pr); err != nil {
			t.Fatal(err)
		}
		if pr.Status != "started" {
			t.Fatalf("resume prompt response = %+v, want status=started (session was idle)", pr)
		}

		srv.wg.Wait() // the plain prompt's own turn, then the auto-armed goal loop to achievement

		srv.mu.Lock()
		var phase2 []Event
		n := 0
		for _, ev := range srv.journal {
			if ev.SessionID != id {
				continue
			}
			n++
			if n > phase1Count {
				phase2 = append(phase2, ev)
			}
		}
		srv.mu.Unlock()

		// The plain prompt's own turn runs first (ending in its own idle) with
		// no goal activity; only then does maybeAutoArmGoal start a fresh loop
		// that achieves against the now-healthy provider.
		firstIdle := -1
		var sawAchieved bool
		for i, ev := range phase2 {
			if firstIdle == -1 && (ev.Type == "goal.eval" || ev.Type == "goal.achieved") {
				t.Fatalf("goal loop ran before the plain prompt's own turn finished: %+v", phase2)
			}
			if firstIdle == -1 && ev.Type == evtSessionStatus && ev.Status == "idle" {
				firstIdle = i
			}
			if ev.Type == "goal.achieved" {
				sawAchieved = true
			}
		}
		if !sawAchieved {
			t.Fatalf("goal events after the resume prompt = %+v, want goal.achieved", phase2)
		}

		afterRec := httptest.NewRecorder()
		afterReq := httptest.NewRequest("GET", "/session/"+id, nil)
		afterReq.SetPathValue("id", id)
		srv.handleGet(afterRec, afterReq)
		var after struct {
			Goal *struct {
				Active bool `json:"active"`
				Paused bool `json:"paused"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(afterRec.Body.Bytes(), &after); err != nil {
			t.Fatal(err)
		}
		if after.Goal == nil || after.Goal.Active {
			t.Fatalf("goal after achievement = %+v, want inactive", after.Goal)
		}
		if after.Goal.Paused {
			t.Errorf("goal.Paused after achievement = true, want false")
		}
	})
}

// TestGoalWorkerParkPauseSurvivesRestartAsRestartReason is invariant 5's
// replay half, extended to the new third pause arm: a session killed while
// its goal is worker-parked must, after a restart, still read active/paused
// — but with pause_reason "restart" winning over the stale "worker_failure"
// fold (pauseArmedGoalsAtBoot always ALSO marks pausedRestart for any
// active goal found unattended at boot, and pauseView's precedence puts
// restart first) — mirroring
// TestGoalReArmAfterRetryableStallRestartNotBackoffPaused's restart-wins
// precedent for the provider-backoff arm, extended here to worker_failure.
func TestGoalWorkerParkPauseSurvivesRestartAsRestartReason(t *testing.T) {
	// Runs under synctest so the deterministic worker retry backoff costs no
	// real wall-clock time; the restart is a SECOND newServer over the SAME dir
	// (ADOPT), constructed only after the first server's goal loop has finished
	// (srv1.wg.Wait) and its journal file is closed. Both servers are driven
	// in-process via direct handler calls (no httptest.Server, whose real
	// listener a bubble forbids).
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:       "test",
			workerErrN: goalWorkerRetriesForTest + 1,
			workerErr:  permanentWorkerErr(),
		}
		mutate := func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		}
		srv1 := newServer(t, dir, prov, 0, mutate)
		id := createSessionDirect(t, srv1, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"cond"}`))
		greq.SetPathValue("id", id)
		srv1.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		srv1.wg.Wait() // the park's own turn ends and the loop exits

		beforeRec := httptest.NewRecorder()
		beforeReq := httptest.NewRequest("GET", "/session/"+id, nil)
		beforeReq.SetPathValue("id", id)
		srv1.handleGet(beforeRec, beforeReq)
		var before struct {
			Goal *struct {
				Active      bool   `json:"active"`
				Paused      bool   `json:"paused"`
				PauseReason string `json:"pause_reason"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(beforeRec.Body.Bytes(), &before); err != nil {
			t.Fatal(err)
		}
		if before.Goal == nil || !before.Goal.Active || !before.Goal.Paused || before.Goal.PauseReason != "worker_failure" {
			t.Fatalf("before restart, goal = %+v, want active/paused/worker_failure", before.Goal)
		}

		if err := srv1.Close(); err != nil {
			t.Fatalf("closing first server: %v", err)
		}

		srv2 := newServer(t, dir, prov, 0, mutate)
		// Close srv2 before the bubble ends: harmless today (New spawns no
		// goroutine), but if it ever does, a leaked goroutine would surface as
		// an obvious close rather than an opaque synctest deadlock.
		defer srv2.Close()

		afterRec := httptest.NewRecorder()
		afterReq := httptest.NewRequest("GET", "/session/"+id, nil)
		afterReq.SetPathValue("id", id)
		srv2.handleGet(afterRec, afterReq)
		var after struct {
			State string `json:"state"`
			Goal  *struct {
				Active      bool   `json:"active"`
				Paused      bool   `json:"paused"`
				PauseReason string `json:"pause_reason"`
			} `json:"goal"`
		}
		if err := json.Unmarshal(afterRec.Body.Bytes(), &after); err != nil {
			t.Fatal(err)
		}
		if after.Goal == nil || !after.Goal.Active {
			t.Fatalf("after restart, goal = %+v, want active", after.Goal)
		}
		if !after.Goal.Paused || after.Goal.PauseReason != "restart" {
			t.Errorf("after restart, goal paused/pause_reason = %v/%q, want true/restart (restart wins over stale worker_failure)", after.Goal.Paused, after.Goal.PauseReason)
		}
		if after.State != "idle" {
			t.Errorf("state after restart = %q, want idle", after.State)
		}

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
			t.Fatal("no boot-time goal.paused record found in the restarted server's journal")
		}
		if pausedEv.GoalPauseReason != "restart" {
			t.Errorf("goal.paused record GoalPauseReason = %q, want %q", pausedEv.GoalPauseReason, "restart")
		}
	})
}

// TestAutoArmAfterRestartResetsPausePresentation is the review-finding red
// test for the latent gap maybeAutoArmGoal's worker-park reset block left
// behind: it resets ONLY pausedWorker, while handleGoal's re-arm branch
// resets all five pause-fold fields (pausedRestart, pausedWorker, retryable,
// retryableClass, waiting). A restart leaves pausedRestart=true
// (pauseArmedGoalsAtBoot); if the goal is resumed by an ordinary prompt's
// tail (maybeAutoArmGoal) rather than a fresh POST /session/{id}/goal,
// pausedRestart is never cleared — GET /session keeps reporting
// paused=true/pause_reason=restart forever, even while the loop is actively
// running (state reads goal-running, not idle) — the exact "operator can't
// tell a live loop from a dead one" trap this whole subsystem exists to
// prevent.
//
// The checkpoint uses blockWorkerAfter (not a race against an SSE event) so
// it is deterministic: the resumed goal loop's own worker call is parked
// in-flight, <-prov.started proves it has actually started, and only then
// is GET /session read — no window where a fast scripted turn could race
// past achievement before the assertion runs.
//
// Red-verified against the pre-fix code (maybeAutoArmGoal's reset block
// resetting only pausedWorker): this test fails at the mid-run assertion,
// where paused is still true, pause_reason is still "restart", and state
// still reads "idle" even though the loop is actively running.
func TestAutoArmAfterRestartResetsPausePresentation(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProv{
		name: "test",
		// worker[0] is consumed by the pre-restart turn (max_turns exhausts
		// after one NOT MET). worker[1] is the plain resume prompt's own
		// turn. blockWorkerAfter=2 lets exactly those two calls through
		// normally, then blocks the THIRD worker call — the auto-armed
		// goal loop's own first turn — indefinitely, giving a deterministic
		// mid-run checkpoint instead of racing a fast scripted turn to
		// achievement.
		worker:           [][]provider.Event{asstTurn("try 1"), asstTurn("plain prompt reply")},
		eval:             [][]provider.Event{asstTurn("NOT MET: nope")},
		blockWorkerAfter: 2,
		started:          make(chan struct{}),
	}
	mutate := func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
	}
	srv1 := newServer(t, dir, prov, 0, mutate)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	sse := h1.openSSE("?from=0", "")
	resp, data := h1.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond", "max_turns": 1})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t) // max-turns exhausted, still active, never cleared
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
	if before.Goal == nil || !before.Goal.Active {
		t.Fatalf("before resume, goal = %+v, want active", before.Goal)
	}
	if !before.Goal.Paused || before.Goal.PauseReason != "restart" {
		t.Fatalf("before resume, goal = %+v, want paused/restart", before.Goal)
	}
	if before.State != "idle" {
		t.Fatalf("state before resume = %q, want idle", before.State)
	}

	// Replay only events from here on: from=0 would replay the first
	// (pre-restart) turn's history, and collectUntilIdle would stop at its
	// own idle. See TestGoalReArmClearsRestartPause's identical comment.
	resp, data = h2.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get seq status %d: %s", resp.StatusCode, data)
	}
	var seqView struct {
		Seq int64 `json:"seq"`
	}
	mustUnmarshal(t, data, &seqView)
	sse2 := h2.openSSE(fmt.Sprintf("?from=%d", seqView.Seq), "")

	// Resume via an ORDINARY prompt, not POST /goal: the whole point is
	// exercising maybeAutoArmGoal's reset block, not handleGoal's (already
	// correct) one.
	resp, data = h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume prompt status %d: %s", resp.StatusCode, data)
	}
	var pr promptAsyncResponse
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Status != "started" {
		t.Fatalf("resume prompt response = %+v, want status=started (session was idle)", pr)
	}

	// First batch: the plain prompt's own turn, ending in its own idle. No
	// goal activity yet — maybeAutoArmGoal hasn't run.
	promptEvs := sse2.collectUntilIdle(t)
	for _, ev := range promptEvs {
		if ev.Type == "goal.eval" || ev.Type == "goal.achieved" {
			t.Fatalf("goal loop ran before the plain prompt's own turn finished: %v", promptEvs)
		}
	}

	// maybeAutoArmGoal's own "busy" status follows immediately, emitted
	// right after it resets the pause-fold fields under s.mu and right
	// before it spawns the fresh loop goroutine.
	armed := sse2.nextEvent(t)
	if armed.Type != "session.status" || armed.Status != "busy" {
		t.Fatalf("event after the prompt's idle = %+v, want session.status busy (maybeAutoArmGoal starting the loop)", armed)
	}

	// The resumed loop's own worker call is now in flight and blocked
	// (blockWorkerAfter=2, this is call #3) — <-started proves it has
	// actually begun, giving a stable, non-racy window to inspect the
	// pause presentation while the loop is provably still running.
	<-prov.started

	mid := h2.getPausedGoalView(id)
	if mid.Goal == nil || !mid.Goal.Active {
		t.Fatalf("goal while the resumed loop's first turn is in flight = %+v, want active", mid.Goal)
	}
	if mid.Goal.Paused {
		t.Errorf("goal while the resumed loop's first turn is in flight: paused=true pause_reason=%q, want paused=false (auto-arm must reset the FULL pause presentation, not just pausedWorker)", mid.Goal.PauseReason)
	}
	if mid.State != "goal-running" {
		t.Errorf("state while the resumed loop's first turn is in flight = %q, want goal-running (loop is actively running, not idle)", mid.State)
	}

	// Teardown: unblock the parked worker call by clearing the goal (its
	// context is the prompt context PursueGoal drives; clearing doesn't by
	// itself cancel an in-flight turn, so also abort to release the
	// blocked stream) — mirrors
	// TestGoalReArmAfterRetryableStallRestartNotBackoffPaused's identical
	// teardown for the same blockWorker shape.
	resp, _ = h2.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear goal status %d", resp.StatusCode)
	}
	_, _ = h2.do("POST", "/session/"+id+"/abort", nil)
}
