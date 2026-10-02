package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/mcp"
)

const mcpToken = "Bearer e2e-mcp"

// mcpPNG is a 1x1 PNG.
const mcpPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// mcpServerDef is one mcp_servers entry. An HTTP server is a fake that
// requires mcpToken; a stdio server is the mcpstdio stub.
type mcpServerDef struct {
	name        string
	spec        harnesstest.MCPSpec
	stdio, sse  bool
	failInit    int
	down        bool
	toolLoading string
}

func mcpSetup(global map[string]any, defs ...mcpServerDef) func(*testing.T, map[string]any) map[string]any {
	return func(t *testing.T, fx map[string]any) map[string]any {
		servers := map[string]any{}
		for _, d := range defs {
			entry := map[string]any{}
			if d.stdio {
				entry["command"] = []string{mcpStubBin(t)}
				entry["env"] = d.spec.StdioEnv()
			} else {
				srv := harnesstest.NewMCPServer(t, d.spec)
				srv.RequireAuthorization(mcpToken)
				srv.FailInitialize(d.failInit)
				srv.SetAvailable(!d.down)
				if d.sse {
					srv.UseSSE()
				}
				entry["url"], entry["headers"] = srv.URL(), map[string]string{"Authorization": mcpToken}
				fx[d.name] = srv
			}
			if d.toolLoading != "" {
				entry["tool_loading"] = d.toolLoading
			}
			servers[d.name] = entry
		}
		cfg := map[string]any{"mcp_servers": servers}
		maps.Copy(cfg, global)
		return cfg
	}
}

var mcpStub struct {
	once sync.Once
	path string
	err  error
}

func mcpStubBin(t *testing.T) string {
	t.Helper()
	mcpStub.once.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			mcpStub.err = err
			return
		}
		mcpStub.path = filepath.Join(filepath.Dir(harnessBin), "mcpstdio")
		cmd := exec.Command("go", "build", "-o", mcpStub.path, "./harnesstest/mcpstdio")
		cmd.Dir = filepath.Dir(wd)
		if out, err := cmd.CombinedOutput(); err != nil {
			mcpStub.err = fmt.Errorf("build mcpstdio: %v\n%s", err, out)
		}
	})
	if mcpStub.err != nil {
		t.Fatal(mcpStub.err)
	}
	return mcpStub.path
}

func mcpText(s string) mcp.CallToolResult {
	return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: s}}}
}

func mcpWeather(instructions string) harnesstest.MCPSpec {
	tool := func(name, desc string) mcp.Tool {
		return mcp.Tool{Name: name, Description: desc, InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}
	}
	isErr := mcpText("upstream timeout")
	isErr.IsError = true
	return harnesstest.MCPSpec{
		Name:         "weather",
		Instructions: instructions,
		Tools: []harnesstest.MCPTool{
			{Def: tool("forecast", "Get the weather forecast for a city"), Result: mcpText("Oslo: 3C, snow")},
			{Def: tool("alerts", "List active weather alerts"), Result: mcpText("no active alerts")},
			{Def: tool("echo", "Return the call arguments"), Echo: true},
			{Def: tool("flaky", "Always reports a tool error"), Result: isErr},
			{Def: tool("strict", "Always fails with a JSON-RPC error"), RPCError: &mcp.RPCError{Code: -32602, Message: "city is required"}},
		},
	}
}

func mcpDocs() harnesstest.MCPSpec {
	res := func(uri, name, mime string) mcp.Resource { return mcp.Resource{URI: uri, Name: name, MimeType: mime} }
	return harnesstest.MCPSpec{
		Name:         "docs",
		Instructions: "Read doc://guide before searching.",
		Tools:        []harnesstest.MCPTool{{Def: mcp.Tool{Name: "search", Description: "Search the docs"}, Result: mcpText("no hits")}},
		Resources: []harnesstest.MCPResource{
			{Resource: res("doc://guide", "guide", "text/markdown"), Text: "# Guide\nbe brief"},
			{Resource: res("doc://logo", "logo", "image/png"), Blob: "aGVsbG8="},
		},
	}
}

type expectSystem struct {
	req        int // 1-based index of the model request
	has, lacks []string
}

func (a expectSystem) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectSystem: request %d of %d", a.req, len(reqs))
	}
	sys := reqs[a.req-1].System
	for _, s := range a.has {
		if !strings.Contains(sys, s) {
			t.Errorf("request %d system prompt lacks %q:\n%s", a.req, s, sys)
		}
	}
	for _, s := range a.lacks {
		if strings.Contains(sys, s) {
			t.Errorf("request %d system prompt holds %q:\n%s", a.req, s, sys)
		}
	}
}

