package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractClaudeCodeAskingSessions(t *testing.T) {
	reader := "---\nname: reader\ndescription: Reads.\ntools: ls\nmodel: claude-code/sonnet\n---\n\nOnly read files.\n"
	runScenarios(t, []scenario{{
		name:   "claudecode_child_and_goal_sessions_ask_no_question",
		driver: claudeLane{mode: "fast_no_drain", ask: true, initTools: "[]"}.newDriver,
		model: []harnesstest.Step{
			evaluatorStep("judge", "MET: ok", true),
			taskStep("delegate", userStarts("delegate"), fixed(spawn("reader", "child work"))),
			{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
			{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
		},
		actions: []action{
			create{as: "root"},
			submit{as: "root", text: "run it"},
			waitIdle{as: "root"},
			create{as: "goal"},
			setGoal{as: "goal", condition: "say done"},
			waitIdle{as: "goal"},
			writeFile{path: ".agents/reader.md", body: reader},
			create{as: "parent", model: "anthropic/claude-fable-5"},
			submit{as: "parent", text: "delegate"},
			awaitRequests{n: 3},
			waitIdle{as: "parent"},
			bindChild{as: "kid", parent: "parent", record: true},
			claudeAwaitText{as: "kid", text: "Done before you finished writing."},
			awaitRequests{n: 4},
			waitIdle{as: "parent"},
			claudeInvocations{as: "root"},
		},
	}})
}
