package harness_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/protocol"
)

func TestMCPToolsOfEachModelCall(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	mcpSrv := harnesstest.NewMCPServer(t, harnesstest.MCPSpec{Name: "weather", Tools: []harnesstest.MCPTool{
		{Def: mcp.Tool{Name: "forecast", Description: "Get the forecast", InputSchema: schema},
			Result: mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: "Oslo: 3C, snow"}}}},
		{Def: mcp.Tool{Name: "alerts", Description: "List alerts", InputSchema: schema},
			Result: mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: "none"}}}},
	}})
	call := func(id, name string, in map[string]any) harnesstest.ToolCall {
		return harnesstest.ToolCall{ID: id, Name: name, Input: in}
	}
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		harnesstest.Step{Name: "select", Match: harnesstest.LastUserText("go"),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call("call_1", "mcp", map[string]any{"action": "select", "tools": []string{"mcp__weather__forecast"}})}}},
		harnesstest.Step{Name: "call", Match: harnesstest.LastToolResult("mcp"),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call("call_2", "mcp__weather__forecast", map[string]any{"city": "Oslo"}),
				call("call_3", "mcp__weather__alerts", map[string]any{})}}},
		harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("mcp__weather__alerts"), Reply: harnesstest.Reply{Text: "done"}})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}},
		MCPServers: map[string]config.MCPServerSpec{"weather": {URL: mcpSrv.URL()}}, MCPToolLoading: "lazy"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "go")
	closeRuntime(t, r)
	reqs := s.Requests()
	forecast, alerts := "mcp__weather__forecast — Get the forecast", "mcp__weather__alerts — List alerts"
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3", len(reqs))
	}
	if !slices.Contains(reqs[0].Tools, "mcp") || slices.Contains(reqs[0].Tools, "mcp__weather__forecast") || !strings.Contains(reqs[0].System, forecast) {
		t.Errorf("first request tools %q, system %q: want the mcp tool and forecast deferred", reqs[0].Tools, reqs[0].System)
	}
	if !slices.Contains(reqs[1].Tools, "mcp__weather__forecast") || strings.Contains(reqs[1].System, forecast) || !strings.Contains(reqs[1].System, alerts) {
		t.Errorf("request after select tools %q, system %q: want forecast loaded and alerts deferred", reqs[1].Tools, reqs[1].System)
	}
	last := reqs[2].Messages[len(reqs[2].Messages)-1].Parts
	if got := last[len(last)-1].Text; got != "none" {
		t.Errorf("result of alerts, which defers, = %q, want the server's text", got)
	}
	want := []harnesstest.MCPCall{{Method: "tools/call", Name: "forecast", Args: map[string]any{"city": "Oslo"}},
		{Method: "tools/call", Name: "alerts", Args: map[string]any{}}}
	if got := mcpSrv.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("server calls = %+v, want %+v", got, want)
	}
}
