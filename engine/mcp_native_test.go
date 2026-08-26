package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func nativeRef() message.ModelRef {
	return message.ModelRef{Provider: "anthropic", Model: "claude-opus-5"}
}

func clientRef() message.ModelRef {
	// A real model on a route with no server-side tool search.
	return message.ModelRef{Provider: "bifrost", Model: "anthropic/claude-opus-5"}
}

// TestNativeModeDefersWithoutACatalog is the core mode split: on a capable
// anthropic model the provider owns discovery, so every definition is sent
// (the API needs them to search and to expand a reference), the deferred
// ones are marked, and harness renders NO catalog segment — a second copy
// of the same list would spend the tokens deferral exists to save.
func TestNativeModeDefersWithoutACatalog(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy, Model: nativeRef()}, map[string]int{"a": 3})
	plan := s.planMCPToolsForModel(s.cfg.MCP.Tools(context.Background()), renderCatalogSegment, nativeRef())

	if !plan.native {
		t.Fatal("plan is not native on a tool-search-capable model")
	}
	if plan.catalog != "" {
		t.Fatalf("native mode rendered a catalog segment:\n%s", plan.catalog)
	}
	if len(plan.defs) != 3 {
		t.Fatalf("native mode sent %d defs, want all 3 — the API needs every definition", len(plan.defs))
	}
	for _, d := range plan.defs {
		if !d.DeferLoading {
			t.Fatalf("%q was not marked defer_loading", d.Name)
		}
		if len(d.InputSchema) == 0 {
			t.Fatalf("%q was sent without its schema", d.Name)
		}
	}
}

// TestClientModeUnchangedOnOtherProviders keeps every non-capable route on
// harness's own mechanism, which is the only one those routes have.
func TestClientModeUnchangedOnOtherProviders(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy, Model: clientRef()}, map[string]int{"a": 3})
	plan := s.planMCPToolsForModel(s.cfg.MCP.Tools(context.Background()), renderCatalogSegment, clientRef())

	if plan.native {
		t.Fatal("plan went native on a route with no server-side tool search")
	}
	if plan.catalog == "" {
		t.Fatal("client mode rendered no catalog segment, so nothing tells the model the tools exist")
	}
	if len(plan.defs) != 0 {
		t.Fatalf("client mode sent %d deferred defs, want 0 until selected", len(plan.defs))
	}
	for _, d := range plan.defs {
		if d.DeferLoading {
			t.Fatalf("%q carries DeferLoading on a route that ignores it", d.Name)
		}
	}
}

// TestNativeModeIgnoresSelectionState encodes the continuation rule that
// makes native deferral survive a reload with no harness bookkeeping: the
// API expands tool_reference blocks throughout the conversation history, so
// a discovered tool stays usable across later turns without re-searching
// and without a selection record. Native plans must therefore neither
// consult nor prune the selected set.
func TestNativeModeIgnoresSelectionState(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy, Model: nativeRef()}, map[string]int{"a": 2})
	ctx := context.Background()
	invented := mcpToolName("a", "no_such_tool")
	s.markMCPToolsSelected(invented)

	plan := s.planMCPToolsForModel(s.cfg.MCP.Tools(ctx), renderCatalogSegment, nativeRef())
	if len(plan.defs) != 2 {
		t.Fatalf("native plan sent %d defs, want the catalog's 2", len(plan.defs))
	}
	// Not reaped: the set is simply not native mode's business.
	if !s.mcpToolSelected(invented) {
		t.Fatalf("native mode pruned %q; selection is the client-side path's state", invented)
	}
}

// TestModelSwapMovesBetweenMechanisms is the mid-session case that must
// never strand a session: swapping to a model without server-side search
// has to bring harness's own catalog back, and swapping to a capable one
// has to drop it.
func TestModelSwapMovesBetweenMechanisms(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy, Model: nativeRef()}, map[string]int{"a": 3})
	all := s.cfg.MCP.Tools(context.Background())

	native := s.planMCPToolsForModel(all, renderCatalogSegment, nativeRef())
	if native.catalog != "" || !native.native {
		t.Fatal("expected a native plan for the capable model")
	}
	client := s.planMCPToolsForModel(all, renderCatalogSegment, clientRef())
	if client.native || client.catalog == "" {
		t.Fatal("after a swap to a non-capable model the session has no discovery path")
	}
	if !strings.Contains(client.catalog, mcpToolName("a", "tool00")) {
		t.Fatalf("catalog does not list the deferred tools:\n%s", client.catalog)
	}
}

// TestNativeRequestCarriesDeferLoading drives Session.Prompt and asserts on
// the provider.Request the adapter would transcode — the production entry
// point, not a hand-built plan.
func TestNativeRequestCarriesDeferLoading(t *testing.T) {
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 3)}
	prov := &scriptedProvider{name: "anthropic", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	var seen []provider.ToolDef
	var system []string
	s := NewSession(Config{
		Providers:      provider.Registry{"anthropic": prov},
		Model:          nativeRef(),
		MCP:            reg,
		MCPToolLoading: MCPToolLoadingLazy,
		OnRequest: func(_ string, _ int, req *provider.Request) {
			seen = append([]provider.ToolDef(nil), req.Tools...)
			system = append([]string(nil), req.System...)
		},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var mcpDefs, deferred int
	for _, d := range seen {
		if isMCPToolName(d.Name) {
			mcpDefs++
			if d.DeferLoading {
				deferred++
			}
		} else if d.DeferLoading {
			t.Fatalf("built-in tool %q was marked defer_loading", d.Name)
		}
	}
	if mcpDefs != 3 || deferred != 3 {
		t.Fatalf("request carried %d MCP defs (%d deferred), want 3/3", mcpDefs, deferred)
	}
	for _, seg := range system {
		if strings.HasPrefix(seg, mcpCatalogHeader) {
			t.Fatalf("native session still shipped a catalog segment:\n%s", seg)
		}
	}
	if _, err := json.Marshal(seen); err != nil {
		t.Fatal(err)
	}
}
