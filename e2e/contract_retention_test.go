package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// seq5000 writes 23893 bytes in 5000 lines, above the 16384 bytes that a
// tool result keeps inline.
const seq5000 = "seq 1 5000"

// bigFile writes 2000 lines of 2000 two-byte runes. read_file returns 8014892
// bytes of it, which alone exceed the retention budget of a session.
const bigFile = `l=$(printf 'é%.0s' $(seq 2000)); for i in $(seq 2000); do echo "$l"; done > big.txt`

func readResult(kv ...any) harnesstest.ToolCall { return ftTool("read_tool_result", ftArgs(kv...)) }

func TestContractToolResultRetention(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "tool_result_retention_keeps_a_preview_and_reads_it_back",
			model: toolChain(
				ftBash("printf 'TOKEN=abcdefgh1234\\n'; "+seq5000),
				readResult("handle", "trh_1", "offset", 5000, "limit", 5),
				readResult("handle", "trh_1", "search", "TOKEN"),
				readResult("handle", "trh_1", "offset", 1, "max_bytes", 300),
				readResult("handle", "trh_9"),
				readResult("handle", "trh_01"),
			),
			actions: oneTurn,
		},
		{
			name: "tool_result_over_the_session_budget_keeps_a_preview_with_a_notice",
			model: toolChain(
				ftBash(bigFile),
				ftRead(ftArgs("path", "big.txt")),
				ftBash(seq5000),
				ftBash("seq 1 3"),
			),
			actions: oneTurn,
		},
		{
			name: "read_tool_result_bounds_the_default_budget",
			model: toolChain(
				ftBash(seq5000),
				readResult("handle", "trh_1", "search", strings.Repeat("a", 20000)),
			),
			actions: oneTurn,
		},
	})
}

func TestContractToolResultAcrossTurns(t *testing.T) {
	bash := func(name, user string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_" + name, Name: "bash", Input: ftArgs("command", seq5000)},
		}}}
	}
	done := func(name, user string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: matchAll(harnesstest.LastUserText(user), harnesstest.LastToolResult("bash")), Reply: harnesstest.Reply{Text: "ok"}}
	}
	runScenarios(t, []scenario{{
		name:   "tool_result_handles_continue_across_turns_and_a_restart",
		config: map[string]any{"compaction_keep_turns": 1},
		model: []harnesstest.Step{
			bash("one", "one"), done("one_done", "one"),
			bash("two", "two"), done("two_done", "two"),
			{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "summary"}},
			{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "ok"}},
		},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "one"},
			waitIdle{as: "a"},
			restart{},
			submit{as: "a", text: "two"},
			waitIdle{as: "a"},
			compact{as: "a"},
			submit{as: "a", text: "three"},
			waitIdle{as: "a"},
		},
	}})
}

func TestContractToolResultPluginHook(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "tool_result_retention_keeps_the_plugin_hooked_result",
		config:  pluginConfig(t, nil),
		model:   toolChain(ftBash(seq5000)),
		actions: oneTurn,
	}})
}

func TestContractToolResultNeedsTheReader(t *testing.T) {
	runScenarios(t, []scenario{{
		name:       "a_child_without_read_tool_result_retains_nothing",
		concurrent: true,
		model: delegation("reader",
			harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{{ID: "toolu_big", Name: "read_file", Input: ftArgs("path", "big.txt")}}},
			harnesstest.Step{Name: "child_after", Match: harnesstest.LastToolResult("read_file"), Reply: harnesstest.Reply{Text: "child done"}}),
		actions: slices.Concat(
			[]action{
				writeFile{path: ".agents/reader.md", body: "---\nname: reader\ndescription: Reads.\ntools: read_file\n---\n\nRead files.\n"},
				writeFile{path: "big.txt", body: strings.Repeat("0123456789\n", 1700)},
			},
			spawnedAfter(5)),
	}})
}
