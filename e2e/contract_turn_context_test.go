package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var turnContextRows = map[string]runtimeRow{
	"turn_context_reaches_the_turn_of_its_input_only":            sameAsServe(),
	"a_turn_context_over_4_kib_is_invalid_request":               sameAsServe(),
	"claudecode_turn_context_reaches_the_cli_with_the_input":     sameAsServe(),
	"claudecode_turn_context_of_a_steer_joins_the_steer_message": sameAsServe(),
}

func TestContractTurnContext(t *testing.T) {
	const model = "anthropic/claude-opus-5-5"
	note := "main moved by 3 commits; your work was rebased onto it."
	runScenarios(t, []scenario{
		{
			name:         "turn_context_reaches_the_turn_of_its_input_only",
			model:        []harnesstest.Step{{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}},
			prefixBreaks: map[string]string{breakHistory: "a later turn does not send the turn context of an earlier input"},
			actions: []action{
				create{as: "a", model: model},
				postInputContext{as: "a", text: "run", context: note}, waitIdle{as: "a"},
				postInputContext{as: "a", text: "run", context: "another note", repeat: true},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
			},
		},
		{
			name:    "a_turn_context_over_4_kib_is_invalid_request",
			actions: []action{create{as: "a", model: model}, postInputContext{as: "a", text: "run", context: strings.Repeat("x", 4097)}},
		},
		{
			name:   "claudecode_turn_context_reaches_the_cli_with_the_input",
			driver: claudeLaneDriver("normal"),
			actions: []action{
				create{as: "a"},
				postInputContext{as: "a", text: "run it", context: note}, waitIdle{as: "a"},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				claudeInputs{as: "a"},
			},
		},
		{
			name:   "claudecode_turn_context_of_a_steer_joins_the_steer_message",
			driver: claudeLaneDriver("steer"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitToolCall{as: "a", callID: "toolu_s"},
				postInputContext{as: "a", text: "second", context: note},
				waitIdle{as: "a"},
				claudeInputs{as: "a"},
			},
		},
	})
}

func (d *runtimeDriver) PostInputContext(t *testing.T, id, text, turnContext string, repeat bool) callResult {
	t.Helper()
	path, body := d.inputAs(id, d.lastInput[id], text, "", "")
	if !repeat {
		path, body = d.input(id, text, "", "")
	}
	body["turn_context"] = turnContext
	return d.call(t, http.MethodPost, path, body)
}
