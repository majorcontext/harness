package claudecode_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/mcpserver"
)

type mcpConfigFile struct {
	MCPServers map[string]map[string]any `json:"mcpServers"`
}

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
		{name: "the CLI gets every configured server beside the bridge", offered: []string{"echo"},
			want: map[string]map[string]any{"chrome-devtools": stdio, "gateway": remote}},
		{name: "a restricted turn keeps the configured servers on the bridge", allowed: []string{"Read", "echo", "mcp__gateway__ping"},
			env: []string{toolsInit, `["Read"]`}, offered: []string{"echo", "mcp__gateway__ping"}, want: map[string]map[string]any{}},
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
