package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestUpdateGoalRequiresActive(t *testing.T) {
	s := NewSession(Config{})
	err := s.UpdateGoal("some new condition")
	if err == nil {
		t.Fatal("UpdateGoal on an inactive goal should error")
	}
	if !strings.Contains(err.Error(), "no active goal") {
		t.Fatalf("error = %q, want it to mention no active goal", err.Error())
	}
}

func TestUpdateGoalRewritesConditionJournalsAndEmits(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	if err := s.RegisterGoal("original condition"); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	if err := s.UpdateGoal("new condition"); err != nil {
		t.Fatalf("UpdateGoal = %v", err)
	}

	cond, ok := s.ActiveGoal()
	if !ok || cond != "new condition" {
		t.Errorf("ActiveGoal = %q, %v; want active with new condition", cond, ok)
	}

	var sawEvent bool
	for _, ev := range evs {
		if ev.Type == EventGoalUpdated {
			sawEvent = true
			if ev.GoalCondition != "new condition" {
				t.Errorf("EventGoalUpdated.GoalCondition = %q, want %q", ev.GoalCondition, "new condition")
			}
		}
	}
	if !sawEvent {
		t.Error("EventGoalUpdated was not emitted")
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.updated"`) || !strings.Contains(log, `"condition":"new condition"`) {
		t.Fatalf("log missing goal.updated record with new condition: %s", log)
	}
}

