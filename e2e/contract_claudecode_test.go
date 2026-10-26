package e2e

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/majorcontext/harness/harnesstest"
)

func claudeLaneDriver(mode string) func(*testing.T, host, string) driver {
	return claudeLane{mode: mode}.newDriver
}

func withActions(base []action, more ...action) []action {
	return append(append([]action{}, base...), more...)
}

var claudeOneTurn = []action{create{as: "a"}, submit{as: "a", text: "run it"}, waitIdle{as: "a"}}

func TestContractClaudeCodeTurns(t *testing.T) {
	again := []action{submit{as: "a", text: "again"}, waitIdle{as: "a"}}
	runScenarios(t, []scenario{
		{
			name:    "claudecode_turn_text_and_tool",
			driver:  claudeLaneDriver("normal"),
			actions: withActions(claudeOneTurn, claudeSession{as: "a"}, claudeInvocations{as: "a"}, claudeInputs{as: "a"}),
		},
		{
			name:    "claudecode_resume_across_turns",
			driver:  claudeLaneDriver("normal"),
			actions: withActions(claudeOneTurn, append(again, claudeInvocations{as: "a"}, claudeInputs{as: "a"})...),
		},
		{
			name:    "claudecode_resume_survives_restart",
			driver:  claudeLaneDriver("normal"),
			actions: withActions(claudeOneTurn, append([]action{restart{}}, append(again, claudeInvocations{as: "a"}, claudeInputs{as: "a"})...)...),
		},
		{
			name:   "claudecode_interrupt_mid_turn",
			driver: claudeLaneDriver("hang_after_text"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitText{as: "a", text: "Working on it."},
				interrupt{as: "a"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
			},
		},
		{
			name:   "claudecode_compact_delegated",
			driver: claudeLaneDriver("compact_turn"),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
				compact{as: "a"},
				restart{},
				claudeJournalEvents{as: "a", prefix: "compaction."},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
				claudeInputs{as: "a"},
			},
		},
		{
			name:   "claudecode_compact_reads_post_tokens",
			driver: claudeLaneDriver("compact_after_tokens"),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
				claudeSession{as: "a"},
				compact{as: "a"},
				claudeSession{as: "a"},
			},
		},
		{
			name:   "claudecode_compact_keeps_the_window",
			driver: claudeLaneDriver("compact_after_window"),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
				claudeSession{as: "a"},
				compact{as: "a"},
				claudeSession{as: "a"},
			},
		},
		{
			name:   "claudecode_queued_prompt_injected_mid_turn",
			driver: claudeLaneDriver("steer"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitToolCall{as: "a", callID: "toolu_s"},
				enqueue{as: "a", text: "second"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			},
		},
	})
}

func TestContractClaudeCodeSteer(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:   "claudecode_steer_during_final_text_runs_as_the_next_turn",
			driver: claudeLaneDriver("steer_final_text"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitText{as: "a", text: "FINAL_TEXT"},
				enqueue{as: "a", text: "second"},
				claudeAwaitAdmitted{as: "a", text: "second", n: 1},
				claudeCloseWindow{},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			},
		},
		{
			name:   "claudecode_steer_held_during_text_joins_at_the_next_tool_boundary",
			driver: claudeLaneDriver("steer_text_then_tool"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitText{as: "a", text: "FINAL_TEXT"},
				enqueue{as: "a", text: "second"},
				claudeAwaitAdmitted{as: "a", text: "second", n: 1},
				writeFile{path: "child.gate", body: "open\n"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			},
		},
		{
			name:   "claudecode_cli_exit_before_answering_a_steer_fails_the_turn",
			driver: claudeLaneDriver("steer_unanswered"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitToolCall{as: "a", callID: "toolu_r"},
				enqueue{as: "a", text: "second"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			},
		},
		{
			name:   "claudecode_interrupt_while_awaiting_a_steer_answer_interrupts_the_turn",
			driver: claudeLaneDriver("steer_hang"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitToolCall{as: "a", callID: "toolu_r"},
				enqueue{as: "a", text: "second"},
				claudeAwaitText{as: "a", text: "tool done"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
			},
		},
		{
			name:   "claudecode_steer_the_cli_queued_past_its_result_answers_in_the_same_turn",
			driver: claudeLaneDriver("steer_untaken"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitToolCall{as: "a", callID: "toolu_r"},
				enqueue{as: "a", text: "second"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			},
		},
	})
}

func TestContractClaudeCodePartialMessages(t *testing.T) {
	live := func(mode string) func(*testing.T, host, string) driver { return claudeLaneDriver(mode) }
	watched := []action{
		create{as: "a"},
		claudeWatchLive{as: "a"},
		submit{as: "a", text: "run it"},
		waitIdle{as: "a"},
		claudeLive{as: "a"},
		claudeSession{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:    "claudecode_text_and_thinking_stream_as_deltas_before_the_item",
			driver:  live("partial_messages"),
			actions: withActions(watched, claudeInvocations{as: "a"}),
		},
		{
			name:    "claudecode_retried_reply_starts_a_new_live_item",
			driver:  live("partial_retry"),
			actions: watched,
		},
		{
			name:    "claudecode_tool_result_between_the_deltas_and_the_frame_of_a_block_keeps_the_live_item",
			driver:  live("partial_tool_result_between_deltas"),
			actions: watched,
		},
		{
			name:    "claudecode_retried_reply_with_a_tool_call_alone_completes_under_a_new_item",
			driver:  live("partial_retry_tool"),
			actions: watched,
		},
	})
}

