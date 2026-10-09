package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

func mirrorFixtures(t *testing.T, names ...string) string {
	t.Helper()
	paths := make([]string, len(names))
	for i, name := range names {
		path, err := filepath.Abs(filepath.Join("..", "harnesstest", "fakeclaude", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		paths[i] = path
	}
	return strings.Join(paths, ",")
}

func TestContractClaudeCodeRestart(t *testing.T) {
	const working = "Working on it."
	started := []action{create{as: "a"}, submit{as: "a", text: "hi"}, claudeAwaitText{as: "a", text: working}}
	settled := []action{waitIdle{as: "a"}, claudeSession{as: "a"}, claudeInvocations{as: "a"}, claudeInputs{as: "a"}}
	runScenarios(t, []scenario{
		{
			name:    "claudecode_restart_mid_turn_stops_the_cli_and_resumes",
			driver:  claudeLane{mode: "thinking", spawnModes: "1=tool_on_interrupt", signals: true}.newDriver,
			actions: withActions(started, append([]action{restart{}}, append(settled, claudeSignals{as: "a"})...)...),
		},
		{
			name:   "claudecode_crash_mid_turn_waits_for_input",
			driver: claudeLane{mode: "thinking", spawnModes: "1=hang_after_text"}.newDriver,
			actions: withActions(started, restart{kill: true}, waitIdle{as: "a"}, claudeSession{as: "a"},
				submit{as: "a", text: "again"}, waitIdle{as: "a"}, claudeSession{as: "a"}, claudeInvocations{as: "a"}, claudeInputs{as: "a"}),
		},
	})
}

func TestContractClaudeCodeMirror(t *testing.T) {
	const working = "Working on it."
	run1, run2 := mirrorFixtures(t, "run1.stdout.jsonl"), mirrorFixtures(t, "run1.stdout.jsonl", "run2.stdout.jsonl")
	mirrorLane := func(fixtures, hangAfter string) func(*testing.T, host, string) driver {
		return claudeLane{mode: "mirror", mirror: true, env: map[string]string{
			"FAKE_CLAUDE_MIRROR_FIXTURE": fixtures, "FAKE_CLAUDE_MIRROR_HANG_AFTER": hangAfter,
		}}.newDriver
	}
	held := func(text string) []action {
		return []action{create{as: "a"}, submit{as: "a", text: text}, claudeAwaitText{as: "a", text: working}}
	}
	facts := []action{waitIdle{as: "a"}, claudeMirror{as: "a"}, claudeInputs{as: "a"}, claudeSession{as: "a"}}
	runScenarios(t, []scenario{
		{
			name:    "claudecode_mirror_continues_a_turn_that_the_cli_took",
			driver:  mirrorLane(run1, "3"),
			actions: withActions(held("hi"), append([]action{restart{}}, facts...)...),
		},
		{
			name:    "claudecode_mirror_restart_before_a_transcript_starts_the_turn_again",
			driver:  mirrorLane(run1, "0"),
			actions: withActions(held("hi"), append([]action{restart{}}, facts...)...),
		},
		{
			name:   "claudecode_mirror_resumes_after_a_restart",
			driver: mirrorLane(run2, ""),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "reply with ok"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "reply with ok again"}, waitIdle{as: "a"},
				submit{as: "a", text: "once more"}, waitIdle{as: "a"},
				claudeMirror{as: "a"}, claudeBackendStates{as: "a"},
			},
		},
		{
			name:    "claudecode_mirror_crash_before_a_transcript_starts_a_new_session",
			driver:  mirrorLane(run1, "1"),
			actions: withActions(held("reply with ok"), restart{kill: true}, waitIdle{as: "a"}, submit{as: "a", text: "again"}, waitIdle{as: "a"}, claudeMirror{as: "a"}, claudeSession{as: "a"}),
		},
	})
}

func TestContractClaudeCodeToolCallVisible(t *testing.T) {
	runScenarios(t, []scenario{{
		name:   "claudecode_running_tool_call_is_in_the_messages_before_its_result",
		driver: claudeLane{mode: "hang_in_tool", signals: true}.newDriver,
		actions: []action{
			create{as: "a"}, submit{as: "a", text: "hi"},
			claudeAwaitToolCall{as: "a", callID: "toolu_h"},
			messagesPage{as: "a"},
			interrupt{as: "a"}, waitIdle{as: "a"},
		},
	}})
}

func TestContractClaudeCodePlaceholder(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "claudecode_placeholder_result_before_the_turn_is_skipped",
		driver:  claudeLane{mode: "placeholder_before_turn"}.newDriver,
		actions: []action{create{as: "a"}, submit{as: "a", text: "hi"}, waitIdle{as: "a"}, claudeUsage{as: "a"}},
	}})
}

func TestContractClaudeCodeInterrupt(t *testing.T) {
	row := func(name, mode string, facts ...action) scenario {
		return scenario{
			name:   name,
			driver: claudeLane{mode: mode, signals: true}.newDriver,
			actions: withActions([]action{
				create{as: "a"}, submit{as: "a", text: "hi"}, claudeAwaitText{as: "a", text: "Working on it."},
				interrupt{as: "a"}, waitIdle{as: "a"}, claudeSignals{as: "a"},
			}, facts...),
		}
	}
	session := claudeSession{as: "a"}
	runScenarios(t, []scenario{
		row("claudecode_interrupt_keeps_the_usage_of_the_result_after_the_signal", "hang_after_text", session),
		row("claudecode_interrupt_closes_a_tool_call_that_the_cli_left_open", "hang_in_tool", session),
		row("claudecode_interrupt_closes_a_tool_call_that_the_cli_printed_on_the_signal", "tool_on_interrupt", session),
		row("claudecode_interrupt_keeps_a_tool_result_that_the_cli_printed_on_the_signal", "tool_result_on_interrupt", session),
		row("claudecode_interrupt_ends_completed_when_the_cli_finishes_on_the_signal", "success_on_interrupt", session),
		row("claudecode_interrupt_ignores_a_placeholder_result", "placeholder_on_interrupt", claudeUsage{as: "a"}),
		row("claudecode_interrupt_of_a_cli_that_exits_with_no_frame", "exit_on_interrupt", session),
	})
}
