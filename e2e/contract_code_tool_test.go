package e2e

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

var codeOn = map[string]any{"code_mode": true}

func codeCall(script string, kv ...any) harnesstest.ToolCall {
	return ftTool("code", ftArgs(append([]any{"script", script}, kv...)...))
}

func codeWith(extra map[string]any) map[string]any {
	out := maps.Clone(codeOn)
	maps.Copy(out, extra)
	return out
}

// nestedRecords records the nested-call records of the session that the log holds, in order.
type nestedRecords struct {
	as     string
	want   int
	hidden string // a value that the arguments of no record may hold
}

func (a nestedRecords) run(t *testing.T, r *run) {
	t.Helper()
	d, ok := r.drv.(*runtimeDriver)
	if !ok {
		t.Fatalf("nestedRecords needs the runtime driver, got %T", r.drv)
	}
	var out []protocol.NestedCallRecorded
	for _, e := range d.events(t, r.id(t, a.as)) {
		if e.Kind != protocol.KindNestedCall {
			continue
		}
		p, err := e.Payload()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, *p.(*protocol.NestedCallRecorded))
	}
	if len(out) != a.want {
		t.Fatalf("the log holds %d nested-call records, want %d: %+v", len(out), a.want, out)
	}
	raw := mustJSON(t, out)
	for _, rec := range out {
		if a.hidden != "" && strings.Contains(string(rec.Arguments), a.hidden) {
			t.Fatalf("the arguments of a nested-call record hold %q: %s", a.hidden, raw)
		}
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	r.record(t, "nested_calls", a.as, callResult{Status: 200, Body: body})
}

func TestContractCodeTool(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "code_mode_true_offers_the_code_tool_beside_the_other_tools",
			config:  codeOn,
			model:   textReply("ok"),
			actions: oneTurn,
		},
		{
			name:   "a_script_calls_two_tools_and_returns_a_filtered_result_in_one_model_round_trip",
			config: codeOn,
			model: toolChain(codeCall(`names = ls()
lines = [l for l in read_file(path="notes.txt").split("\n") if "needle" in l]
print(names)
lines`)),
			actions: []action{
				writeFile{path: "notes.txt", body: "hay\nneedle one\nhay\nneedle two\n"},
				create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
			},
		},
		{
			name:    "a_tool_error_stops_the_script",
			config:  codeOn,
			model:   toolChain(codeCall("read_file(path=\"missing.txt\")\nprint(\"after\")")),
			actions: oneTurn,
		},
		{
			name:    "attempt_returns_the_error_and_the_script_goes_on",
			config:  codeOn,
			model:   toolChain(codeCall("r = attempt(read_file, path=\"missing.txt\")\nprint(r.ok)\nr.text")),
			actions: oneTurn,
		},
	})
}

func TestContractCodeToolNested(t *testing.T) {
	gate := gateServer
	runScenarios(t, []scenario{
		{
			name:   "parallel_runs_calls_on_one_file_in_list_order_and_returns_results_in_list_order",
			config: codeOn,
			model: toolChain(codeCall(`parallel([
    lambda: write_file(path="p.txt", content="one"),
    lambda: edit_file(path="p.txt", old_string="one", new_string="two"),
    lambda: read_file(path="p.txt"),
    lambda: bash(command="echo a"),
    lambda: bash(command="echo b"),
])`)),
			actions: oneTurn,
		},
		{
			name:   "parallel_overlaps_read_only_calls_and_keeps_list_order",
			config: codeOn,
			setup:  mcpSetup(nil, gate),
			model:  toolChain(codeCall(`parallel([lambda: mcp__gate__hold(id="a"), lambda: mcp__gate__hold(id="b")])`)),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 2},
				releaseCall{server: "gate", id: "b"},
				releaseCall{server: "gate", id: "a"},
				waitIdle{as: "a"},
			},
		},
		{
			name:    "a_nested_call_passes_the_before_hook_and_a_deny_becomes_the_error",
			config:  codeWith(pluginConfig(t, nil)),
			model:   toolChain(codeCall("r = attempt(bash, command=\"echo block-me\")\nr.text")),
			actions: oneTurn,
		},
		{
			name:   "a_nested_call_refuses_a_tool_that_the_allowed_tools_leave_out",
			config: codeOn,
			model:  toolChain(codeCall("[attempt(\"bash\", command=\"echo hi\").text, ls()]")),
			actions: []action{
				create{as: "a", allowed: []string{"code", "ls"}}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
			},
		},
		{
			name:    "a_script_cannot_call_code_task_goal_model_mcp_or_ask_user_question",
			config:  codeOn,
			model:   toolChain(codeCall("[attempt(n).text for n in [\"code\", \"task\", \"goal\", \"model\", \"mcp\", \"ask_user_question\"]]")),
			actions: oneTurn,
		},
		{
			name:    "a_script_that_names_task_directly_fails_with_an_undefined_name",
			config:  codeOn,
			model:   toolChain(codeCall("task(action=\"list\")")),
			actions: oneTurn,
		},
		{
			name:   "each_nested_call_has_one_durable_record_that_the_next_request_does_not_hold",
			config: codeOn,
			model:  toolChain(codeCall("ls()\nread_file(path=\"notes.txt\")\nattempt(grep, pattern=\"(\")\n1")),
			actions: []action{
				writeFile{path: "notes.txt", body: "hello\n"},
				create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				nestedRecords{as: "a", want: 3},
			},
		},
	})
}