// claudeChildReportRows are the runtime rows of the child report rows of the
// Claude Code lane that runtimeRows does not list.
var claudeChildReportRows = map[string]runtimeRow{
	"claudecode_busy_parent_reads_a_child_report_at_the_next_tool_result": sameAsServe(),
}

func TestContractClaudeCodeChildReport(t *testing.T) {
	settled := []action{waitIdle{as: "a"}, claudeInputs{as: "a"}}
	reported := func(n int) []action {
		return append([]action{
			create{as: "a"},
			submit{as: "a", text: "delegate"},
			claudeAwaitAdmitted{as: "a", text: "A background task you started has finished", n: n},
			claudeCloseWindow{},
			claudeAwaitText{as: "a", text: "noted"},
		}, settled...)
	}
	runScenarios(t, []scenario{
		{
			name:    "claudecode_busy_parent_reads_a_child_report_at_the_next_tool_result",
			driver:  claudeLaneDriver("child_report_at_tool"),
			actions: []action{create{as: "a"}, submit{as: "a", text: "delegate"}, waitIdle{as: "a"}, claudeSession{as: "a"}, claudeInputs{as: "a"}},
		},
		{
			name:    "claudecode_child_report_with_no_tool_call_in_flight_waits_for_the_next_turn",
			driver:  claudeLaneDriver("child_report"),
			actions: reported(1),
		},
		{
			name:    "claudecode_child_reports_with_no_tool_call_in_flight_share_the_next_turn",
			driver:  claudeLaneDriver("child_reports"),
			actions: reported(2),
		},
		{
			name:    "claudecode_long_child_result_has_a_readable_handle",
			driver:  claudeLaneDriver("child_report_long"),
			actions: reported(1),
		},
		{
			name:   "claudecode_queued_prompt_and_child_report_share_a_turn",
			driver: claudeLaneDriver("child_report_after_prompt"),
			actions: append([]action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				claudeAwaitText{as: "a", text: "Delegating."},
				enqueueNext{as: "a", text: "next step"},
				writeFile{path: "child.gate", body: "open\n"},
				claudeAwaitAdmitted{as: "a", text: "A background task you started has finished", n: 1},
				claudeCloseWindow{},
				claudeAwaitText{as: "a", text: "noted"},
			}, settled...),
		},
	})
}

func TestContractClaudeCodeConfig(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "claudecode_runs_in_the_work_dir",
		driver:  claudeLaneDriver("normal"),
		actions: withActions(claudeOneTurn, claudeWorkDir{as: "a"}),
	}, {
		name:    "claudecode_append_system_prompt_reaches_the_cli_as_one_value",
		driver:  claudeLane{mode: "normal", extra: map[string]any{"append_system_prompt": []string{"one", "two"}}}.newDriver,
		actions: withActions(claudeOneTurn, claudeSystemPrompt{as: "a", contains: "one\n\ntwo"}),
	}, {
		name:    "claudecode_append_system_prompt_over_the_argument_limit_reaches_the_cli",
		driver:  claudeLane{mode: "normal", extra: map[string]any{"append_system_prompt": []string{strings.Repeat("a", 200<<10)}}}.newDriver,
		actions: withActions(claudeOneTurn, claudeSystemPrompt{as: "a", contains: strings.Repeat("a", 200<<10)}),
	}, {
		name:    "claudecode_compact_result_with_no_local_command_ends_the_turn",
		driver:  claudeLane{mode: "compact_turn", env: map[string]string{"FAKECLAUDE_COMPACT_LOCAL_COMMAND": ""}}.newDriver,
		actions: withActions(claudeOneTurn, claudeJournalEvents{as: "a", prefix: "compaction."}, claudeSession{as: "a"}),
	}})
}

