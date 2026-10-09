package harness_test

import (
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

func goalRuntime(t *testing.T, f *family, evaluator string) *harness.Runtime {
	t.Helper()
	return familyRuntime(t, harness.NewMemStore(), f, nil, config.Config{GoalEvaluatorModel: evaluator}, "")
}

func judging(verdict string, more func(eventlog.Part) []eventlog.Message) func(eventlog.Part) []eventlog.Message {
	return func(p eventlog.Part) []eventlog.Message {
		if strings.HasPrefix(p.Text, "GOAL CONDITION:") {
			return []eventlog.Message{say(verdict)}
		}
		return more(p)
	}
}

func TestGoalToolAdjustKeepsTheTurnLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adjusted := false
		f := &family{answer: judging("NOT MET: more", func(p eventlog.Part) []eventlog.Message {
			if strings.HasPrefix(p.Text, "The goal has not been met yet.") && !adjusted {
				adjusted = true
				return []eventlog.Message{calls("goal", map[string]any{"action": "adjust", "condition": "say done now"})}
			}
			return []eventlog.Message{say("working")}
		})}
		r := goalRuntime(t, f, "fake/eval")
		s := create(t, r)
		if err := s.SetGoal(bg, protocol.Goal{Condition: "say done", MaxTurns: 2}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		judged := 0
		for _, req := range f.reqs {
			if strings.HasPrefix(req.History[0].Parts[0].Text, "GOAL CONDITION:") {
				judged++
			}
		}
		if g := s.View().Goal; g == nil || g.State != "exhausted" || judged != 2 || g.Condition != "say done now" {
			t.Errorf("goal %+v after %d judged turns, want say done now exhausted after 2", g, judged)
		}
		closeRuntime(t, r)
	})
}
