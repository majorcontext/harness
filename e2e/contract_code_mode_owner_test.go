package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// expectInputModes checks the mode text that each user line of the CLI carried, in order across spawns: "code", "normal",
// or "" for a line with no mode text.
type expectInputModes struct{ want []string }

func (a expectInputModes) run(t *testing.T, r *run) {
	t.Helper()
	f, err := os.Open(claudeDriverOf(t, r).stdinLog)
	if err != nil {
		t.Fatalf("open stdin log: %v", err)
	}
	defer func() { _ = f.Close() }()
	got := []string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		facts := inputFacts(t, sc.Bytes())
		if facts["type"] != "user" {
			continue
		}
		text := string(mustJSON(t, facts["content"]))
		switch {
		case strings.Contains(text, modeCode):
			got = append(got, "code")
		case strings.Contains(text, modeNormal):
			got = append(got, "normal")
		default:
			got = append(got, "")
		}
	}
	if !slices.Equal(got, a.want) {
		t.Fatalf("the CLI got the mode texts %q with its inputs, want %q", got, a.want)
	}
}

func claudeEvents(t *testing.T, r *run, as string) []protocol.Event {
	t.Helper()
	return claudeDriverOf(t, r).laneHost.(*runtimeDriver).events(t, r.id(t, as))
}

// expectPinRecords checks the number of code-mode pin records of the session.
type expectPinRecords struct {
	as string
	n  int
}

func (a expectPinRecords) run(t *testing.T, r *run) {
	t.Helper()
	n := 0
	for _, ev := range claudeEvents(t, r, a.as) {
		if ev.Kind == protocol.KindPinRecorded && decodeEvent[protocol.PinRecorded](t, ev).Notice == protocol.PinCodeMode {
			n++
		}
	}
	if n != a.n {
		t.Fatalf("the log holds %d code-mode pin records, want %d", n, a.n)
	}
}

// expectResultText checks that the result of tool call callID holds text, or lacks it when absent is true.
type expectResultText struct {
	as, callID, text string
	absent           bool
}

func (a expectResultText) run(t *testing.T, r *run) {
	t.Helper()
	found := false
	for _, ev := range claudeEvents(t, r, a.as) {
		if ev.Kind != protocol.KindItemCompleted {
			continue
		}
		for _, p := range decodeEvent[protocol.ItemCompleted](t, ev).Message.Parts {
			if p.Type != protocol.LogPartToolResult || p.CallID != a.callID {
				continue
			}
			found = true
			if has := strings.Contains(p.Text, a.text); has == a.absent {
				t.Fatalf("result of %s = %q, want it to hold %q: %v", a.callID, p.Text, a.text, !a.absent)
			}
		}
	}
	if !found {
		t.Fatalf("the log holds no result of call %s", a.callID)
	}
}

// claudeRememberTools keeps the tools that the hosted MCP server offered the CLI after its newest run under key.
type claudeRememberTools struct{ key string }

