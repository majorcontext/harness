package e2e

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractChildren(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	runScenarios(t, []scenario{
		{
			name:       "task_child_result_reaches_parent",
			concurrent: true,
			model: []harnesstest.Step{
				{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("child done"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				// The child holds its reply until the parent's ack request exists, so the
				// result cannot ride on that request.
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 4},
				waitIdle{as: "a"},
			},
		},
		{
			name:       "child_error_delivered",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				// The child holds its tool call until the parent's ack request exists, so
				// the failure cannot ride on that request.
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."},
				}}}},
				{Name: "child_error", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "child request rejected"}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("A background task"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 5},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				getSession{as: "kid"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractChildrenBusyParent(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	runScenarios(t, []scenario{
		{
			name:       "child_report_reaches_a_busy_parent_at_the_tool_boundary",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				// The child holds its tool call until the parent's ack request exists, so
				// the child ends while the parent runs a tool.
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."}},
				}}},
				{Name: "child_done", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{Text: "child done"}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "sleep 0.3"}},
				}}},
				{Name: "after", Reply: harnesstest.Reply{Text: "parent done"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 4},
				release{step: "ack"},
				waitIdle{as: "a"},
			},
		},
		{
			name:       "child_error_reaches_a_busy_parent_at_the_tool_boundary",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."}},
				}}},
				{Name: "child_error", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "child request rejected"}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "sleep 0.3"}},
				}}},
				{Name: "after", Reply: harnesstest.Reply{Text: "parent done"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 4},
				release{step: "ack"},
				waitIdle{as: "a"},
			},
		},
	})
}

func TestContractChildReportText(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	busyAck := harnesstest.Step{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
		{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "sleep 0.3"}},
	}}}
	childLooks := harnesstest.Step{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
		{ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."}},
	}}}
	after := harnesstest.Step{Name: "after", Reply: harnesstest.Reply{Text: "parent done"}, Repeat: true}
	busyActions := []action{
		create{as: "a"},
		submit{as: "a", text: "delegate"},
		awaitRequests{n: 3},
		release{step: "child"},
		awaitRequests{n: 4},
		release{step: "ack"},
		waitIdle{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:       "child_usage_limit_reaches_a_busy_parent",
			concurrent: true,
			model: []harnesstest.Step{
				delegate, childLooks,
				{Name: "child_wall", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}},
				busyAck, after,
			},
			actions: busyActions,
		},
		{
			name:       "child_rate_limit_reaches_a_busy_parent",
			concurrent: true,
			config:     map[string]any{"prompt_retries": 0},
			model: []harnesstest.Step{
				delegate, childLooks,
				{Name: "child_wall", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: "slow down"}},
				busyAck, after,
			},
			actions: busyActions,
		},
		{
			name:       "child_long_result_reaches_a_busy_parent",
			concurrent: true,
			model: []harnesstest.Step{
				delegate, childLooks,
				{Name: "child_done", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{Text: strings.Repeat("long result ", 400)}},
				busyAck, after,
			},
			actions: busyActions,
		},
	})
}

