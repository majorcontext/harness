package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func summaryRequest(r harnesstest.Request) bool {
	return harnesstest.SystemContains("You are summarizing a prefix")(r)
}

func usedText(user string, in int) harnesstest.Step {
	return harnesstest.Step{Name: user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "re " + user, Usage: harnesstest.Usage{Input: in, Output: 1}}}
}

var (
	overThreshold = map[string]any{"context_window_tokens": 1000, "compaction_keep_turns": 1}
	twoTurns      = []action{
		create{as: "a"},
		submit{as: "a", text: "one"}, waitIdle{as: "a"},
		submit{as: "a", text: "two"}, waitIdle{as: "a"},
	}
)

func TestContractSessionAutoCompaction(t *testing.T) {
	text := usedText
	runScenarios(t, []scenario{
		{
			name:   "auto_compaction_with_a_failed_summary_keeps_the_history",
			config: overThreshold,
			model: []harnesstest.Step{
				text("one", 5), text("two", 900),
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "summary failed"}},
				text("three", 5),
			},
			actions: withActions(twoTurns, submit{as: "a", text: "three"}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:   "a_negative_compaction_threshold_is_the_default_threshold",
			config: map[string]any{"context_window_tokens": 1000, "compaction_keep_turns": 1, "compaction_threshold": -1},
			model: []harnesstest.Step{
				text("one", 5), text("two", 500), text("three", 900),
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				text("four", 5),
			},
			actions: withActions(twoTurns,
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
		{
			name:   "auto_compaction_uses_the_window_of_the_session_model",
			config: map[string]any{"context_window_tokens": 0, "compaction_keep_turns": 1},
			model: []harnesstest.Step{
				text("one", 300_000), text("two", 300_000),
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}, Repeat: true},
				text("three", 5),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				setModel{as: "a", model: "anthropic/claude-haiku-4-5"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractSessionAutoCompactionGuards(t *testing.T) {
	text := usedText
	runScenarios(t, []scenario{
		{
			name:   "auto_compaction_estimates_the_context_when_no_call_reports_prompt_tokens",
			config: overThreshold,
			model: []harnesstest.Step{
				{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: strings.Repeat("x", 4000), Usage: harnesstest.Usage{Output: 1}}},
				{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "re two", Usage: harnesstest.Usage{Output: 1}}},
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "re three"}},
			},
			actions: withActions(twoTurns, submit{as: "a", text: "three"}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name:   "auto_compaction_estimates_the_context_when_the_newest_turn_reports_no_prompt_tokens",
			config: overThreshold,
			model: []harnesstest.Step{
				text("one", 5),
				{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: strings.Repeat("x", 4000), Usage: harnesstest.Usage{Output: 1}}},
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "re three"}},
			},
			actions: withActions(twoTurns, submit{as: "a", text: "three"}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
		{
			name: "auto_compaction_runs_again_after_a_model_change_moves_the_window", prefixBreaks: compactionBreak(),
			config: map[string]any{"context_window_tokens": 0, "compaction_keep_turns": 1},
			model: []harnesstest.Step{
				text("one", 5), text("two", 900_000),
				{Name: "summary1", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				text("three", 900_000),
				{Name: "summary2", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				text("four", 5),
			},
			actions: withActions(twoTurns,
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				setModel{as: "a", model: "anthropic/claude-haiku-4-5"},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
		{
			name: "auto_compaction_waits_for_the_reading_to_fall_before_it_runs_again", prefixBreaks: compactionBreak(),
			config: overThreshold,
			model: []harnesstest.Step{
				text("one", 5), text("two", 900),
				{Name: "summary1", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				text("three", 900), text("four", 5), text("five", 900),
				{Name: "summary2", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				text("six", 5),
			},
			actions: withActions(twoTurns,
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				submit{as: "a", text: "five"}, waitIdle{as: "a"},
				submit{as: "a", text: "six"}, waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
		{
			name:   "auto_compaction_waits_for_the_reading_to_fall_after_an_empty_summary",
			config: overThreshold,
			model: []harnesstest.Step{
				text("one", 5), text("two", 900),
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: " "}},
				text("three", 900), text("four", 5),
			},
			actions: withActions(twoTurns,
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
	})
}

func TestContractSessionStoppedCompaction(t *testing.T) {
	text := usedText
	runScenarios(t, []scenario{
		{
			name:   "restart_during_auto_compaction_runs_the_queued_input_on_the_next_owner",
			config: overThreshold,
			model: []harnesstest.Step{
				text("one", 5), text("two", 900),
				{Name: "held", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist", Block: true}},
				{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}, Repeat: true},
				{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "re three"}, Repeat: true},
			},
			actions: withActions(twoTurns,
				submit{as: "a", text: "three"},
				awaitRequests{n: 3},
				restart{},
				waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
		{
			name:   "interrupt_stops_an_auto_compaction",
			config: overThreshold,
			model: []harnesstest.Step{
				text("one", 5), text("two", 900),
				{Name: "held", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist", Block: true}},
				{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "re three"}, Repeat: true},
			},
			actions: withActions(twoTurns,
				submit{as: "a", text: "three"},
				awaitRequests{n: 3},
				compact{as: "a"},
				interrupt{as: "a"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
	})
}

func TestContractSessionContextOverflow(t *testing.T) {
	answer := func(in string) harnesstest.Step {
		return harnesstest.Step{Name: in, Match: harnesstest.LastUserText(in), Reply: harnesstest.Reply{Text: "re " + in}}
	}
	overflow := func(name string, repeat bool) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText("charlie"), Repeat: repeat,
			Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}}
	}
	summary := harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}, Repeat: true}
	keepOne := map[string]any{"compaction_keep_turns": 1}
	threeTurns := []action{
		create{as: "a"},
		submit{as: "a", text: "alpha"}, waitIdle{as: "a"},
		submit{as: "a", text: "bravo"}, waitIdle{as: "a"},
		submit{as: "a", text: "charlie"}, waitIdle{as: "a"},
		getSession{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:   "context_overflow_compacts_and_runs_the_turn_again",
			config: keepOne,
			model: []harnesstest.Step{
				answer("alpha"), answer("bravo"), overflow("overflow", false), summary,
				{Name: "charlie", Match: harnesstest.LastUserText("charlie"), Reply: harnesstest.Reply{Text: "re charlie"}, Repeat: true},
			},
			actions: threeTurns,
		},
		{
			name:    "context_overflow_after_the_compaction_fails_the_turn",
			config:  keepOne,
			model:   []harnesstest.Step{answer("alpha"), answer("bravo"), overflow("overflow", false), summary, overflow("again", true)},
			actions: threeTurns,
		},
		{
			name:   "a_stalled_summary_fails_the_overflowed_turn",
			config: map[string]any{"compaction_keep_turns": 1, "stream_idle_timeout_s": 1, "stream_max_retries": 0},
			model: []harnesstest.Step{
				answer("alpha"), answer("bravo"), overflow("overflow", false),
				{Name: "stall", Match: summaryRequest, Reply: harnesstest.Reply{Text: "partial", Block: true}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "alpha"}, waitIdle{as: "a"},
				submit{as: "a", text: "bravo"}, waitIdle{as: "a"},
				submit{as: "a", text: "charlie"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}

var midTurnCompactionRows = map[string]runtimeRow{
	"a_turn_that_passes_the_threshold_compacts_before_its_next_model_call":           sameAsServe(),
	"context_overflow_in_a_single_turn_folds_the_turn_and_calls_the_model_again":     sameAsServe(),
	"a_summary_that_overflows_is_retried_without_its_oldest_message_after_the_first": sameAsServe(),
}

func TestContractSessionMidTurnCompaction(t *testing.T) {
	echo := func(n int, id, word string, in int) harnesstest.Step {
		return harnesstest.Step{Name: id, Match: func(r harnesstest.Request) bool { return !summaryRequest(r) && assistantTurns(n)(r) },
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: "bash", Input: map[string]any{"command": "echo " + word}}},
				Usage: harnesstest.Usage{Input: in, Output: 1}}}
	}
	has := func(r harnesstest.Request, kind, text, id string) bool {
		for _, m := range r.Messages {
			for _, p := range m.Parts {
				if p.Kind == kind && strings.Contains(p.Text, text) && p.ToolUseID == id {
					return true
				}
			}
		}
		return false
	}
	foldedTo := func(kept string) harnesstest.Matcher {
		return func(r harnesstest.Request) bool {
			return !summaryRequest(r) && strings.Contains(r.Messages[0].Parts[0].Text, "gist") && has(r, "text", "go", "") &&
				has(r, "tool_use", "", kept) && !has(r, "tool_use", "", "toolu_1")
		}
	}
	folded := foldedTo("toolu_2")
	summary := harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}, Repeat: true}
	done := harnesstest.Step{Name: "done", Match: folded, Reply: harnesstest.Reply{Text: "done"}}
	overflow := harnesstest.Step{Name: "overflow", Match: func(r harnesstest.Request) bool { return !summaryRequest(r) && assistantTurns(2)(r) },
		Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}}
	oneTurn := []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, getSession{as: "a"}, messagesPage{as: "a"}}
	runScenarios(t, []scenario{
		{
			name:    "a_turn_that_passes_the_threshold_compacts_before_its_next_model_call",
			config:  overThreshold,
			model:   []harnesstest.Step{echo(0, "toolu_1", "one", 10), echo(1, "toolu_2", "two", 900), summary, done},
			actions: oneTurn,
		},
		{
			name:    "context_overflow_in_a_single_turn_folds_the_turn_and_calls_the_model_again",
			config:  map[string]any{"compaction_keep_turns": 1},
			model:   []harnesstest.Step{echo(0, "toolu_1", "one", 10), echo(1, "toolu_2", "two", 10), overflow, summary, done},
			actions: oneTurn,
		},
		{
			name:   "a_summary_that_overflows_is_retried_without_its_oldest_message_after_the_first",
			config: map[string]any{"compaction_keep_turns": 1},
			model: []harnesstest.Step{echo(0, "toolu_1", "one", 10), echo(1, "toolu_2", "two", 10), echo(2, "toolu_3", "three", 10),
				{Name: "overflow", Match: func(r harnesstest.Request) bool { return !summaryRequest(r) && assistantTurns(3)(r) },
					Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
				{Name: "summary overflow", Match: func(r harnesstest.Request) bool { return summaryRequest(r) && has(r, "tool_use", "", "toolu_1") },
					Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
				{Name: "summary", Match: func(r harnesstest.Request) bool {
					return summaryRequest(r) && !has(r, "tool_use", "", "toolu_1") && r.Messages[0].Role == "user" && has(r, "text", "go", "")
				}, Reply: harnesstest.Reply{Text: "gist"}},
				{Name: "done", Match: foldedTo("toolu_3"), Reply: harnesstest.Reply{Text: "done"}}},
			actions: oneTurn,
		},
	})
}

func TestContractSessionBusyCommands(t *testing.T) {
	runScenarios(t, []scenario{{
		name: "compact_during_a_turn_is_session_busy",
		model: []harnesstest.Step{
			{Name: "slow", Reply: harnesstest.Reply{Text: "working", Block: true}},
		},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "first"},
			awaitRequests{n: 1},
			compact{as: "a"},
			release{step: "slow"},
			waitIdle{as: "a"},
			getSession{as: "a"},
		},
	}})
}
