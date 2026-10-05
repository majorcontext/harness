package e2e

import (
	"fmt"
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

// goalChain scripts one goal tool call per model request of the turn that
// starts with "go", then a final text reply. The evaluator is never matched.
func goalChain(calls ...map[string]any) []harnesstest.Step {
	steps := make([]harnesstest.Step, 0, len(calls)+1)
	for i, c := range calls {
		steps = append(steps, harnesstest.Step{
			Name:  fmt.Sprintf("goal%d", i+1),
			Match: matchAll(notEvaluator, harnesstest.LastUserText("go"), assistantTurns(i)),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: fmt.Sprintf("toolu_%d", i+1), Name: "goal", Input: c}}},
		})
	}
	return append(steps, harnesstest.Step{Name: "went", Match: matchAll(notEvaluator, assistantTurns(len(calls))), Reply: harnesstest.Reply{Text: "went"}})
}

func TestContractGoalTool(t *testing.T) {
	status := ftArgs("action", "status")
	set := func(c string) map[string]any { return ftArgs("action", "set", "condition", c) }
	adjust := func(c string) map[string]any { return ftArgs("action", "adjust", "condition", c) }
	runScenarios(t, []scenario{
		{
			name: "goal_tool_actions_report_and_refuse",
			model: append([]harnesstest.Step{evaluatorStep("judge", "MET: ok", true)}, append(goalChain(
				status, set(" tests pass "), status, set("b"),
			), agentStep("condition", "ok", true))...),
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name: "goal_tool_refusals_copy_the_engine_wording",
			model: append([]harnesstest.Step{evaluatorStep("judge", "MET: ok", true)}, append(goalChain(
				adjust("x"), set(" "), ftArgs("action", "clear"),
			), agentStep("condition", "ok", true))...),
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name: "goal_tool_adjust_after_set_runs_the_adjusted_condition",
			model: append([]harnesstest.Step{evaluatorStep("judge", "MET: ok", true)}, append(goalChain(set("b"), adjust("c")),
				agentStep("condition", "ok", true))...),
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name: "goal_tool_set_runs_the_condition_as_its_own_turn",
			model: append([]harnesstest.Step{evaluatorStep("judge", "MET: ok", true)}, append(goalChain(set("say done")),
				agentStep("condition", "ok", true))...),
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name: "goal_tool_adjust_keeps_the_turn_limit",
			model: []harnesstest.Step{
				evaluatorStep("judge", "NOT MET: more", true),
				{Name: "first", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working"}},
				{Name: "adjust", Match: matchAll(notEvaluator, harnesstest.LastUserText("The goal has not been met yet")), Reply: harnesstest.Reply{
					ToolCalls: []harnesstest.ToolCall{{ID: "toolu_adjust", Name: "goal", Input: adjust("say done now")}}}},
				{Name: "after", Match: harnesstest.LastToolResult("goal"), Reply: harnesstest.Reply{Text: "working"}},
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 2},
				awaitGoalExhausted{},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name:   "no_goal_evaluator_means_no_goal",
			config: map[string]any{"goal_evaluator_model": ""},
			model:  []harnesstest.Step{{Name: "ok", Reply: harnesstest.Reply{Text: "ok"}}},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "hi"},
				waitIdle{as: "a"},
				updateGoal{as: "a", condition: "say done"},
				command{as: "a", text: "/goal ship it"},
				awaitCommands{as: "a"},
				commandRecords{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractGoalDeferred(t *testing.T) {
	deferredThenGo := func(g setGoal) []action {
		return []action{create{as: "a"}, g, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	}
	runScenarios(t, []scenario{
		{
			name: "deferred_goal_judges_finished_turn",
			model: []harnesstest.Step{
				agentStep("work", "done", false),
				evaluatorStep("judge", "MET: said done", false),
			},
			actions: deferredThenGo(setGoal{as: "a", condition: "say done", deferred: true}),
		},
		{
			name: "deferred_goal_with_max_turns",
			model: []harnesstest.Step{
				agentStep("work", "worked", true),
				evaluatorStep("judge", "NOT MET: keep going", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 2, deferred: true},
				submit{as: "a", text: "go"},
				awaitGoalExhausted{},
				waitIdle{as: "a"},
			},
		},
	})
}

func TestContractGoalDeferredQueue(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "queued_prompt_runs_before_deferred_auto_arm",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				agentStep("rest", "done", true),
				evaluatorStep("judge", "MET: said done", true),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				setGoal{as: "a", condition: "say done", deferred: true},
				release{step: "slow"},
				waitIdle{as: "a"},
			},
		},
		{
			name: "busy_deferred_goal_with_max_turns",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				agentStep("work", "worked", true),
				evaluatorStep("judge", "NOT MET: keep going", true),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				setGoal{as: "a", condition: "say done", maxTurns: 2, deferred: true},
				release{step: "slow"},
				awaitGoalExhausted{},
				waitIdle{as: "a"},
			},
		},
		{
			name: "persisted_queue_dispatches_after_deferred_arm",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				agentStep("rest", "done", true),
				evaluatorStep("judge", "MET: said done", true),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				restart{},
				expectQueued{as: "a", texts: []string{"second"}},
				setGoal{as: "a", condition: "say done", deferred: true},
				waitIdle{as: "a"},
				expectQueued{as: "a"},
			},
		},
	})
}
