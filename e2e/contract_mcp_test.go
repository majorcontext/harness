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
	dir         bool // a stdio server starts in a directory named "stubdir"
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
				if d.dir {
					entry["dir"] = filepath.Join(t.TempDir(), "stubdir")
					if err := os.MkdirAll(entry["dir"].(string), 0o755); err != nil {
						t.Fatal(err)
					}
				}
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

func mcpText(s string) harnesstest.MCPResult {
	return harnesstest.MCPResult{Content: []harnesstest.MCPContent{{Type: harnesstest.MCPContentText, Text: s}}}
}

func mcpWeather(instructions string) harnesstest.MCPSpec {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	isErr := mcpText("upstream timeout")
	isErr.IsError = true
	return harnesstest.MCPSpec{
		Name:         "weather",
		Instructions: instructions,
		Tools: []harnesstest.MCPTool{
			{Name: "forecast", Description: "Get the weather forecast for a city", InputSchema: schema, Result: mcpText("Oslo: 3C, snow")},
			{Name: "alerts", Description: "List active weather alerts", InputSchema: schema, Result: mcpText("no active alerts")},
			{Name: "echo", Description: "Return the call arguments", InputSchema: schema, Echo: true},
			{Name: "flaky", Description: "Always reports a tool error", InputSchema: schema, Result: isErr},
			{Name: "strict", Description: "Always fails with a JSON-RPC error", InputSchema: schema, RPCError: &harnesstest.MCPError{Code: -32602, Message: "city is required"}},
		},
	}
}

func mcpDocs() harnesstest.MCPSpec {
	return harnesstest.MCPSpec{
		Name:         "docs",
		Instructions: "Read doc://guide before searching.",
		Tools:        []harnesstest.MCPTool{{Name: "search", Description: "Search the docs", Result: mcpText("no hits")}},
		Resources: []harnesstest.MCPResource{
			{URI: "doc://guide", Name: "guide", MimeType: "text/markdown", Text: "# Guide\nbe brief"},
			{URI: "doc://logo", Name: "logo", MimeType: "image/png", Blob: "aGVsbG8="},
		},
	}
}

type expectSystem struct {
	req        int // 1-based index of the model request
	has, lacks []string
	sameAs     int // when set, the 1-based index of the request whose system prompt this one equals
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
	if a.sameAs > 0 && a.sameAs <= len(reqs) && reqs[a.sameAs-1].System != sys {
		t.Errorf("request %d system prompt differs from request %d:\n%s\n---\n%s", a.req, a.sameAs, sys, reqs[a.sameAs-1].System)
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

type stopMCPServer struct{ server string }

func (a stopMCPServer) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	srv.Close()
}

type refuseMCPServer struct{ server string }

func (a refuseMCPServer) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	srv.SetAvailable(false)
}

// mcpEchoTools is a spec of n tools named t01, t02, ...
func mcpEchoTools(n int) harnesstest.MCPSpec {
	spec := harnesstest.MCPSpec{Name: "weather"}
	for i := 1; i <= n; i++ {
		spec.Tools = append(spec.Tools, harnesstest.MCPTool{
			Name: fmt.Sprintf("t%02d", i), Description: "numbered tool",
			Echo: true,
		})
	}
	return spec
}

// callThenDone scripts a request that calls c after turns assistant
// messages and a request that replies "done" after the call.
func callThenDone(turns int, c harnesstest.ToolCall) []harnesstest.Step {
	c.ID = fmt.Sprintf("toolu_%d", turns+1)
	return []harnesstest.Step{
		{Name: fmt.Sprintf("call%d", turns+1), Match: assistantTurns(turns), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{c}}},
		{Name: fmt.Sprintf("done%d", turns+1), Match: assistantTurns(turns + 1), Reply: harnesstest.Reply{Text: "done"}},
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
				Name: "mixed", Description: "Returns every content kind",
				Result: harnesstest.MCPResult{Content: []harnesstest.MCPContent{
					{Type: harnesstest.MCPContentText, Text: "plain"},
					{Type: harnesstest.MCPContentImage, MimeType: "image/png", Data: mcpPNG},
					{Type: harnesstest.MCPContentResourceLink, URI: "doc://x", Name: "x"},
					{Type: harnesstest.MCPContentResource, Resource: &harnesstest.MCPEmbedded{URI: "doc://y", Text: "embedded"}},
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
			actions: append(append([]action{}, oneTurn...), expectMCPCalls{server: "weather", want: []harnesstest.MCPCall{
				{Method: "tools/call", Name: "echo", Args: map[string]any{"from": "weather"}, Authorization: mcpToken},
			}}),
		},
	})
}

