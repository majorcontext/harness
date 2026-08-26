package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// End-to-end coverage for lazy MCP tools: a REAL *MCPManager speaking the
// real Streamable HTTP transport to an httptest MCP server, driven through
// a real Session.Prompt turn. Everything below the provider is production
// code -- connect, tools/list, the plan, the catalog segment, the mcp tool's
// own select action, and tools/call back to the same server.
//
// The provider is scripted, because a live model is the one component a
// test cannot make deterministic. The transport is in-process for the
// reason AGENTS.md gives: the subprocess machinery is not what is under
// test here, so a stdio fixture would add a process boundary and buy
// nothing.

// lazyE2EServer builds an httptest MCP server exposing n tools, each with a
// canned text result naming itself.
func lazyE2EServer(t *testing.T, n int) (*fakeMCPHTTPServer, string) {
	t.Helper()
	if n > 26 {
		// The letter-suffix naming below collides past 'z'; fail loudly
		// instead of silently degrading a future caller's tool names.
		t.Fatalf("lazyE2EServer supports at most 26 tools, got %d", n)
	}
	srv := &fakeMCPHTTPServer{}
	for i := 0; i < n; i++ {
		name := "tool" + string(rune('a'+i))
		srv.tools = append(srv.tools, fakeMCPTool{
			name:        name,
			description: "does " + name,
			content:     []map[string]any{textContent("ran " + name)},
		})
	}
	return srv, srv.start(t)
}

