package harness_test

import (
	"errors"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
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