func TestContractCodeToolSafety(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "a_parallel_inside_a_parallel_function_fails",
			config:  codeOn,
			model:   toolChain(codeCall("parallel([lambda: parallel([lambda: ls()])])")),
			actions: oneTurn,
		},
		{
			name:   "a_cyclic_argument_fails_the_script_and_the_session_goes_on",
			config: codeOn,
			model: toolChain(codeCall("x = []\nx.append(x)\nattempt(\"ls\", path=x)"), codeCall("x = []\nx.append(x)\nx"),
				codeCall("[1, 2]")),
			actions: oneTurn,
		},
		{
			name:   "parallel_functions_read_the_values_that_they_share",
			config: codeOn,
			model: toolChain(codeCall(`xs = list(range(1000))
def total():
    t = 0
    for _ in range(50):
        for x in xs:
            t += x
    return t
parallel([total, total, total, total, total, total, total, total])`)),
			actions: oneTurn,
		},
		{
			name:    "a_parallel_function_cannot_change_a_value_that_it_shares",
			config:  codeOn,
			model:   toolChain(codeCall("xs = [1]\nparallel([lambda: xs.append(2)])")),
			actions: oneTurn,
		},
		{
			name:   "a_script_that_shares_an_exponential_tuple_fails_inside_its_wall_clock_and_the_session_goes_on",
			config: codeOn,
			model: toolChain(
				codeCall("x = ()\nfor _ in range(60):\n    x = (x, x)\nparallel([lambda: 1])", "timeout_s", 20),
				codeCall("x = ()\nfor _ in range(60):\n    x = (x, x)\nprint(x)", "timeout_s", 20),
				codeCall("x = ()\nfor _ in range(60):\n    x = (x, x)\njson.encode(x)", "timeout_s", 20),
				codeCall("x = ()\nfor _ in range(60):\n    x = (x, x)\nx", "timeout_s", 20),
				codeCall("def f():\n    def g():\n        return g\n    return parallel([g])\nf()", "timeout_s", 20),
				codeCall("def outer():\n    def g():\n        return g\n    return [g].append\nparallel([outer()])", "timeout_s", 20),
				codeCall("x = []\nfor _ in range(2000000):\n    x = [x]\nparallel([lambda: 1])", "timeout_s", 20),
				codeCall("[1]")),
			actions: oneTurn,
		},
		{
			name:   "a_nested_call_records_its_arguments_with_secrets_masked",
			config: codeOn,
			model: toolChain(codeCall("attempt(bash, command=\"echo TOKEN=abcdefgh1234\")\n" +
				"r = attempt(bash, command=\"echo zyxwvu''ts9876\")\nattempt(ls, path=\".\", api_key=r.text, nested={\"password\": [r.text]}, headers={\"Authorization\": \"Bearer \" + r.text})")),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				nestedRecords{as: "a", want: 3, hidden: "zyxwvuts9876"},
			},
		},
	})
}

