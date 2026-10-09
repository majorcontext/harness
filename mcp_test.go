package harness_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

func TestAllowedMCPToolsOfAnOwnedLoopBackend(t *testing.T) {
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{
		Providers:  map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI}},
		MCPServers: map[string]config.MCPServerSpec{"weather": {URL: "http://127.0.0.1:1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	for allowed, want := range map[string]error{"mcp__weather__forecast": nil, "mcp": nil, "list_mcp_resources": nil, "nope": harness.ErrInvalidRequest} {
		_, err := r.Create(bg, protocol.CreateSession{ID: "s-" + allowed, Model: "claude-code/opus", AllowedTools: []string{allowed}})
		if !errors.Is(err, want) {
			t.Errorf("Create allowing %q = %v, want %v", allowed, err, want)
		}
	}
}

const (
	mcpDownNotice   = `[mcp: unavailable — weather (initialize failed; retrying). Tools from these servers are temporarily absent and may return later in this session.]`
	mcpParkedNotice = `[mcp: unavailable — weather (initialize failed; use the mcp tool action "connect" to retry). Tools from these servers are temporarily absent and may return later in this session.]`
	mcpRecovered    = `[mcp: every configured server is connected again.]`
	mcpStatusCall   = "status"
)

func mcpWeatherSpec() harnesstest.MCPSpec {
	return harnesstest.MCPSpec{Name: "weather", Tools: []harnesstest.MCPTool{
		{Name: "forecast", Description: "Get the forecast", Result: harnesstest.MCPResult{Content: []harnesstest.MCPContent{{Type: harnesstest.MCPContentText, Text: "snow"}}}}}}
}

func mcpDocsSpec() harnesstest.MCPSpec {
	return harnesstest.MCPSpec{Name: "docs", Tools: []harnesstest.MCPTool{{Name: "search", Description: "Search the docs"}}}
}

func mcpActions() *family {
	return &family{answer: func(p eventlog.Part) []eventlog.Message {
		switch p.Text {
		case mcpStatusCall:
			return []eventlog.Message{calls("mcp", map[string]any{"action": "status"})}
		case "connect":
			return []eventlog.Message{calls("mcp", map[string]any{"action": "connect", "server": "weather"})}
		}
		return []eventlog.Message{say("ok")}
	}}
}

func mcpRuntime(t *testing.T, f *family, servers map[string]*harnesstest.MCPServer) *harness.Runtime {
	t.Helper()
	cfg := config.Config{MCPServers: map[string]config.MCPServerSpec{}}
	for name, s := range servers {
		cfg.MCPServers[name] = config.MCPServerSpec{URL: s.URL()}
	}
	return familyRuntime(t, harness.NewMemStore(), f, nil, cfg, "")
}

// elapse waits d on the fake clock of a synctest bubble and lets every goroutine settle.
func elapse(d time.Duration) {
	<-time.NewTimer(d).C
	synctest.Wait()
}

// mcpNotice returns the newest MCP notice that req pins.
func mcpNotice(req turn.Request) string {
	pins := slices.DeleteFunc(req.Pins, func(p turn.Pin) bool { return p.Kind != "mcp" })
	if len(pins) == 0 {
		return ""
	}
	return pins[len(pins)-1].Text
}

func toolNames(req turn.Request) []string {
	var out []string
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	return out
}

func TestMCPServerDownAtBootComesUpAfterARetry(t *testing.T) {
	weather, docs := harnesstest.NewMCPServer(t, mcpWeatherSpec()), harnesstest.NewMCPServer(t, mcpDocsSpec())
	weather.FailInitialize(1)
	synctest.Test(t, func(t *testing.T) {
		f := mcpActions()
		r := mcpRuntime(t, f, map[string]*harnesstest.MCPServer{"weather": weather, "docs": docs})
		s := create(t, r)
		submit(t, s, text("a", "first"))
		req, _ := f.last("s1", "first")
		if got := mcpNotice(req); got != mcpDownNotice || slices.Contains(toolNames(req), "mcp__weather__forecast") || !slices.Contains(toolNames(req), "mcp__docs__search") {
			t.Errorf("first call tools %v, notice %q; want docs tools only and the notice", toolNames(req), got)
		}
		prompt := req.Instructions
		elapse(time.Second)
		submit(t, s, text("b", "second"))
		req, _ = f.last("s1", "second")
		want := []string{"mcp__docs__search", "mcp__weather__forecast"}
		if got := slices.DeleteFunc(toolNames(req), func(n string) bool { return !strings.HasPrefix(n, "mcp__") }); !slices.Equal(got, want) || mcpNotice(req) != mcpRecovered || req.Instructions != prompt {
			t.Errorf("second call tools %v, notice %q; want %v, the recovery line, and the prompt of the first call", got, mcpNotice(req), want)
		}
		closeRuntime(t, r)
	})
}

func TestMCPServerThatStaysDownParksUntilTheModelConnects(t *testing.T) {
	weather := harnesstest.NewMCPServer(t, mcpWeatherSpec())
	weather.SetAvailable(false)
	const statusLine = `{"servers":[{"name":"weather","connected":false,"attempts":%d,"parked":%t,"reason":"initialize failed"}]}`
	synctest.Test(t, func(t *testing.T) {
		f := mcpActions()
		r := mcpRuntime(t, f, map[string]*harnesstest.MCPServer{"weather": weather})
		s := create(t, r)
		turnN := 0
		ask := func(in string) turn.Request {
			turnN++
			submit(t, s, text(fmt.Sprint("i", turnN), in))
			req, _ := f.last("s1", in)
			return req
		}
		last := func() string { res := f.results("s1"); return res[len(res)-1] }
		ask(mcpStatusCall)
		if got, want := last(), fmt.Sprintf(statusLine, 1, false); got != want {
			t.Errorf("status at boot = %s, want %s", got, want)
		}
		elapse(time.Second)
		if got := mcpNotice(ask(mcpStatusCall)); got != mcpDownNotice {
			t.Errorf("notice after one retry = %q, want the retrying notice", got)
		}
		if got, want := last(), fmt.Sprintf(statusLine, 2, false); got != want {
			t.Errorf("status after one retry = %s, want %s", got, want)
		}
		elapse(time.Hour)
		if got := mcpNotice(ask(mcpStatusCall)); got != mcpParkedNotice {
			t.Errorf("notice after the last retry = %q, want the parked notice", got)
		}
		if got, want := last(), fmt.Sprintf(statusLine, 4, true); got != want {
			t.Errorf("status after the last retry = %s, want %s", got, want)
		}
		weather.SetAvailable(true)
		ask("connect")
		req := ask("after")
		if mcpNotice(req) != mcpRecovered || !slices.Contains(toolNames(req), "mcp__weather__forecast") {
			t.Errorf("call after connect has tools %v, notice %q; want the tool and the recovery line", toolNames(req), mcpNotice(req))
		}
		closeRuntime(t, r)
	})
}
