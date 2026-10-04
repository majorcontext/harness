package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/mcpserver"
	"github.com/majorcontext/harness/protocol"
)

type mcpConfigFile struct {
	MCPServers map[string]map[string]any `json:"mcpServers"`
}

// hist is the history tool that every turn of a backend that owns its loop gets.
const hist = "get_conversation_history"

func TestClaudeCodeGetsTheConfiguredMCPServers(t *testing.T) {
	reg := mcpserver.NewRegistry("gateway", "1")
	reg.RegisterTool(mcp.Tool{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (mcp.CallToolResult, error) { return mcp.CallToolResult{}, nil })
	gateway := httptest.NewServer(reg)
	defer gateway.Close()
	stdio := map[string]any{"command": "chrome-devtools-mcp-absent", "args": []any{"--headless"}, "env": map[string]any{"A": "1"}}
	remote := map[string]any{"type": "http", "url": gateway.URL, "headers": map[string]any{"Authorization": "Bearer t"}}
	for _, tc := range []struct {
		name    string
		allowed []string
		env     []string
		offered []string
		want    map[string]map[string]any
	}{
		{name: "the CLI gets every configured server beside the bridge", offered: []string{"echo", hist},
			want: map[string]map[string]any{"chrome-devtools": stdio, "gateway": remote}},
		{name: "a restricted turn keeps the configured servers on the bridge", allowed: []string{"Read", "echo", "mcp__gateway__ping"},
			env: []string{toolsInit, `["Read"]`}, offered: []string{"echo", hist, "mcp__gateway__ping"}, want: map[string]map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mcpLog, configLog := filepath.Join(dir, "mcp"), filepath.Join(dir, "config")
			argvLog := fakeClaude(t, "mcp", append(tc.env, "FAKE_CLAUDE_MCP_CALL", "echo", "FAKE_CLAUDE_MCP_LOG", mcpLog,
				"FAKE_CLAUDE_MCP_CONFIG_LOG", configLog)...)
			bin, err := fakeClaudeBin()
			if err != nil {
				t.Fatal(err)
			}
			r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Tools: []harness.Tool{newProbe("echo", false)}, Config: config.Config{
				Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin}},
				MCPServers: map[string]config.MCPServerSpec{
					"chrome-devtools": {Command: []string{"chrome-devtools-mcp-absent", "--headless"}, Env: []string{"A=1", "malformed"}, Dir: dir},
					"gateway":         {URL: gateway.URL, Headers: map[string]string{"Authorization": "Bearer t"}},
				}}})
			if err != nil {
				t.Fatal(err)
			}
			defer closeRuntime(t, r)
			turnOf(t, createClaude(t, r, tc.allowed), text("a", "hi"))
			runs := jsonLines[mcpRun](t, mcpLog)
			if len(runs) != 1 || !slices.Equal(runs[0].Tools, tc.offered) {
				t.Fatalf("bridge runs = %+v, want one that offers %q", runs, tc.offered)
			}
			want := map[string]map[string]any{"harness": {"type": "http", "url": runs[0].URL}}
			for k, v := range tc.want {
				want[k] = v
			}
			if got := jsonLines[mcpConfigFile](t, configLog); len(got) != 1 || !reflect.DeepEqual(got[0].MCPServers, want) {
				t.Errorf("--mcp-config = %+v, want %+v", got, want)
			}
			if argv := jsonLines[[]string](t, argvLog)[0]; !slices.Contains(argv, "--strict-mcp-config") {
				t.Errorf("argv = %q, want --strict-mcp-config", argv)
			}
		})
	}
}

type lookup struct{}

func (lookup) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "lookup"} }

func (lookup) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, nil
}

// probe is an embedder tool that sends each call to ended when it returns.
// A probe with started closes it, then runs until its ctx ends.
type probe struct {
	name    string
	started chan struct{}
	ended   chan protocol.ToolCall
}

func newProbe(name string, blocks bool) probe {
	p := probe{name: name, ended: make(chan protocol.ToolCall, 1)}
	if blocks {
		p.started = make(chan struct{})
	}
	return p
}

func (p probe) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: p.name} }

func (p probe) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	defer func() { p.ended <- c }()
	if p.started != nil {
		close(p.started)
		<-ctx.Done()
		return protocol.ToolResult{}, context.Cause(ctx)
	}
	return protocol.ToolResult{Text: p.name + " " + string(c.Arguments)}, nil
}

// mcpRun is what the fake CLI logged of the harness MCP endpoint.
type mcpRun struct {
	URL   string
	Tools []string
}

