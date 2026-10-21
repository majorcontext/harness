package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// askLane is the driver of a host over the default model API model, with the
// serve flag --ask-user-question when ask is set.
func askLane(ask bool) func(t *testing.T, h host, modelURL string) driver {
	return func(t *testing.T, h host, modelURL string) driver {
		t.Helper()
		var args []string
		if ask {
			args = append(args, "--ask-user-question")
		}
		return h.open(t, writeGoalConfigWith(t, modelURL, nil), nil, args...)
	}
}

func laneOf(t *testing.T, r *run) laneHost {
	t.Helper()
	d, ok := r.drv.(laneHost)
	if !ok {
		t.Fatalf("scenario action needs a lane driver, got %T", r.drv)
	}
	return d
}

// awaitCall waits until the log of the session holds the tool call callID.
type awaitCall struct{ as, callID string }

func (a awaitCall) run(t *testing.T, r *run) {
	t.Helper()
	laneOf(t, r).awaitToolCall(t, r.id(t, a.as), a.callID)
}

// answerQuestion answers the open question call callID with answers, from
// the raw source fields of from when it is set.
type answerQuestion struct {
	as, callID, from string
	answers          map[string]string
}

func (a answerQuestion) run(t *testing.T, r *run) {
	body, err := json.Marshal(a.answers)
	if err != nil {
		t.Fatal(err)
	}
	r.record(t, "answer_question", a.as, laneOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{answer: string(body), from: a.from}))
}

// respondQuestion answers the open question call callID with the raw response
// and source fields of with.
type respondQuestion struct{ as, callID, with string }

func (a respondQuestion) run(t *testing.T, r *run) {
	r.record(t, "respond_question", a.as, laneOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{from: a.with}))
}

// dismissQuestion dismisses the open question call callID, from the raw
// source fields of from when it is set.
type dismissQuestion struct{ as, callID, from string }

func (a dismissQuestion) run(t *testing.T, r *run) {
	r.record(t, "dismiss_question", a.as, laneOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{dismiss: true, from: a.from}))
}

func askSteps() []harnesstest.Step {
	return []harnesstest.Step{
		{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
			ID: "toolu_ask", Name: "ask_user_question", Input: map[string]any{"questions": []any{map[string]any{
				"question": "Which database?", "header": "DB", "multiSelect": false,
				"options": []any{
					map[string]any{"label": "PostgreSQL", "description": "a"},
					map[string]any{"label": "SQLite", "description": "b"},
				},
			}}},
		}}}},
		{Name: "noted", Match: harnesstest.LastToolResult("ask_user_question"), Reply: harnesstest.Reply{Text: "Noted."}},
		{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
	}
}

// boxFrom names a resolution from another box.
const boxFrom = `"source":"box","source_id":"box:box_b","source_label":"builder (box of ann)"`

func TestContractModelAPIQuestions(t *testing.T) {
	asked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}}
	answers := map[string]string{"Which database?": "SQLite"}
	runScenarios(t, []scenario{
		{
			name:    "model_api_question_parks_then_answer_resumes",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, answerQuestion{as: "a", callID: "toolu_ask", answers: answers}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:    "model_api_question_dismissed_by_resolve",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, dismissQuestion{as: "a", callID: "toolu_ask"}, getSession{as: "a"}, submit{as: "a", text: "go on"}, waitIdle{as: "a"}),
		},
		{
			name:    "model_api_question_answered_from_a_named_source_names_it_in_the_result",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, answerQuestion{as: "a", callID: "toolu_ask", answers: answers, from: boxFrom}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:    "model_api_question_answered_with_a_response_from_a_named_source_names_it_once",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, respondQuestion{as: "a", callID: "toolu_ask", with: `"response":"use SQLite",` + boxFrom}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:   "model_api_question_with_a_blank_response_takes_only_a_valid_answer",
			driver: askLane(true),
			model:  askSteps(),
			actions: withActions(asked, respondQuestion{as: "a", callID: "toolu_ask", with: `"answer":["SQLite"],"response":" \n\t"`},
				respondQuestion{as: "a", callID: "toolu_ask", with: `"answer":{"Which database?":"SQLite"},"response":" \n\t"`}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:    "model_api_question_answered_with_a_response_reads_that_the_user_responded",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, respondQuestion{as: "a", callID: "toolu_ask", with: `"response":"use SQLite"`}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:    "model_api_question_dismissed_from_a_named_source_names_it_in_the_result",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, dismissQuestion{as: "a", callID: "toolu_ask", from: boxFrom}, getSession{as: "a"}, submit{as: "a", text: "go on"}, waitIdle{as: "a"}),
		},
		{
			name:    "model_api_question_dismissed_by_next_prompt",
			driver:  askLane(true),
			model:   askSteps(),
			actions: withActions(asked, submit{as: "a", text: "never mind"}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:    "model_api_session_without_the_option_is_not_offered_the_question_tool",
			driver:  askLane(false),
			model:   []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "ok"}}},
			actions: []action{create{as: "a"}, submit{as: "a", text: "hello"}, waitIdle{as: "a"}},
		},
	})
}

