package engine

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestToolDefsSortedByName guards the prompt-cache prefix. Session.tools is a
// map, so an unsorted toolDefs emits the built-in tool array in a different
// order on every request. Tools sit at the FRONT of the cached prefix on every
// provider (Anthropic caches tools, then system, then messages), so one
// reordering invalidates the whole prefix and rewrites it — measured live on
// 2026-08-25: two consecutive turns of one session both reported
// cache_creation_input_tokens > 0 and cache_read_input_tokens = 0, because the
// tools array had reshuffled between them.
//
// The loop runs the production entry point repeatedly: Go randomizes map
// iteration per range, so one call proves little and many calls make an
// unsorted build fail every time.
func TestToolDefsSortedByName(t *testing.T) {
	s := NewSession(Config{ModelTool: true})
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		defs := s.toolDefs(ctx)
		if len(defs) < 2 {
			t.Fatalf("toolDefs returned %d defs, want the built-in set", len(defs))
		}
		names := make([]string, len(defs))
		for j, d := range defs {
			names[j] = d.Name
		}
		if !sort.StringsAreSorted(names) {
			t.Fatalf("call %d: tool names not sorted: %v", i, names)
		}
	}
}

// TestToolDefsByteIdenticalAcrossCalls is the property the cache actually
// needs: two requests built from the same session must serialize the same
// tools array byte for byte.
func TestToolDefsByteIdenticalAcrossCalls(t *testing.T) {
	s := NewSession(Config{ModelTool: true})
	ctx := context.Background()
	first, err := json.Marshal(s.toolDefs(ctx))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := json.Marshal(s.toolDefs(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("call %d differs from call 0:\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}

// TestToolDefsGroupOrderIsBuiltinsThenMCPThenPlugins: sorting applies WITHIN
// the built-in group only. MCP tools follow (already sorted by server, then
// tool, by rebuildToolsLocked) and plugin tools come last, in configured
// order. Keeping the groups in place means adding an MCP server never
// reorders the built-in block ahead of it.
func TestToolDefsGroupOrderIsBuiltinsThenMCPThenPlugins(t *testing.T) {
	mcp := &stubOrderRegistry{defs: []provider.ToolDef{
		{Name: "mcp__srv__alpha", Description: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "mcp__srv__beta", Description: "b", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	s := NewSession(Config{MCP: mcp})
	defs := s.toolDefs(context.Background())
	var builtinCount int
	for _, d := range defs {
		if len(d.Name) > 5 && d.Name[:5] == "mcp__" {
			break
		}
		builtinCount++
	}
	if builtinCount == 0 || builtinCount == len(defs) {
		t.Fatalf("expected built-ins then MCP tools, got %d built-ins of %d defs", builtinCount, len(defs))
	}
	want := []string{"mcp__srv__alpha", "mcp__srv__beta"}
	for i, w := range want {
		if got := defs[builtinCount+i].Name; got != w {
			t.Errorf("def[%d] = %q, want %q", builtinCount+i, got, w)
		}
	}
}

// stubOrderRegistry is a minimal MCPRegistry that only reports tools.
type stubOrderRegistry struct {
	defs []provider.ToolDef
}

func (s *stubOrderRegistry) Tools(context.Context) []provider.ToolDef { return s.defs }

func (s *stubOrderRegistry) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

func (s *stubOrderRegistry) CallServerTool(context.Context, string, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}
