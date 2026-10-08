package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractSessionGoals(t *testing.T) {
	armed := func(g setGoal) []action {
		return []action{create{as: "a"}, g, waitIdle{as: "a"}, getSession{as: "a"}}
	}
	runScenarios(t, []scenario{
		{
			name: "goal_impossible_verdict_fails_the_goal",
			model: []harnesstest.Step{
				agentStep("work", "done", true),
				evaluatorStep("judge", "IMPOSSIBLE: no way", true),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
		{
			name: "goal_set_on_a_busy_session_judges_the_running_turn",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				evaluatorStep("judge", "MET: ok", true),
				agentStep("rest", "ok", true),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				release{step: "slow"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "goal_clear_and_input_during_a_goal_turn",
			model: []harnesstest.Step{
				{Name: "cleared", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				evaluatorStep("judge1", "NOT MET: a", false),
				evaluatorStep("judge2", "NOT MET: b", false),
				evaluatorStep("judge3", "MET: ok", false),
				agentStep("rest", "ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done"},
				awaitRequests{n: 1},
				clearGoal{as: "a"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				setGoal{as: "a", condition: "say done"},
				awaitRequests{n: 2},
				enqueueNext{as: "a", text: "hi"},
				release{step: "slow"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractSessionGoalInterrupt(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "interrupt_during_a_goal_turn_ends_the_goal",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				evaluatorStep("judge", "NOT MET: more", true),
				agentStep("rest", "ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				awaitRequests{n: 1},
				enqueueNext{as: "a", text: "hi"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "goal_set_after_an_interrupt_runs_normally",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				agentStep("work", "done", true),
				evaluatorStep("judge", "MET: ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				awaitRequests{n: 1},
				interrupt{as: "a"},
				waitIdle{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractSessionGoalEvaluation(t *testing.T) {
	held := func(name string, matcher harnesstest.Matcher) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: matcher, Reply: harnesstest.Reply{Text: "MET: ok", Block: true}}
	}
	runScenarios(t, []scenario{
		{
			name: "interrupt_during_goal_evaluation_keeps_the_goal",
			model: []harnesstest.Step{
				agentStep("work", "done", true),
				held("judge", isEvaluator),
				evaluatorStep("again", "MET: ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				awaitRequests{n: 2},
				compact{as: "a"},
				interrupt{as: "a"},
				release{step: "judge"},
				submit{as: "a", text: "next"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "goal_judges_the_last_turn_after_a_restart",
			model: []harnesstest.Step{
				agentStep("work", "done", false),
				held("held", isEvaluator),
				evaluatorStep("judge", "MET: ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				awaitRequests{n: 2},
				restart{},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}
