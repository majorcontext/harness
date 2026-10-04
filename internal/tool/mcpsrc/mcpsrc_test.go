package mcpsrc_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/protocol"
)

var bg = context.Background()

func content(c ...mcp.Content) mcp.CallToolResult { return mcp.CallToolResult{Content: c} }

func textResult(s string) mcp.CallToolResult {
	return content(mcp.Content{Type: mcp.ContentTypeText, Text: s})
}

func weather() harnesstest.MCPSpec {
	failed := textResult("upstream timeout")
	failed.IsError = true
	tool := func(name, desc string, res mcp.CallToolResult) harnesstest.MCPTool {
		return harnesstest.MCPTool{Def: mcp.Tool{Name: name, Description: desc}, Result: res}
	}
	return harnesstest.MCPSpec{Name: "weather", Instructions: "Call forecast first.", Tools: []harnesstest.MCPTool{
		tool("forecast", "Get the weather forecast for a city\nmore detail", textResult("Oslo: 3C, snow")),
		tool("alerts", "List active weather alerts", textResult("none")),
		tool("flaky", "Always fails", failed),
		{Def: mcp.Tool{Name: "strict", Description: "Rejects its call"}, RPCError: &mcp.RPCError{Code: -32602, Message: "city is required"}},
		tool("mixed", "Returns every content kind", content(
			mcp.Content{Type: mcp.ContentTypeText, Text: "plain"},
			mcp.Content{Type: mcp.ContentTypeImage, MimeType: "image/png", Data: "aGVsbG8="},
			mcp.Content{Type: mcp.ContentTypeResourceLink, URI: "doc://x", Name: "x"},
			mcp.Content{Type: mcp.ContentTypeResource, Resource: &mcp.EmbeddedResource{URI: "doc://y", Text: "embedded"}})),
	}}
}

func docs() harnesstest.MCPSpec {
	return harnesstest.MCPSpec{Name: "docs", PageSize: 1,
		Tools: []harnesstest.MCPTool{{Def: mcp.Tool{Name: "search", Description: "Search the docs"}, Result: textResult("no hits")}},
		Resources: []harnesstest.MCPResource{
			{Resource: mcp.Resource{URI: "doc://guide", Name: "guide", MimeType: "text/markdown"}, Text: "# Guide"},
			{Resource: mcp.Resource{URI: "doc://logo", Name: "logo", MimeType: "image/png"}, Blob: "aGVsbG8="},
		}}
}

// source serves weather and docs over HTTP and applies edit to the config.
func source(t *testing.T, edit func(*config.Config, map[string]*harnesstest.MCPServer)) (*mcpsrc.Source, map[string]*harnesstest.MCPServer) {
	t.Helper()
	srv := map[string]*harnesstest.MCPServer{"weather": harnesstest.NewMCPServer(t, weather()), "docs": harnesstest.NewMCPServer(t, docs())}
	cfg := config.Config{MCPServers: map[string]config.MCPServerSpec{}}
	for name, s := range srv {
		cfg.MCPServers[name] = config.MCPServerSpec{URL: s.URL()}
	}
	if edit != nil {
		edit(&cfg, srv)
	}
	s := mcpsrc.New(cfg)
	t.Cleanup(s.Close)
	return s, srv
}

func lazy(c *config.Config, _ map[string]*harnesstest.MCPServer) { c.MCPToolLoading = "lazy" }

func auto(threshold int) func(*config.Config, map[string]*harnesstest.MCPServer) {
	return func(c *config.Config, _ map[string]*harnesstest.MCPServer) {
		c.MCPToolLoading, c.MCPToolLoadingThreshold = "auto", threshold
	}
}

func names(tools []turn.Tool) string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Spec().Name)
	}
	return strings.Join(out, " ")
}

