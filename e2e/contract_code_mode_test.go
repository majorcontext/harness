package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// createCode creates a session with code_mode true.
type createCode struct{ as string }

func (a createCode) run(t *testing.T, r *run) {
	t.Helper()
	res := r.drv.CreateCodeMode(t)
	body, _ := res.Body.(map[string]any)
	id, _ := body["id"].(string)
	if res.Status/100 != 2 || id == "" {
		t.Fatalf("create with code_mode = %d %v", res.Status, res.Body)
	}
	r.ids[a.as] = id
	r.aliases = append(r.aliases, a.as)
}

// tryCreateCode creates a session with code_mode true and records the response. It binds no alias.
type tryCreateCode struct{}

func (tryCreateCode) run(t *testing.T, r *run) {
	t.Helper()
	r.record(t, "create_code_mode", "", r.drv.CreateCodeMode(t))
}

// setCodeMode patches code_mode of the session and records the response, which must have the status (200 when zero).
type setCodeMode struct {
	as     string
	on     bool
	status int
}

func (a setCodeMode) run(t *testing.T, r *run) {
	t.Helper()
	res := r.drv.SetCodeMode(t, r.id(t, a.as), a.on)
	if want := cmpStatus(a.status); res.Status != want {
		t.Fatalf("set code_mode %v = %d %v, want %d", a.on, res.Status, res.Body, want)
	}
	r.record(t, "set_code_mode", a.as, res)
}

// expectModeNotices checks that the engine-context blocks of request req that state a mode hold the texts of want, in order.
type expectModeNotices struct {
	req  int
	want []string
}

func (a expectModeNotices) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectModeNotices: request %d of %d", a.req, len(reqs))
	}
	var got []string
	for _, m := range reqs[a.req-1].Messages {
		for _, p := range m.Parts {
			if isEngineContext(p) && strings.Contains(p.Text, "[mode:") {
				got = append(got, p.Text)
			}
		}
	}
	if len(got) != len(a.want) {
		t.Fatalf("request %d holds %d mode notices %q, want %d %q", a.req, len(got), got, len(a.want), a.want)
	}
	for i, w := range a.want {
		if !strings.Contains(got[i], w) {
			t.Errorf("request %d mode notice %d = %q, want %q in it", a.req, i, got[i], w)
		}
	}
	if sys := reqs[a.req-1].System; strings.Contains(sys, "[mode:") {
		t.Errorf("request %d system prompt holds a mode: %q", a.req, sys)
	}
}

// expectCodeLog checks the log of the session: notices is the number of code-mode pin records, and declared the base mode of each declaration in order.
type expectCodeLog struct {
	as       string
	notices  int
	declared []bool
}

func (a expectCodeLog) run(t *testing.T, r *run) {
	t.Helper()
	d, ok := r.drv.(*runtimeDriver)
	if !ok {
		t.Fatalf("expectCodeLog needs the runtime driver, got %T", r.drv)
	}
	notices, declared := 0, []bool{}
	for _, e := range d.events(t, r.id(t, a.as)) {
		p, err := e.Payload()
		if err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case protocol.KindPinRecorded:
			if p.(*protocol.PinRecorded).Notice == "code-mode" {
				notices++
			}
		case protocol.KindPrefixDeclared:
			var f map[string]any
			if err := json.Unmarshal(mustJSON(t, p), &f); err != nil {
				t.Fatal(err)
			}
			on, _ := f["code_mode"].(bool)
			declared = append(declared, on)
		}
	}
	if notices != a.notices || !slices.Equal(declared, a.declared) {
		t.Fatalf("the log holds %d code-mode notices and the declared modes %v, want %d and %v", notices, declared, a.notices, a.declared)
	}
}

const (
	modeCode   = "[mode: code."
	modeNormal = "[mode: normal."
	refusal    = "Call it from the script of a code call"
)