func TestContractClaudeCodePlugins(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "claudecode_turn_gets_no_plugin_system_segment",
		driver:  claudeLane{mode: "normal", extra: pluginConfig(t, nil)}.newDriver,
		actions: withActions(claudeOneTurn, claudeSystemPrompt{as: "a", contains: fixtureSegment}),
	}})
}

// claudeMCPRows are the runtime rows of the MCP rows of the Claude Code lane
// that runtimeRows does not list.
var claudeMCPRows = map[string]runtimeRow{
	"claudecode_each_mcp_server_keeps_its_instructions_after_a_long_one":     sameAsServe(),
	"claudecode_mcp_server_with_no_allowed_tool_keeps_its_instructions":      sameAsServe(),
	"claudecode_mcp_skills_reach_the_cli_in_the_harness_server_instructions": sameAsServe(),
	"claudecode_mcp_skill_loads_through_the_harness_endpoint":                sameAsServe(),
}

func TestContractClaudeCodeMCPServers(t *testing.T) {
	runScenarios(t, []scenario{{
		name: "claudecode_configured_mcp_tool_runs_through_the_harness_endpoint",
		driver: func(t *testing.T, h host, modelURL string) driver {
			cfg := mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("")})(t, map[string]any{})
			return claudeLane{mode: "normal", callTool: "mcp__weather__forecast", callArgs: map[string]any{"city": "Oslo"}, mcp: cfg["mcp_servers"].(map[string]any)}.newDriver(t, h, modelURL)
		},
		actions: withActions(claudeOneTurn, claudeMCPConfig{as: "a"}, claudeToolCall{as: "a"}),
	}, {
		name: "claudecode_endpoint_initialize_carries_the_mcp_server_instructions",
		driver: func(t *testing.T, h host, modelURL string) driver {
			cfg := mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("Call forecast before alerts.")})(t, map[string]any{})
			return claudeLane{mode: "normal", callTool: "mcp__weather__initialize", mcp: cfg["mcp_servers"].(map[string]any)}.newDriver(t, h, modelURL)
		},
		actions: withActions(claudeOneTurn, claudeToolCall{as: "a"}),
	}, {
		name: "claudecode_each_mcp_server_keeps_its_instructions_after_a_long_one",
		driver: func(t *testing.T, h host, modelURL string) driver {
			long := mcpWeather(strings.Repeat("a", 3900) + "ALPHA END")
			cfg := mcpSetup(nil, mcpServerDef{name: "alpha", spec: long}, mcpServerDef{name: "zeta", spec: mcpWeather("Call forecast before alerts.")})(t, map[string]any{})
			return claudeLane{mode: "normal", callTool: "server_instructions", mcp: cfg["mcp_servers"].(map[string]any)}.newDriver(t, h, modelURL)
		},
		actions: withActions(claudeOneTurn, claudeServerInstructions{as: "a", has: map[string]string{
			"alpha": strings.Repeat("a", 3900) + "ALPHA END",
			"zeta":  "Call forecast before alerts.",
		}}),
	}, {
		name: "claudecode_mcp_server_with_no_allowed_tool_keeps_its_instructions",
		driver: func(t *testing.T, h host, modelURL string) driver {
			cfg := mcpSetup(nil, mcpServerDef{name: "alpha", spec: mcpWeather("")}, mcpServerDef{name: "zeta", spec: mcpWeather("Call forecast before alerts.")})(t, map[string]any{})
			return claudeLane{mode: "normal", callTool: "server_instructions", mcp: cfg["mcp_servers"].(map[string]any)}.newDriver(t, h, modelURL)
		},
		actions: []action{
			create{as: "a", allowed: []string{"bash", "mcp__alpha__forecast"}}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
			claudeServerInstructions{as: "a", has: map[string]string{"zeta": "Call forecast before alerts."}},
		},
	}, {
		name: "claudecode_mcp_skills_reach_the_cli_in_the_harness_server_instructions",
		driver: func(t *testing.T, h host, modelURL string) driver {
			cfg := mcpSetup(nil, skillServer("docs", refundsSkill()))(t, map[string]any{})
			return claudeLane{mode: "normal", callTool: "server_instructions", mcp: cfg["mcp_servers"].(map[string]any)}.newDriver(t, h, modelURL)
		},
		actions: withActions(claudeOneTurn, claudeServerInstructions{as: "a", has: map[string]string{
			"harness": `<skill server="docs" uri="skill://acme/billing/refunds/SKILL.md" name="refunds">Process refund requests per company policy</skill>`,
		}}),
	}, {
		name: "claudecode_mcp_skill_loads_through_the_harness_endpoint",
		driver: func(t *testing.T, h host, modelURL string) driver {
			cfg := mcpSetup(nil, skillServer("docs", refundsSkill()))(t, map[string]any{})
			return claudeLane{mode: "normal", callTool: "load_mcp_skill", callArgs: map[string]any{"server": "docs", "uri": refundsURI},
				mcp: cfg["mcp_servers"].(map[string]any)}.newDriver(t, h, modelURL)
		},
		actions: withActions(claudeOneTurn, claudeToolCall{as: "a"}),
	}})
}