func (a claudeRememberTools) run(t *testing.T, r *run) {
	t.Helper()
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	result, _ := body["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	names := []string{}
	for _, tool := range list {
		m, _ := tool.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	slices.Sort(names)
	if r.fx == nil {
		r.fx = map[string]any{}
	}
	raw, _ := json.Marshal(names)
	r.fx["offered:"+a.key] = string(raw)
}

// claudeSameTools checks that the tools kept under a and b are the same and not empty.
type claudeSameTools struct{ a, b string }

func (a claudeSameTools) run(t *testing.T, r *run) {
	t.Helper()
	x, y := r.fx["offered:"+a.a], r.fx["offered:"+a.b]
	if x == nil || x == "[]" || x != y {
		t.Fatalf("the endpoint offered %v with %s and %v with %s, want the same tools", x, a.a, y, a.b)
	}
}

var ownerModeRows = map[string]runtimeRow{
	"claudecode_session_created_in_code_mode_gets_the_code_text_with_its_first_input_once":     sameAsServe(),
	"claudecode_switch_to_code_mode_sends_the_code_text_with_the_next_input_once":              sameAsServe(),
	"claudecode_switch_back_to_normal_mode_sends_the_normal_text_with_the_next_input_once":     sameAsServe(),
	"claudecode_restart_in_code_mode_sends_no_mode_text_again":                                 sameAsServe(),
	"claudecode_backend_compaction_in_code_mode_sends_the_code_text_with_the_next_input":       sameAsServe(),
	"claudecode_backend_compaction_in_normal_mode_after_a_switch_back_sends_no_mode_text":      sameAsServe(),
	"claudecode_direct_bash_in_code_mode_is_refused_and_a_script_calls_it":                     sameAsServe(),
	"claudecode_mcp_call_in_code_mode_is_refused_by_its_target_and_a_script_calls_it":          sameAsServe(),
	"claudecode_endpoint_lists_the_same_tools_in_both_modes":                                   sameAsServe(),
	"claudecode_model_change_between_a_harness_model_and_the_cli_model_tells_the_new_one_once": sameAsServe(),
	"claudecode_session_in_code_mode_updates_while_the_declaration_lacks_code":                 sameAsServe(),
}

func TestContractCodeModeClaudeCodeInput(t *testing.T) {
	cc := func(mode, spawns string) func(*testing.T, host, string) driver {
		return claudeLane{mode: mode, spawnModes: spawns, extra: codeOn}.newDriver
	}
	turnA := func(text string) []action { return []action{submit{as: "a", text: text}, waitIdle{as: "a"}} }
	runScenarios(t, []scenario{
		{
			name:   "claudecode_session_created_in_code_mode_gets_the_code_text_with_its_first_input_once",
			driver: cc("normal", ""),
			actions: modeSeq([]action{createCode{as: "a"}}, turnA("one"), turnA("two"), []action{
				expectInputModes{want: []string{"code", ""}}, expectPinRecords{as: "a", n: 1},
			}),
		},
		{
			name:   "claudecode_switch_to_code_mode_sends_the_code_text_with_the_next_input_once",
			driver: cc("normal", ""),
			actions: modeSeq([]action{create{as: "a"}}, turnA("one"), []action{setCodeMode{as: "a", on: true}}, turnA("two"), turnA("three"), []action{
				expectInputModes{want: []string{"", "code", ""}}, expectPinRecords{as: "a", n: 1},
			}),
		},
		{
			name:   "claudecode_switch_back_to_normal_mode_sends_the_normal_text_with_the_next_input_once",
			driver: cc("normal", ""),
			actions: modeSeq([]action{createCode{as: "a"}}, turnA("one"), []action{setCodeMode{as: "a", on: false}}, turnA("two"), turnA("three"), []action{
				expectInputModes{want: []string{"code", "normal", ""}}, expectPinRecords{as: "a", n: 2},
			}),
		},
		{
			name:   "claudecode_restart_in_code_mode_sends_no_mode_text_again",
			driver: cc("normal", ""),
			actions: modeSeq([]action{createCode{as: "a"}}, turnA("one"), []action{restart{}}, turnA("two"), []action{
				expectInputModes{want: []string{"code", ""}}, expectPinRecords{as: "a", n: 1},
			}),
		},
		{
			name:   "claudecode_backend_compaction_in_code_mode_sends_the_code_text_with_the_next_input",
			driver: cc("compact_turn", "1=normal,3=normal,4=normal"),
			actions: modeSeq([]action{createCode{as: "a"}}, turnA("one"), []action{compact{as: "a"}}, turnA("two"), turnA("three"), []action{
				expectInputModes{want: []string{"code", "", "code", ""}}, expectPinRecords{as: "a", n: 2},
			}),
		},
		{
			name:   "claudecode_backend_compaction_in_normal_mode_after_a_switch_back_sends_no_mode_text",
			driver: cc("compact_turn", "1=normal,2=normal,4=normal"),
			actions: modeSeq([]action{createCode{as: "a"}}, turnA("one"), []action{setCodeMode{as: "a", on: false}}, turnA("two"),
				[]action{compact{as: "a"}}, turnA("three"), []action{
					expectInputModes{want: []string{"code", "normal", "", ""}}, expectPinRecords{as: "a", n: 2},
				}),
		},
	})
}

func TestContractCodeModeClaudeCodeRefusal(t *testing.T) {
	scripted := turnCall{"code", map[string]any{"script": `bash(command="echo scripted")`}}
	forecast := map[string]any{"action": "call", "tool": "mcp__weather__forecast", "arguments": map[string]any{"city": "Oslo"}}
	runScenarios(t, []scenario{
		{
			name: "claudecode_direct_bash_in_code_mode_is_refused_and_a_script_calls_it",
			driver: claudeLane{mode: "turn_calls", extra: codeOn, turnCalls: [][]turnCall{
				{{"bash", map[string]any{"command": "echo direct"}}}, {scripted},
				{{"mcp", map[string]any{"action": "select", "tools": []string{"bash"}}}},
			}}.newDriver,
			actions: []action{
				createCode{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				expectResultText{as: "a", callID: "toolu_t0_0", text: refusal},
				expectResultText{as: "a", callID: "toolu_t0_0", text: "direct\n", absent: true},
				expectResultText{as: "a", callID: "toolu_t1_0", text: "scripted"},
				expectResultText{as: "a", callID: "toolu_t2_0", text: "code mode:", absent: true},
				expectPinRecords{as: "a", n: 1},
			},
		},
		{
			name: "claudecode_mcp_call_in_code_mode_is_refused_by_its_target_and_a_script_calls_it",
			driver: func(t *testing.T, h host, modelURL string) driver {
				fx := map[string]any{}
				cfg := mcpSetup(codeWith(map[string]any{"mcp_tool_loading": "lazy"}), mcpServerDef{name: "weather", spec: mcpWeather(""), noListChanged: true})(t, fx)
				d := claudeLane{mode: "turn_calls", extra: cfg, turnCalls: [][]turnCall{
					{{"mcp", forecast}}, {{"code", map[string]any{"script": `mcp__weather__forecast(city="Oslo")`}}},
				}}.newDriver(t, h, modelURL).(*claudeDriver)
				d.fx = fx
				return d
			},
			actions: []action{
				createCode{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				expectResultText{as: "a", callID: "toolu_t0_0", text: "code mode: mcp__weather__forecast is a function of a script"},
				expectResultText{as: "a", callID: "toolu_t1_0", text: "code mode:", absent: true},
			},
		},
	})
}

func TestContractCodeModeClaudeCodeTools(t *testing.T) {
	runScenarios(t, []scenario{{
		name:   "claudecode_endpoint_lists_the_same_tools_in_both_modes",
		driver: claudeLane{mode: "normal", listTools: true, extra: codeOn}.newDriver,
		actions: []action{
			create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, claudeRememberTools{key: "normal"},
			setCodeMode{as: "a", on: true},
			submit{as: "a", text: "two"}, waitIdle{as: "a"}, claudeRememberTools{key: "code"},
			claudeSameTools{a: "normal", b: "code"}, expectInputModes{want: []string{"", "code"}},
		},
	}})
}

func TestContractCodeModeClaudeCodeModelChange(t *testing.T) {
	native := replyTo("native", "native", "native reply")
	runScenarios(t, []scenario{
		{
			name:         "claudecode_model_change_between_a_harness_model_and_the_cli_model_tells_the_new_one_once",
			prefixBreaks: foreignHistoryBreak(),
			driver:       claudeLane{mode: "normal", extra: codeOn}.newDriver,
			model:        []harnesstest.Step{native},
			actions: []action{
				createCode{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"},
				setModel{as: "a", model: "anthropic/claude-fable-5"}, submit{as: "a", text: "native"}, waitIdle{as: "a"},
				setModel{as: "a", model: "claude-code/sonnet"}, submit{as: "a", text: "back"}, waitIdle{as: "a"},
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				expectModeNotices{req: 1, want: []string{modeCode}},
				expectInputModes{want: []string{"code", "code", ""}},
			},
		},
		{
			name:   "claudecode_session_in_code_mode_updates_while_the_declaration_lacks_code",
			driver: claudeLane{mode: "normal"}.newDriver,
			model:  []harnesstest.Step{native},
			actions: []action{
				create{as: "a"}, setModel{as: "a", model: "anthropic/claude-fable-5"}, submit{as: "a", text: "native"}, waitIdle{as: "a"},
				writeFile{path: ".harness.json", body: `{"code_mode": true}`}, restart{},
				setModel{as: "a", model: "claude-code/sonnet"}, setCodeMode{as: "a", on: true},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectInputModes{want: []string{"code"}},
			},
		},
	})
}
