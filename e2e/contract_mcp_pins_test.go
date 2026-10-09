package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractMCPNoticeAfterCompaction(t *testing.T) {
	answered := func(tool string) harnesstest.Matcher {
		return func(r harnesstest.Request) bool {
			for i := len(r.Messages) - 1; i >= 0 && r.Messages[i].Role != "assistant"; i-- {
				if harnesstest.LastToolResult(tool)(harnesstest.Request{Messages: r.Messages[i : i+1]}) {
					return true
				}
			}
			return false
		}
	}
	prompt := func(text string) harnesstest.Matcher {
		return func(r harnesstest.Request) bool {
			return harnesstest.LastUserText(text)(r) && !answered("mcp")(r) && !answered("mcp__weather__forecast")(r)
		}
	}
	call := func(id string, c harnesstest.ToolCall) []harnesstest.ToolCall {
		c.ID = id
		return []harnesstest.ToolCall{c}
	}
	forecast := mcpTool("weather", "forecast", "city", "Oslo")
	model := []harnesstest.Step{
		{Name: "connect", Match: prompt("go"), Reply: harnesstest.Reply{ToolCalls: call("toolu_connect", mcpAction("action", "connect", "server", "weather"))}},
		{Name: "connected", Match: answered("mcp"), Reply: harnesstest.Reply{Text: "connected"}},
		{Name: "first forecast", Match: prompt("bravo"), Reply: harnesstest.Reply{ToolCalls: call("toolu_first", forecast)}},
		{Name: "first answer", Match: answered("mcp__weather__forecast"), Reply: harnesstest.Reply{Text: "snow"}},
		{Name: "summary", Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: harnesstest.Reply{Text: "gist"}},
		{Name: "second forecast", Match: prompt("charlie"), Reply: harnesstest.Reply{ToolCalls: call("toolu_second", forecast)}},
		{Name: "second answer", Match: answered("mcp__weather__forecast"), Reply: harnesstest.Reply{Text: "still snow"}},
	}
	actions := []action{
		create{as: "a"},
		submit{as: "a", text: "go"}, waitIdle{as: "a"},
		submit{as: "a", text: "bravo"}, waitIdle{as: "a"},
		compact{as: "a"},
		submit{as: "a", text: "charlie"}, waitIdle{as: "a"},
	}
	setup := mcpSetup(map[string]any{"compaction_keep_turns": 1}, mcpServerDef{name: "weather", spec: mcpWeather(""), failInit: 1})
	runScenarios(t, []scenario{
		{name: "mcp_notice_then_compaction_keeps_a_call_paired", setup: setup, model: model, actions: actions},
		{name: "bifrost_mcp_notice_then_compaction_keeps_a_call_paired", chat: true, setup: setup, model: model, actions: actions},
	})
}