// claudeServerInstructions checks the instructions that the CLI of the newest
// run gives its model for each MCP server, after the cut of the CLI: the
// instructions of each server of has hold its text, and no server text is cut.
// It records the length in runes of the instructions of each server.
type claudeServerInstructions struct {
	as  string
	has map[string]string
}

func (a claudeServerInstructions) run(t *testing.T, r *run) {
	t.Helper()
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	lengths := map[string]int{}
	for server, v := range body {
		text, _ := v.(string)
		lengths[server] = utf8.RuneCountInString(text)
		if strings.HasSuffix(text, "\u2026 [truncated]") {
			t.Errorf("the CLI cut the instructions of server %q: ...%s", server, text[max(0, len(text)-80):])
		}
	}
	for server, want := range a.has {
		if text, _ := body[server].(string); !strings.Contains(text, want) {
			t.Errorf("the instructions of server %q lack %.60q; the CLI got %v", server, want, lengths)
		}
	}
	r.record(t, "server_instructions", a.as, callResult{Status: http.StatusOK, Body: lengths})
}

func TestContractClaudeCodeFrames(t *testing.T) {
	row := func(name, mode string, more ...action) scenario {
		return scenario{name: name, driver: claudeLaneDriver(mode), actions: withActions(claudeOneTurn, more...)}
	}
	goalRow := func(name, mode string) scenario {
		return scenario{name: name, driver: claudeLaneDriver(mode), actions: []action{
			create{as: "a"}, setGoal{as: "a", condition: "say done"}, waitIdle{as: "a"},
			claudeSession{as: "a"}, claudeInvocations{as: "a"},
		}}
	}
	session := claudeSession{as: "a"}
	runScenarios(t, []scenario{
		row("claudecode_thinking_block_is_reasoning", "thinking", session),
		row("claudecode_queued_notification_result_does_not_end_the_turn", "queued_empty_result", session),
		row("claudecode_compact_refuses_keep_turns", "normal", claudeCompactKeeping{as: "a", keep: 1}, session),
		row("claudecode_cli_compaction_is_logged", "compact_boundary", claudeJournalEvents{as: "a", prefix: "compaction."}, session),
		row("claudecode_compact_post_tokens_yields_to_assistant_usage", "compact_boundary_post_tokens", session),
		row("claudecode_error_result_fails_turn", "error", session, claudeInvocations{as: "a"}),
		row("claudecode_cli_exit_runs_once", "crash", session, claudeInvocations{as: "a"}),
		goalRow("claudecode_error_during_execution_pauses_a_goal_and_keeps_its_errors", "error"),
		goalRow("claudecode_credential_refusal_in_errors_fails_a_goal", "credential_error"),
		row("claudecode_rate_limit_event_reaches_subscription_usage", "rate_limit_event", session),
		row("claudecode_rejected_usage_limit_ends_the_turn_provider_exhausted", "usage_limit_rejected", session, claudeTurnEnds{as: "a"}, claudeInvocations{as: "a"}),
		row("claudecode_context_window_from_model_usage", "per_call_usage", session),
	})
}