func calls(cs ...protocol.ToolCall) []eventlog.Message {
	m := eventlog.Message{Role: eventlog.RoleAssistant}
	for _, c := range cs {
		m.Parts = append(m.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	return []eventlog.Message{m}
}

func call(name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: "c1", Name: name, Arguments: json.RawMessage(args)}
}

const (
	all         = "mcp list_mcp_resources read_mcp_resource mcp__docs__search mcp__weather__alerts mcp__weather__flaky mcp__weather__forecast mcp__weather__mixed mcp__weather__strict"
	forecastRow = "mcp__weather__forecast — Get the weather forecast for a city"
)

func TestToolset(t *testing.T) {
	selectForecast := calls(call("mcp", `{"action":"select","tools":["mcp__weather__forecast"]}`))
	for _, tc := range []struct {
		name            string
		edit            func(*config.Config, map[string]*harnesstest.MCPServer)
		history         []eventlog.Message
		allowed         []string
		tools, deferred string
		has, lacks      []string
	}{
		{name: "eager describes every tool and the instructions once", tools: all,
			has:   []string{"<mcp_instructions>\nThis session can also list and read MCP resources", `<server name="weather" tools="mcp__weather__alerts, mcp__weather__flaky, mcp__weather__forecast, mcp__weather__mixed, mcp__weather__strict">` + "\nCall forecast first.\n</server>"},
			lacks: []string{"Deferred MCP tools", `name="docs"`}},
		{name: "lazy defers every tool to the catalog", edit: lazy, tools: "mcp list_mcp_resources read_mcp_resource",
			deferred: "mcp__docs__search mcp__weather__alerts mcp__weather__flaky mcp__weather__forecast mcp__weather__mixed mcp__weather__strict",
			has:      []string{"Deferred MCP tools", forecastRow + "\n"}, lacks: []string{"more detail"}},
		{name: "a select in the history loads the tool", edit: lazy, history: selectForecast,
			tools:    "mcp list_mcp_resources read_mcp_resource mcp__weather__forecast",
			deferred: "mcp__docs__search mcp__weather__alerts mcp__weather__flaky mcp__weather__mixed mcp__weather__strict",
			lacks:    []string{forecastRow}},
		{name: "a call in the history loads the tool", edit: lazy, history: calls(call("mcp__weather__alerts", `{}`)),
			tools:    "mcp list_mcp_resources read_mcp_resource mcp__weather__alerts",
			deferred: "mcp__docs__search mcp__weather__flaky mcp__weather__forecast mcp__weather__mixed mcp__weather__strict"},
		{name: "auto stays eager at the threshold", tools: all,
			edit: auto(6)},
		{name: "auto defers over the threshold", tools: "mcp list_mcp_resources read_mcp_resource",
			deferred: "mcp__docs__search mcp__weather__alerts mcp__weather__flaky mcp__weather__forecast mcp__weather__mixed mcp__weather__strict",
			edit:     auto(5)},
		{name: "a server mode overrides the global mode", tools: "mcp list_mcp_resources read_mcp_resource mcp__docs__search",
			deferred: "mcp__weather__alerts mcp__weather__flaky mcp__weather__forecast mcp__weather__mixed mcp__weather__strict",
			edit: func(c *config.Config, _ map[string]*harnesstest.MCPServer) {
				c.MCPServers["weather"] = config.MCPServerSpec{URL: c.MCPServers["weather"].URL, ToolLoading: "lazy"}
			}},
		{name: "allowed tools restrict the tools and the catalog", edit: lazy, allowed: []string{"mcp__weather__alerts"},
			deferred: "mcp__weather__alerts", lacks: []string{forecastRow}},
		{name: "a server down at start has no tools", tools: "mcp mcp__docs__search", lacks: []string{"list_mcp_resources"},
			edit: func(c *config.Config, s map[string]*harnesstest.MCPServer) {
				s["weather"].FailInitialize(1)
				c.MCPServers["docs"] = config.MCPServerSpec{URL: harnesstest.NewMCPServer(t, harnesstest.MCPSpec{Name: "docs",
					Tools: []harnesstest.MCPTool{{Def: mcp.Tool{Name: "search"}}}}).URL()}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := source(t, tc.edit)
			ts := s.Toolset(bg, tc.history, tc.allowed)
			if got := names(ts.Tools); got != tc.tools {
				t.Errorf("tools = %q, want %q", got, tc.tools)
			}
			if got := names(ts.Deferred); got != tc.deferred {
				t.Errorf("deferred = %q, want %q", got, tc.deferred)
			}
			for _, s := range tc.has {
				if !strings.Contains(ts.Prompt, s) {
					t.Errorf("prompt lacks %q:\n%s", s, ts.Prompt)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(ts.Prompt, s) {
					t.Errorf("prompt holds %q:\n%s", s, ts.Prompt)
				}
			}
		})
	}
}

func TestToolCalls(t *testing.T) {
	stop := func(_ *config.Config, s map[string]*harnesstest.MCPServer) { s["weather"].Close() }
	for _, tc := range []struct {
		name    string
		edit    func(*config.Config, map[string]*harnesstest.MCPServer)
		history []eventlog.Message
		call    protocol.ToolCall
		want    string
		isError bool
		lose    string
	}{
		{name: "a tool result", call: call("mcp__weather__forecast", `{"city":"Oslo"}`), want: "Oslo: 3C, snow"},
		{name: "a tool error", call: call("mcp__weather__flaky", `{}`), want: "upstream timeout", isError: true},
		{name: "an RPC error", call: call("mcp__weather__strict", `{}`), want: "error: mcp: tools/call strict: mcp: rpc error -32602: city is required"},
		{name: "binary content becomes a line", call: call("mcp__weather__mixed", `{}`),
			want: "plain\n[image content, 5 bytes, image/png]\nresource: doc://x (x)\nembedded"},
		{name: "a lost server hides its endpoint", lose: "docs", call: call("mcp__docs__search", `{}`), want: `error: mcp: server "docs": call failed: connection refused`},
		{name: "paged resources of every server", call: call("list_mcp_resources", `{}`),
			want: `{"resources":[{"uri":"doc://guide","name":"guide","mimeType":"text/markdown","server":"docs"},{"uri":"doc://logo","name":"logo","mimeType":"image/png","server":"docs"}]}`},
		{name: "resources of an unknown server", call: call("list_mcp_resources", `{"server":"nope"}`),
			want: `error: list_mcp_resources: mcp: server "nope" is not configured`},
		{name: "a text resource", call: call("read_mcp_resource", `{"server":"docs","uri":"doc://guide"}`), want: "# Guide"},
		{name: "a binary resource", call: call("read_mcp_resource", `{"server":"docs","uri":"doc://logo"}`),
			want: "[binary resource: doc://logo, 5 bytes, image/png]"},
		{name: "a missing resource", call: call("read_mcp_resource", `{"server":"docs","uri":"doc://missing"}`),
			want: "error: read_mcp_resource: mcp: resources/read doc://missing: mcp: rpc error -32002: resource not found: doc://missing"},
		{name: "select sorts each name", edit: lazy, history: calls(call("mcp__weather__alerts", `{}`)),
			call: call("mcp", `{"action":"select","tools":["mcp__weather__forecast","mcp__weather__alerts","mcp__weather__nope","bogus","mcp__weather__forecast"]}`),
			want: `{"selected":["mcp__weather__forecast"],"already":["mcp__weather__alerts"],"pending":[],"missing":["mcp__weather__nope","bogus"],"note":"selected tools are callable from the next request in this turn"}`},
		{name: "select of a server that is down is pending", edit: func(c *config.Config, s map[string]*harnesstest.MCPServer) { lazy(c, s); stop(c, s) },
			call: call("mcp", `{"action":"select","tools":["mcp__weather__forecast"]}`),
			want: `{"selected":[],"already":[],"pending":["mcp__weather__forecast"],"missing":[],"note":"no tool was loaded: every name you selected belongs to a server that is not connected. They load once that server connects; see the mcp tool's connect action"}`},
		{name: "select needs names", edit: lazy, call: call("mcp", `{"action":"select","tools":[]}`), want: `error: mcp: select requires a non-empty "tools" array`},
		{name: "search ranks the tools", edit: lazy, history: calls(call("mcp__weather__alerts", `{}`)), call: call("mcp", `{"action":"search","query":"weather alerts","limit":2}`),
			want: `{"matches":[{"name":"mcp__weather__alerts","server":"weather","description":"List active weather alerts","loaded":true},` +
				`{"name":"mcp__weather__forecast","server":"weather","description":"Get the weather forecast for a city","loaded":false}],"total":5,"truncated":true}`},
		{name: "search needs a query", edit: lazy, call: call("mcp", `{"action":"search","query":"  "}`), want: `error: mcp: search requires a non-empty "query" argument`},
		{name: "an eager source has no search", call: call("mcp", `{"action":"search","query":"x"}`), want: `error: mcp: unknown action "search" (want "connect")`},
		{name: "connect of an unknown server", call: call("mcp", `{"action":"connect","server":"nope"}`), want: `error: mcp: unknown server "nope" (configured: docs, weather)`},
		{name: "connect of a connected server", call: call("mcp", `{"action":"connect","server":"docs"}`), want: `{"server":"docs","connected":true,"message":"already connected"}`},
		{name: "connect of a server that refuses", edit: func(c *config.Config, s map[string]*harnesstest.MCPServer) { s["weather"].SetAvailable(false) },
			call: call("mcp", `{"action":"connect","server":"weather"}`), want: `error: mcp: connect for "weather" failed: initialize failed`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, srv := source(t, tc.edit)
			ts := s.Toolset(bg, tc.history, nil)
			if tc.lose != "" {
				srv[tc.lose].Close()
			}
			got, isError := run(ts, tc.call)
			if got != tc.want || isError != tc.isError {
				t.Errorf("result = %q (error %v), want %q (error %v)", got, isError, tc.want, tc.isError)
			}
		})
	}
}

func run(ts turn.Toolset, c protocol.ToolCall) (string, bool) {
	for _, tool := range append(ts.Tools, ts.Deferred...) {
		if tool.Spec().Name == c.Name {
			res, err := tool.Run(bg, c)
			if err != nil {
				return "error: " + err.Error(), false
			}
			return res.Text, res.IsError
		}
	}
	return "no tool " + c.Name, false
}

func TestConnectAddsToolsButNotInstructions(t *testing.T) {
	s, _ := source(t, func(_ *config.Config, s map[string]*harnesstest.MCPServer) { s["weather"].FailInitialize(1) })
	before := s.Toolset(bg, nil, nil)
	got, _ := run(before, call("mcp", `{"action":"connect","server":"weather"}`))
	after := s.Toolset(bg, nil, nil)
	if want := `{"server":"weather","connected":true,"message":"connected"}`; got != want {
		t.Errorf("connect = %q, want %q", got, want)
	}
	if names(after.Tools) != all || after.Prompt != before.Prompt || strings.Contains(after.Prompt, "Call forecast first.") {
		t.Errorf("after connect: tools %q, prompt %q; want every tool and the prompt of the first connect %q", names(after.Tools), after.Prompt, before.Prompt)
	}
}
