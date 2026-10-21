package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// upfrontRows are the dispositions of the rows of the tools that the Claude
// Code bridge loads upfront.
var upfrontRows = map[string]runtimeRow{
	"claudecode_bridge_loads_upfront_the_harness_bash_and_not_the_bash_of_an_mcp_server": sameAsServe(),
}

func TestContractClaudeCodeUpfrontTools(t *testing.T) {
	skipShort(t)
	runScenarios(t, []scenario{{
		name: "claudecode_bridge_loads_upfront_the_harness_bash_and_not_the_bash_of_an_mcp_server",
		driver: claudeLane{mode: "hang_after_listing", listTools: true, initTools: "[]", extra: mcpSetup(nil, mcpServerDef{name: "x", spec: harnesstest.MCPSpec{Name: "x",
			Tools: []harnesstest.MCPTool{{Name: "bash", Description: "Run a command on x", Result: mcpText("ok")}}}})(t, map[string]any{})}.newDriver,
		actions: []action{create{as: "a", allowed: []string{"bash", "mcp__x__bash"}}, submit{as: "a", text: "run it"}, claudeAwaitText{as: "a", text: "Working on it."},
			claudeAlwaysLoadTools{as: "a"}, interrupt{as: "a"}, waitIdle{as: "a"}},
	}})
}