type expectMCPCalls struct {
	server string
	want   []harnesstest.MCPCall
}

func (a expectMCPCalls) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	if got := srv.Calls(); !reflect.DeepEqual(got, a.want) {
		t.Errorf("server %s calls = %+v, want %+v", a.server, got, a.want)
	}
}

func mcpTool(server, tool string, kv ...any) harnesstest.ToolCall {
	return ftTool("mcp__"+server+"__"+tool, ftArgs(kv...))
}

func mcpAction(kv ...any) harnesstest.ToolCall { return ftTool("mcp", ftArgs(kv...)) }

func textReply(s string) []harnesstest.Step {
	return []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: s}}}
}

func TestContractMCPEager(t *testing.T) {
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	runScenarios(t, []scenario{
		{
			name:    "mcp_eager_lists_namespaced_tools",
			setup:   mcpSetup(nil, weather),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, lacks: []string{"Deferred MCP tools", "<mcp_instructions>"}}),
		},
		{
			name:  "mcp_tool_call_result",
			setup: mcpSetup(nil, weather),
			model: toolChain(mcpTool("weather", "forecast", "city", "Oslo")),
			actions: append(append([]action{}, oneTurn...), expectMCPCalls{server: "weather", want: []harnesstest.MCPCall{
				{Method: "tools/call", Name: "forecast", Args: map[string]any{"city": "Oslo"}, Authorization: mcpToken},
			}}),
		},
		{
			name:  "mcp_tool_error_and_rpc_error_reach_model",
			setup: mcpSetup(nil, weather),
			model: toolChain(
				mcpTool("weather", "flaky"),
				mcpTool("weather", "strict"),
				mcpTool("weather", "missing"),
			),
			actions: oneTurn,
		},
		{
			name:    "mcp_http_sse_reply",
			setup:   mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), sse: true}),
			model:   toolChain(mcpTool("weather", "echo", "city", "Rome")),
			actions: oneTurn,
		},
		{
			name:    "mcp_stdio_server_call",
			setup:   mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), stdio: true}),
			model:   toolChain(mcpTool("weather", "echo", "city", "Lima")),
			actions: oneTurn,
		},
		{
			name: "mcp_paged_tool_list_is_merged",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: func() harnesstest.MCPSpec {
				spec := mcpWeather("")
				spec.PageSize = 2
				return spec
			}()}),
			model:   textReply("hi"),
			actions: oneTurn,
		},
		{
			name: "mcp_non_text_results_become_text_and_blobs",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: harnesstest.MCPSpec{Name: "weather", Tools: []harnesstest.MCPTool{{
				Def: mcp.Tool{Name: "mixed", Description: "Returns every content kind"},
				Result: mcp.CallToolResult{Content: []mcp.Content{
					{Type: mcp.ContentTypeText, Text: "plain"},
					{Type: mcp.ContentTypeImage, MimeType: "image/png", Data: mcpPNG},
					{Type: mcp.ContentTypeResourceLink, URI: "doc://x", Name: "x"},
					{Type: mcp.ContentTypeResource, Resource: &mcp.EmbeddedResource{URI: "doc://y", Text: "embedded"}},
				}},
			}}}}),
			model:   toolChain(mcpTool("weather", "mixed")),
			actions: oneTurn,
		},
		{
			name:  "mcp_two_servers_share_a_tool_name",
			setup: mcpSetup(nil, weather, mcpServerDef{name: "stdio-weather", spec: mcpWeather(""), stdio: true}),
			model: toolChain(
				mcpTool("weather", "echo", "from", "weather"),
				mcpTool("stdio-weather", "echo", "from", "stdio"),
			),
			actions: oneTurn,
		},
	})
}