func TestContractMCPInstructionsAndResources(t *testing.T) {
	withTurn := func(a ...action) []action { return append(append([]action{}, oneTurn...), a...) }
	runScenarios(t, []scenario{
		{
			name:  "mcp_instructions_in_system_prompt",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("Call forecast before alerts.")}),
			model: textReply("hi"),
			actions: withTurn(expectSystem{req: 1,
				has: []string{
					"<mcp_instructions>",
					`<server name="weather" tools="mcp__weather__alerts, mcp__weather__echo, mcp__weather__flaky, mcp__weather__forecast, mcp__weather__strict">`,
					"Call forecast before alerts.",
				},
				lacks: []string{"list_mcp_resources"},
			}),
		},
		{
			name:  "mcp_instructions_are_cut_at_4000_runes",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("  " + strings.Repeat("\u00e9", 4000) + "TAIL  ")}),
			model: textReply("hi"),
			actions: withTurn(expectSystem{req: 1,
				has:   []string{strings.Repeat("\u00e9", 4000) + "\u2026 [truncated]\n</server>"},
				lacks: []string{"TAIL"},
			}),
		},
		{
			name: "mcp_instruction_tool_names_stop_at_2048_bytes",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: func() harnesstest.MCPSpec {
				spec := mcpEchoTools(150)
				spec.Instructions = "Use the numbered tools."
				return spec
			}()}),
			model: textReply("hi"),
			actions: withTurn(expectSystem{req: 1,
				has:   []string{`mcp__weather__t54 names truncated at byte budget">`},
				lacks: []string{"mcp__weather__t55"},
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
			actions: withTurn(expectSystem{req: 1, has: []string{
				"This session can also list and read MCP resources",
				"Read doc://guide before searching.",
			}}),
		},
		{
			name: "mcp_resources_paged_list_is_merged",
			setup: mcpSetup(nil, mcpServerDef{name: "docs", spec: func() harnesstest.MCPSpec {
				spec := mcpDocs()
				spec.PageSize = 1
				return spec
			}()}),
			model:   toolChain(ftTool("list_mcp_resources", ftArgs("server", "docs"))),
			actions: oneTurn,
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
			name:  "mcp_catalog_lists_200_deferred_tools_then_counts_the_rest",
			setup: mcpSetup(lazy, mcpServerDef{name: "weather", spec: mcpEchoTools(205)}),
			model: textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1,
				has: []string{
					"mcp__weather__t94 \u2014 numbered tool",
					"\n... and 5 more tools; use mcp(action=\"search\", query=\"...\") to find them",
				},
				lacks: []string{"mcp__weather__t95 ", "mcp__weather__t99 "},
			}),
		},
		{
			name:    "mcp_auto_default_threshold_stays_eager_at_20_tools",
			setup:   mcpSetup(map[string]any{"mcp_tool_loading": "auto"}, mcpServerDef{name: "weather", spec: mcpEchoTools(20)}),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, lacks: []string{"Deferred MCP tools"}}),
		},
		{
			name:    "mcp_auto_default_threshold_defers_at_21_tools",
			setup:   mcpSetup(map[string]any{"mcp_tool_loading": "auto"}, mcpServerDef{name: "weather", spec: mcpEchoTools(21)}),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, has: []string{"Deferred MCP tools"}}),
		},
		{
			name:    "mcp_auto_defers_over_threshold",
			setup:   mcpSetup(map[string]any{"mcp_tool_loading": "auto", "mcp_tool_loading_threshold": 2}, weather),
			model:   textReply("hi"),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, has: []string{"Deferred MCP tools"}}),
		},
		{
			name:    "mcp_auto_stays_eager_under_threshold",
			setup:   mcpSetup(map[string]any{"mcp_tool_loading": "auto", "mcp_tool_loading_threshold": 6}, weather),
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
			// connects later never joins them. The notice and the recovery
			// line are in the conversation, so the system prompt never moves.
			actions: append(append([]action{}, oneTurn...),
				expectSystem{req: 1, lacks: []string{"[mcp:", "<mcp_instructions>"}},
				expectSystem{req: 3, lacks: []string{"[mcp:", "<mcp_instructions>", "Call forecast before alerts."}, sameAs: 1},
			),
		},
		{
			name: "mcp_connect_adds_tools_but_not_instructions",
			setup: mcpSetup(nil,
				mcpServerDef{name: "weather", spec: mcpWeather("Call forecast before alerts."), failInit: 1},
				mcpServerDef{name: "docs", spec: mcpDocs()}),
			model: toolChain(
				mcpAction("action", "connect", "server", "weather"),
				mcpTool("weather", "forecast", "city", "Oslo"),
			),
			actions: append(append([]action{}, oneTurn...),
				expectSystem{req: 1, has: []string{"Read doc://guide before searching."}, lacks: []string{"Call forecast before alerts.", "[mcp:"}},
				expectSystem{req: 2, has: []string{"Read doc://guide before searching."}, lacks: []string{"Call forecast before alerts.", "[mcp:"}, sameAs: 1},
				expectSystem{req: 3, has: []string{"Read doc://guide before searching."}, lacks: []string{"Call forecast before alerts.", "[mcp:"}, sameAs: 1},
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
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, lacks: []string{"[mcp:"}}, expectSystem{req: 4, sameAs: 1}),
		},
	})
}