// endedCall returns the call that p ran to its end, and checks that the
// MCP endpoint of run and the --mcp-config file of argv are gone.
func endedCall(t *testing.T, p probe, run mcpRun, argv []string) protocol.ToolCall {
	t.Helper()
	if resp, err := http.Post(run.URL, "application/json", strings.NewReader("{}")); err == nil {
		_ = resp.Body.Close()
		t.Errorf("POST %s after the turn = %s, want a closed endpoint", run.URL, resp.Status)
	}
	if i := slices.Index(argv, "--mcp-config"); i < 0 {
		t.Errorf("argv = %q, want --mcp-config", argv)
	} else if _, err := os.Stat(argv[i+1]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s after the turn: %v, want it removed", argv[i+1], err)
	}
	select {
	case c := <-p.ended:
		return c
	default:
		t.Fatalf("tool %s did not return before the turn ended", p.name)
		return protocol.ToolCall{}
	}
}

func TestClaudeCodeRunsEmbedderToolsOverMCP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		env     []string
		offered []string
		args    []string
	}{
		{name: "a tool call through MCP runs the embedder tool and hides the operator MCP servers", offered: []string{"echo", "hidden", hist},
			args: []string{"--strict-mcp-config", "--allowedTools", "mcp__harness"}},
		{name: "a restricted tool is not offered", allowed: []string{"Read", "echo"}, env: []string{toolsInit, `["Read"]`},
			offered: []string{"echo", hist}, args: []string{"--tools", "Read", "--strict-mcp-config"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mcpLog := filepath.Join(t.TempDir(), "mcp")
			argvLog := fakeClaude(t, "mcp", append(tc.env, "FAKE_CLAUDE_MCP_CALL", "echo", "FAKE_CLAUDE_MCP_LOG", mcpLog)...)
			st, echo := harness.NewMemStore(), newProbe("echo", false)
			r := retryingRuntime(t, st, nil, false, 0, nil, echo, newProbe("hidden", false))
			defer closeRuntime(t, r)
			turnOf(t, createClaude(t, r, tc.allowed), text("a", "hi"))
			wantLog(t, st, 2, "input.admitted a", "turn.started a", "backend.state", "item.completed assistant toolu_m",
				`item.completed tool toolu_m echo {"q":"hi"}`, "item.completed assistant done", "context.measured", "turn.ended completed")
			runs := jsonLines[mcpRun](t, mcpLog)
			if len(runs) != 1 || !slices.Equal(runs[0].Tools, tc.offered) {
				t.Fatalf("MCP runs = %+v, want one that offers %q", runs, tc.offered)
			}
			argv := jsonLines[[]string](t, argvLog)[0]
			if c := endedCall(t, echo, runs[0], argv); c.ID != "toolu_m" || c.Name != "echo" {
				t.Errorf("call = %+v, want ID toolu_m and name echo", c)
			}
			if !hasArgs(argv, tc.args...) {
				t.Errorf("argv = %q, want %q in it", argv, tc.args)
			}
			if names := toolPartNames(t, st); !slices.Equal(names, []string{"echo", "echo"}) {
				t.Errorf("recorded tool part names = %q, want the embedder name echo for the call and the result", names)
			}
		})
	}
}

func TestClaudeCodeInterruptStopsAnMCPToolCall(t *testing.T) {
	mcpLog := filepath.Join(t.TempDir(), "mcp")
	argvLog := fakeClaude(t, "mcp", "FAKE_CLAUDE_MCP_CALL", "block", "FAKE_CLAUDE_MCP_LOG", mcpLog, "FAKE_CLAUDE_SIGNAL_LOG", filepath.Join(t.TempDir(), "signals"))
	st, block := harness.NewMemStore(), newProbe("block", true)
	r := retryingRuntime(t, st, nil, false, 0, nil, block)
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	if _, err := s.Submit(bg, text("a", "hi")); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for e, err := range s.Events(bg, 0) {
			if err != nil || e.Kind == "turn.ended" {
				return
			}
		}
	}()
	select {
	case <-block.started:
	case <-ended:
		t.Fatal("the turn ended before the tool started")
	}
	if err := s.Interrupt(bg, protocol.Interrupt{}); err != nil {
		t.Fatal(err)
	}
	<-ended
	wantLog(t, st, 2, "input.admitted a", "turn.started a", "backend.state", "item.completed assistant toolu_m",
		"context.measured", "item.completed tool toolu_m "+interrupted, "turn.ended interrupted stopped")
	if c := endedCall(t, block, jsonLines[mcpRun](t, mcpLog)[0], jsonLines[[]string](t, argvLog)[0]); c.ID != "toolu_m" {
		t.Errorf("call = %+v, want ID toolu_m", c)
	}
}
