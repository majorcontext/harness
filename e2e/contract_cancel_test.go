package e2e

import "testing"

// cancelRows are the runtime dispositions of the rows of this file.
var cancelRows = map[string]runtimeRow{
	"claudecode_cli_that_drops_a_tool_request_stops_the_call": sameAsServe(),
	"claudecode_interrupt_stops_a_running_tool_call":          sameAsServe(),
	"interrupt_stops_a_running_bash_call":                     sameAsServe(),
	"file_tools_refuse_a_named_pipe":                    sameAsServe(),
}

func joinRows(base, more map[string]runtimeRow) map[string]runtimeRow {
	for k, v := range more {
		base[k] = v
	}
	return base
}

func TestContractToolCallCancel(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "claudecode_cli_that_drops_a_tool_request_stops_the_call",
		driver:  claudeLane{mode: "abandon_tool"}.newDriver,
		actions: claudeOneTurn,
	}, {
		name:   "claudecode_interrupt_stops_a_running_tool_call",
		driver: claudeLane{mode: "hold_tool", signals: true}.newDriver,
		actions: []action{create{as: "a"}, submit{as: "a", text: "run it"}, claudeAwaitToolCall{as: "a", callID: "toolu_slow"},
			interrupt{as: "a"}, waitIdle{as: "a"}, claudeSession{as: "a"}},
	}, {
		name:  "interrupt_stops_a_running_bash_call",
		model: toolChain(ftBash("sleep 600"))[:1],
		actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, awaitCall{as: "a", callID: "toolu_1"},
			interrupt{as: "a"}, waitIdle{as: "a"}},
	}})
}