func TestContractCodeToolLimits(t *testing.T) {
	lazy := map[string]any{"mcp_tool_loading": "lazy"}
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	gate := gateServer
	runScenarios(t, []scenario{
		{
			name:   "interrupt_cancels_the_script_and_its_running_calls",
			config: codeOn,
			setup:  mcpSetup(nil, gate),
			model:  toolChain(codeCall(`mcp__gate__hold(id="a")`))[:1],
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 1},
				awaitCall{as: "a", callID: "toolu_1"},
				interrupt{as: "a"},
				awaitAbandoned{server: "gate", n: 1},
				awaitTurnEnd{as: "a"},
				waitIdle{as: "a"},
			},
		},
		{
			name:   "a_script_over_its_wall_clock_fails_and_cancels_its_running_calls",
			config: codeOn,
			setup:  mcpSetup(nil, gate),
			model:  toolChain(codeCall(`mcp__gate__hold(id="a")`, "timeout_s", 1)),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitAbandoned{server: "gate", n: 1},
				waitIdle{as: "a"},
			},
		},
		{
			name:    "a_script_over_its_step_limit_fails",
			config:  codeOn,
			model:   toolChain(codeCall("print(\"start\")\nwhile True:\n    pass")),
			actions: oneTurn,
		},
		{
			name:   "a_deferred_mcp_tool_is_found_described_and_called_from_a_script",
			config: codeWith(lazy),
			setup:  mcpSetup(nil, weather),
			model: toolChain(codeCall(`found = [t["name"] for t in search_tools("forecast")]
print(found)
print(describe_tool("mcp__weather__forecast")["name"])
mcp__weather__forecast(city="Oslo")`)),
			actions: oneTurn,
		},
	})
}

func TestContractCodeToolClaudeCode(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "claudecode_bridge_describes_code_when_code_mode_is_true",
		driver:  claudeLane{mode: "normal", listTools: true, extra: codeOn}.newDriver,
		actions: withActions(claudeOneTurn, claudeOfferedTools{as: "a"}),
	}})
}

func TestCodeModeFalseChangesNoRequest(t *testing.T) {
	skipShort(t)
	t.Parallel()
	sc := scenario{
		name:    "code_mode_false",
		model:   toolChain(ftBash("echo hi")),
		actions: oneTurn,
	}
	off := sc
	off.config = map[string]any{"code_mode": false}
	a, b := runScenario(t, sc, serveHost), runScenario(t, off, serveHost)
	if string(mustJSON(t, a)) != string(mustJSON(t, b)) {
		t.Fatalf("code_mode false changed a request:\n%s", lineDiff(string(mustJSON(t, a)), string(mustJSON(t, b))))
	}
	for _, r := range b.Requests {
		if slices.Contains(r.Tools, "code") {
			t.Fatalf("tools = %v, want no code", r.Tools)
		}
	}
}

var codeToolRows = map[string]runtimeRow{
	"a_script_that_shares_an_exponential_tuple_fails_inside_its_wall_clock_and_the_session_goes_on": sameAsServe(),
	"a_parallel_inside_a_parallel_function_fails":                                                   sameAsServe(),
	"a_cyclic_argument_fails_the_script_and_the_session_goes_on":                                    sameAsServe(),
	"parallel_functions_read_the_values_that_they_share":                                            sameAsServe(),
	"a_parallel_function_cannot_change_a_value_that_it_shares":                                      sameAsServe(),
	"a_nested_call_records_its_arguments_with_secrets_masked":                                       sameAsServe(),
	"code_mode_true_offers_the_code_tool_beside_the_other_tools":                                    sameAsServe(),
	"a_script_calls_two_tools_and_returns_a_filtered_result_in_one_model_round_trip":                sameAsServe(),
	"a_tool_error_stops_the_script":                                                                 sameAsServe(),
	"attempt_returns_the_error_and_the_script_goes_on":                                              sameAsServe(),
	"parallel_runs_calls_on_one_file_in_list_order_and_returns_results_in_list_order":               sameAsServe(),
	"parallel_overlaps_read_only_calls_and_keeps_list_order":                                        sameAsServe(),
	"a_nested_call_passes_the_before_hook_and_a_deny_becomes_the_error":                             sameAsServe(),
	"a_nested_call_refuses_a_tool_that_the_allowed_tools_leave_out":                                 sameAsServe(),
	"a_script_cannot_call_code_task_goal_model_mcp_or_ask_user_question":                            sameAsServe(),
	"a_script_that_names_task_directly_fails_with_an_undefined_name":                                sameAsServe(),
	"each_nested_call_has_one_durable_record_that_the_next_request_does_not_hold":                   sameAsServe(),
	"interrupt_cancels_the_script_and_its_running_calls":                                            sameAsServe(),
	"a_script_over_its_wall_clock_fails_and_cancels_its_running_calls":                              sameAsServe(),
	"a_script_over_its_step_limit_fails":                                                            sameAsServe(),
	"a_deferred_mcp_tool_is_found_described_and_called_from_a_script":                               sameAsServe(),
	"claudecode_bridge_describes_code_when_code_mode_is_true":                                       sameAsServe(),
}
