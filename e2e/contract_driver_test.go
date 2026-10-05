package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractDriver(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	twoTurns := []action{
		create{as: "a"},
		submit{as: "a", text: "one"},
		waitIdle{as: "a"},
		submit{as: "a", text: "two"},
		waitIdle{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:  "driver_settings_and_reads",
			model: []harnesstest.Step{text("one", "one", "1")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				setThinking{as: "a", level: "high"},
				setServiceTier{as: "a", tier: "priority"},
				setModel{as: "a", model: "anthropic/claude-fable-5"},
				setModel{as: "a", model: ""},
				getSession{as: "a"},
				listSessions{},
				sessionStatus{},
				messagesPage{as: "a", limit: 1},
				bootstrap{as: "a", limit: 1},
				journalPage{as: "a", from: 1, limit: 3},
			},
		},
		{
			name:   "driver_compact",
			config: map[string]any{"compaction_keep_turns": 1},
			model: []harnesstest.Step{
				text("one", "one", "1"),
				text("two", "two", "2"),
				{Name: "summary", Reply: harnesstest.Reply{Text: "gist"}},
			},
			actions: append(append([]action{}, twoTurns...),
				compact{as: "a"},
				getSession{as: "a"},
			),
		},
		{
			name:  "driver_resume_streams",
			model: []harnesstest.Step{text("one", "one", "1")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				sseResume{afterSeq: 0},
				sseResume{as: "a", afterSeq: 2, header: true},
				sseResume{as: "a", afterSeq: 2, scoped: true},
			},
		},
	})
}

func TestContractDriverQueue(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	slow := harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	runScenarios(t, []scenario{
		{
			name:  "driver_queue_goal_and_end",
			model: []harnesstest.Step{slow, agentStep("tail", "ok", true)},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				sendToSession{as: "a", text: "third"},
				deleteQueued{as: "a"},
				expectQueued{as: "a"},
				updateGoal{as: "a", condition: "x"},
				clearGoal{as: "a"},
				endSession{as: "a"},
				cancelTree{as: "a"},
				awaitCanceled{step: "slow"},
				waitIdle{as: "a"},
				endSession{as: "a"},
			},
		},
		{
			// Known defect, pinned: cancel_tree marks an idle root canceled and drops the child's queued send.
			name:       "driver_child_send_and_cancel",
			concurrent: true,
			model: []harnesstest.Step{
				{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "rest"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				getSession{as: "kid"},
				sendToSession{as: "kid", text: "more"},
				cancelTree{as: "a"},
				waitIdle{as: "kid"},
				getSession{as: "kid"},
				waitIdle{as: "a"},
			},
		},
		{
			name:    "driver_clean_restart",
			model:   []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, restart{}, getSession{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"}},
		},
	})
}
