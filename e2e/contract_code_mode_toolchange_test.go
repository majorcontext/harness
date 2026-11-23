package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const toolChangeBetaHeader = "mid-conversation-tool-changes-2026-07-01"

// toolChangeOn is the entry of the anthropic provider that offers tool changes on the fake base URL.
var toolChangeOn = map[string]any{"tool_changes": true}

// described are the tools that code mode keeps in the tools array.
var described = []string{"code", "goal", "model", "task", "read_tool_result"}

// expectDeferred checks request req: the tools of has carry defer_loading, no tool of described does, and the beta header is there. With none, the request defers no tool and sends no beta header.
type expectDeferred struct {
	req      int
	has      []string
	none     bool
	onlyBeta bool // no tool carries defer_loading, and the beta header is there
}

func (a expectDeferred) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectDeferred: request %d of %d", a.req, len(reqs))
	}
	req := reqs[a.req-1]
	beta := req.Header.Get("Anthropic-Beta")
	if a.none {
		if len(req.DeferredTools) > 0 || strings.Contains(beta, toolChangeBetaHeader) {
			t.Errorf("request %d defers %v with the beta %q, want none", a.req, req.DeferredTools, beta)
		}
		return
	}
	if a.onlyBeta && len(req.DeferredTools) > 0 {
		t.Errorf("request %d defers %v, want none", a.req, req.DeferredTools)
	}
	if !strings.Contains(beta, toolChangeBetaHeader) {
		t.Errorf("request %d beta header = %q, want %s", a.req, beta, toolChangeBetaHeader)
	}
	for _, n := range a.has {
		if !slices.Contains(req.DeferredTools, n) {
			t.Errorf("request %d does not defer %q: %v", a.req, n, req.DeferredTools)
		}
	}
	for _, n := range described {
		if slices.Contains(req.DeferredTools, n) {
			t.Errorf("request %d defers %q, which code mode keeps described", a.req, n)
		}
		if !slices.Contains(req.Tools, n) {
			t.Errorf("request %d lacks the described tool %q: %v", a.req, n, req.Tools)
		}
	}
}

// expectToolChanges checks the blocks of the tool-change messages of request req: one message, with a removal of each hidden tool and no addition (removes), or an addition of each hidden tool and no removal (adds), and no block for a described tool. With none, the request holds no such message.
type expectToolChanges struct {
	req           int
	removes, adds bool
	none          bool
}

func (a expectToolChanges) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectToolChanges: request %d of %d", a.req, len(reqs))
	}
	var removed, added []string
	messages := 0
	for _, m := range reqs[a.req-1].Messages {
		if m.Role != "system" {
			continue
		}
		messages++
		for _, p := range m.Parts {
			if p.Kind == "tool_removal" {
				removed = append(removed, p.ToolName)
			} else {
				added = append(added, p.ToolName)
			}
		}
	}
	if a.none {
		if messages > 0 {
			t.Errorf("request %d holds %d tool-change messages, want none", a.req, messages)
		}
		return
	}
	if messages != 1 {
		t.Errorf("request %d holds %d tool-change messages, want 1", a.req, messages)
	}
	changed, other := removed, added
	if a.adds {
		changed, other = added, removed
	}
	for _, n := range hiddenSample {
		if !slices.Contains(changed, n) {
			t.Errorf("request %d lacks the block of %q: removed %v, added %v", a.req, n, removed, added)
		}
	}
	for _, n := range described {
		if slices.Contains(changed, n) || slices.Contains(other, n) {
			t.Errorf("request %d changes %q, which code mode keeps described", a.req, n)
		}
	}
	if len(other) > 0 {
		t.Errorf("request %d holds the unwanted blocks %v", a.req, other)
	}
}

// expectRemovedThenAdded checks that request req holds two tool-change messages, the first with a removal and the second with an addition of each hidden tool, and that the tools array lacks code.
type expectRemovedThenAdded struct{ req int }

