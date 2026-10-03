package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func isEvaluator(r harnesstest.Request) bool {
	return strings.Contains(r.System, goalEvaluatorMarker)
}

func notEvaluator(r harnesstest.Request) bool { return !isEvaluator(r) }

func agentStep(name, text string, repeat bool) harnesstest.Step {
	return harnesstest.Step{Name: name, Match: notEvaluator, Reply: harnesstest.Reply{Text: text}, Repeat: repeat}
}

func evaluatorStep(name, verdict string, repeat bool) harnesstest.Step {
	return harnesstest.Step{
		Name:   name,
		Match:  isEvaluator,
		Reply:  harnesstest.Reply{Text: verdict},
		Repeat: repeat,
	}
}

type awaitGoalExhausted struct{}

func (awaitGoalExhausted) run(t *testing.T, r *run) {
	t.Helper()
	r.drv.AwaitGoalExhausted(t)
}

func TestContractGoal(t *testing.T) {
	armed := func(g setGoal) []action {
		return []action{create{as: "a"}, g, waitIdle{as: "a"}}
	}
	deferredThenGo := func(g setGoal) []action {
		return []action{create{as: "a"}, g, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	}
	runScenarios(t, []scenario{
		{
			name: "goal_met_first_turn",
			model: []harnesstest.Step{
				agentStep("work", "done", false),
				evaluatorStep("judge", "MET: said done", false),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
		{
			name: "goal_not_met_then_met",
			model: []harnesstest.Step{
				agentStep("try", "try", false),
				evaluatorStep("judge1", "NOT MET: say done", false),
				agentStep("finish", "done", false),
				evaluatorStep("judge2", "MET: said done", false),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
		{
			name: "deferred_goal_judges_finished_turn",
			model: []harnesstest.Step{
				agentStep("work", "done", false),
				evaluatorStep("judge", "MET: said done", false),
			},
			actions: deferredThenGo(setGoal{as: "a", condition: "say done", deferred: true}),
		},
		{
			name: "goal_exhausts_max_turns",
			model: []harnesstest.Step{
				agentStep("try", "try", true),
				evaluatorStep("judge", "NOT MET: keep going", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 2},
				awaitGoalExhausted{},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}