// codeModeRows is the disposition of the rows of the code-mode setting.
var codeModeRows = map[string]runtimeRow{
	"a_switch_to_code_mode_appends_one_notice_and_keeps_the_prefix":                     sameAsServe(),
	"a_switch_back_to_normal_mode_appends_one_notice_and_keeps_the_prefix":              sameAsServe(),
	"a_session_created_in_code_mode_gets_one_notice_after_its_first_declaration":        sameAsServe(),
	"a_compaction_in_code_mode_keeps_no_old_notice_and_adds_one_after_the_declaration":  sameAsServe(),
	"a_compaction_in_normal_mode_after_a_switch_back_adds_no_notice":                    sameAsServe(),
	"a_restart_in_code_mode_sends_the_same_bytes_and_appends_no_notice":                 sameAsServe(),
	"a_hidden_tool_called_directly_in_code_mode_is_refused_and_runs_from_a_script":      sameAsServe(),
	"a_mode_switch_fails_while_the_declaration_lacks_code_and_works_after_a_compaction": sameAsServe(),
	"create_with_code_mode_fails_while_the_config_key_is_false":                         sameAsServe(),
	"a_declaration_without_code_after_a_code_mode_notice_states_that_the_tool_is_gone":  sameAsServe(),
	"a_child_starts_in_the_mode_of_its_parent":                                          sameAsServe(),
}

var (
	keepOneCode = codeWith(keepOneTurn)
	modeOne     = replyTo("one", "one", "1")
	modeTwo     = replyTo("two", "two", "2")
	modeThree   = replyTo("three", "three", "3")
)

func modeTurn(text string) []action {
	return []action{submit{as: "a", text: text}, waitIdle{as: "a"}}
}

func modeSeq(parts ...[]action) []action {
	var out []action
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func modeSame(reqs ...int) []action {
	var out []action
	for _, n := range reqs {
		out = append(out, expectTools{req: n, sameAs: 1}, expectSystem{req: n, sameAs: 1})
	}
	return out
}

func TestContractCodeModeSwitch(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:   "a_switch_to_code_mode_appends_one_notice_and_keeps_the_prefix",
			config: codeOn,
			model:  []harnesstest.Step{modeOne, modeTwo, modeThree},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: true}}, modeTurn("two"), modeTurn("three"),
				modeSame(2, 3), []action{
					expectModeNotices{req: 1}, expectModeNotices{req: 2, want: []string{modeCode}}, expectModeNotices{req: 3, want: []string{modeCode}},
					expectCodeLog{as: "a", notices: 1, declared: []bool{false}},
				}),
		},
		{
			name:   "a_switch_back_to_normal_mode_appends_one_notice_and_keeps_the_prefix",
			config: codeOn,
			model:  []harnesstest.Step{modeOne, modeTwo, modeThree},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: false}}, modeTurn("two"), modeTurn("three"),
				modeSame(2, 3), []action{
					expectModeNotices{req: 1, want: []string{modeCode}},
					expectModeNotices{req: 2, want: []string{modeCode, modeNormal}},
					expectModeNotices{req: 3, want: []string{modeCode, modeNormal}},
					expectCodeLog{as: "a", notices: 2, declared: []bool{true}},
				}),
		},
		{
			name:   "a_session_created_in_code_mode_gets_one_notice_after_its_first_declaration",
			config: codeOn,
			model:  []harnesstest.Step{modeOne, modeTwo},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), modeTurn("two"), modeSame(2), []action{
				expectModeNotices{req: 1, want: []string{modeCode}}, expectModeNotices{req: 2, want: []string{modeCode}},
				expectCodeLog{as: "a", notices: 1, declared: []bool{true}},
			}),
		},
	})
}