func TestContractMCPRuntime(t *testing.T) {
	forecast := mcpTool("weather", "forecast", "city", "Oslo")
	runScenarios(t, []scenario{
		{
			name: "mcp_stdio_server_starts_in_configured_dir",
			setup: mcpSetup(nil, mcpServerDef{name: "where", stdio: true, dir: true, spec: harnesstest.MCPSpec{
				Name:  "where",
				Tools: []harnesstest.MCPTool{{Name: "cwd", Description: "Report the working directory", Cwd: true}},
			}}),
			model:   toolChain(mcpTool("where", "cwd")),
			actions: oneTurn,
		},
		{
			name:  "mcp_server_lost_mid_session_hides_the_endpoint",
			setup: mcpSetup(nil, mcpServerDef{name: "weather", spec: mcpWeather("")}),
			model: append(callThenDone(0, forecast), callThenDone(2, forecast)...),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				stopMCPServer{server: "weather"},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
			},
		},
		{
			name: "mcp_status_reports_connected_and_unavailable_servers",
			setup: mcpSetup(nil,
				mcpServerDef{name: "weather", spec: mcpWeather("")},
				mcpServerDef{name: "docs", spec: mcpDocs(), down: true}),
			model: toolChain(mcpAction("action", "status")),
			actions: append(append([]action{}, oneTurn...), expectSystem{req: 1, lacks: []string{"[mcp:"}},
				expectSystem{req: 2, sameAs: 1}),
		},
	})
}

func TestContractMCPRefusals(t *testing.T) {
	weather := mcpServerDef{name: "weather", spec: mcpWeather("")}
	forecast := mcpTool("weather", "forecast", "city", "Oslo")
	lazy := map[string]any{"mcp_tool_loading": "lazy"}
	runScenarios(t, []scenario{
		{
			name:  "mcp_tool_action_refusals",
			setup: mcpSetup(nil, weather, mcpServerDef{name: "docs", spec: mcpDocs()}),
			model: toolChain(
				mcpAction("action", "search", "query", "forecast"),
				mcpAction("action", "connect", "server", "weather"),
				mcpAction("action", "connect", "server", "docs"),
			),
			actions: append([]action{stopMCPServer{server: "docs"}}, oneTurn...),
		},
		{
			name:  "mcp_refused_call_hides_the_response_body",
			setup: mcpSetup(nil, weather),
			model: append(callThenDone(0, forecast), callThenDone(2, forecast)...),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "go"},
				waitIdle{as: "a"},
				refuseMCPServer{server: "weather"},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
			},
		},
		{
			name:    "mcp_select_of_a_down_server_is_pending",
			setup:   mcpSetup(lazy, weather),
			model:   toolChain(mcpAction("action", "select", "tools", []string{"mcp__weather__forecast"})),
			actions: append([]action{stopMCPServer{server: "weather"}}, oneTurn...),
		},
		{
			name:  "mcp_search_ranks_the_tools",
			setup: mcpSetup(lazy, weather),
			model: toolChain(
				mcpTool("weather", "alerts"),
				mcpAction("action", "search", "query", "weather alerts", "limit", 2),
			),
			actions: oneTurn,
		},
	})
}