func TestContractMCPInstructionsAndResources(t *testing.T) {
	withDocs := func(a ...action) []action { return append(append([]action{}, oneTurn...), a...) }
	runScenarios(t, []scenario{
		{
			name:  "mcp_instructions_in_system_prompt",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("Call forecast before alerts.")}),
			model: textReply("hi"),
			actions: withDocs(expectSystem{req: 1,
				has: []string{
					"<mcp_instructions>",
					`<server name="weather" tools="mcp__weather__alerts, mcp__weather__echo, mcp__weather__flaky, mcp__weather__forecast, mcp__weather__strict">`,
					"Call forecast before alerts.",
				},
				lacks: []string{"list_mcp_resources"},
			}),
		},
		{
			name:  "mcp_resources_list_and_read",
			setup: mcpSetup(nil, mcpServerDef{name: "docs", spec: mcpDocs()}),
			model: toolChain(
				ftTool("list_mcp_resources", ftArgs()),
				ftTool("read_mcp_resource", ftArgs("server", "docs", "uri", "doc://guide")),
				ftTool("read_mcp_resource", ftArgs("server", "docs", "uri", "doc://logo")),
				ftTool("read_mcp_resource", ftArgs("server", "docs", "uri", "doc://missing")),
				ftTool("list_mcp_resources", ftArgs("server", "nope")),
			),
			actions: withDocs(expectSystem{req: 1, has: []string{
				"This session can also list and read MCP resources",
				"Read doc://guide before searching.",
			}}),
		},
	})
}

func TestContractMCPLazyLoading(t *testing.T) {
	lazy := map[string]any{"mcp_tool_loading": "lazy"}
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	forecastLine := "mcp__weather__forecast \u2014 Get the weather forecast for a city"
	runScenarios(t, []scenario{
		{
			name:  "mcp_lazy_search_select_then_call",
			setup: mcpSetup(lazy, weather),
			model: toolChain(
				mcpAction("action", "search", "query", "forecast"),
				mcpAction("action", "select", "tools", []string{"mcp__weather__forecast"}),
				mcpTool("weather", "forecast", "city", "Oslo"),
			),
			actions: append(append([]action{}, oneTurn...),
				expectSystem{req: 1, has: []string{"Deferred MCP tools", forecastLine}},
				expectSystem{req: 3, has: []string{"Deferred MCP tools"}, lacks: []string{forecastLine}},
			),
		},
		{
			name:    "mcp_lazy_call_without_select_loads_the_tool",
			setup:   mcpSetup(lazy, weather),
			model:   toolChain(mcpTool("weather", "forecast", "city", "Oslo")),
			actions: oneTurn,
		},
		{
			name:  "mcp_lazy_select_reports_each_name",
			setup: mcpSetup(lazy, weather),
			model: toolChain(
				mcpAction("action", "select", "tools", []string{"mcp__weather__alerts", "mcp__weather__nope", "bogus"}),
				mcpAction("action", "select", "tools", []string{"mcp__weather__alerts"}),
				mcpAction("action", "select", "tools", []string{}),
				mcpAction("action", "search", "query", "  "),
			),
			actions: oneTurn,
		},
		{
			name: "mcp_per_server_tool_loading_overrides_global",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), toolLoading: "lazy"},
				mcpServerDef{name: "docs", spec: mcpDocs()}),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, has: []string{"mcp__weather__alerts"}, lacks: []string{"mcp__docs__search \u2014"}}),
		},
		{
			name:    "mcp_auto_defers_over_threshold",
			setup:   mcpSetup(map[string]any{"mcp_tool_loading": "auto", "mcp_tool_loading_threshold": 2}, weather),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, has: []string{"Deferred MCP tools"}}),
		},
		{
			name:    "mcp_auto_stays_eager_under_threshold",
			setup:   mcpSetup(map[string]any{"mcp_tool_loading": "auto", "mcp_tool_loading_threshold": 5}, weather),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, lacks: []string{"Deferred MCP tools"}}),
		},
	})
}

func TestContractMCPAvailability(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:  "mcp_unavailable_at_start_then_connect",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("Call forecast before alerts."), failInit: 1}),
			model: toolChain(
				mcpAction("action", "connect", "server", "weather"),
				mcpTool("weather", "forecast", "city", "Oslo"),
			),
			// Instructions render once, on the first request: a server that
			// connects later never joins them.
			actions: append(append([]action{}, oneTurn...),
				expectSystem{req: 1, lacks: []string{"<mcp_instructions>"}},
				expectSystem{req: 3, lacks: []string{"<mcp_instructions>", "Call forecast before alerts."}},
			),
		},
		{
			name:  "mcp_unavailable_connect_fails_with_classified_reason",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather(""), down: true}),
			model: toolChain(
				mcpAction("action", "connect", "server", "weather"),
				mcpAction("action", "connect", "server", "nope"),
				mcpTool("weather", "forecast", "city", "Oslo"),
			),
			actions: oneTurn,
		},
	})
}
