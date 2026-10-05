package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractTaskActions(t *testing.T) {
	general := "general-purpose"
	runScenarios(t, []scenario{
		{
			name:       "task_status_and_log_of_a_settled_child",
			concurrent: true,
			model: delegation(general, childDone, taskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any {
				return []map[string]any{onSession("status", kid), onSession("log", kid), onSession("log", kid, "tail", 1)}
			}))),
			actions: slices.Concat(spawned, []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}}),
		},
		{
			name:       "task_status_and_log_of_a_failed_child",
			concurrent: true,
			model: []harnesstest.Step{
				taskStep("delegate", userStarts("delegate"), fixed(spawn(general, "child work"))),
				{Name: "child", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_ls", Name: "ls", Input: map[string]any{"path": "."}},
				}}},
				{Name: "child_end", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "request rejected api_key=abcd1234efgh5678 " + strings.Repeat("detail ", 100)}},
				{Name: "ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
				taskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("status", kid), onSession("log", kid, "tail", 2)}
				})),
				{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			},
			actions: slices.Concat(spawnedAfter(5), []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}}),
		},
		{
			name:       "task_cancel_and_send_to_a_running_child",
			concurrent: true,
			model: delegation(general, childRuns, busyTaskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any {
				return []map[string]any{onSession("send", kid, "prompt", "more work"), onSession("cancel", kid)}
			}))),
			actions: slices.Concat(spawnedRunning, []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}, waitIdle{as: "kid"}}),
		},
		{
			name:       "task_spawn_past_max_tree_tokens_is_refused",
			concurrent: true,
			driver:     limited("max_tree_tokens", "HARNESS_MAX_TREE_TOKENS", 10),
			model:      delegation(general, childDone, taskStep("again", userStarts("again"), fixed(spawn(general, "second work")))),
			actions:    slices.Concat(spawned, []action{submit{as: "a", text: "again"}, waitIdle{as: "a"}}),
		},
	})
}

func TestContractTaskSend(t *testing.T) {
	general := "general-purpose"
	runScenarios(t, []scenario{
		{
			name:       "task_send_runs_a_settled_child_again",
			concurrent: true,
			model: delegation(general, childRuns,
				taskStep("queue", userStarts("queue it"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "queued work")}
				})),
				harnesstest.Step{Name: "queued", Match: userStarts("queued work"), Reply: harnesstest.Reply{Text: "queued done"}},
				taskStep("fresh", userStarts("again"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "fresh work")}
				})),
				harnesstest.Step{Name: "fresh_child", Match: userStarts("fresh work"), Reply: harnesstest.Reply{Text: "fresh done", Block: true}}),
			actions: slices.Concat(spawnedRunning, []action{
				submit{as: "a", text: "queue it"},
				awaitRequests{n: 5},
				waitIdle{as: "a"},
				release{step: "child"},
				awaitRequests{n: 7},
				waitIdle{as: "a"},
				submit{as: "a", text: "again"},
				awaitRequests{n: 10},
				waitIdle{as: "a"},
				release{step: "fresh_child"},
				awaitRequests{n: 11},
				waitIdle{as: "a"},
				waitIdle{as: "kid"},
			}),
		},
		{
			name:       "task_two_sends_to_a_settled_child_need_one_slot",
			concurrent: true,
			driver:     limited("max_concurrent_tasks", "HARNESS_MAX_CONCURRENT_TASKS", 1),
			model: delegation(general, childDone,
				taskStep("sends", userStarts("two sends"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "more one"), onSession("send", kid, "prompt", "more two")}
				})),
				harnesstest.Step{Name: "more_one", Match: userStarts("more one"), Reply: harnesstest.Reply{Text: "one done", Block: true}},
				harnesstest.Step{Name: "more_two", Match: userStarts("more two"), Reply: harnesstest.Reply{Text: "two done", Block: true}}),
			actions: slices.Concat(spawned, []action{
				submit{as: "a", text: "two sends"},
				awaitRequests{n: 7},
				waitIdle{as: "a"},
				release{step: "more_one"},
				awaitRequests{n: 8},
				release{step: "more_two"},
				awaitRequests{n: 9},
				waitIdle{as: "a"},
				waitIdle{as: "kid"},
			}),
		},
	})
}

func TestContractTaskRefusedActions(t *testing.T) {
	general := "general-purpose"
	refuse := taskStep("refuse", matchAll(rootStarts("delegate"), lastResultHas(`"parent_id"`)), func(r harnesstest.Request) []map[string]any {
		kid := kidOf(r)
		return []map[string]any{
			onSession("status", resultID(r, parentIDPattern)),
			onSession("log", "ses_nope"),
			ftArgs("action", "status"),
			onSession("log", kid, "tail", -1),
			onSession("stop", kid),
		}
	})
	runScenarios(t, []scenario{{
		name:       "task_action_refusals",
		concurrent: true,
		model: delegation(general, childDone,
			taskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any { return []map[string]any{onSession("status", kid)} })),
			refuse),
		actions: slices.Concat(spawned, []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}}),
	}})
}
