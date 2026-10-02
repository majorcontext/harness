package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

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
