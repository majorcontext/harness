package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractBanner(t *testing.T) {
	answer := func(in string) harnesstest.Step {
		return harnesstest.Step{Name: in, Match: harnesstest.LastUserText(in), Reply: harnesstest.Reply{Text: "re " + in}}
	}
	summarized := func(r harnesstest.Request) bool {
		return strings.Contains(r.Messages[0].Parts[0].Text, "gist") && harnesstest.LastUserText("charlie")(r) && !harnesstest.LastToolResult("bash")(r)
	}
	runScenarios(t, []scenario{{
		name: "banner_holds_its_place_when_a_turn_compacts_in_the_middle",
		model: []harnesstest.Step{
			answer("alpha"), answer("bravo"),
			{Name: "overflow", Match: harnesstest.LastUserText("charlie"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
			{Name: "summary", Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: harnesstest.Reply{Text: "gist"}, Repeat: true},
			{Name: "call", Match: summarized, Repeat: true, Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
				{ID: "toolu_echo", Name: "bash", Input: map[string]any{"command": "echo banner"}},
			}}},
			{Name: "done", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}, Repeat: true},
		},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "alpha"}, waitIdle{as: "a"},
			submit{as: "a", text: "bravo"}, waitIdle{as: "a"},
			restart{},
			submit{as: "a", text: "charlie"}, waitIdle{as: "a"},
		},
	}})
}

func TestContractCompaction(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	summary := harnesstest.Step{Name: "summary", Reply: harnesstest.Reply{Text: "gist"}}
	keepOne := map[string]any{"compaction_keep_turns": 1}
	twoTurns := []action{
		create{as: "a"},
		submit{as: "a", text: "one"},
		waitIdle{as: "a"},
		submit{as: "a", text: "two"},
		waitIdle{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:   "compact_manual",
			config: keepOne,
			model:  []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3")},
			actions: append(append([]action{}, twoTurns...),
				compact{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "three"},
				waitIdle{as: "a"},
				messagesPage{as: "a"},
			),
		},
		{
			name:   "compact_survives_restart",
			config: keepOne,
			model:  []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3")},
			actions: append(append([]action{}, twoTurns...),
				compact{as: "a"},
				messagesPage{as: "a"},
				getSession{as: "a"},
				restart{},
				messagesPage{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "three"},
				waitIdle{as: "a"},
			),
		},
		{
			name:   "auto_compact_on_threshold",
			config: map[string]any{"context_window_tokens": 1000, "compaction_keep_turns": 1},
			model: []harnesstest.Step{
				text("one", "one", "1"),
				{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2", Usage: harnesstest.Usage{Input: 900, Output: 1}}},
				summary,
				text("three", "three", "3"),
				text("four", "four", "4"),
			},
			actions: append(append([]action{}, twoTurns...),
				getSession{as: "a"},
				submit{as: "a", text: "three"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				submit{as: "a", text: "four"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			),
		},
	})
}