func TestContractCodeModeDeclaration(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "a_compaction_in_code_mode_keeps_no_old_notice_and_adds_one_after_the_declaration", prefixBreaks: compactionBreak(),
			config: keepOneCode,
			model:  []harnesstest.Step{modeOne, modeTwo, gistSummary, modeThree},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), modeTurn("two"), []action{compact{as: "a"}}, modeTurn("three"), []action{
				expectModeNotices{req: 2, want: []string{modeCode}}, expectModeNotices{req: 4, want: []string{modeCode}},
				expectCodeLog{as: "a", notices: 2, declared: []bool{true, true}},
			}),
		},
		{
			name: "a_compaction_in_normal_mode_after_a_switch_back_adds_no_notice", prefixBreaks: compactionBreak(),
			config: keepOneCode,
			model:  []harnesstest.Step{modeOne, modeTwo, gistSummary, modeThree},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: false}}, modeTurn("two"), []action{compact{as: "a"}}, modeTurn("three"), []action{
				expectModeNotices{req: 2, want: []string{modeCode, modeNormal}}, expectModeNotices{req: 4},
				expectCodeLog{as: "a", notices: 2, declared: []bool{true, false}},
			}),
		},
		{
			name:   "a_restart_in_code_mode_sends_the_same_bytes_and_appends_no_notice",
			config: codeOn,
			model:  []harnesstest.Step{modeOne, modeTwo},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), []action{restart{}}, modeTurn("two"), modeSame(2), []action{
				expectModeNotices{req: 2, want: []string{modeCode}},
				expectCodeLog{as: "a", notices: 1, declared: []bool{true}},
			}),
		},
		{
			name:         "a_mode_switch_fails_while_the_declaration_lacks_code_and_works_after_a_compaction",
			prefixBreaks: compactionBreak(),
			config:       keepOneTurn,
			model:        []harnesstest.Step{modeOne, modeTwo, gistSummary, modeThree, replyTo("four", "four", "4")},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), modeTurn("two"), []action{
				setCodeMode{as: "a", on: true, status: http.StatusBadRequest},
				writeFile{path: ".harness.json", body: `{"code_mode": true}`},
				restart{},
				setCodeMode{as: "a", on: true, status: http.StatusBadRequest},
				compact{as: "a"},
			}, modeTurn("three"), []action{
				setCodeMode{as: "a", on: true},
			}, modeTurn("four"), []action{
				expectTools{req: 4, has: []string{"code"}}, expectModeNotices{req: 4}, expectModeNotices{req: 5, want: []string{modeCode}},
				expectCodeLog{as: "a", notices: 1, declared: []bool{false, false}},
			}),
		},
		{
			name:    "create_with_code_mode_fails_while_the_config_key_is_false",
			actions: []action{tryCreateCode{}},
		},
	})
}

func TestContractCodeModeEnforcement(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:         "a_declaration_without_code_after_a_code_mode_notice_states_that_the_tool_is_gone",
			prefixBreaks: prefixBreak("a model change declares the live tools, which lack the code tool", breakTools),
			config:       codeOn,
			model:        afterFirstTurn(ftRead(map[string]any{"path": "notes.txt"})),
			actions: []action{
				writeFile{path: "notes.txt", body: "needle in a haystack\n"},
				createCode{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"},
				writeFile{path: ".harness.json", body: `{"code_mode": false}`},
				restart{},
				setModel{as: "a", model: "anthropic/claude-opus-5"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectTools{req: 1, has: []string{"code"}}, expectTools{req: 2, lacks: []string{"code"}},
				expectModeNotices{req: 2, want: []string{modeCode, "The code tool is gone."}},
				expectTools{req: 3, text: []string{"needle in a haystack"}},
				expectNoText{req: 3, text: refusal},
			},
		},
		{
			name:   "a_hidden_tool_called_directly_in_code_mode_is_refused_and_runs_from_a_script",
			config: codeOn,
			model:  toolChain(ftRead(map[string]any{"path": "notes.txt"}), codeCall(`read_file(path="notes.txt")`)),
			actions: []action{
				writeFile{path: "notes.txt", body: "needle in a haystack\n"},
				createCode{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				expectTools{req: 2, text: []string{refusal}},
				expectTools{req: 3, text: []string{"needle in a haystack"}},
			},
		},
		{
			name:       "a_child_starts_in_the_mode_of_its_parent",
			concurrent: true,
			config:     codeOn,
			model: []harnesstest.Step{
				{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				{Name: "child", Match: func(r harnesstest.Request) bool {
					return harnesstest.LastUserText("child work")(r) && strings.Contains(strings.Join(flat(r.Messages), "\n"), modeCode)
				}, Reply: harnesstest.Reply{Text: "child done"}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("child done"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				createCode{as: "a"}, submit{as: "a", text: "delegate"}, awaitRequests{n: 4}, waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true}, getSession{as: "kid"},
			},
		},
	})
}
