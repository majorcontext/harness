package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractSessionOps(t *testing.T) {
	slow := harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	rest := agentStep("rest", "ok", true)
	resumed := harnesstest.Step{Name: "resumed", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{Text: "ok", Block: true}}
	runScenarios(t, []scenario{
		{
			name:  "queue_delete_while_busy",
			model: []harnesstest.Step{slow, rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				enqueue{as: "a", text: "third"},
				enqueue{as: "a", text: "fourth"},
				expectQueued{as: "a", texts: []string{"second", "third", "fourth"}},
				getSession{as: "a"},
				deleteQueued{as: "a"},
				expectQueued{as: "a"},
				getSession{as: "a"},
				release{step: "slow"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "queue_survives_clean_restart_then_delete",
			model: []harnesstest.Step{slow, resumed, rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				enqueue{as: "a", text: "third"},
				enqueue{as: "a", text: "fourth"},
				restart{},
				expectQueued{as: "a", texts: []string{"second", "third", "fourth"}},
				getSession{as: "a"},
				deleteQueued{as: "a"},
				expectQueued{as: "a"},
				getSession{as: "a"},
				release{step: "resumed"},
				waitIdle{as: "a"},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "queue_survives_clean_restart_then_drains_with_next_prompt",
			model: []harnesstest.Step{slow, rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				enqueue{as: "a", text: "third"},
				enqueue{as: "a", text: "fourth"},
				restart{},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractSessionOpsGoals(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:  "goal_update_deferred_goal_waits_for_first_turn",
			model: []harnesstest.Step{agentStep("work", "done", true), evaluatorStep("judge", "MET: said done", true)},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say hello", deferred: true},
				getSession{as: "a"},
				updateGoal{as: "a", condition: "say done"},
				getSession{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				clearGoal{as: "a"},
				clearGoal{as: "a"},
			},
		},
		{
			name:  "goal_cleared_before_first_turn",
			model: []harnesstest.Step{agentStep("work", "done", true)},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say done", deferred: true},
				clearGoal{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
			},
		},
		{
			name: "goal_update_while_busy",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				evaluatorStep("judge", "MET: ok", true),
				agentStep("rest", "ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say hello", maxTurns: 3},
				awaitRequests{n: 1},
				updateGoal{as: "a", condition: "say done"},
				getSession{as: "a"},
				release{step: "slow"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "goal_busy_send_is_queued",
			model: []harnesstest.Step{
				{Name: "slow", Match: notEvaluator, Reply: harnesstest.Reply{Text: "working", Block: true}},
				evaluatorStep("judge", "MET: ok", true),
				agentStep("rest", "ok", true),
			},
			actions: []action{
				create{as: "a"},
				setGoal{as: "a", condition: "say hello", maxTurns: 3},
				awaitRequests{n: 1},
				sendToSession{as: "a", text: "extra"},
				getSession{as: "a"},
				release{step: "slow"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractSessionOpsSettings(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "goal_provider_exhausted_parks",
			model: []harnesstest.Step{
				{Name: "wall", Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: "You have reached your specified API usage limits. You will regain access on 2099-01-01 at 00:00 UTC."}},
				{Name: "hold", Reply: harnesstest.Reply{Text: "partial", Block: true}},
				agentStep("rest", "ok", true),
			},
			actions: []action{
				create{as: "a", staysActive: true},
				setGoal{as: "a", condition: "say done", maxTurns: 3},
				awaitRequests{n: 2},
				getSession{as: "a"},
				clearGoal{as: "a"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name: "session_settings_validation_and_persistence",
			// Model-derived windows are the behavior under test, so the driver override stays off.
			config: map[string]any{"context_window_tokens": 0},
			model:  []harnesstest.Step{agentStep("rest", "ok", true)},
			actions: []action{
				create{as: "a"},
				setModel{as: "a", model: "anthropic/no-such-model"},
				setModel{as: "a", model: "nosuch/model"},
				setModel{as: "a", model: "bare"},
				setModel{as: "a", model: "anthropic/claude-haiku-4-5"},
				setThinking{as: "a", level: "bogus"},
				setThinking{as: "a", level: "high"},
				setThinking{as: "a", level: "high"},
				// Defect: any service tier is accepted.
				setServiceTier{as: "a", tier: "bogus"},
				// Defect: the anthropic adapter sends no service_tier.
				setServiceTier{as: "a", tier: "priority"},
				getSession{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				restart{},
				getSession{as: "a"},
				setThinking{as: "a", level: ""},
				setServiceTier{as: "a", tier: ""},
				getSession{as: "a"},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
			},
		},
		{
			name: "settings_model_change_reaches_the_next_model_call_of_a_turn",
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{Text: "checking", Block: true, ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo hi"}}}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"},
				awaitRequests{n: 1},
				setModel{as: "a", model: "anthropic/claude-haiku-4-5"},
				release{step: "call"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "builtin_commands_run_and_record",
			model: []harnesstest.Step{agentStep("rest", "ok", true)},
			actions: []action{
				commands{},
				create{as: "a"},
				command{as: "a", text: "/thinking high"},
				command{as: "a", text: "/compact abc"},
				command{as: "a", text: "/cost"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				bootstrap{as: "a"},
			},
		},
	})
}

// recordSystem records the instruction line of the system prompt of each model request.
type recordSystem struct{}

func (recordSystem) run(t *testing.T, r *run) {
	const header = "Project instructions from AGENTS.md:\n\n"
	lines := []any{}
	for _, req := range r.fake.Requests() {
		_, rest, _ := strings.Cut(req.System, header)
		first, _, _ := strings.Cut(rest, "\n")
		lines = append(lines, first)
	}
	r.record(t, "system_instructions", "", callResult{Status: http.StatusOK, Body: lines})
}

func TestContractSessionStart(t *testing.T) {
	turn := func(user string) harnesstest.Step {
		return harnesstest.Step{Name: user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "ok"}}
	}
	runScenarios(t, []scenario{{
		name:  "instructions_are_read_when_the_session_starts",
		model: []harnesstest.Step{turn("q1"), turn("q2"), turn("q3"), turn("q4")},
		actions: []action{
			writeFile{path: "AGENTS.md", body: "RULES-ALPHA\n"},
			create{as: "a"},
			writeFile{path: "AGENTS.md", body: "RULES-LATE\n"},
			submit{as: "a", text: "q1"},
			waitIdle{as: "a"},
			submit{as: "a", text: "q2"},
			waitIdle{as: "a"},
			writeFile{path: "AGENTS.md", body: "RULES-BRAVO\n"},
			create{as: "b"},
			writeFile{path: "AGENTS.md", body: "RULES-LATE\n"},
			submit{as: "b", text: "q3"},
			waitIdle{as: "b"},
			submit{as: "b", text: "q4"},
			waitIdle{as: "b"},
			recordSystem{},
		},
	}})
}

func TestContractSessionOpsEnd(t *testing.T) {
	slow := harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	rest := agentStep("rest", "ok", true)
	runScenarios(t, []scenario{
		{
			name:  "end_session_semantics",
			model: []harnesstest.Step{slow, rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				endSession{as: "a"},
				setModel{as: "a", model: "anthropic/claude-fable-5"},
				release{step: "slow"},
				waitIdle{as: "a"},
				endSession{as: "a"},
				endSession{as: "a"},
				endMissingSession{},
				getSession{as: "a"},
				listSessions{},
				// Ending only evicts residency: the session stays on disk and a send runs a new turn.
				sendToSession{as: "a", text: "after end"},
				waitIdle{as: "a"},
				setThinking{as: "a", level: "high"},
				getSession{as: "a"},
			},
		},
	})
}
