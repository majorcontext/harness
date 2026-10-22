package mcpsrc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/mcp"
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
	s := mcpsrc.New(cfg, "", nil)
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

func TestOptedInHTTPServerReceivesCallingSessionID(t *testing.T) {
	var gotHeader string
	reg := mcp.NewRegistry("weather", "1")
	reg.RegisterTool(mcp.Tool{Name: "forecast", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
			return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: "ok"}}}, nil
		})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotHeader = r.Header.Get(mcp.HarnessSessionIDHeader)
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	src := mcpsrc.New(config.Config{MCPServers: map[string]config.MCPServerSpec{
		"weather": {URL: srv.URL, IncludeSessionIDHeader: true},
	}}, "", nil)
	defer src.Close()
	var tools []turn.Tool
	for _, tool := range src.Toolset(bg, nil, nil, "").Tools {
		if tool.Spec().Name == "mcp__weather__forecast" {
			tools = append(tools, tool)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("weather tools = %d, want one", len(tools))
	}
	ctx := turn.WithSessionID(bg, "child-session")
	if _, err := tools[0].Run(ctx, call("mcp__weather__forecast", `{}`)); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "child-session" {
		t.Fatalf("an HTTP MCP server opted in receives the calling session's ID (the child's for a subagent) in the header, in both the native and claude-code backends; got %q", gotHeader)
	}
	srcNoOpt := mcpsrc.New(config.Config{MCPServers: map[string]config.MCPServerSpec{"weather": {URL: srv.URL}}}, "", nil)
	defer srcNoOpt.Close()
	for _, tool := range srcNoOpt.Toolset(bg, nil, nil, "").Tools {
		if tool.Spec().Name == "mcp__weather__forecast" {
			if _, err := tool.Run(ctx, call("mcp__weather__forecast", `{}`)); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if gotHeader != "" {
		t.Fatalf("an HTTP MCP server that did not opt in receives no session ID header; got %q", gotHeader)
	}
}

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
	s := mcpsrc.New(config.Config{MCPServers: map[string]config.MCPServerSpec{"weather": {URL: srv.URL()}}, MCPToolLoading: "lazy"}, "", nil)
	t.Cleanup(s.Close)
	got, _ := run(s.Toolset(bg, nil, nil, ""), call("mcp", `{"action":"search","query":"weather forecast"}`))
	if first := `{"matches":[{"name":"mcp__weather__weather"`; !strings.HasPrefix(got, first) {
		t.Errorf("search = %s, want a name and server match (55) above two description matches and a server match (25)", got)
	}
}

func TestCallEndsWithItsContextWhileAnotherDialHoldsTheServer(t *testing.T) {
	dialing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(dialing) })
		<-release
	}))
	s := mcpsrc.New(config.Config{MCPServers: map[string]config.MCPServerSpec{"slow": {URL: srv.URL, ConnectTimeoutS: 3600}}}, "", nil)
	t.Cleanup(func() {
		close(release)
		s.Close()
		srv.Close()
	})
	go s.Toolset(bg, nil, nil, "")
	<-dialing
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := s.Call(ctx, "slow", "forecast", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call err = %v, want the cause of the canceled context", err)
	}
}

func attemptsOf(t *testing.T, s *mcpsrc.Source, server string) int {
	t.Helper()
	out, _ := run(s.Toolset(bg, nil, nil, ""), call("mcp", `{"action":"status"}`))
	var st struct {
		Servers []struct {
			Name     string
			Attempts int
			Parked   bool
		}
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status = %q: %v", out, err)
	}
	for _, sv := range st.Servers {
		if sv.Name == server {
			return sv.Attempts
		}
	}
	t.Fatalf("status = %q has no server %q", out, server)
	return 0
}

func TestCallFailsFastOnADownServerAndNeverDialsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		s := mcpsrc.New(config.Config{MCPServers: map[string]config.MCPServerSpec{"down": {URL: url}}}, "", nil)
		defer s.Close()
		if got := attemptsOf(t, s, "down"); got != 1 {
			t.Fatalf("attempts after the first connect = %d, want 1", got)
		}
		_, err := s.Call(bg, "down", "forecast", nil)
		if err == nil || !strings.Contains(err.Error(), "retrying in the background") {
			t.Fatalf("Call while backing off = %v, want the retrying reason", err)
		}
		if got := attemptsOf(t, s, "down"); got != 1 {
			t.Errorf("attempts after a Call while backing off = %d, want 1", got)
		}
		timer := time.NewTimer(time.Minute)
		<-timer.C
		before := attemptsOf(t, s, "down")
		_, err = s.Call(bg, "down", "forecast", nil)
		if err == nil || !strings.Contains(err.Error(), "parked until the mcp tool connects it") {
			t.Fatalf("Call on a parked server = %v, want the parked reason", err)
		}
		if got := attemptsOf(t, s, "down"); got != before || before != 4 {
			t.Errorf("attempts = %d after the Call, %d before, want 4 and unchanged", got, before)
		}
	})
}

func TestCallDialsAServerThatNothingHasTried(t *testing.T) {
	s, _ := source(t, nil)
	res, err := s.Call(bg, "weather", "forecast", nil)
	if err != nil || res.Text != "Oslo: 3C, snow" {
		t.Fatalf("Call = %+v, %v, want the forecast from a first-use dial", res, err)
	}
	if got := attemptsOf(t, s, "weather"); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}