func TestContractClaudeCodeHistory(t *testing.T) {
	native := harnesstest.Step{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}}
	runScenarios(t, []scenario{{
		name: "claudecode_history_bridge_after_a_mid_turn_model_change", prefixBreaks: foreignHistoryBreak(),
		driver: claudeLane{mode: "normal", historyTool: true}.newDriver,
		model: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
				{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "echo hi"}},
			}}},
			{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "native reply"}, Repeat: true},
		},
		actions: withActions(claudeOneTurn,
			setModel{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "native"},
			awaitRequests{n: 1},
			setModel{as: "a", model: "claude-code/sonnet"},
			release{step: "call"},
			waitIdle{as: "a"},
			submit{as: "a", text: "back"}, waitIdle{as: "a"},
			claudeInvocations{as: "a"},
			claudeHistoryTool{as: "a"},
		),
	}, {
		name: "claudecode_history_bridge_after_a_turn_whose_cli_never_started", prefixBreaks: foreignHistoryBreak(),
		driver: claudeLane{mode: "normal", historyTool: true, spawnModes: "2=crash_before_init"}.newDriver,
		model:  []harnesstest.Step{native},
		actions: withActions(claudeOneTurn,
			setModel{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "native"}, waitIdle{as: "a"},
			setModel{as: "a", model: "claude-code/sonnet"},
			submit{as: "a", text: "back"}, waitIdle{as: "a"},
			submit{as: "a", text: "again"}, waitIdle{as: "a"},
			claudeSession{as: "a"},
			claudeInvocations{as: "a"},
			claudeHistoryTool{as: "a"},
		),
	}, {
		name:   "claudecode_compaction_then_a_harness_model_reads_the_banner_and_the_summary",
		driver: claudeLane{mode: "compact_turn"}.newDriver,
		model: []harnesstest.Step{
			{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}},
		},
		actions: withActions(claudeOneTurn,
			compact{as: "a"},
			setModel{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "native"}, waitIdle{as: "a"},
		),
	}, {
		name:   "claudecode_parallel_calls_then_a_chat_model_gets_a_valid_request",
		driver: claudeLane{mode: "parallel_calls", chat: true}.newDriver,
		chat:   true,
		model: []harnesstest.Step{
			{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}},
		},
		actions: withActions(claudeOneTurn,
			setModel{as: "a", model: "bifrost/" + bifrostModel},
			submit{as: "a", text: "native"}, waitIdle{as: "a"},
		),
	}, {
		name: "claudecode_history_bridge_after_native_turn", prefixBreaks: foreignHistoryBreak(),
		driver: claudeLane{mode: "normal", historyTool: true}.newDriver,
		model:  []harnesstest.Step{native},
		actions: withActions(claudeOneTurn,
			setModel{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "native"}, waitIdle{as: "a"},
			setModel{as: "a", model: "claude-code/sonnet"},
			submit{as: "a", text: "back"}, waitIdle{as: "a"},
			submit{as: "a", text: "again"}, waitIdle{as: "a"},
			claudeInvocations{as: "a"},
			claudeHistoryTool{as: "a"},
		),
	}})
}

func TestContractClaudeCodeSettings(t *testing.T) {
	runScenarios(t, []scenario{{
		name:   "settings_change_to_claude_code_mid_turn_fails_the_turn",
		driver: claudeLaneDriver("normal"),
		model: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
				{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "echo hi"}},
			}}},
			{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}, Repeat: true},
		},
		actions: []action{
			create{as: "a", model: "anthropic/claude-fable-5"},
			submit{as: "a", text: "native"},
			awaitRequests{n: 1},
			setModel{as: "a", model: "claude-code/sonnet"},
			release{step: "call"},
			waitIdle{as: "a"},
			getSession{as: "a"},
			submit{as: "a", text: "again"},
			waitIdle{as: "a"},
			claudeSession{as: "a"},
			claudeInvocations{as: "a"},
		},
	}})
}

