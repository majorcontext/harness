package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// tsTool is a client tool def, deferred or not.
func tsTool(name string, defer_ bool) provider.ToolDef {
	return provider.ToolDef{
		Name:         name,
		Description:  "does " + name,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		DeferLoading: defer_,
	}
}

func tsRequest(tools ...provider.ToolDef) *provider.Request {
	return &provider.Request{
		Model:    message.ModelRef{Provider: "anthropic", Model: "claude-opus-5"},
		System:   []string{"sys"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		Tools:    tools,
	}
}

// TestDeferredToolsEmitSearchToolAndDeferLoading is the wire shape from the
// tool-search doc: the search tool entry, every definition still sent, and
// defer_loading only on the tools the caller deferred.
func TestDeferredToolsEmitSearchToolAndDeferLoading(t *testing.T) {
	out, err := transcodeRequest(tsRequest(
		tsTool("bash", false),
		tsTool("mcp__github__create_issue", true),
		tsTool("mcp__github__list_issues", true),
	), DefaultCacheTTL)
	if err != nil {
		t.Fatal(err)
	}

	// The search tool leads, so the array's opening bytes are stable.
	if len(out.Tools) != 4 {
		t.Fatalf("got %d tools, want the search tool plus all 3 definitions", len(out.Tools))
	}
	if out.Tools[0].Type != toolSearchToolType || out.Tools[0].Name != toolSearchToolName {
		t.Fatalf("first tool = %+v, want the bm25 search tool", out.Tools[0])
	}
	// A server tool entry carries type+name only.
	if out.Tools[0].Description != "" || len(out.Tools[0].InputSchema) != 0 || out.Tools[0].DeferLoading {
		t.Fatalf("search tool entry carries client-tool fields: %+v", out.Tools[0])
	}

	// Every definition is still sent, deferred or not — the API needs them
	// server-side to run the search and expand references.
	byName := map[string]apiToolDef{}
	for _, tool := range out.Tools[1:] {
		byName[tool.Name] = tool
		if len(tool.InputSchema) == 0 {
			t.Errorf("%q was sent without its input schema", tool.Name)
		}
	}
	if byName["bash"].DeferLoading {
		t.Error("a non-deferred tool was marked defer_loading")
	}
	for _, name := range []string{"mcp__github__create_issue", "mcp__github__list_issues"} {
		if !byName[name].DeferLoading {
			t.Errorf("%q was not marked defer_loading", name)
		}
	}
}

// TestNoDeferredToolsEmitsNoSearchTool keeps the default path byte-identical
// to what it was before native delegation existed.
func TestNoDeferredToolsEmitsNoSearchTool(t *testing.T) {
	out, err := transcodeRequest(tsRequest(tsTool("bash", false), tsTool("read_file", false)), DefaultCacheTTL)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("got %d tools, want exactly the 2 client tools", len(out.Tools))
	}
	for _, tool := range out.Tools {
		if tool.Type != "" {
			t.Fatalf("a server tool entry appeared with nothing deferred: %+v", tool)
		}
	}
	raw, err := json.Marshal(out.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "defer_loading") {
		t.Fatalf("defer_loading reached the wire with nothing deferred: %s", raw)
	}
}

// TestAllToolsDeferredFallsBackToEager guards the one documented 400: "At
// least one tool must have defer_loading=false. All tools cannot be
// deferred." A caller that defers everything gets eager tools rather than a
// failed turn.
func TestAllToolsDeferredFallsBackToEager(t *testing.T) {
	out, err := transcodeRequest(tsRequest(tsTool("a", true), tsTool("b", true)), DefaultCacheTTL)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("got %d tools, want the 2 client tools with no search tool", len(out.Tools))
	}
	for _, tool := range out.Tools {
		if tool.DeferLoading {
			t.Fatalf("%q stayed deferred with no non-deferred tool to satisfy the API: %+v", tool.Name, tool)
		}
		if tool.Type != "" {
			t.Fatalf("a search tool was emitted with every tool deferred: %+v", tool)
		}
	}
}

// TestDeferredToolsCarryNoCacheControl is the cache-composition guard.
//
// harness places its cache breakpoints on the last SYSTEM block and the last
// message block, never inside the tools array (apiToolDef has no
// cache_control field at all), so "a deferred tool carrying a breakpoint" is
// unreachable by construction rather than by convention. That matters
// because Anthropic's caching doc puts the tool-definitions breakpoint on
// the last tool in the array — which, once tools are deferred, is a deferred
// tool. This test pins the property so a future per-tool breakpoint cannot
// land there without failing here first.
//
// Deferral does not weaken the caching harness does have: the API excludes
// deferred definitions from the system-prompt prefix, so the prefix the
// system breakpoint covers is untouched.
func TestDeferredToolsCarryNoCacheControl(t *testing.T) {
	out, err := transcodeRequest(tsRequest(
		tsTool("bash", false),
		tsTool("mcp__github__create_issue", true),
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_control") {
		t.Fatalf("a cache breakpoint reached the tools array: %s", raw)
	}
	// The breakpoint harness does set is still on the last system block,
	// and the deferred tool did not disturb it.
	if n := len(out.System); n == 0 || out.System[n-1].CacheControl == nil {
		t.Fatalf("system breakpoint missing with tools deferred: %+v", out.System)
	}
	if out.System[len(out.System)-1].CacheControl.TTL != CacheTTL1h {
		t.Fatalf("system breakpoint TTL = %q, want %q", out.System[len(out.System)-1].CacheControl.TTL, CacheTTL1h)
	}
}

// TestToolArrayByteStableWithDeferral is the #164 property, extended to the
// native path: two requests that change no tool state must serialize the
// same tools array, search tool included.
func TestToolArrayByteStableWithDeferral(t *testing.T) {
	tools := []provider.ToolDef{tsTool("bash", false), tsTool("mcp__a__x", true), tsTool("mcp__a__y", true)}
	first, err := transcodeRequest(tsRequest(tools...), DefaultCacheTTL)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(first.Tools)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		out, err := transcodeRequest(tsRequest(tools...), DefaultCacheTTL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(out.Tools)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("call %d differs:\nwant %s\ngot  %s", i, want, got)
		}
	}
}
