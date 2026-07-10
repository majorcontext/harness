package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestPursueGoalPausesOnAskUser is the red-first test for docs/design/
// question-tool.md §2's goal-loop pause: a worker turn that ends because
// ask_user ran returns immediately with Reason "awaiting_input", WITHOUT
// ever calling the evaluator, WITHOUT clearing the goal, and WITHOUT
// consuming a retry attempt.
func TestPursueGoalPausesOnAskUser(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopToolUse, toolCall("tc1", "ask_user", `{"questions":[{"question":"which env?"}]}`)),
		},
		// No evaluator turns scripted: evaluateGoal must never be called.
	}
	var evs []Event
	s := goalSession(t, prov, t.TempDir())
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	res, err := s.PursueGoal(context.Background(), "deploy the service", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal error = %v, want nil", err)
	}
	if res.Achieved || res.Reason != "awaiting_input" || res.Turns != 1 {
		t.Fatalf("result = %+v, want {Achieved:false Turns:1 Reason:awaiting_input}", res)
	}
	if prov.ei != 0 {
		t.Errorf("evaluator was called (ei=%d), want it never invoked while paused", prov.ei)
	}
	for _, ev := range evs {
		if ev.Type == EventGoalEval || ev.Type == EventGoalStalled || ev.Type == EventGoalCleared || ev.Type == EventGoalAchieved {
			t.Errorf("unexpected goal event %s while paused on ask_user", ev.Type)
		}
	}
	cond, active := s.ActiveGoal()
	if !active || cond != "deploy the service" {
		t.Errorf("ActiveGoal = (%q, %v), want the goal to remain active (not cleared)", cond, active)
	}
	callID, awaiting := s.AwaitingQuestion()
	if !awaiting || callID != "tc1" {
		t.Errorf("AwaitingQuestion = (%q, %v), want (tc1, true)", callID, awaiting)
	}
}

// TestPursueGoalResumeAnswerConsumedOnce is the red-first test for
// GoalOptions.ResumeAnswer: folded into turn 1's directive exactly once —
// the guidance directive on later turns must carry only the evaluator's
// reason, never the answer text again.
func TestPursueGoalResumeAnswerConsumedOnce(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "used the answer"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "still going"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: keep going"),
			evalTurn("MET: done"),
		},
	}
	s := goalSession(t, prov, t.TempDir())

	// Mirrors the server's resume path: the goal is already active (paused
	// on ask_user, never cleared), so RegisterGoal is called first, exactly
	// like an ordinary PursueGoal(Registered:true) resume.
	if err := s.RegisterGoal("deploy the service"); err != nil {
		t.Fatal(err)
	}
	res, err := s.PursueGoal(context.Background(), "deploy the service", GoalOptions{
		Evaluator:    evalModel,
		Registered:   true,
		ResumeAnswer: "staging",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Achieved || res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns", res)
	}

	h := s.History()
	// user(turn1 directive), asst, user(guidance), asst.
	if len(h) != 4 {
		t.Fatalf("history len = %d: %+v", len(h), h)
	}
	turn1 := h[0].Parts.Text()
	if !strings.Contains(turn1, "deploy the service") || !strings.Contains(turn1, "staging") {
		t.Errorf("turn 1 directive = %q, want condition + resume answer", turn1)
	}
	guidance := h[2].Parts.Text()
	if strings.Contains(guidance, "staging") {
		t.Errorf("guidance directive = %q, must not repeat the resume answer", guidance)
	}
	if !strings.Contains(guidance, "keep going") {
		t.Errorf("guidance directive = %q, want the evaluator reason", guidance)
	}
}