func TestContractClaudeCodeQuestions(t *testing.T) {
	lane := claudeLane{mode: "question", ask: true, signals: true}.newDriver
	parked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, claudeSession{as: "a"}}
	answers := map[string]string{"Which database?": "SQLite"}
	runScenarios(t, []scenario{
		{
			name:   "claudecode_question_parks_then_answer_resumes",
			driver: lane,
			actions: withActions(parked,
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
				claudeInputs{as: "a"},
			),
		},
		{
			name:   "claudecode_question_dismissed_by_next_prompt",
			driver: lane,
			actions: withActions(parked,
				submit{as: "a", text: "never mind"}, waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
				claudeInputs{as: "a"},
			),
		},
		{
			name:   "claudecode_question_dismissed_by_compact",
			driver: lane,
			actions: withActions(parked,
				compact{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			),
		},
		{
			name:   "claudecode_question_turn_records_its_cost_after_the_response_ends",
			driver: claudeLane{mode: "question", ask: true, signals: true, env: map[string]string{"FAKE_CLAUDE_QUESTION_GATE": "1"}}.newDriver,
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, claudeAwaitToolCall{as: "a", callID: "toolu_q"},
				writeFile{path: "child.gate", body: "open\n"}, waitIdle{as: "a"}, claudeSession{as: "a"}, claudeDismiss{as: "a", callID: "toolu_q"}},
		},
		{
			name:   "claudecode_steer_during_a_pending_question_stays_queued_for_the_next_turn",
			driver: claudeLane{mode: "question", ask: true, signals: true, env: map[string]string{"FAKE_CLAUDE_QUESTION_GATE": "1"}}.newDriver,
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, claudeAwaitToolCall{as: "a", callID: "toolu_q"},
				enqueue{as: "a", text: "second"}, claudeAwaitAdmitted{as: "a", text: "second", n: 1},
				writeFile{path: "child.gate", body: "open\n"}, waitIdle{as: "a"}, claudeSession{as: "a"}, claudeInputs{as: "a"}},
		},
		{
			name:    "claudecode_question_denied_by_a_plugin_hook_gets_an_error_result_and_the_turn_goes_on",
			driver:  claudeLane{mode: "question", ask: true, signals: true, extra: pluginConfig(t, map[string]any{"deny_ask": "Which database?"})}.newDriver,
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, claudeSession{as: "a"}},
		},
		{
			name: "claudecode_answered_question_replays_to_a_model_api_session", prefixBreaks: foreignHistoryBreak(),
			driver: lane,
			model:  []harnesstest.Step{{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}}},
			actions: withActions(parked,
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				waitIdle{as: "a"},
				setModel{as: "a", model: "anthropic/claude-fable-5"},
				submit{as: "a", text: "native"}, waitIdle{as: "a"},
				requestHoldsCall{name: "ask_user_question", result: "User has answered your questions"},
			),
		},
		{
			name:   "claudecode_question_unknown_call_id_conflicts",
			driver: lane,
			actions: withActions(parked,
				claudeAnswer{as: "a", callID: "toolu_other", answers: answers},
				claudeSession{as: "a"},
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				waitIdle{as: "a"},
			),
		},
	})
}

func TestContractClaudeCodeQuestionRefusals(t *testing.T) {
	lane := claudeLane{mode: "question", ask: true, signals: true}.newDriver
	parked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, claudeSession{as: "a"}}
	answers := map[string]string{"Which database?": "SQLite"}
	raw := func(answer string) action { return claudeRawAnswer{as: "a", callID: "toolu_q", answer: answer} }
	runScenarios(t, []scenario{
		{
			name:   "claudecode_question_dismissed_by_resolve",
			driver: lane,
			actions: withActions(parked,
				claudeDismiss{as: "a", callID: "toolu_q"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
				claudeInputs{as: "a"},
			),
		},
		{
			name:   "claudecode_question_dismissed_by_a_model_of_another_provider",
			driver: lane,
			actions: withActions(parked,
				setModel{as: "a", model: "claude-code/opus"},
				claudeSession{as: "a"},
				setModel{as: "a", model: "anthropic/claude-fable-5"},
				claudeSession{as: "a"},
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				claudeInvocations{as: "a"},
			),
		},
		{
			name:   "claudecode_question_answer_bodies_that_are_refused",
			driver: lane,
			actions: withActions(parked,
				raw(""), raw(`{}`), raw(`{ }`), raw(`null`), raw("  null  "), raw("\n{}\t"), raw(`["SQLite"]`), raw(`{"Which database?":1}`),
				claudeRawAnswer{as: "a", callID: "toolu_q", answer: `{"Which database?":"SQLite"}`, dismiss: true},
				claudeSession{as: "a"},
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				waitIdle{as: "a"},
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
			),
		},
	})
}

