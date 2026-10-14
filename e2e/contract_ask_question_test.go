package e2e

import (
	"encoding/json"
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

// answerQuestion answers the open question call callID with answers.
type answerQuestion struct {
	as, callID string
	answers    map[string]string
}

func (a answerQuestion) run(t *testing.T, r *run) {
	body, err := json.Marshal(a.answers)
	if err != nil {
		t.Fatal(err)
	}
	r.record(t, "answer_question", a.as, laneOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{answer: string(body)}))
}

// dismissQuestion dismisses the open question call callID.
type dismissQuestion struct{ as, callID string }

func (a dismissQuestion) run(t *testing.T, r *run) {
	r.record(t, "dismiss_question", a.as, laneOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{dismiss: true}))
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
