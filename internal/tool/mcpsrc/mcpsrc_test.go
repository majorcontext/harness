package mcpsrc_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var bg = context.Background()

func content(c ...harnesstest.MCPContent) harnesstest.MCPResult {
	return harnesstest.MCPResult{Content: c}
}

func textResult(s string) harnesstest.MCPResult {
	return content(harnesstest.MCPContent{Type: harnesstest.MCPContentText, Text: s})
}

func weather() harnesstest.MCPSpec {
	failed := textResult("upstream timeout")
	failed.IsError = true
	tool := func(name, desc string, res harnesstest.MCPResult) harnesstest.MCPTool {
		return harnesstest.MCPTool{Name: name, Description: desc, Result: res}
	}
	return harnesstest.MCPSpec{Name: "weather", Instructions: "Call forecast first.", Tools: []harnesstest.MCPTool{
		tool("forecast", "Get the weather forecast for a city\nmore detail", textResult("Oslo: 3C, snow")),
		tool("alerts", "List active weather alerts", textResult("none")),
		tool("flaky", "Always fails", failed),
		{Name: "strict", Description: "Rejects its call", RPCError: &harnesstest.MCPError{Code: -32602, Message: "city is required"}},
		tool("mixed", "Returns every content kind", content(
			harnesstest.MCPContent{Type: harnesstest.MCPContentText, Text: "plain"},
			harnesstest.MCPContent{Type: harnesstest.MCPContentImage, MimeType: "image/png", Data: "aGVsbG8="},
			harnesstest.MCPContent{Type: harnesstest.MCPContentResourceLink, URI: "doc://x", Name: "x"},
			harnesstest.MCPContent{Type: harnesstest.MCPContentResource, Resource: &harnesstest.MCPEmbedded{URI: "doc://y", Text: "embedded"}})),
	}}
}

func docs() harnesstest.MCPSpec {
	return harnesstest.MCPSpec{Name: "docs", PageSize: 1,
		Tools: []harnesstest.MCPTool{{Name: "search", Description: "Search the docs", Result: textResult("no hits")}},
		Resources: []harnesstest.MCPResource{
			{URI: "doc://guide", Name: "guide", MimeType: "text/markdown", Text: "# Guide"},
			{URI: "doc://logo", Name: "logo", MimeType: "image/png", Blob: "aGVsbG8="},
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

func names(tools []turn.Tool) string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Spec().Name)
	}
	return strings.Join(out, " ")
}

func call(name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: "c1", Name: name, Arguments: json.RawMessage(args)}
}

const forecastRow = "mcp__weather__forecast — Get the weather forecast for a city"

func TestToolsetRestrictedByAllowedTools(t *testing.T) {
	for _, tc := range []struct {
		name            string
		edit            func(*config.Config, map[string]*harnesstest.MCPServer)
		allowed         []string
		tools, deferred string
		has, lacks      []string
	}{
		{name: "allowed tools restrict the tools and the catalog", edit: lazy, allowed: []string{"mcp__weather__alerts"},
			deferred: "mcp__weather__alerts", lacks: []string{forecastRow}},
		{name: "allowed tools restrict the tool names of the instructions", allowed: []string{"mcp__weather__alerts"},
			tools: "mcp__weather__alerts", has: []string{`tools="mcp__weather__alerts">`}, lacks: []string{"mcp__weather__forecast"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := source(t, tc.edit)
			ts := s.Toolset(bg, nil, tc.allowed, "")
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

func TestSearchScoresEachField(t *testing.T) {
	srv := harnesstest.NewMCPServer(t, harnesstest.MCPSpec{Name: "weather", Tools: []harnesstest.MCPTool{
		{Name: "other", Description: "weather forecast"}, {Name: "weather", Description: "x"}}})
	s := mcpsrc.New(config.Config{MCPServers: map[string]config.MCPServerSpec{"weather": {URL: srv.URL()}}, MCPToolLoading: "lazy"})
	t.Cleanup(s.Close)
	got, _ := run(s.Toolset(bg, nil, nil, ""), call("mcp", `{"action":"search","query":"weather forecast"}`))
	if first := `{"matches":[{"name":"mcp__weather__weather"`; !strings.HasPrefix(got, first) {
		t.Errorf("search = %s, want a name and server match (55) above two description matches and a server match (25)", got)
	}
}