// askLaneWith is askLane over a config that adds extra.
func askLaneWith(extra map[string]any) func(t *testing.T, h host, modelURL string) driver {
	return func(t *testing.T, h host, modelURL string) driver {
		t.Helper()
		return h.open(t, writeGoalConfigWith(t, modelURL, scenarioConfig(extra)), nil, "--ask-user-question")
	}
}

func askQuestion(question string, labels ...string) map[string]any {
	return askQuestionWith(question, "Q", labels...)
}

func askQuestionWith(question, header string, labels ...string) map[string]any {
	var opts []any
	for _, l := range labels {
		opts = append(opts, map[string]any{"label": l, "description": "d"})
	}
	return map[string]any{"question": question, "header": header, "multiSelect": false, "options": opts}
}

func askCall(id, question string) harnesstest.ToolCall {
	return harnesstest.ToolCall{ID: id, Name: "ask_user_question", Input: map[string]any{"questions": []any{map[string]any{
		"question": question, "header": "Q", "multiSelect": false,
		"options": []any{
			map[string]any{"label": "A", "description": "a"},
			map[string]any{"label": "B", "description": "b"},
		},
	}}}}
}

// resultFollowsCall checks that, in the newest model request, the message after the
// assistant message that holds the call callID is the message that holds its result.
type resultFollowsCall struct{ callID string }

func (a resultFollowsCall) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	msgs := reqs[len(reqs)-1].Messages
	for i, m := range msgs {
		for _, p := range m.Parts {
			if p.Kind != "tool_use" || p.ToolUseID != a.callID {
				continue
			}
			if i+1 >= len(msgs) || msgs[i+1].Parts[0].Kind != "tool_result" {
				t.Fatalf("the message after the call %s is not its result: %+v", a.callID, msgs[i+1:])
			}
			return
		}
	}
	t.Fatalf("no call %s in the newest request", a.callID)
}

func TestContractModelAPIQuestionEdges(t *testing.T) {
	asked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}}
	answers := map[string]string{"Which database?": "SQLite"}
	followUp := harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("ask_user_question"), Reply: harnesstest.Reply{Text: "after"}}
	rest := harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	runScenarios(t, []scenario{
		{
			name:   "model_api_question_answered_after_a_restart_keeps_the_result_next_to_its_call",
			driver: askLane(true),
			model:  askSteps(),
			actions: withActions(asked, restart{}, answerQuestion{as: "a", callID: "toolu_ask", answers: answers}, waitIdle{as: "a"},
				resultFollowsCall{callID: "toolu_ask"}),
		},
		{
			name:   "model_api_second_question_call_gets_an_error_when_the_first_is_denied",
			driver: askLaneWith(pluginConfig(t, map[string]any{"deny_ask": "Denied?"})),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{askCall("toolu_a", "Denied?"), askCall("toolu_b", "Which?")}}},
				followUp, rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name:   "model_api_question_with_invalid_input_gets_an_error_result",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_bad", Name: "ask_user_question", Input: map[string]any{"questions": []any{map[string]any{"question": "Which?", "options": []any{}}}}},
				}}},
				followUp, rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
	})
}

func TestContractModelAPIQuestionInput(t *testing.T) {
	followUp := harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("ask_user_question"), Reply: harnesstest.Reply{Text: "after"}}
	rest := harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	runScenarios(t, []scenario{
		{
			name:   "model_api_question_with_repeated_question_texts_gets_an_error_result",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_dup", Name: "ask_user_question", Input: map[string]any{"questions": []any{askQuestion("Same?", "A", "B"), askQuestion("Same?", "C", "D")}}},
				}}},
				followUp, rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name:   "model_api_question_with_repeated_option_labels_gets_an_error_result",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_dup", Name: "ask_user_question", Input: map[string]any{"questions": []any{askQuestion("Which?", "A", "A")}}},
				}}},
				followUp, rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name:   "model_api_question_with_a_long_header_gets_an_error_result",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_long", Name: "ask_user_question", Input: map[string]any{"questions": []any{askQuestionWith("Which?", "A very long header", "A", "B")}}},
				}}},
				followUp, rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name:   "model_api_question_with_preview_metadata_answers_and_annotations_opens_a_request",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_ask", Name: "ask_user_question", Input: map[string]any{
						"questions": []any{map[string]any{"question": "Which layout?", "header": "Layout", "multiSelect": false, "options": []any{
							map[string]any{"label": "Grid", "description": "a grid", "preview": "+--+--+"},
							map[string]any{"label": "List", "description": "a list"},
						}}},
						"metadata": map[string]any{"source": "test"}, "answers": map[string]any{"x": "y"}, "annotations": map[string]any{"x": map[string]any{"notes": "n"}},
					}},
				}}},
				followUp, rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, getSession{as: "a"},
				answerQuestion{as: "a", callID: "toolu_ask", answers: map[string]string{"Which layout?": "Grid"}}, waitIdle{as: "a"}},
		},
	})
}