func TestContractClaudeCodeQuestionResults(t *testing.T) {
	parked := []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, claudeSession{as: "a"}}
	runScenarios(t, []scenario{
		{
			name:   "claudecode_question_beside_a_running_call_keeps_the_call_result_and_parks",
			driver: claudeLane{mode: "question_sibling", ask: true, signals: true}.newDriver,
			actions: withActions(parked,
				submit{as: "a", text: "never mind"}, waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
			),
		},
		{
			name:   "claudecode_steer_between_a_call_and_a_later_question_of_one_response_stays_queued",
			driver: claudeLane{mode: "question_late_sibling", ask: true, signals: true, env: map[string]string{"FAKE_CLAUDE_QUESTION_GATE": "1"}}.newDriver,
			actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, claudeAwaitToolCall{as: "a", callID: "toolu_s"},
				enqueue{as: "a", text: "second"}, claudeAwaitAdmitted{as: "a", text: "second", n: 1},
				writeFile{path: "child.gate", body: "open\n"}, waitIdle{as: "a"}, claudeSession{as: "a"}, claudeInputs{as: "a"}},
		},
	})
}

// claudeAlwaysLoadTools records the sorted names of the tools that the hosted
// MCP server offered the CLI with the _meta field anthropic/alwaysLoad true.
type claudeAlwaysLoadTools struct{ as string }

