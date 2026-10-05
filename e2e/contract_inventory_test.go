package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

var rowNamePattern = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)+$`)

// contractRowNames maps each scenario row name found in the contract_*_test.go
// tables to the test function that declares it. A row is a keyed name field,
// the first field of a positional struct row, or the first argument of a
// row(...) helper call.
func contractRowNames(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("contract_*_test.go")
	syncFiles, serr := filepath.Glob("runtime_sync*_test.go")
	files = append(files, syncFiles...)
	if err != nil || serr != nil || len(files) == 0 {
		t.Fatalf("no contract test files: %v", err)
	}
	names := map[string]string{}
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				for _, v := range rowNameLiterals(n) {
					if s, err := strconv.Unquote(v.Value); err == nil && rowNamePattern.MatchString(s) {
						names[s] = fn.Name.Name
					}
				}
				return true
			})
		}
	}
	return names
}

func rowNameLiterals(n ast.Node) []*ast.BasicLit {
	var vals []ast.Expr
	switch n := n.(type) {
	case *ast.CallExpr:
		if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "row" && len(n.Args) > 0 {
			vals = append(vals, n.Args[0])
		}
	case *ast.CompositeLit:
		switch typ := n.Type.(type) {
		case *ast.ArrayType, *ast.MapType:
			return nil
		case *ast.Ident:
			if typ.Name == "mcpServerDef" {
				return nil
			}
		}
		for i, el := range n.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "name" {
					vals = append(vals, kv.Value)
				}
			} else if i == 0 && len(n.Elts) > 1 {
				vals = append(vals, el)
			}
		}
	}
	var lits []*ast.BasicLit
	for _, v := range vals {
		if bl, ok := v.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			lits = append(lits, bl)
		}
	}
	return lits
}

// boxesFeatures lists every harness feature that boxes uses, each with the
// contract rows that exercise it.
var boxesFeatures = []struct {
	feature   string
	scenarios []string
}{
	{"config providers.anthropic", []string{"text_reply", "one_tool_round_trip", "provider_429_then_ok"}},
	{"config providers.bifrost", []string{"bifrost_text_reply", "bifrost_tool_round_trip"}},
	{"config providers.openai", []string{"openai_key_http_sse_text_turn"}},
	{"config providers.codex", []string{"codex_http_sse_tool_round_trip_resends_history", "codex_ws_chains_two_turns"}},
	{"config providers.claude-code", []string{"claudecode_turn_text_and_tool", "claudecode_resume_across_turns"}},
	{"config goal_evaluator_model", []string{"bifrost_goal_met_first_turn", "bifrost_goal_not_met_then_met"}},
	{"config session_sync=fsync", []string{"session_sync_fsync_reports_fsync", "session_sync_default_reports_fsync"}},
	{"config context_window_required", []string{"create_checks_the_model", "create_takes_an_unknown_model_when_no_window_is_required"}},
	{"config mcp_tool_loading", []string{"mcp_lazy_search_select_then_call", "mcp_auto_defers_over_threshold", "mcp_per_server_tool_loading_overrides_global"}},
	{"config mcp_servers", []string{"mcp_eager_lists_namespaced_tools", "mcp_tool_call_result", "mcp_http_sse_reply", "mcp_stdio_server_call", "claudecode_configured_mcp_servers_reach_the_cli"}},
	{"config append_system_prompt", []string{"system_segments_order_append_layers_then_instructions_then_skills"}},
	{"config plugins", []string{"plugin_tools_listed_and_run", "plugin_boxes_style_command_and_dir", "plugin_before_hook_rewrites_and_blocks"}},

	{"config owner_epoch and sync", []string{"sync_conflict_is_final_and_ends_the_session", "sync_server_error_is_sent_again", "sync_splits_a_batch_under_the_body_cap", "catch_up_conflict_skips_the_session_and_reports_it"}},
	{"GET /models", []string{"models_lists_the_configured_providers"}},
	{"POST /sessions/{id}/answer", []string{"claudecode_question_parks_then_answer_resumes"}},
	{"DELETE /sessions/{id}", []string{"end_session_semantics"}},
	{"GET /sessions", []string{"status_and_list_cold_after_restart"}},
	{"POST /sessions", []string{"text_reply"}},
	{"GET /sessions/{id}", []string{"session_settings_validation_and_persistence", "goal_update_while_busy"}},
	{"GET /sessions/{id}/messages pages", []string{"messages_page_windows", "messages_page_after_compaction"}},
	{"GET /sessions/{id}/messages bootstrap window", []string{"bootstrap_cold_then_resident_windows"}},
	{"GET /sessions/{id}/events page", []string{"journal_pages_follow_cursor"}},
	{"GET /sessions/{id}/inputs", []string{"queue_survives_clean_restart_then_delete"}},
	{"DELETE /sessions/{id}/inputs/{input}", []string{"queue_delete_while_busy"}},
	{"POST /sessions/{id}/inputs enqueue", []string{"enqueue_while_busy_runs_after"}},
	{"POST /sessions/{id}/inputs prompt", []string{"text_reply", "two_turns_keep_history"}},
	{"POST /sessions/{id}/interrupt", []string{"interrupt_idle_is_noop", "interrupt_drops_unfinished_text_then_queue_continues"}},
	{"POST /sessions/{id}/inputs send", []string{"send_to_child_and_cancel_tree", "goal_busy_send_is_queued"}},
	{"PATCH /sessions/{id} model", []string{"session_settings_validation_and_persistence"}},
	{"PATCH /sessions/{id} effort", []string{"session_settings_validation_and_persistence"}},
	{"PATCH /sessions/{id} service_tier", []string{"session_settings_validation_and_persistence"}},
	{"PUT /sessions/{id}/goal", []string{
		"goal_met_first_turn",
		"goal_update_while_busy",
		"deferred_goal_judges_finished_turn",
		"goal_update_deferred_goal_waits_for_first_turn",
	}},
	{"PUT /sessions/{id}/goal deferred", []string{
		"deferred_goal_with_max_turns",
		"busy_deferred_goal_with_max_turns",
		"persisted_queue_dispatches_after_deferred_arm",
		"queued_prompt_runs_before_deferred_auto_arm",
	}},
	{"DELETE /sessions/{id}/goal", []string{"goal_cleared_before_first_turn"}},
	{"POST /sessions/{id}/compact", []string{"compact_manual", "compact_survives_restart"}},
	{"GET /commands", []string{"builtin_commands_run_and_record"}},
	{"GET /workspace/changes", []string{
		"git_changes_uncommitted_scope_reports_modified_deleted_and_untracked_files",
		"git_changes_branch_scope_diffs_the_work_tree_against_the_default_branch",
	}},
	{"GET /processes, POST /processes/{name}/{action}", []string{"process_http_lifecycle", "process_tool_from_the_model"}},
	{"GET /sessions/{id}/events stream", []string{"sse_resume_cursor", "sse_resume_after_kill"}},
}

// knownGaps lists features that no contract row exercises yet, each with a
// one-line reason.
var knownGaps = []struct{ feature, reason string }{}

func TestContractInventory(t *testing.T) {
	skipShort(t)
	rows := contractRowNames(t)
	seen := map[string]bool{}
	mapped := map[string]bool{}
	for _, f := range boxesFeatures {
		if seen[f.feature] {
			t.Errorf("feature %q is listed twice", f.feature)
		}
		seen[f.feature] = true
		mapped[f.feature] = len(f.scenarios) > 0
		if len(f.scenarios) == 0 {
			t.Errorf("feature %q maps to no contract row; list it under knownGaps with a reason", f.feature)
		}
		for _, name := range f.scenarios {
			if _, ok := rows[name]; !ok {
				t.Errorf("feature %q maps to %q, which is not a row in any TestContract* table", f.feature, name)
			}
		}
	}
	for _, g := range knownGaps {
		if g.reason == "" {
			t.Errorf("known gap %q has no reason", g.feature)
		}
		if mapped[g.feature] {
			t.Errorf("known gap %q is also mapped to a row; remove it from knownGaps", g.feature)
		}
		t.Logf("known gap: %s: %s", g.feature, g.reason)
	}
}