// TestLazyMCPEndToEnd is the whole feature in one turn, against a real
// server: the model sees a catalog and no schemas, selects one tool through
// the real mcp tool, gets exactly that schema on the next round, calls it,
// and the call reaches the server.
func TestLazyMCPEndToEnd(t *testing.T) {
	srv, url := lazyE2EServer(t, 4)
	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	target := mcpToolName("weather", "toolb")
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		// Round 1: the model can only reach the catalog, so it selects.
		asstTurn(provider.StopToolUse, toolCall("tc1", mcpSessionToolName,
			`{"action":"select","tools":["`+target+`"]}`)),
		// Round 2: the schema is loaded, so it calls the tool directly.
		asstTurn(provider.StopToolUse, toolCall("tc2", target, `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}

	type round struct {
		system []string
		tools  []string
	}
	var rounds []round
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
		// auto with a threshold below the server's catalog: the same
		// decision a real box makes, rather than a blanket lazy.
		MCPToolLoading:          MCPToolLoadingAuto,
		MCPToolLoadingThreshold: 2,
		OnRequest: func(_ string, _ int, req *provider.Request) {
			r := round{system: append([]string(nil), req.System...)}
			for _, d := range req.Tools {
				if isMCPToolName(d.Name) {
					r.tools = append(r.tools, d.Name)
				}
			}
			rounds = append(rounds, r)
		},
	})

	if _, err := s.Prompt(context.Background(), "check the weather"); err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 3 {
		t.Fatalf("saw %d requests, want 3", len(rounds))
	}

	// Round 1: no MCP schema at all, and a catalog naming every tool.
	if len(rounds[0].tools) != 0 {
		t.Fatalf("round 1 carried MCP schemas %v, want none", rounds[0].tools)
	}
	catalog := soleSegment(t, rounds[0].system, mcpCatalogHeader)
	for _, want := range []string{"mcp__weather__toola", target, "mcp__weather__toolc", "mcp__weather__toold"} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("catalog does not list %q:\n%s", want, catalog)
		}
	}
	if !strings.Contains(catalog, "does toolb") {
		t.Fatalf("catalog dropped the server's own description:\n%s", catalog)
	}

	// Round 2: exactly the selected schema, and the catalog no longer lists
	// it.
	if len(rounds[1].tools) != 1 || rounds[1].tools[0] != target {
		t.Fatalf("round 2 carried %v, want exactly [%s]", rounds[1].tools, target)
	}
	catalog2 := soleSegment(t, rounds[1].system, mcpCatalogHeader)
	if strings.Contains(catalog2, target) {
		t.Fatalf("catalog still lists the loaded tool:\n%s", catalog2)
	}
	if !strings.Contains(catalog2, "mcp__weather__toola") {
		t.Fatalf("catalog dropped an unselected tool:\n%s", catalog2)
	}

	// The call reached the real server, unnamespaced.
	if len(srv.calls) != 1 || srv.calls[0] != "toolb" {
		t.Fatalf("server saw calls %v, want exactly [toolb]", srv.calls)
	}

	// The tool's own output reached history, so the model actually got the
	// result rather than an error about an unknown tool.
	var got string
	for _, m := range s.History() {
		for _, p := range m.Parts {
			if tr, ok := p.(*message.ToolResult); ok && tr.CallID == "tc2" {
				for _, c := range tr.Content {
					if txt, ok := c.(*message.Text); ok {
						got = txt.Text
					}
				}
			}
		}
	}
	if got != "ran toolb" {
		t.Fatalf("tool result = %q, want %q", got, "ran toolb")
	}
}

// TestLazyMCPEndToEndSurvivesReload continues the same session across a
// LoadSession round trip against the same live server: the selection is
// restored from the log, the schema is back in the tools array on the first
// request after the reload, and the model calls the tool with no second
// select.
func TestLazyMCPEndToEndSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	srv, url := lazyE2EServer(t, 4)
	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	target := mcpToolName("weather", "toolc")
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", mcpSessionToolName,
			`{"action":"select","tools":["`+target+`"]}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "loaded"}),
	}}
	cfg := Config{
		Providers:               provider.Registry{"test": prov},
		Model:                   message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir:              dir,
		MCP:                     mgr,
		MCPToolLoading:          MCPToolLoadingLazy,
		MCPToolLoadingThreshold: 2,
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "load it"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	// A fresh process would build a fresh session from the same log. The
	// second script calls the tool straight away: no select, because the
	// restored selection must already have loaded it.
	prov2 := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc9", target, `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg2 := cfg
	cfg2.Providers = provider.Registry{"test": prov2}
	var firstTools []string
	cfg2.OnRequest = func(_ string, _ int, req *provider.Request) {
		if firstTools != nil {
			return
		}
		firstTools = []string{}
		for _, d := range req.Tools {
			if isMCPToolName(d.Name) {
				firstTools = append(firstTools, d.Name)
			}
		}
	}
	loaded, err := LoadSession(cfg2, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Prompt(context.Background(), "use it"); err != nil {
		t.Fatal(err)
	}
	if len(firstTools) != 1 || firstTools[0] != target {
		t.Fatalf("first request after the reload carried %v, want the restored [%s]", firstTools, target)
	}
	// The load-bearing restore check is firstTools above: CallTool routes
	// by name whether or not the schema was restored, so this call
	// assertion only proves the wired transport still works end to end —
	// it cannot catch a broken restore on its own (review note).
	if len(srv.calls) != 1 || srv.calls[0] != "toolc" {
		t.Fatalf("server saw calls %v, want exactly [toolc]", srv.calls)
	}
}

// TestLazyMCPEndToEndSearchFindsADeferredTool drives the discovery half
// against the real server: search ranks a tool the model cannot yet call
// and reports it as not loaded, then select flips that answer.
func TestLazyMCPEndToEndSearchFindsADeferredTool(t *testing.T) {
	_, url := lazyE2EServer(t, 4)
	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	s := NewSession(Config{MCP: mgr, MCPToolLoading: MCPToolLoadingLazy})
	var res mcpSearchResult
	runMCPAction(t, s, `{"action":"search","query":"toold"}`, &res)
	if len(res.Matches) != 1 || res.Matches[0].Name != mcpToolName("weather", "toold") {
		t.Fatalf("matches = %+v, want the one toold entry", res.Matches)
	}
	if res.Matches[0].Server != "weather" || res.Matches[0].Loaded {
		t.Fatalf("match = %+v, want server weather and loaded=false", res.Matches[0])
	}

	runMCPAction(t, s, `{"action":"select","tools":["`+mcpToolName("weather", "toold")+`"]}`, &mcpSelectResult{})
	runMCPAction(t, s, `{"action":"search","query":"toold"}`, &res)
	if !res.Matches[0].Loaded {
		t.Fatalf("match after select = %+v, want loaded=true", res.Matches[0])
	}
}

// TestLazyMCPEndToEndEagerIsUnchanged is the zero-behavior-change control:
// the same server, the same manager, no configuration, and every schema is
// in the tools array with no catalog segment at all.
func TestLazyMCPEndToEndEagerIsUnchanged(t *testing.T) {
	_, url := lazyE2EServer(t, 4)
	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	var system []string
	var tools []string
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
		OnRequest: func(_ string, _ int, req *provider.Request) {
			system = append([]string(nil), req.System...)
			for _, d := range req.Tools {
				if isMCPToolName(d.Name) {
					tools = append(tools, d.Name)
				}
			}
		},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("eager session carried %d MCP schemas %v, want all 4", len(tools), tools)
	}
	for _, seg := range system {
		if strings.HasPrefix(seg, mcpCatalogHeader) {
			t.Fatalf("eager session rendered a catalog segment:\n%s", seg)
		}
	}
	// The mcp tool advertises its original two actions only.
	schema := string(s.tools[mcpSessionToolName].Def.InputSchema)
	if strings.Contains(schema, `"select"`) {
		t.Fatalf("eager session advertises select:\n%s", schema)
	}
}

// soleSegment returns the one system segment starting with prefix, failing
// the test when there is not exactly one.
func soleSegment(t *testing.T, system []string, prefix string) string {
	t.Helper()
	var found []string
	for _, seg := range system {
		if strings.HasPrefix(seg, prefix) {
			found = append(found, seg)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d segments with the expected prefix, want 1:\n%q", len(found), system)
	}
	return found[0]
}