func (a claudeAlwaysLoadTools) run(t *testing.T, r *run) {
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	result, _ := body["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	names := []string{}
	for _, tool := range list {
		m, _ := tool.(map[string]any)
		if meta, _ := m["_meta"].(map[string]any); meta["anthropic/alwaysLoad"] == true {
			names = append(names, m["name"].(string))
		}
	}
	slices.Sort(names)
	r.record(t, "always_load_tools", a.as, callResult{Status: http.StatusOK, Body: names})
}

func TestContractClaudeCodeTools(t *testing.T) {
	skipShort(t)
	setArgs := map[string]any{"action": "set", "model": "anthropic/claude-fable-5"}
	runScenarios(t, []scenario{{
		name:    "claudecode_cli_gets_the_tools_of_the_engine_bridge",
		driver:  claudeLane{mode: "normal", listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeOfferedTools{as: "a"}),
	}, {
		name:    "claudecode_bridge_marks_exactly_the_read_only_tools_with_read_only_hint",
		driver:  claudeLane{mode: "normal", listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeReadOnlyTools{as: "a"}),
	}, {
		name:    "claudecode_bridge_loads_ask_user_question_and_the_file_and_shell_tools_upfront",
		driver:  claudeLane{mode: "normal", ask: true, listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeAlwaysLoadTools{as: "a"}),
	}, {
		name: "claudecode_bridge_passes_the_read_only_hint_of_an_mcp_tool_through",
		driver: claudeLane{mode: "hang_after_listing", listTools: true, initTools: "[]", extra: mcpSetup(nil, mcpServerDef{name: "notes", spec: harnesstest.MCPSpec{Name: "notes",
			Tools: []harnesstest.MCPTool{{Name: "find", Description: "Find a note", ReadOnly: true, Result: mcpText("none")},
				{Name: "add", Description: "Add a note", Result: mcpText("ok")}}}})(t, map[string]any{})}.newDriver,
		actions: []action{create{as: "a", allowed: []string{"mcp__notes__find", "mcp__notes__add"}}, submit{as: "a", text: "run it"}, claudeAwaitText{as: "a", text: "Working on it."},
			claudeReadOnlyTools{as: "a"}, interrupt{as: "a"}, waitIdle{as: "a"}},
	}, {
		name:    "claudecode_bridge_model_tool_offers_deferred_set_and_status",
		driver:  claudeLane{mode: "normal", listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeOfferedModelTool{as: "a"}),
	}, {
		name:    "claudecode_bridge_defers_set_on_the_model_tool",
		driver:  claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{{{"model", setArgs}}, {{"model", map[string]any{"action": "status"}}}}}.newDriver,
		actions: withActions(claudeOneTurn, requireSessionModel{as: "a", model: "anthropic/claude-fable-5"}, claudeJournalEvents{as: "a", prefix: "settings."}),
	}, {
		name:    "claudecode_plugin_hook_denies_a_bash_call_of_the_cli",
		driver:  claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{{{"bash", map[string]any{"command": "echo block-me"}}}}, extra: pluginConfig(t, nil)}.newDriver,
		actions: claudeOneTurn,
	}, {
		name:    "claudecode_bridge_serves_a_text_item_for_a_tool_with_no_output",
		driver:  claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{{{"bash", map[string]any{"command": "true"}}}}}.newDriver,
		actions: claudeOneTurn,
	}, {
		name: "claudecode_bridge_image_result_reaches_the_log_and_a_model_api_replay", prefixBreaks: foreignHistoryBreak(),
		driver: claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{{{"read_file", map[string]any{"path": "pic.png"}}}}}.newDriver,
		model:  []harnesstest.Step{{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}}},
		actions: append([]action{writeFile{path: "pic.png", body: pngBody()}},
			withActions(claudeOneTurn,
				setModel{as: "a", model: "anthropic/claude-fable-5"},
				submit{as: "a", text: "native"}, waitIdle{as: "a"})...),
	}, {
		name:    "claudecode_built_in_tool_that_returns_a_result_fails_the_turn",
		driver:  claudeLane{mode: "builtin_tool"}.newDriver,
		actions: withActions(claudeOneTurn, claudeSession{as: "a"}),
	}, {
		name:    "claudecode_refused_built_in_call_is_a_failed_tool_call_and_the_turn_goes_on",
		driver:  claudeLane{mode: "builtin_refused", turnCalls: [][]turnCall{{{"bash", map[string]any{"command": "echo from-harness"}}}}}.newDriver,
		actions: withActions(claudeOneTurn, claudeSession{as: "a"}),
	}, {
		name:    "claudecode_refused_call_to_an_unknown_tool_name_is_a_failed_tool_call_and_the_turn_goes_on",
		driver:  claudeLane{mode: "unknown_refused", turnCalls: [][]turnCall{{{"bash", map[string]any{"command": "echo from-harness"}}}}}.newDriver,
		actions: withActions(claudeOneTurn, claudeSession{as: "a"}),
	}, {
		name:    "claudecode_built_in_tool_that_ran_and_failed_fails_the_turn",
		driver:  claudeLane{mode: "builtin_ran_failed"}.newDriver,
		actions: withActions(claudeOneTurn, claudeSession{as: "a"}),
	}, {
		name:    "claudecode_init_frame_with_a_built_in_tool_fails_the_turn",
		driver:  claudeLane{mode: "normal", initTools: `["Bash"]`}.newDriver,
		actions: withActions(claudeOneTurn, claudeSession{as: "a"}),
	}, {
		name:    "claudecode_parallel_edit_file_calls_on_one_path_lose_no_edit",
		driver:  claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{editCalls(), {{"read_file", map[string]any{"path": "lines.txt"}}}}}.newDriver,
		actions: append([]action{writeFile{path: "lines.txt", body: numberedLines()}}, claudeOneTurn...),
	}, {
		name:    "claudecode_session_info_lists_the_tools_that_the_call_offered",
		driver:  claudeLane{mode: "normal", callTool: "session_info", callArgs: map[string]any{}}.newDriver,
		actions: withActions(claudeOneTurn, claudeToolCall{as: "a"}),
	}, {
		name: "claudecode_large_read_file_result_is_a_preview_with_a_handle_that_read_tool_result_reads",
		driver: claudeLane{mode: "turn_calls", turnCalls: [][]turnCall{{{"read_file", map[string]any{"path": "big.txt"}}},
			{{"read_tool_result", map[string]any{"handle": "$HANDLE", "offset": 1, "limit": 2}}}}}.newDriver,
		actions: append([]action{writeFile{path: "big.txt", body: strings.Repeat("line of the large file with some padding text\n", 3000)}}, claudeOneTurn...),
	}})
}

func pngBody() string {
	b, err := base64.StdEncoding.DecodeString(mcpPNG)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func editCalls() []turnCall {
	var calls []turnCall
	for i := range 16 {
		calls = append(calls, turnCall{"edit_file", map[string]any{"path": "lines.txt",
			"old_string": fmt.Sprintf("line-%d-old", i), "new_string": fmt.Sprintf("line-%d-new", i)}})
	}
	return calls
}

func numberedLines() string {
	var b strings.Builder
	for i := range 16 {
		fmt.Fprintf(&b, "line-%d-old\n", i)
	}
	return b.String()
}