func (a expectRemovedThenAdded) run(t *testing.T, r *run) {
	t.Helper()
	req := r.fake.Requests()[a.req-1]
	var kinds [][]string
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		var names []string
		for _, p := range m.Parts {
			names = append(names, p.Kind+":"+p.ToolName)
		}
		kinds = append(kinds, names)
	}
	if len(kinds) != 2 {
		t.Fatalf("request %d holds the tool-change messages %v, want a removal and an addition", a.req, kinds)
	}
	for i, kind := range []string{"tool_removal:", "tool_addition:"} {
		for _, n := range hiddenSample {
			if !slices.Contains(kinds[i], kind+n) {
				t.Errorf("message %d of request %d lacks %s%s: %v", i+1, a.req, kind, n, kinds[i])
			}
		}
	}
	if slices.Contains(req.Tools, "code") {
		t.Errorf("request %d holds the code tool: %v", a.req, req.Tools)
	}
}

// expectSameToolChange checks that request req holds the same tool-change blocks as request like, and the same tools array.
type expectSameToolChange struct{ req, like int }

func (a expectSameToolChange) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	blocks := func(req harnesstest.Request) (out []string) {
		for _, m := range req.Messages {
			if m.Role == "system" {
				for _, p := range m.Parts {
					out = append(out, p.Kind+":"+p.ToolName)
				}
			}
		}
		return out
	}
	got, want := reqs[a.req-1], reqs[a.like-1]
	if len(blocks(got)) == 0 || !slices.Equal(blocks(got), blocks(want)) || !slices.Equal(got.Tools, want.Tools) || !slices.Equal(got.DeferredTools, want.DeferredTools) {
		t.Errorf("request %d holds blocks %v, tools %v, deferred %v; request %d holds %v, %v, %v", a.req, blocks(got), got.Tools, got.DeferredTools, a.like, blocks(want), want.Tools, want.DeferredTools)
	}
}

var toolChangeRows = map[string]runtimeRow{
	"a_session_created_in_code_mode_defers_the_hidden_tools_from_its_first_request":                         sameAsServe(),
	"a_switch_to_code_mode_appends_one_tool_change_message_and_keeps_the_prefix":                            sameAsServe(),
	"a_switch_back_to_normal_mode_adds_the_hidden_tools_by_reference":                                       sameAsServe(),
	"a_compaction_after_a_switch_moves_the_mode_into_the_array_and_leaves_no_tool_change":                   sameAsServe(),
	"a_restart_after_a_switch_sends_the_same_tool_change_message":                                           sameAsServe(),
	"a_model_without_the_capability_keeps_the_engine_context_form":                                          sameAsServe(),
	"a_session_with_code_declared_that_never_enters_code_mode_sends_the_beta_header_from_its_first_request": sameAsServe(),
	"a_session_without_the_code_tool_sends_no_beta_header":                                                  sameAsServe(),
	"a_declaration_without_code_after_a_tool_change_adds_the_tools_again_and_states_that_code_is_gone":      sameAsServe(),
	"a_provider_entry_that_turns_tool_changes_off_keeps_the_engine_context_form_on_a_good_model":            sameAsServe(),
}

var hiddenSample = []string{"read_file", "write_file", "edit_file", "glob", "grep", "ls", "bash", "process", "session_info"}

func TestContractCodeModeToolChange(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:   "a_session_created_in_code_mode_defers_the_hidden_tools_from_its_first_request",
			config: codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), modeTurn("two"), modeSame(2), []action{
				expectDeferred{req: 1, has: hiddenSample}, expectDeferred{req: 2, has: hiddenSample},
				expectToolChanges{req: 1, none: true}, expectToolChanges{req: 2, none: true},
				expectModeNotices{req: 1}, expectModeNotices{req: 2},
				expectCodeLog{as: "a", notices: 0, declared: []bool{true}},
			}),
		},
		{
			name:   "a_switch_to_code_mode_appends_one_tool_change_message_and_keeps_the_prefix",
			config: codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo, modeThree},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: true}}, modeTurn("two"), modeTurn("three"),
				modeSame(2, 3), []action{
					expectDeferred{req: 1, onlyBeta: true}, expectDeferred{req: 2, onlyBeta: true}, expectDeferred{req: 3, onlyBeta: true},
					expectToolChanges{req: 1, none: true},
					expectToolChanges{req: 2, removes: true}, expectToolChanges{req: 3, removes: true},
					expectModeNotices{req: 2}, expectModeNotices{req: 3},
					expectCodeLog{as: "a", notices: 1, declared: []bool{false}},
				}),
		},
		{
			name:   "a_switch_back_to_normal_mode_adds_the_hidden_tools_by_reference",
			config: codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo, modeThree},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: false}}, modeTurn("two"), modeTurn("three"),
				modeSame(2, 3), []action{
					expectDeferred{req: 1, has: hiddenSample}, expectDeferred{req: 2, has: hiddenSample},
					expectToolChanges{req: 1, none: true},
					expectToolChanges{req: 2, adds: true}, expectToolChanges{req: 3, adds: true},
					expectModeNotices{req: 2}, expectCodeLog{as: "a", notices: 1, declared: []bool{true}},
				}),
		},
	})
}

