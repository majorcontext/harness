package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

type awaitHeld struct {
	server string
	n      int
}

func (a awaitHeld) run(t *testing.T, r *run) {
	t.Helper()
	if !mcpFixture(t, r, a.server).AwaitHeld(a.n, waitBound) {
		t.Fatalf("server %s did not hold %d calls at once; the tool calls of the batch do not overlap\nstderr:\n%s", a.server, a.n, r.drv.Stderr())
	}
}

type awaitAbandoned struct {
	server string
	n      int
}

func (a awaitAbandoned) run(t *testing.T, r *run) {
	t.Helper()
	if !mcpFixture(t, r, a.server).AwaitAbandoned(a.n, waitBound) {
		t.Fatalf("server %s did not see %d held calls canceled\nstderr:\n%s", a.server, a.n, r.drv.Stderr())
	}
}

type awaitServed struct {
	server, tool string
}

func (a awaitServed) run(t *testing.T, r *run) {
	t.Helper()
	srv := mcpFixture(t, r, a.server)
	testpoll.Until(t, waitBound, "server never served "+a.tool, func() bool {
		return slices.ContainsFunc(srv.Calls(), func(c harnesstest.MCPCall) bool { return c.Method == "tools/call" && c.Name == a.tool })
	}, 15*time.Millisecond)
}

type releaseCall struct{ server, id string }

func (a releaseCall) run(t *testing.T, r *run) { mcpFixture(t, r, a.server).Release(a.id) }

func mcpFixture(t *testing.T, r *run, name string) *harnesstest.MCPServer {
	t.Helper()
	srv, ok := r.fx[name].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", name)
	}
	return srv
}

var gateServer = mcpServerDef{name: "gate", spec: harnesstest.MCPSpec{Name: "gate", Tools: []harnesstest.MCPTool{
	{Name: "hold", Description: "Waits for its release", Echo: true, Gated: true, ReadOnly: true},
}}}

var fastServer = mcpServerDef{name: "fast", spec: harnesstest.MCPSpec{Name: "fast", Tools: []harnesstest.MCPTool{
	{Name: "quick", Description: "Answers at once", Echo: true, ReadOnly: true},
}}}

func quickCall(id string) harnesstest.ToolCall {
	c := mcpTool("fast", "quick", "id", id)
	c.ID = "toolu_" + id
	return c
}

func holdCall(id string) harnesstest.ToolCall {
	c := mcpTool("gate", "hold", "id", id)
	c.ID = "toolu_" + id
	return c
}

func oneBatch(calls ...harnesstest.ToolCall) []harnesstest.Step {
	return []harnesstest.Step{
		{Name: "batch", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: calls}},
		{Name: "done", Match: assistantTurns(1), Reply: harnesstest.Reply{Text: "done"}},
	}
}

func TestContractParallelTools(t *testing.T) {
	gate, hold, batch := gateServer, holdCall, oneBatch
	runScenarios(t, []scenario{
		{
			name:  "two_slow_tool_calls_of_one_message_overlap_and_log_in_call_order",
			setup: mcpSetup(nil, gate),
			model: batch(hold("a"), hold("b")),
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 2},
				releaseCall{server: "gate", id: "b"},
				releaseCall{server: "gate", id: "a"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "interrupt_cancels_every_running_tool_call_of_a_batch",
			setup: mcpSetup(nil, gate),
			model: batch(hold("a"), hold("b"))[:1],
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 2},
				awaitCall{as: "a", callID: "toolu_b"},
				interrupt{as: "a"},
				awaitAbandoned{server: "gate", n: 2},
				awaitTurnEnd{as: "a"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "interrupt_records_the_result_of_a_call_that_finished_beside_a_held_one",
			setup: mcpSetup(nil, gate, fastServer),
			model: batch(hold("a"), quickCall("q"))[:1],
			actions: []action{
				create{as: "a"}, submit{as: "a", text: "go"},
				awaitHeld{server: "gate", n: 1},
				awaitServed{server: "fast", tool: "quick"},
				awaitCall{as: "a", callID: "toolu_q"},
				interrupt{as: "a"},
				awaitAbandoned{server: "gate", n: 1},
				awaitTurnEnd{as: "a"},
				waitIdle{as: "a"},
			},
		},
	})
}

func TestContractParallelToolsState(t *testing.T) {
	batch := oneBatch
	runScenarios(t, []scenario{
		{
			name: "retained_results_of_one_batch_keep_their_own_handles",
			model: []harnesstest.Step{
				{Name: "batch", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_a", Name: "bash", Input: ftArgs("command", "head -c 25000 /dev/zero | tr '\\0' A")},
					{ID: "toolu_b", Name: "bash", Input: ftArgs("command", "head -c 25000 /dev/zero | tr '\\0' B")},
				}}},
				{Name: "reads", Match: matchAll(assistantTurns(1), harnesstest.LastToolResult("bash")), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_ra", Name: "read_tool_result", Input: ftArgs("handle", "trh_1", "max_bytes", 300)},
					{ID: "toolu_rb", Name: "read_tool_result", Input: ftArgs("handle", "trh_2", "max_bytes", 300)},
				}}},
				{Name: "done", Match: assistantTurns(2), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: oneTurn,
		},
		{
			name: "calls_on_one_file_run_in_call_order_within_a_batch",
			model: []harnesstest.Step{
				{Name: "seed", Match: harnesstest.LastUserText("seed"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_seed", Name: "bash", Input: map[string]any{"command": "printf one > a.txt"}},
				}}},
				{Name: "batch", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_read", Name: "read_file", Input: map[string]any{"path": "a.txt"}},
					{ID: "toolu_write", Name: "write_file", Input: map[string]any{"path": "a.txt", "content": "two"}},
					{ID: "toolu_edit", Name: "edit_file", Input: map[string]any{"path": "a.txt", "old_string": "two", "new_string": "three"}},
					{ID: "toolu_read2", Name: "read_file", Input: map[string]any{"path": "a.txt"}},
					{ID: "toolu_other", Name: "write_file", Input: map[string]any{"path": "b.txt", "content": "b"}},
				}}},
				{Name: "done", Match: assistantTurns(2), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "seed"}, waitIdle{as: "a"}},
		},
		{
			name:   "plugin_hooks_run_for_each_call_of_a_batch",
			config: pluginConfig(t, nil),
			model: batch(
				harnesstest.ToolCall{ID: "toolu_rewrite", Name: "bash", Input: map[string]any{"command": "echo rewrite-me"}},
				harnesstest.ToolCall{ID: "toolu_block", Name: "bash", Input: map[string]any{"command": "echo block-me"}},
				harnesstest.ToolCall{ID: "toolu_plain", Name: "bash", Input: map[string]any{"command": "echo plain"}},
			),
			actions: oneTurn,
		},
	})
}
