package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

type awaitEarlyAccountTool struct{}

func (awaitEarlyAccountTool) run(t *testing.T, r *run) {
	t.Helper()
	t.Cleanup(func() { r.fake.Release("call:end") })
	if !r.fake.AwaitBlocked("call:end", waitBound) {
		t.Fatal("model did not reach stream gate")
	}
	testpoll.Until(t, waitBound, "tool did not execute while the model stream was open", func() bool {
		_, err := os.Stat(filepath.Join(r.drv.Workdir(), "early-marker"))
		return err == nil
	}, 15*time.Millisecond)
	r.fake.Release("call:end")
}

func TestContractAccountEarlyTool(t *testing.T) {
	tool := ftWrite("early-marker", "done")
	tool.ID = "call_1"
	call := harnesstest.Step{Name: "call", Reply: harnesstest.Reply{EndBlock: true, ToolCalls: []harnesstest.ToolCall{tool}}}
	done := harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "done"}}
	runScenarios(t, []scenario{
		{name: "account_early_tool", driver: newAccountProxyDriver, model: []harnesstest.Step{call, done}, actions: []action{create{as: "a"}, setAccounts{as: "a", accounts: map[string]string{"codex": "acct_selected"}}, submit{as: "a", text: "go"}, awaitEarlyAccountTool{}, waitIdle{as: "a"}, recordProxyAccounts{expected: []string{"acct_selected"}}}},
		{name: "unqualified_early_tool", driver: newAccountProxyDriver, model: []harnesstest.Step{call, done}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, awaitEarlyAccountTool{}, waitIdle{as: "a"}, recordProxyAccounts{unqualified: true}}},
	})
}