func TestContractModelAPIQuestionScope(t *testing.T) {
	rest := harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	runScenarios(t, []scenario{
		{
			name:   "model_api_session_info_lists_the_question_tool_when_it_is_offered",
			driver: askLane(true),
			model:  toolChain(harnesstest.ToolCall{Name: "session_info", Input: map[string]any{}}),
			actions: []action{create{as: "a"}, submit{as: "a", text: "who are you"}, waitIdle{as: "a"},
				requestHolds{text: "ask_user_question"}},
		},
		{
			name:       "model_api_child_session_is_not_offered_the_question_tool",
			concurrent: true,
			driver:     askLane(true),
			model: []harnesstest.Step{
				{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "delegate"}, awaitRequests{n: 3}, waitIdle{as: "a"},
				cancelTree{as: "a"}, awaitCanceled{step: "child"}, waitIdle{as: "a"}},
		},
		{
			name:   "model_api_goal_session_is_not_offered_the_question_tool",
			driver: askLane(true),
			model:  []harnesstest.Step{agentStep("work", "done", false), evaluatorStep("judge", "MET: said done", false)},
			actions: []action{create{as: "a"}, setGoal{as: "a", condition: "say done", maxTurns: 3}, waitIdle{as: "a"},
				getSession{as: "a"}},
		},
	})
}

// requestHolds checks that the newest model request holds text in a message.
type requestHolds struct{ text string }

func (a requestHolds) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	for _, m := range reqs[len(reqs)-1].Messages {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, a.text) {
				return
			}
		}
	}
	t.Fatalf("the newest request does not hold %q", a.text)
}

func TestContractModelAPIQuestionStops(t *testing.T) {
	ask := harnesstest.Step{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{askCall("toolu_ask", "Which?")}}}
	rest := harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	runScenarios(t, []scenario{
		{
			name:    "model_api_interrupt_during_the_before_hook_of_a_question_opens_no_request",
			driver:  askLaneWith(pluginConfig(t, map[string]any{"before_delay_ms": 3000})),
			model:   []harnesstest.Step{ask, rest},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, awaitCall{as: "a", callID: "toolu_ask"}, interrupt{as: "a"}, awaitTurnEnd{as: "a"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name:   "model_api_goal_set_during_a_model_call_refuses_the_question",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "ask", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{askCall("toolu_ask", "Which?")}, Block: true}},
				{Name: "after", Match: harnesstest.LastToolResult("ask_user_question"), Reply: harnesstest.Reply{Text: "ok"}},
				evaluatorStep("judge", "MET: ok", true), rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, awaitRequests{n: 1}, setGoal{as: "a", condition: "say ok", maxTurns: 3},
				release{step: "ask"}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
		{
			name:   "model_api_handoff_while_a_sibling_tool_runs_opens_no_request",
			driver: askLane(true),
			model: []harnesstest.Step{
				{Name: "calls", Match: harnesstest.LastUserText("pick a db"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_slow", Name: "bash", Input: map[string]any{"command": "touch tool-running; sleep 1"}},
					askCall("toolu_ask", "Which?"),
				}}},
				rest,
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, awaitFile{path: "tool-running"}, restart{}, waitIdle{as: "a"}, getSession{as: "a"}},
		},
	})
}

// claudeAskDescription records the description of ask_user_question that the
// hosted MCP server offered the CLI after its newest run.
type claudeAskDescription struct{ as string }

func (a claudeAskDescription) run(t *testing.T, r *run) {
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	result, _ := body["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	for _, tool := range list {
		if m, _ := tool.(map[string]any); m["name"] == "ask_user_question" {
			r.record(t, "ask_description", a.as, callResult{Status: http.StatusOK, Body: m["description"]})
			return
		}
	}
	t.Fatal("the hosted MCP server did not offer ask_user_question")
}

func TestContractQuestionToolDescription(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "question_tool_description_says_to_ask_with_the_tool_and_not_in_text",
		driver:  claudeLane{mode: "normal", ask: true, listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeAskDescription{as: "a"}),
	}})
}