func TestContractCodeModeToolChangeDeclaration(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "a_compaction_after_a_switch_moves_the_mode_into_the_array_and_leaves_no_tool_change", prefixBreaks: compactionBreak(),
			config: keepOneCode, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo, gistSummary, modeThree},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: true}}, modeTurn("two"), []action{compact{as: "a"}}, modeTurn("three"), []action{
				expectToolChanges{req: 2, removes: true}, expectDeferred{req: 4, has: hiddenSample}, expectToolChanges{req: 4, none: true}, expectModeNotices{req: 4},
				expectCodeLog{as: "a", notices: 1, declared: []bool{false, true}},
			}),
		},
		{
			name:   "a_restart_after_a_switch_sends_the_same_tool_change_message",
			config: codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo, modeThree},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: true}}, modeTurn("two"), []action{restart{}}, modeTurn("three"), []action{
				expectSameToolChange{req: 3, like: 2}, expectCodeLog{as: "a", notices: 1, declared: []bool{false}},
			}),
		},
		{
			name:  "a_session_without_the_code_tool_sends_no_beta_header",
			entry: toolChangeOn,
			model: []harnesstest.Step{modeOne},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), []action{
				expectDeferred{req: 1, none: true}, expectToolChanges{req: 1, none: true},
			}),
		},
		{
			name:         "a_declaration_without_code_after_a_tool_change_adds_the_tools_again_and_states_that_code_is_gone",
			prefixBreaks: prefixBreak("a model change declares the live tools, which lack the code tool", breakTools),
			config:       codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo, modeThree},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: true}}, modeTurn("two"), []action{
				writeFile{path: ".harness.json", body: `{"code_mode": false}`},
				restart{},
				setModel{as: "a", model: "anthropic/claude-opus-5"},
			}, modeTurn("three"), []action{
				expectToolChanges{req: 2, removes: true},
				expectRemovedThenAdded{req: 3},
				expectModeNotices{req: 3, want: []string{"The code tool is gone."}},
			}),
		},
		{
			name:   "a_model_without_the_capability_keeps_the_engine_context_form",
			config: codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo},
			actions: modeSeq([]action{create{as: "a", model: "anthropic/claude-sonnet-4-6"}}, modeTurn("one"), []action{setCodeMode{as: "a", on: true}}, modeTurn("two"), []action{
				expectDeferred{req: 2, none: true}, expectToolChanges{req: 2, none: true}, expectModeNotices{req: 2, want: []string{modeCode}},
			}),
		},
		{
			name:   "a_session_with_code_declared_that_never_enters_code_mode_sends_the_beta_header_from_its_first_request",
			config: codeOn, entry: toolChangeOn,
			model: []harnesstest.Step{modeOne, modeTwo},
			actions: modeSeq([]action{create{as: "a"}}, modeTurn("one"), modeTurn("two"), modeSame(2), []action{
				expectDeferred{req: 1, onlyBeta: true}, expectDeferred{req: 2, onlyBeta: true}, expectToolChanges{req: 2, none: true},
				expectModeNotices{req: 2}, expectCodeLog{as: "a", notices: 0, declared: []bool{false}},
			}),
		},
		{
			name:   "a_provider_entry_that_turns_tool_changes_off_keeps_the_engine_context_form_on_a_good_model",
			config: codeOn, entry: map[string]any{"tool_changes": false},
			model: []harnesstest.Step{modeOne},
			actions: modeSeq([]action{createCode{as: "a"}}, modeTurn("one"), []action{
				expectDeferred{req: 1, none: true}, expectToolChanges{req: 1, none: true}, expectModeNotices{req: 1, want: []string{modeCode}},
			}),
		},
	})
}
