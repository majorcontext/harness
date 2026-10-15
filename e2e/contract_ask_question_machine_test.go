package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// sendFrom posts text as an input of source while a question is open.
type sendFrom struct{ as, text, source, delivery string }

func (a sendFrom) run(t *testing.T, r *run) {
	r.record(t, "send_from_"+a.source, a.as, laneOf(t, r).sendFrom(t, r.id(t, a.as), a.text, a.source, a.delivery))
}

// awaitChild waits until the first child of the session has settled.
type awaitChild struct{ as string }

func (a awaitChild) run(t *testing.T, r *run) {
	t.Helper()
	parent := r.id(t, a.as)
	r.drv.AwaitChildSettled(t, parent, r.drv.Child(t, parent, 0))
}

func TestContractModelAPIQuestionKeptOpenByMachineInput(t *testing.T) {
	asked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}}
	answers := map[string]string{"Which database?": "SQLite"}
	delegateAsk := []harnesstest.Step{
		{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_task", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"}},
			askCall("toolu_ask", "Which database?"),
		}}},
		{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done", Block: true}},
		{Name: "noted", Match: harnesstest.LastToolResult("ask_user_question"), Reply: harnesstest.Reply{Text: "Noted."}},
		{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
	}
	runScenarios(t, []scenario{
		{
			name:   "model_api_question_stays_open_when_an_input_of_a_client_source_arrives",
			driver: askLane(true),
			model:  askSteps(),
			actions: withActions(asked, sendFrom{as: "a", text: "ci failed", source: "github"}, getSession{as: "a"},
				expectQueued{as: "a", texts: []string{"ci failed"}}, expectNoResolved{as: "a"},
				answerQuestion{as: "a", callID: "toolu_ask", answers: answers}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:       "model_api_question_stays_open_when_a_child_report_arrives",
			concurrent: true,
			driver:     askLane(true),
			model:      delegateAsk,
			actions: withActions(asked, release{step: "child"}, awaitChild{as: "a"}, getSession{as: "a"}, expectNoResolved{as: "a"},
				answerQuestion{as: "a", callID: "toolu_ask", answers: answers}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:   "model_api_queued_machine_input_runs_after_the_question_is_dismissed",
			driver: askLane(true),
			model:  askSteps(),
			actions: withActions(asked, sendFrom{as: "a", text: "ci failed", source: "github"},
				dismissQuestion{as: "a", callID: "toolu_ask"}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:   "model_api_goal_set_while_machine_input_waits_behind_a_question_dismisses_it",
			driver: askLane(true),
			model: []harnesstest.Step{
				askSteps()[0], evaluatorStep("judge", "MET: ok", true), {Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			},
			actions: withActions(asked, sendFrom{as: "a", text: "ci failed", source: "github"},
				setGoal{as: "a", condition: "say ok", maxTurns: 3}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:   "model_api_typed_input_dismisses_the_question_and_runs_with_the_queued_machine_input",
			driver: askLane(true),
			model:  askSteps(),
			actions: withActions(asked, sendFrom{as: "a", text: "ci failed", source: "github"}, sendFrom{as: "a", text: "never mind", source: "typed"},
				waitIdle{as: "a"}, getSession{as: "a"}),
		},
	})
}

// answerWorkSteps asks, then makes a tool call in the turn that the answer starts.
func answerWorkSteps() []harnesstest.Step {
	return []harnesstest.Step{
		askSteps()[0],
		{Name: "done", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}},
		{Name: "work", Match: harnesstest.LastToolResult("ask_user_question"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "echo hi"}},
		}}},
		{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
	}
}

func TestContractModelAPIQuestionHeldInputDelivery(t *testing.T) {
	asked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}}
	answers := map[string]string{"Which database?": "SQLite"}
	runScenarios(t, []scenario{
		{
			name:   "model_api_held_steer_input_joins_the_answer_turn_at_its_next_tool_call",
			driver: askLane(true),
			model:  answerWorkSteps(),
			actions: withActions(asked, sendFrom{as: "a", text: "ci failed", source: "github"},
				answerQuestion{as: "a", callID: "toolu_ask", answers: answers}, waitIdle{as: "a"}, requestHolds{text: "OPERATOR MESSAGES"}, getSession{as: "a"}),
		},
		{
			name:   "model_api_held_queue_input_runs_as_the_next_turn_after_the_answer_turn",
			driver: askLane(true),
			model:  answerWorkSteps(),
			actions: withActions(asked, sendFrom{as: "a", text: "ci failed", source: "github", delivery: "queue"},
				answerQuestion{as: "a", callID: "toolu_ask", answers: answers}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
	})
}

func TestContractClaudeCodeQuestionKeptOpenByMachineInput(t *testing.T) {
	lane := claudeLane{mode: "question", ask: true, signals: true}.newDriver
	parked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, claudeSession{as: "a"}}
	runScenarios(t, []scenario{{
		name:   "claudecode_question_stays_open_when_an_input_of_a_client_source_arrives",
		driver: lane,
		actions: withActions(parked,
			sendFrom{as: "a", text: "ci failed", source: "github"}, getSession{as: "a"},
			expectQueued{as: "a", texts: []string{"ci failed"}}, expectNoResolved{as: "a"},
			claudeAnswer{as: "a", callID: "toolu_q", answers: map[string]string{"Which database?": "SQLite"}},
			waitIdle{as: "a"}, getSession{as: "a"},
			claudeSession{as: "a"},
			claudeInvocations{as: "a"},
			claudeInputs{as: "a"},
		),
	}, {
		name:   "claudecode_held_machine_input_runs_when_a_model_of_another_provider_dismisses_the_question",
		driver: lane,
		model:  []harnesstest.Step{{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}},
		actions: withActions(parked,
			sendFrom{as: "a", text: "ci failed", source: "github"},
			setModel{as: "a", model: "anthropic/claude-fable-5"}, waitIdle{as: "a"}, getSession{as: "a"},
		),
	}})
}

// expectNoResolved checks that the log of the session holds no request.resolved record.
type expectNoResolved struct{ as string }

func (a expectNoResolved) run(t *testing.T, r *run) {
	t.Helper()
	if got := laneOf(t, r).journalEvents(t, r.id(t, a.as), "request.resolved"); len(got) != 0 {
		t.Fatalf("session %s resolved a request: %v", a.as, got)
	}
}
