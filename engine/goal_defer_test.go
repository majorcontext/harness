package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func userTexts(s *Session) []string {
	var out []string
	for _, m := range s.History() {
		if m.Role == message.RoleUser {
			out = append(out, m.Parts.Text())
		}
	}
	return out
}

// A deferred goal must evaluate the turn that already ran before it posts
// anything: the condition text never becomes a user turn.
func TestPursueGoalDeferredEvaluatesBeforeAnyTurn(t *testing.T) {
	const cond = "write a summary"
	tests := []struct {
		name        string
		eval        [][]provider.Event
		worker      [][]provider.Event
		wantMet     bool
		wantTurns   int
		wantUsers   int
		wantGuidanc string
	}{
		{
			name:      "met ends with no extra turn",
			eval:      [][]provider.Event{evalTurn("MET: summary exists")},
			worker:    [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "first"})},
			wantMet:   true,
			wantTurns: 0,
			wantUsers: 1,
		},
		{
			name: "not met sends guidance, not the condition",
			eval: [][]provider.Event{evalTurn("NOT MET: summary is missing"), evalTurn("MET: ok")},
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "first"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "second"}),
			},
			wantMet:     true,
			wantTurns:   1,
			wantUsers:   2,
			wantGuidanc: "summary is missing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &goalProvider{worker: tc.worker, eval: tc.eval}
			s := goalSession(t, prov, t.TempDir())
			if err := s.RegisterGoalDeferred(cond); err != nil {
				t.Fatal(err)
			}
			if prov.workerCall != 0 {
				t.Fatalf("worker calls after RegisterGoalDeferred = %d, want 0", prov.workerCall)
			}
			if _, err := s.Prompt(context.Background(), "hello"); err != nil {
				t.Fatal(err)
			}
			res, err := s.PursueGoal(context.Background(), cond, GoalOptions{Registered: true, Evaluator: evalModel})
			if err != nil {
				t.Fatal(err)
			}
			if res.Achieved != tc.wantMet || res.Turns != tc.wantTurns {
				t.Fatalf("result = %+v, want achieved=%v turns=%d", res, tc.wantMet, tc.wantTurns)
			}
			users := userTexts(s)
			if len(users) != tc.wantUsers {
				t.Fatalf("user turns = %q, want %d", users, tc.wantUsers)
			}
			for _, u := range users {
				if u == cond {
					t.Errorf("condition posted as a user turn: %q", users)
				}
			}
			if tc.wantGuidanc != "" && !strings.Contains(users[1], tc.wantGuidanc) {
				t.Errorf("guidance = %q, want it to contain %q", users[1], tc.wantGuidanc)
			}
		})
	}
}

// The deferral covers only the first loop entry: a later loop over the same
// goal posts the condition like any non-deferred goal. The preliminary
// evaluation does not count against MaxTurns.
func TestPursueGoalDeferralConsumedByFirstLoop(t *testing.T) {
	const cond = "write a summary"
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "first"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "second"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "third"}),
		},
		eval: [][]provider.Event{evalTurn("NOT MET: x"), evalTurn("NOT MET: y"), evalTurn("MET: ok")},
	}
	s := goalSession(t, prov, t.TempDir())
	if err := s.RegisterGoalDeferred(cond); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	opts := GoalOptions{Registered: true, MaxTurns: 1, Evaluator: evalModel}
	if res, err := s.PursueGoal(context.Background(), cond, opts); err != nil || res.Reason != "max turns" {
		t.Fatalf("first loop = %+v, %v; want max turns", res, err)
	}
	if users := userTexts(s); len(users) != 2 || !strings.Contains(users[1], "x") {
		t.Fatalf("user turns = %q, want one guidance turn after the prompt", users)
	}
	opts.MaxTurns = 0
	if _, err := s.PursueGoal(context.Background(), cond, opts); err != nil {
		t.Fatal(err)
	}
	if users := userTexts(s); len(users) != 3 || users[2] != cond {
		t.Fatalf("user turns = %q, want the second loop to post the condition", users)
	}
}

// DeferActiveGoal arms the one-shot deferral on a goal left active by an
// abort or restart.
func TestDeferActiveGoalSkipsConditionTurn(t *testing.T) {
	const cond = "write a summary"
	prov := &goalProvider{
		worker: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "first"})},
		eval:   [][]provider.Event{evalTurn("MET: ok")},
	}
	s := goalSession(t, prov, t.TempDir())
	if s.DeferActiveGoal() {
		t.Fatal("DeferActiveGoal reported true with no active goal")
	}
	if err := s.RegisterGoal(cond); err != nil {
		t.Fatal(err)
	}
	if !s.DeferActiveGoal() {
		t.Fatal("DeferActiveGoal reported false with an active goal")
	}
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PursueGoal(context.Background(), cond, GoalOptions{Registered: true, Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if users := userTexts(s); len(users) != 1 {
		t.Fatalf("user turns = %q, want only the prompt", users)
	}
}
