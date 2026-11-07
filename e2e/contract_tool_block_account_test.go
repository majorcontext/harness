package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type accountToolBlockSSE struct{ toolBlockStream }

func (a accountToolBlockSSE) run(t *testing.T, r *run) {
	d := r.drv.(*accountProxyDriver)
	copy := *r
	copy.drv = d.laneHost
	toolBlockSSE(a).run(t, &copy)
}

func TestContractAccountToolBlockStream(t *testing.T) {
	call := harnesstest.ToolCall{ID: "call_1", Name: "no_such_tool", Input: map[string]any{"value": "test"}}
	runScenarios(t, []scenario{{
		name: "account_tool_block_sse", driver: newAccountProxyDriver,
		model: []harnesstest.Step{{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call}}}, {Name: "answer", Reply: harnesstest.Reply{Text: "done"}}},
		actions: []action{create{as: "a"}, setAccounts{as: "a", accounts: map[string]string{"codex": "acct_selected"}},
			accountToolBlockSSE{toolBlockStream{want: []string{"tool_block.started:call_1:no_such_tool", "tool_block.available:call_1:no_such_tool"}, inputs: map[string]map[string]any{"call_1": call.Input}}},
			recordProxyAccounts{expected: []string{"acct_selected"}}},
	}})
}
