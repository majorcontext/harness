package e2e

import (
	"testing"

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
			name:   "claudecode_compact_after_tokens_reads_zero",
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
			driver: claudeLaneDriver("queue_injection"),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run it"},
				claudeAwaitText{as: "a", text: "WAITING_FOR_QUEUE"},
				enqueue{as: "a", text: "second"},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
			},
		},
	})
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
			name:    "claudecode_child_report_waits_for_the_next_turn",
			driver:  claudeLaneDriver("child_report"),
			actions: reported(1),
		},
		{
			name:    "claudecode_child_reports_share_the_next_turn",
			driver:  claudeLaneDriver("child_reports"),
			actions: reported(2),
		},
		{
			name:    "claudecode_long_child_result_has_no_readable_handle",
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

func TestContractClaudeCodeMCPServers(t *testing.T) {
	servers := map[string]any{
		"chrome-devtools": map[string]any{"command": []string{"chrome-devtools-mcp-absent", "--headless"}, "env": []string{"A=1", "malformed"}, "dir": "/nonexistent"},
		"gateway":         map[string]any{"url": "http://127.0.0.1:1/mcp", "headers": map[string]string{"Authorization": "Bearer t"}},
	}
	runScenarios(t, []scenario{{
		name:    "claudecode_configured_mcp_servers_reach_the_cli",
		driver:  claudeLane{mode: "normal", mcp: servers}.newDriver,
		actions: withActions(claudeOneTurn, claudeMCPConfig{as: "a"}, claudeInvocations{as: "a"}),
	}})
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
		row("claudecode_subagent_frames_keep_parent", "subagent", claudeMessageParents{as: "a"}),
		row("claudecode_subagent_and_main_frames_keep_their_wire_order", "parallel_tools_crossing", claudeMessageParents{as: "a"}),
		row("claudecode_queued_notification_result_does_not_end_the_turn", "queued_empty_result", session),
		row("claudecode_compact_refuses_keep_turns", "normal", claudeCompactKeeping{as: "a", keep: 1}, session),
		row("claudecode_cli_compaction_is_logged", "compact_boundary", claudeJournalEvents{as: "a", prefix: "compaction."}, session),
		row("claudecode_error_result_fails_turn", "error", session, claudeInvocations{as: "a"}),
		row("claudecode_cli_exit_runs_once", "crash", session, claudeInvocations{as: "a"}),
		goalRow("claudecode_error_during_execution_pauses_a_goal_and_keeps_its_errors", "error"),
		goalRow("claudecode_credential_refusal_in_errors_fails_a_goal", "credential_error"),
		row("claudecode_rate_limit_event_reaches_subscription_usage", "rate_limit_event", session),
		row("claudecode_context_window_from_model_usage", "per_call_usage", session),
	})
}

func TestContractClaudeCodeHistory(t *testing.T) {
	native := harnesstest.Step{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}}
	runScenarios(t, []scenario{{
		name:   "claudecode_history_bridge_after_a_mid_turn_model_change",
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
		name:   "claudecode_history_bridge_after_a_turn_whose_cli_never_started",
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
		name:   "claudecode_history_bridge_after_native_turn",
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
	lane := claudeLane{mode: "question", ask: true}.newDriver
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
			name:   "claudecode_question_answer_run_takes_no_steer_input",
			driver: claudeLane{mode: "question_continues", ask: true}.newDriver,
			actions: withActions(parked,
				claudeAnswer{as: "a", callID: "toolu_q", answers: answers},
				claudeAwaitText{as: "a", text: "WAITING_FOR_QUEUE"},
				enqueue{as: "a", text: "second"},
				claudeCloseWindow{},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInputs{as: "a"},
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
	lane := claudeLane{mode: "question", ask: true}.newDriver
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
			name:   "claudecode_answered_call_with_no_result_gets_a_cut_off_result",
			driver: claudeLane{mode: "question_no_result", ask: true}.newDriver,
			actions: withActions(parked,
				claudeAnswer{as: "a", callID: "toolu_q", answers: map[string]string{"Which database?": "SQLite"}},
				waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
			),
		},
		{
			name:   "claudecode_question_sibling_call_gets_a_result_when_the_turn_parks",
			driver: claudeLane{mode: "question_sibling", ask: true}.newDriver,
			actions: withActions(parked,
				submit{as: "a", text: "never mind"}, waitIdle{as: "a"},
				claudeSession{as: "a"},
				claudeInvocations{as: "a"},
			),
		},
	})
}

func TestContractClaudeCodeTools(t *testing.T) {
	setModel := map[string]any{"action": "set", "model": "anthropic/claude-fable-5"}
	runScenarios(t, []scenario{{
		name:    "claudecode_cli_gets_the_tools_of_the_engine_bridge",
		driver:  claudeLane{mode: "normal", listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeOfferedTools{as: "a"}),
	}, {
		name:    "claudecode_bridge_model_tool_offers_list_only",
		driver:  claudeLane{mode: "normal", listTools: true}.newDriver,
		actions: withActions(claudeOneTurn, claudeOfferedModelTool{as: "a"}),
	}, {
		name:    "claudecode_bridge_refuses_set_on_the_model_tool",
		driver:  claudeLane{mode: "normal", callTool: "model", callArgs: setModel}.newDriver,
		actions: withActions(claudeOneTurn, claudeToolCall{as: "a"}),
	}})
}
