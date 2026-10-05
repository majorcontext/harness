package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractReplayReads(t *testing.T) {
	toolTurn := []harnesstest.Step{
		{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
			ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo replay"},
		}}}},
		{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ran"}},
	}
	slow := harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	reads := func(as string) []action {
		return []action{
			listSessions{},
			getSession{as: as},
			sessionStatus{},
			messagesPage{as: as, limit: 10},
			bootstrap{as: as},
			journalPage{as: as},
		}
	}
	cat := func(parts ...[]action) []action {
		var out []action
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	runScenarios(t, []scenario{
		{
			name:  "replay_after_kill_full_transcript",
			model: append(append([]harnesstest.Step{}, toolTurn...), slow),
			actions: cat(
				[]action{create{as: "a"}, submit{as: "a", text: "run"}, waitIdle{as: "a"}},
				reads("a"),
				[]action{submit{as: "a", text: "again"}, awaitRequests{n: 3}, restart{kill: true}},
				reads("a"),
			),
		},
		{
			name:  "sse_resume_cursor",
			model: append(append([]harnesstest.Step{}, toolTurn...), slow),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run"},
				waitIdle{as: "a"},
				submit{as: "a", text: "again"},
				awaitRequests{n: 3},
				sseResume{afterSeq: 4},
				sseResume{afterSeq: 4, header: true},
				sseResume{as: "a", afterSeq: 4, scoped: true},
				release{step: "slow"},
				waitIdle{as: "a"},
				sseResume{afterSeq: 4, header: true},
			},
		},
	})
}

func TestContractReplayReadsB(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	threeTurns := []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), text("three", "three", "3")}
	threeTurnActions := []action{
		create{as: "a"},
		submit{as: "a", text: "one"},
		waitIdle{as: "a"},
		submit{as: "a", text: "two"},
		waitIdle{as: "a"},
		submit{as: "a", text: "three"},
		waitIdle{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:  "messages_page_windows",
			model: threeTurns,
			actions: append(append([]action{}, threeTurnActions...),
				messagesPage{as: "a", limit: 2},
				messagesPage{as: "a", limit: 2, beforeSeq: 5},
				messagesPage{as: "a", limit: 2, beforeSeq: 3},
				messagesPage{as: "a", limit: 2, beforeSeq: 1},
				messagesPage{as: "a", limit: 2, beforeSeq: 100},
				messagesPage{as: "a", limit: 1001},
				messagesPage{as: "a", beforeSeq: 4},
			),
		},
		{
			name:  "bootstrap_cold_then_resident_windows",
			model: threeTurns,
			actions: append(append([]action{}, threeTurnActions...),
				restart{},
				bootstrap{as: "a", limit: 2},
				bootstrap{as: "a", limit: 1000},
				bootstrap{as: "a"},
				bootstrap{as: "a", limit: 2},
				bootstrap{as: "a", limit: 1001}, // possible defect: accepted here, while messages_page rejects 1001 with 400
			),
		},
		{
			name:  "bootstrap_cold_window_after_kill",
			model: threeTurns,
			actions: append(append([]action{}, threeTurnActions...),
				restart{kill: true},
				bootstrap{as: "a", limit: 2},
				messagesPage{as: "a", limit: 2},
				messagesPage{as: "a", limit: 2, beforeSeq: 5},
				bootstrap{as: "a", limit: 2},
			),
		},
		{
			// Possible defect: stream_from differs between the resident and the cold bootstrap.
			name:   "messages_page_after_compaction",
			config: map[string]any{"compaction_keep_turns": 1},
			model:  append(append([]harnesstest.Step{}, threeTurns...), harnesstest.Step{Name: "summary", Reply: harnesstest.Reply{Text: "gist"}}),
			actions: append(append([]action{}, threeTurnActions...),
				compact{as: "a"},
				messagesPage{as: "a", limit: 1},
				messagesPage{as: "a", limit: 2, beforeSeq: 6},
				messagesPage{as: "a", limit: 10},
				bootstrap{as: "a", limit: 2},
				journalPage{as: "a", from: 10},
				restart{},
				messagesPage{as: "a", limit: 10},
				bootstrap{as: "a", limit: 2},
			),
		},
	})
}

func TestContractReplayReadsC(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	threeTurns := []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), text("three", "three", "3")}
	threeTurnActions := []action{
		create{as: "a"},
		submit{as: "a", text: "one"},
		waitIdle{as: "a"},
		submit{as: "a", text: "two"},
		waitIdle{as: "a"},
		submit{as: "a", text: "three"},
		waitIdle{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:  "sse_resume_after_kill",
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				create{as: "b"},
				submit{as: "b", text: "two"},
				waitIdle{as: "b"},
				restart{kill: true},
				sseResume{afterSeq: 0},
				sseResume{as: "b", afterSeq: 6, header: true},
				sseResume{as: "b", afterSeq: 6, scoped: true},
			},
		},
		{
			name:  "journal_pages_follow_cursor",
			model: threeTurns,
			actions: append(append([]action{}, threeTurnActions...),
				journalPage{as: "a", limit: 3},
				journalPage{as: "a", from: 3, limit: 3},
				journalPage{as: "a", from: 6, limit: 3},
				journalPage{as: "a", from: 9, limit: 3},
				journalPage{as: "a", from: 1000},
				restart{},
				journalPage{as: "a", from: 6, limit: 2},
			),
		},
		{
			name: "list_sessions_in_creation_order",
			actions: []action{
				create{as: "a", staysActive: true}, create{as: "b", staysActive: true}, create{as: "c", staysActive: true},
				create{as: "d", staysActive: true}, create{as: "e", staysActive: true}, create{as: "f", staysActive: true},
				listSessions{},
				restart{},
				listSessions{},
			},
		},
		{
			name:  "status_and_list_cold_after_restart",
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				create{as: "b"},
				submit{as: "b", text: "two"},
				waitIdle{as: "b"},
				restart{},
				sessionStatus{},
				listSessions{},
				getSession{as: "b"},
				sessionStatus{},
			},
		},
	})
}