func TestUpdateGoalSameConditionNoop(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	if err := s.RegisterGoal("same condition"); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	if err := s.UpdateGoal("same condition"); err != nil {
		t.Fatalf("UpdateGoal = %v, want nil for a same-condition update", err)
	}
	for _, ev := range evs {
		if ev.Type == EventGoalUpdated {
			t.Error("EventGoalUpdated emitted for a same-condition update")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "goal.updated") {
		t.Fatalf("log has a goal.updated record for a same-condition update: %s", string(data))
	}
}

func TestLoadSessionFoldsGoalUpdated(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	if err := s.RegisterGoal("original condition"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGoal("updated condition"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGoal("final condition"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cond, ok := loaded.ActiveGoal()
	if !ok || cond != "final condition" {
		t.Errorf("resumed ActiveGoal = %q, %v; want active with the last updated condition", cond, ok)
	}
}

func TestUpdateGoalEmptyConditionRejected(t *testing.T) {
	s := NewSession(Config{})
	if err := s.RegisterGoal("original condition"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGoal("   "); err == nil {
		t.Fatal("UpdateGoal with a whitespace-only condition should error")
	}
	cond, ok := s.ActiveGoal()
	if !ok || cond != "original condition" {
		t.Errorf("ActiveGoal = %q, %v; want unchanged original condition", cond, ok)
	}
}

// scriptedGoalUpdateProvider serves the worker and evaluator models (keyed
// apart by req.Tools, same convention as goalProvider) from two independent
// scripts, and lets a test inject a callback right after a given Stream call
// number completes. Every call in PursueGoal's loop here is fully
// synchronous — no channel, no goroutine boundary — so calling
// Session.UpdateGoal or Session.ClearGoal from afterCall lands deterministically
// between two specific provider calls (e.g. "after turn 1's evaluator call,
// before turn 2's worker call") without any of the channel-gating machinery
// blockingEvalProvider needs for a genuinely concurrent race.
type scriptedGoalUpdateProvider struct {
	worker [][]provider.Event
	eval   [][]provider.Event
	wi, ei int

	requests []*provider.Request
	calls    int

	// afterCall, when set, is invoked with the 1-indexed call number right
	// after that call's Stream returns its (already-built) scripted stream —
	// i.e. before the caller has consumed any of the stream's events.
	afterCall func(n int)
}

func (p *scriptedGoalUpdateProvider) Name() string { return "test" }

func (p *scriptedGoalUpdateProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	p.calls++
	p.requests = append(p.requests, req)
	var stream provider.Stream
	if len(req.Tools) == 0 {
		ev := p.eval[p.ei]
		p.ei++
		stream = &scriptedStream{events: ev}
	} else {
		ev := p.worker[p.wi]
		p.wi++
		stream = &scriptedStream{events: ev}
	}
	if p.afterCall != nil {
		p.afterCall(p.calls)
	}
	return stream, nil
}

// TestPursueGoalPicksUpUpdatedConditionNextTurn is Task 2's headline test
// (invariant 2, plan §"Loop contract"): a running PursueGoal must re-read the
// goal condition at every turn boundary instead of trusting the condition
// parameter it was called with. Turn 1 runs against the original condition
// and comes back NOT MET; right after that evaluator call returns (and
// before turn 2's worker call is issued), the test calls UpdateGoal with a
// new condition. Turn 2's directive (the guidance message) AND turn 2's
// evaluator request must both carry the NEW condition, and turn 2's MET
// verdict must be honored (it is current, not stale).
//
// Pre-fix, this is red: PursueGoal's per-turn liveness check was
// s.goalActiveWith(condition) — an exact string match against the ORIGINAL
// condition parameter, never reassigned — so the moment UpdateGoal rewrites
// s.goalCondition, that check reads as "cleared" (condition changed = goal
// gone, the round-3 conflation this task retires) and the loop exits with
// Reason "goal cleared" instead of running turn 2 at all.
func TestPursueGoalPicksUpUpdatedConditionNextTurn(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedGoalUpdateProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1 done"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2 done"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: keep going"),
			evalTurn("MET: looks done"),
		},
	}
	s := goalSession(t, prov, dir)
	prov.afterCall = func(n int) {
		if n == 2 { // right after turn 1's evaluator call completes
			if err := s.UpdateGoal("the NEW condition"); err != nil {
				t.Fatalf("UpdateGoal = %v", err)
			}
		}
	}

	res, err := s.PursueGoal(context.Background(), "the original condition", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Achieved || res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns", res)
	}

	if len(prov.requests) != 4 {
		t.Fatalf("provider saw %d requests, want 4 (turn1 worker+eval, turn2 worker+eval)", len(prov.requests))
	}
	// call 3 (index 2) is turn 2's worker call: its directive is the
	// guidance message, which must carry the updated condition and must not
	// carry the stale original.
	turn2Worker := prov.requests[2]
	directive := turn2Worker.Messages[len(turn2Worker.Messages)-1].Parts.Text()
	if !strings.Contains(directive, "the NEW condition") {
		t.Errorf("turn 2 worker directive = %q, want it to contain the updated condition", directive)
	}
	if strings.Contains(directive, "the original condition") {
		t.Errorf("turn 2 worker directive still carries the stale condition: %q", directive)
	}
	// call 4 (index 3) is turn 2's evaluator call: its GOAL CONDITION section
	// (the first line block, distinct from the CONVERSATION TRANSCRIPT below
	// it, which legitimately still quotes turn 1's original directive as
	// history) must carry the updated condition.
	turn2Eval := prov.requests[3]
	evalContent := turn2Eval.Messages[0].Parts.Text()
	if !strings.HasPrefix(evalContent, "GOAL CONDITION:\nthe NEW condition\n") {
		t.Errorf("turn 2 evaluator request's GOAL CONDITION section = %q, want it to lead with the updated condition", evalContent)
	}
}

// TestStaleMetVerdictDiscarded is Task 2's headline test for invariant 3: a
// MET verdict computed against generation N must be discarded — no
// goal.achieved, no goal.eval record — once UpdateGoal has moved the goal to
// generation N+1 while that evaluator call was in flight, and the loop must
// continue running against the new condition rather than stopping.
//
// Uses blockingEvalProvider (see TestClearGoalDuringPendingEvaluationIsCleanStop)
// to park turn 1's evaluator call on a channel; the test calls UpdateGoal
// while it is in flight, then releases it so the (now stale) MET verdict
// lands. Turn 2 then runs against the updated condition and achieves for
// real, so the log should show exactly one goal.eval and one goal.achieved
// record — turn 2's, not turn 1's discarded verdict.
func TestStaleMetVerdictDiscarded(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	prov := &blockingEvalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1 done"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2 done"}),
		},
		entered: entered,
		release: release,
		evalOut: "MET: looks done",
	}
	s := goalSession(t, prov, dir)

	type outcome struct {
		res *GoalResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.PursueGoal(context.Background(), "original condition", GoalOptions{Evaluator: evalModel, MaxTurns: 2})
		done <- outcome{res, err}
	}()

	<-entered // turn 1's evaluator call is in flight, blocked on release

	if err := s.UpdateGoal("new condition"); err != nil {
		t.Fatalf("UpdateGoal = %v", err)
	}

	releaseOnce.Do(func() { close(release) }) // let the now-stale MET verdict land

	out := <-done
	if out.err != nil {
		t.Fatalf("PursueGoal error = %v", out.err)
	}
	if !out.res.Achieved || out.res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved on turn 2 against the updated condition", out.res)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.updated"`) {
		t.Errorf("log missing goal.updated record: %s", log)
	}
	if strings.Contains(log, `"type":"goal.cleared"`) {
		t.Errorf("log contains goal.cleared, want none (goal was updated then achieved, never cleared): %s", log)
	}
	if n := strings.Count(log, `"type":"goal.eval"`); n != 1 {
		t.Errorf("log has %d goal.eval record(s), want exactly 1 (turn 1's stale MET verdict must not be journaled): %s", n, log)
	}
	if n := strings.Count(log, `"type":"goal.achieved"`); n != 1 {
		t.Errorf("log has %d goal.achieved record(s), want exactly 1: %s", n, log)
	}
	if !strings.Contains(log, `"turn":2`) {
		t.Errorf("goal.eval record should be turn 2's, not turn 1's: %s", log)
	}
}

// TestClearGoalStillStopsUpdatedLoop is Task 2's headline test for invariant
// 4: even after an UpdateGoal has bumped the generation mid-loop, a
// subsequent ClearGoal must still stop the loop cleanly at the very next
// turn boundary — clear detection keys on goalActive alone, never on the
// generation or the condition string, so an update immediately followed by a
// clear behaves exactly like a clear on its own.
func TestClearGoalStillStopsUpdatedLoop(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedGoalUpdateProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1 done"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: keep going"),
		},
	}
	s := goalSession(t, prov, dir)
	prov.afterCall = func(n int) {
		if n == 2 { // right after turn 1's evaluator call completes
			if err := s.UpdateGoal("adjusted condition"); err != nil {
				t.Fatalf("UpdateGoal = %v", err)
			}
			if !s.ClearGoal() {
				t.Fatal("ClearGoal returned false for an active goal")
			}
		}
	}

	res, err := s.PursueGoal(context.Background(), "original condition", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if res.Achieved {
		t.Fatalf("result = %+v, want a clean cleared stop", res)
	}
	if res.Reason != "goal cleared" {
		t.Errorf("reason = %q, want %q", res.Reason, "goal cleared")
	}
	if res.Turns != 1 {
		t.Errorf("turns = %d, want 1 (turn 2 must never run)", res.Turns)
	}
	if len(prov.requests) != 2 {
		t.Errorf("provider saw %d requests, want exactly 2 (turn 1's worker+eval calls only)", len(prov.requests))
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.updated"`) {
		t.Errorf("log missing goal.updated record: %s", log)
	}
	if !strings.Contains(log, `"type":"goal.cleared"`) {
		t.Errorf("log missing goal.cleared record: %s", log)
	}
}