func TestContractChildCrashReport(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	runScenarios(t, []scenario{
		{
			// A parent that a restart reopens runs its queued input, and the reopened child ends crashed into that busy turn.
			name:       "child_crash_reaches_a_busy_parent",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting", Block: true}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "rest"}, Repeat: true},
				{Name: "next", Match: harnesstest.LastUserText("next"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "sleep 0.3"}},
				}}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				enqueueNext{as: "a", text: "next"},
				bindChild{as: "kid", parent: "a", record: true},
				restart{kill: true},
				getSession{as: "a"},
				getSession{as: "kid"},
				release{step: "next"},
				waitIdle{as: "kid"},
				waitIdle{as: "a"},
				getSession{as: "kid"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractChildrenControl(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	runScenarios(t, []scenario{
		{
			// Known defect, pinned: after SIGKILL the running child reloads idle and the parent loses its children.
			name:       "child_crash_recovered",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
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
				restart{kill: true},
				getSession{as: "kid"},
				waitIdle{as: "kid"},
				getSession{as: "a"},
			},
		},
		{
			// Known defect, pinned: cancel_tree marks an idle root canceled and drops the child's queued send.
			name:       "send_to_child_and_cancel_tree",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "rest"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				sendToSession{as: "kid", text: "more"},
				cancelTree{as: "a"},
				waitIdle{as: "kid"},
				getSession{as: "kid"},
				getSession{as: "a"},
				sendToSession{as: "kid", text: "again"},
				waitIdle{as: "kid"},
				getSession{as: "kid"},
			},
		},
	})
}

func TestContractChildUsageLimit(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	runScenarios(t, []scenario{
		{
			name:       "child_usage_limit_delivered",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."},
				}}}},
				{Name: "child_wall", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("A background task"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 5},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				getSession{as: "kid"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractChildrenEnd(t *testing.T) {
	model := []harnesstest.Step{
		{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
			ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
		}}}},
		{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
		{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
		{Name: "rest", Reply: harnesstest.Reply{Text: "rest"}, Repeat: true},
	}
	endIdleParent := []action{
		create{as: "a"},
		submit{as: "a", text: "delegate"},
		awaitRequests{n: 3},
		waitIdle{as: "a"},
		bindChild{as: "kid", parent: "a", record: true, staysActive: true},
		endSession{as: "a"},
	}
	runScenarios(t, []scenario{
		{
			name:       "end_idle_parent_cancels_running_child",
			concurrent: true,
			model:      model,
			actions:    append(slices.Clone(endIdleParent), getSession{as: "kid"}, getSession{as: "a"}),
		},
		{
			name:       "end_then_send_runs_no_report_of_the_stopped_child",
			concurrent: true,
			model:      model,
			actions:    append(slices.Clone(endIdleParent), submit{as: "a", text: "again"}, waitIdle{as: "a"}, getSession{as: "a"}),
		},
	})
}

// The engine refuses input to a session that its DELETE canceled, so no serve
// golden holds these rows: they run on the runtime host only. A stop that
// the end walk made sends no report; any other stop still reports canceled.
func TestContractRuntimeChildReportsAfterEnd(t *testing.T) {
	skipShort(t)
	if os.Getenv(runtimeEnv) == "" {
		return
	}
	call := func(id, prompt string) harnesstest.ToolCall {
		return harnesstest.ToolCall{ID: id, Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": prompt}}
	}
	blocked := func(name, text string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(text), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	}
	ack := harnesstest.Step{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}, Repeat: true}
	rest := harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "rest"}, Repeat: true}
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call("toolu_1", "child work")}}}
	endA := []action{
		create{as: "a"},
		submit{as: "a", text: "delegate"},
		awaitRequests{n: 3},
		waitIdle{as: "a"},
		bindChild{as: "kid", parent: "a", record: true, staysActive: true},
		endSession{as: "a"},
	}
	for _, tc := range []struct {
		name    string
		model   []harnesstest.Step
		actions []action
		session string
		reports bool
	}{
		{
			name:  "a direct interrupt after the end reports canceled to the closed parent",
			model: []harnesstest.Step{delegate, blocked("child", "child work"), ack, blocked("more", "more"), rest},
			actions: append(slices.Clone(endA),
				submit{as: "kid", text: "more"}, awaitRequests{n: 4}, interrupt{as: "kid"}, awaitRequests{n: 5}),
			session: "a",
			reports: true,
		},
		{
			name: "a tree interrupt after the end settles the closed parent with no report input",
			model: []harnesstest.Step{delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call("toolu_2", "grand work")}}},
				blocked("grand", "grand work"), ack, blocked("more", "more"), rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 6},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				bindChild{as: "grand", parent: "kid", record: true, staysActive: true},
				endSession{as: "kid"},
				submit{as: "grand", text: "more"},
				awaitRequests{n: 7},
				cancelTree{as: "a"},
				submit{as: "kid", text: "hello"},
			},
			session: "kid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := runScenario(t, scenario{concurrent: true, model: tc.model, actions: tc.actions}, runtimeHost)
			var reported bool
			for _, m := range obs.Sessions[tc.session] {
				for _, p := range m.Parts {
					reported = reported || m.Role == "user" && strings.Contains(p.Text, "outcome: canceled")
				}
			}
			if reported != tc.reports {
				t.Errorf("%s holds a canceled report = %v, want %v: %+v", tc.session, reported, tc.reports, obs.Sessions[tc.session])
			}
		})
	}
}
