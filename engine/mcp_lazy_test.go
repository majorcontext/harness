package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// lazyFakeRegistry is a full MCPRegistry with the two narrow surfaces
// mcp_lazy.go reads through: ConfiguredNames (mcpConfigReader) and Status
// (mcpStatusReader). It never touches the network, so a test can pin an
// exact catalog and an exact per-server connection state.
type lazyFakeRegistry struct {
	tools     []provider.ToolDef
	names     []string
	connected map[string]bool
	calls     int
}

func (r *lazyFakeRegistry) Tools(context.Context) []provider.ToolDef {
	r.calls++
	return append([]provider.ToolDef(nil), r.tools...)
}

func (r *lazyFakeRegistry) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

func (r *lazyFakeRegistry) CallServerTool(context.Context, string, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

func (r *lazyFakeRegistry) ConfiguredNames() []string {
	return append([]string(nil), r.names...)
}

func (r *lazyFakeRegistry) Status() []MCPServerStatus {
	out := make([]MCPServerStatus, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, MCPServerStatus{Name: n, Connected: r.connected[n], Attempts: 1})
	}
	return out
}

// lazyTools builds n tools for one server, named tool00, tool01, ...
func lazyTools(server string, n int) []provider.ToolDef {
	out := make([]provider.ToolDef, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool%02d", i)
		out = append(out, provider.ToolDef{
			Name:        mcpToolName(server, name),
			Description: "does " + name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	return out
}

// lazySession builds a session whose MCP registry holds the given servers,
// each fully connected, with cfg applied on top.
func lazySession(t *testing.T, cfg Config, servers map[string]int) (*Session, *lazyFakeRegistry) {
	t.Helper()
	reg := &lazyFakeRegistry{connected: map[string]bool{}}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	// Deterministic registry order: sorted by server, then tool — the order
	// MCPManager.rebuildToolsLocked produces.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		reg.names = append(reg.names, name)
		reg.connected[name] = true
		reg.tools = append(reg.tools, lazyTools(name, servers[name])...)
	}
	cfg.MCP = reg
	return NewSession(cfg), reg
}

func mcpDefNames(defs []provider.ToolDef) []string {
	var out []string
	for _, d := range defs {
		if isMCPToolName(d.Name) {
			out = append(out, d.Name)
		}
	}
	return out
}

// # Effective mode

// TestResolveMCPLoadingModes pins resolveMCPLoading's per-server decision
// across every
// combination of global mode, per-server override, and the auto threshold —
// including the threshold boundary itself (at, one below, one above), which
// is where an off-by-one silently changes what the model can call.
func TestResolveMCPLoadingModes(t *testing.T) {
	tests := []struct {
		name      string
		global    MCPToolLoading
		threshold int
		override  map[string]MCPToolLoading
		tools     int // catalog size for server "a"
		want      bool
	}{
		{name: "unset global is eager", global: MCPToolLoadingUnset, tools: 50, want: false},
		{name: "explicit eager", global: MCPToolLoadingEager, tools: 50, want: false},
		{name: "lazy defers whatever the size", global: MCPToolLoadingLazy, tools: 1, want: true},
		{name: "auto below threshold", global: MCPToolLoadingAuto, threshold: 5, tools: 4, want: false},
		{name: "auto at threshold", global: MCPToolLoadingAuto, threshold: 5, tools: 5, want: false},
		{name: "auto one above threshold", global: MCPToolLoadingAuto, threshold: 5, tools: 6, want: true},
		{
			name: "per-server lazy under global eager", global: MCPToolLoadingEager,
			override: map[string]MCPToolLoading{"a": MCPToolLoadingLazy}, tools: 1, want: true,
		},
		{
			name: "per-server eager under global lazy", global: MCPToolLoadingLazy,
			override: map[string]MCPToolLoading{"a": MCPToolLoadingEager}, tools: 50, want: false,
		},
		{
			name: "per-server eager under global auto over threshold", global: MCPToolLoadingAuto,
			threshold: 5, override: map[string]MCPToolLoading{"a": MCPToolLoadingEager}, tools: 50, want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := lazySession(t, Config{
				MCPToolLoading:          tc.global,
				MCPToolLoadingThreshold: tc.threshold,
				MCPToolLoadingByServer:  tc.override,
			}, map[string]int{"a": tc.tools})

			plan := s.planMCPTools(context.Background())
			deferred := len(mcpDefNames(plan.defs)) == 0 && plan.catalog != ""
			if deferred != tc.want {
				t.Fatalf("deferred = %v, want %v (defs=%d, catalog empty=%v)",
					deferred, tc.want, len(mcpDefNames(plan.defs)), plan.catalog == "")
			}
		})
	}
}

// TestAutoThresholdCountsPinnedEagerServers proves the threshold measures
// the WHOLE catalog, pinned servers included. Server "pinned" holds enough
// tools to cross the threshold by itself; server "auto" holds one. If the
// count skipped pinned servers, "auto" would stay registered.
func TestAutoThresholdCountsPinnedEagerServers(t *testing.T) {
	s, _ := lazySession(t, Config{
		MCPToolLoading:          MCPToolLoadingAuto,
		MCPToolLoadingThreshold: 5,
		MCPToolLoadingByServer:  map[string]MCPToolLoading{"pinned": MCPToolLoadingEager},
	}, map[string]int{"pinned": 10, "auto": 1})

	plan := s.planMCPTools(context.Background())
	got := mcpDefNames(plan.defs)
	for _, name := range got {
		if strings.HasPrefix(name, mcpToolName("auto", "")) {
			t.Fatalf("tool %q from the auto server is registered; the pinned server's 10 tools should have crossed the threshold of 5", name)
		}
	}
	if len(got) != 10 {
		t.Fatalf("registered %d MCP defs, want the pinned server's 10", len(got))
	}
	if !strings.Contains(plan.catalog, mcpToolName("auto", "tool00")) {
		t.Fatalf("catalog does not list the deferred auto tool:\n%s", plan.catalog)
	}
}

// TestNonPositiveThresholdResolvesToDefault guards the always-defer bug: a
// clamp to a floor of 1 would make len(catalog) > 1 true for all but the
// emptiest catalog, so a zero or negative threshold must resolve to the
// engine default instead.
func TestNonPositiveThresholdResolvesToDefault(t *testing.T) {
	for _, threshold := range []int{0, -1, -100} {
		s, _ := lazySession(t, Config{
			MCPToolLoading:          MCPToolLoadingAuto,
			MCPToolLoadingThreshold: threshold,
		}, map[string]int{"a": 3})

		if got := s.mcpDeferThreshold(); got != defaultMCPDeferThreshold {
			t.Fatalf("threshold %d resolved to %d, want %d", threshold, got, defaultMCPDeferThreshold)
		}
		plan := s.planMCPTools(context.Background())
		if plan.catalog != "" {
			t.Fatalf("threshold %d deferred a 3-tool catalog:\n%s", threshold, plan.catalog)
		}
	}
}

// TestSessionWithoutMCPToolNeverDefers is the subagent-lockout guard. A
// child restricted to an agent definition that omits "mcp" has no select
// path, so deferring its schemas would take away the tools AND the only way
// to load them back.
func TestSessionWithoutMCPToolNeverDefers(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"a": 3})
	if _, ok := s.tools[mcpSessionToolName]; !ok {
		t.Fatal("test setup: session has no mcp tool to remove")
	}
	if err := restrictTools(s, []string{"bash"}); err != nil {
		t.Fatal(err)
	}

	if s.sessionCanDefer() {
		t.Fatal("sessionCanDefer = true for a session with no mcp tool")
	}
	plan := s.planMCPTools(context.Background())
	if plan.catalog != "" {
		t.Fatalf("a session with no mcp tool deferred anyway:\n%s", plan.catalog)
	}
	if got := len(mcpDefNames(plan.defs)); got != 3 {
		t.Fatalf("registered %d MCP defs, want all 3 eagerly", got)
	}
}

// TestSessionCanDeferBothDirections pins the advertisement gate slice 2
// consumes. A global-mode-only gate is wrong in BOTH directions, so both
// are asserted.
func TestSessionCanDeferBothDirections(t *testing.T) {
	tests := []struct {
		name     string
		global   MCPToolLoading
		override map[string]MCPToolLoading
		want     bool
	}{
		{name: "global eager, no override", global: MCPToolLoadingEager, want: false},
		{
			name: "global eager with one per-server lazy", global: MCPToolLoadingEager,
			override: map[string]MCPToolLoading{"b": MCPToolLoadingLazy}, want: true,
		},
		{
			name: "global lazy with every server pinned eager", global: MCPToolLoadingLazy,
			override: map[string]MCPToolLoading{"a": MCPToolLoadingEager, "b": MCPToolLoadingEager}, want: false,
		},
		{name: "global auto", global: MCPToolLoadingAuto, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := lazySession(t, Config{
				MCPToolLoading:         tc.global,
				MCPToolLoadingByServer: tc.override,
			}, map[string]int{"a": 1, "b": 1})
			if got := s.sessionCanDefer(); got != tc.want {
				t.Fatalf("sessionCanDefer = %v, want %v", got, tc.want)
			}
		})
	}
}

// # The tools array

// TestDeferredServerContributesZeroDefs asserts the surplus direction too:
// a deferred server contributes NO defs, not merely fewer, and an eager
// sibling contributes every one of its own.
func TestDeferredServerContributesZeroDefs(t *testing.T) {
	s, _ := lazySession(t, Config{
		MCPToolLoading:         MCPToolLoadingEager,
		MCPToolLoadingByServer: map[string]MCPToolLoading{"lazy": MCPToolLoadingLazy},
	}, map[string]int{"lazy": 4, "keep": 3})

	got := mcpDefNames(s.toolDefs(context.Background()))
	if len(got) != 3 {
		t.Fatalf("got %d MCP defs %v, want exactly the eager server's 3", len(got), got)
	}
	for _, name := range got {
		if strings.HasPrefix(name, "mcp__lazy__") {
			t.Fatalf("deferred server contributed %q", name)
		}
	}
}

// TestSelectedToolRejoinsTheArray proves the loaded half of the partition:
// a selected name from a deferred server is back in the tools array with
// its schema, and disappears from the catalog listing.
func TestSelectedToolRejoinsTheArray(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"a": 3})
	ctx := context.Background()
	want := mcpToolName("a", "tool01")

	if added := s.markMCPToolsSelected(want); len(added) != 1 || added[0] != want {
		t.Fatalf("markMCPToolsSelected = %v, want [%s]", added, want)
	}
	plan := s.planMCPTools(ctx)
	got := mcpDefNames(plan.defs)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("defs = %v, want exactly [%s]", got, want)
	}
	var schema provider.ToolDef
	for _, d := range plan.defs {
		if d.Name == want {
			schema = d
		}
	}
	if len(schema.InputSchema) == 0 {
		t.Fatalf("selected tool %q carries no input schema", want)
	}
	if strings.Contains(plan.catalog, want) {
		t.Fatalf("catalog still lists the selected tool %q:\n%s", want, plan.catalog)
	}
	if !strings.Contains(plan.catalog, mcpToolName("a", "tool00")) {
		t.Fatalf("catalog dropped an unselected tool:\n%s", plan.catalog)
	}
}

// TestMarkMCPToolsSelectedRejectsMalformedNames keeps a name no server can
// own out of the selected set, matching the select-time and replay rules.
func TestMarkMCPToolsSelectedRejectsMalformedNames(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"a": 1})
	for _, bad := range []string{"", "tool00", "mcp__", "mcp____tool", "mcp__a__", "bash"} {
		if added := s.markMCPToolsSelected(bad); len(added) != 0 {
			t.Fatalf("markMCPToolsSelected(%q) accepted it: %v", bad, added)
		}
		if s.mcpToolSelected(bad) {
			t.Fatalf("malformed name %q entered the selected set", bad)
		}
	}
}

// TestToolDefsByteStableUnderDeferral is the prompt-cache property, asserted
// on BYTES rather than membership (see AGENTS.md, "The tool array is
// byte-stable across requests"): repeated builds that change no selection
// must serialize identically, and one selection must change the array
// exactly once and then hold still again.
func TestToolDefsByteStableUnderDeferral(t *testing.T) {
	s, _ := lazySession(t, Config{
		MCPToolLoading:          MCPToolLoadingAuto,
		MCPToolLoadingThreshold: 2,
	}, map[string]int{"a": 3, "b": 3})
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

	s.markMCPToolsSelected(mcpToolName("b", "tool00"))
	afterSelect, err := json.Marshal(s.toolDefs(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSelect) == string(first) {
		t.Fatal("selecting a deferred tool did not change the tools array")
	}
	for i := 0; i < 20; i++ {
		got, err := json.Marshal(s.toolDefs(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(afterSelect) {
			t.Fatalf("post-select call %d differs:\nwant: %s\ngot:  %s", i, afterSelect, got)
		}
	}
}

// TestPlanCallsRegistryToolsOnce guards the one-call rule: Tools() is what
// triggers a server's first connect, so building the defs and the catalog
// must not dial twice.
func TestPlanCallsRegistryToolsOnce(t *testing.T) {
	s, reg := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"a": 3})
	reg.calls = 0
	if _, catalog := s.toolDefsWithCatalog(context.Background()); catalog == "" {
		t.Fatal("expected a catalog for a lazy session")
	}
	if reg.calls != 1 {
		t.Fatalf("MCPRegistry.Tools called %d times, want 1", reg.calls)
	}
}

// # The catalog segment

// TestCatalogSegmentExactText pins the rendered bytes, header included: the
// segment is the only in-band documentation the model gets for a deferred
// tool, and a golden comparison is what stops the format drifting.
func TestCatalogSegmentExactText(t *testing.T) {
	deferred := []provider.ToolDef{
		{Name: mcpToolName("z", "beta"), Description: "second tool"},
		{Name: mcpToolName("a", "alpha"), Description: "first tool\nwith a second line that must not appear"},
		{Name: mcpToolName("a", "gamma"), Description: ""},
	}
	want := mcpCatalogHeader + "\n\n" +
		"mcp__a__alpha — first tool\n" +
		"mcp__a__gamma\n" +
		"mcp__z__beta — second tool"
	if got := mcpCatalogSegment(deferred); got != want {
		t.Fatalf("catalog segment mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestCatalogSegmentSortsByFullName proves the listing order comes from the
// segment, not from the incoming slice: the registry's server-then-tool
// order and full-name order genuinely differ for servers "a" and "a0".
func TestCatalogSegmentSortsByFullName(t *testing.T) {
	registryOrder := []provider.ToolDef{
		{Name: mcpToolName("a", "z")},  // server "a" sorts before "a0"
		{Name: mcpToolName("a0", "b")}, // but "mcp__a0__b" sorts before "mcp__a__z"
	}
	got := mcpCatalogSegment(registryOrder)
	reversed := mcpCatalogSegment([]provider.ToolDef{registryOrder[1], registryOrder[0]})
	if got != reversed {
		t.Fatalf("segment depends on input order:\n%q\n%q", got, reversed)
	}
	iA0 := strings.Index(got, "mcp__a0__b")
	iAZ := strings.Index(got, "mcp__a__z")
	if iA0 < 0 || iAZ < 0 || iA0 > iAZ {
		t.Fatalf("listing is not in full-name order:\n%s", got)
	}
}

// TestCatalogSegmentTruncatesDescription cuts a long description on a
// UTF-8 boundary and never mid-rune.
func TestCatalogSegmentTruncatesDescription(t *testing.T) {
	long := strings.Repeat("é", mcpCatalogDescriptionMax) // 2 bytes per rune
	got := mcpCatalogSegment([]provider.ToolDef{{Name: mcpToolName("a", "t"), Description: long}})
	line := strings.TrimPrefix(got[strings.Index(got, "mcp__a__t"):], "mcp__a__t — ")
	if len(line) > mcpCatalogDescriptionMax {
		t.Fatalf("description line is %d bytes, want <= %d", len(line), mcpCatalogDescriptionMax)
	}
	if !strings.Contains(got, "é") || strings.Contains(got, "\ufffd") {
		t.Fatalf("description truncated mid-rune:\n%s", got)
	}
	// A clipped line must say so, or it reads as if the description ended
	// there.
	if !strings.HasSuffix(line, mcpCatalogEllipsis) {
		t.Fatalf("clipped description carries no marker: %q", line)
	}
	// An unclipped one must NOT carry the marker. Checked on the tool LINE,
	// since the header itself contains a literal query="..." example.
	short := mcpCatalogSegment([]provider.ToolDef{{Name: mcpToolName("a", "t"), Description: "short one"}})
	shortLine := short[strings.Index(short, "mcp__a__t"):]
	if strings.Contains(shortLine, mcpCatalogEllipsis) {
		t.Fatalf("an unclipped description gained a marker: %q", shortLine)
	}
}

// TestReapIgnoresAServerMissingFromTheSnapshot is the race guard: the
// catalog snapshot and the connection state are read at two different
// instants, so a server that connects in the gap reports Connected while
// the snapshot still predates its tools. Reaping on connection state alone
// would drop a valid selection in that window.
func TestReapIgnoresAServerMissingFromTheSnapshot(t *testing.T) {
	s, reg := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"a": 1})
	// "late" is connected but contributes nothing to this snapshot — the
	// shape a connect racing the Tools() read produces.
	reg.names = append(reg.names, "late")
	reg.connected["late"] = true
	name := mcpToolName("late", "tool00")
	s.markMCPToolsSelected(name)

	s.planMCPTools(context.Background())
	if !s.mcpToolSelected(name) {
		t.Fatalf("%q was reaped against a snapshot that does not represent its server", name)
	}

	// Once the snapshot DOES represent that server without the tool, the
	// name is genuinely stale and goes.
	reg.tools = append(reg.tools, provider.ToolDef{Name: mcpToolName("late", "other")})
	s.planMCPTools(context.Background())
	if s.mcpToolSelected(name) {
		t.Fatalf("%q survived a snapshot that represents its server without it", name)
	}
}

// TestCatalogSegmentBoundsTheListing keeps a pathological catalog from
// re-creating the cost deferral removes: past the bound the segment names a
// fixed number of tools and points at search for the rest.
func TestCatalogSegmentBoundsTheListing(t *testing.T) {
	over := mcpCatalogListingMax + 7
	got := mcpCatalogSegment(lazyTools("a", over))
	lines := strings.Split(got, "\n")
	// header, blank, N tool lines, 1 trailing line
	if want := 2 + mcpCatalogListingMax + 1; len(lines) != want {
		t.Fatalf("segment has %d lines, want %d", len(lines), want)
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "... and 7 more tools") {
		t.Fatalf("trailing line = %q, want the \"... and 7 more tools\" pointer", last)
	}
}

// TestCatalogSegmentEmptyWhenNothingDeferred keeps the happy path free: no
// deferral, no segment, no bytes.
func TestCatalogSegmentEmptyWhenNothingDeferred(t *testing.T) {
	if got := mcpCatalogSegment(nil); got != "" {
		t.Fatalf("segment = %q, want empty", got)
	}
	s, _ := lazySession(t, Config{}, map[string]int{"a": 50})
	if _, catalog := s.toolDefsWithCatalog(context.Background()); catalog != "" {
		t.Fatalf("an eager session rendered a catalog:\n%s", catalog)
	}
}

// # The reap

// TestReapDropsStaleSelectionOfConnectedServer closes the hallucinated-name
// hole: a name a CONNECTED server does not hold is dropped from the set.
func TestReapDropsStaleSelectionOfConnectedServer(t *testing.T) {
	s, _ := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"a": 2})
	invented := mcpToolName("a", "no_such_tool")
	s.markMCPToolsSelected(invented)

	s.planMCPTools(context.Background())
	if s.mcpToolSelected(invented) {
		t.Fatalf("invented name %q survived the reap against a connected server", invented)
	}
}

// TestReapKeepsSelectionOfUnconnectedServer is the other half: a selection
// for a server that has NOT connected is kept, so it arms itself the moment
// that server does connect. Reaping it would break self-heal.
func TestReapKeepsSelectionOfUnconnectedServer(t *testing.T) {
	s, reg := lazySession(t, Config{MCPToolLoading: MCPToolLoadingLazy}, map[string]int{"up": 1})
	reg.names = append(reg.names, "down")
	reg.connected["down"] = false
	pending := mcpToolName("down", "later")
	s.markMCPToolsSelected(pending)

	ctx := context.Background()
	s.planMCPTools(ctx)
	if !s.mcpToolSelected(pending) {
		t.Fatalf("pending selection %q was reaped while its server was unconnected", pending)
	}

	// The server connects and does hold the tool: it arms with no second
	// select call.
	reg.connected["down"] = true
	reg.tools = append(reg.tools, provider.ToolDef{Name: pending, Description: "later"})
	got := mcpDefNames(s.planMCPTools(ctx).defs)
	if len(got) != 1 || got[0] != pending {
		t.Fatalf("defs = %v, want the armed pending tool [%s]", got, pending)
	}
}

// TestReapIsANoOpWithoutStatusSurface proves the narrow-interface read
// degrades safely: with no connection state there is no evidence a name is
// stale, so nothing is reaped.
func TestReapIsANoOpWithoutStatusSurface(t *testing.T) {
	s := NewSession(Config{MCP: minimalFakeMCPRegistry{}, MCPToolLoading: MCPToolLoadingLazy})
	name := mcpToolName("a", "t")
	s.markMCPToolsSelected(name)
	s.planMCPTools(context.Background())
	if !s.mcpToolSelected(name) {
		t.Fatalf("%q was reaped by a registry with no Status surface", name)
	}
}

// # splitMCPToolName

func TestSplitMCPToolName(t *testing.T) {
	tests := []struct {
		in             string
		server, remote string
		ok             bool
	}{
		{in: "mcp__github__create_issue", server: "github", remote: "create_issue", ok: true},
		{in: "mcp__a__b__c", server: "a", remote: "b__c", ok: true},
		{in: "mcp__a__b", server: "a", remote: "b", ok: true},
		{in: "mcp____b"},
		{in: "mcp__a__"},
		{in: "mcp__ab"},
		{in: "bash"},
		{in: ""},
	}
	for _, tc := range tests {
		server, remote, ok := splitMCPToolName(tc.in)
		if ok != tc.ok || server != tc.server || remote != tc.remote {
			t.Fatalf("splitMCPToolName(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, server, remote, ok, tc.server, tc.remote, tc.ok)
		}
	}
}

// # Request assembly

// TestCatalogSegmentPositionInAssembledSystem drives the production entry
// point — Session.Prompt — and asserts the segment lands where the design
// says: after the base system prompt, project instructions, and the skills
// catalog, and BEFORE any hook (system.transform) segment. A hand-built
// slice would prove nothing about the path a real turn takes.
func TestCatalogSegmentPositionInAssembledSystem(t *testing.T) {
	work := t.TempDir()
	writeInstr(t, filepath.Join(work, "AGENTS.md"), "instr body")
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "one", "Skill one")

	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 3)}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	var seen []reqSnapshot
	s := NewSession(Config{
		Providers:      provider.Registry{"test": prov},
		Model:          message.ModelRef{Provider: "test", Model: "m1"},
		System:         []string{"base"},
		WorkDir:        work,
		SkillsDirs:     []string{skills},
		Hooks:          &fakeHooks{segments: []string{"hook seg"}},
		MCP:            reg,
		MCPToolLoading: MCPToolLoadingLazy,
		OnRequest:      func(_ string, turn int, req *provider.Request) { seen = append(seen, snapshotRequest(turn, req)) },
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("OnRequest fired %d times, want 1", len(seen))
	}
	sys := seen[0].system
	if len(sys) != 6 {
		t.Fatalf("system has %d segments, want 6 (base, tool-batching, instructions, skills, mcp catalog, hook):\n%q", len(sys), sys)
	}
	if sys[0] != "base" || !isBatchingSegment(sys[1]) || !strings.Contains(sys[2], "instr body") || !strings.Contains(sys[3], "Skill one") {
		t.Fatalf("segments 0-3 are not base/tool-batching/instructions/skills:\n%q", sys)
	}
	if !strings.HasPrefix(sys[4], mcpCatalogHeader) {
		t.Fatalf("segment 4 is not the MCP catalog:\n%q", sys[4])
	}
	if sys[5] != "hook seg" {
		t.Fatalf("segment 5 is not the hook segment:\n%q", sys[5])
	}
	for _, name := range seen[0].tools {
		if isMCPToolName(name) {
			t.Fatalf("deferred MCP tool %q reached the request's tools array", name)
		}
	}
}

// TestUnconfiguredProviderTriggersNoMCPConnect guards the streamTurn
// ordering: the tool plan's Tools(ctx) call is what dials a server for the
// first time and spawns a child process for every stdio server, so a turn
// that cannot resolve its provider must return before the plan runs. A plan
// computed at the top of streamTurn fails this.
func TestUnconfiguredProviderTriggersNoMCPConnect(t *testing.T) {
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 1)}
	s := NewSession(Config{
		Providers:      provider.Registry{},
		Model:          message.ModelRef{Provider: "nope", Model: "m1"},
		MCP:            reg,
		MCPToolLoading: MCPToolLoadingLazy,
	})
	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("Prompt succeeded with an unconfigured provider, want an error")
	}
	if reg.calls != 0 {
		t.Fatalf("MCPRegistry.Tools called %d times on a failed-provider turn, want 0", reg.calls)
	}
}

// TestUnconfiguredProviderSkipsSystemTransform documents the one hook the
// streamTurn reorder moved. chat.params still fires on every turn, because
// provider resolution needs the model it returns. system.transform now runs
// after that resolution, so a turn that cannot resolve its provider returns
// without firing it: building a system prompt for a request that is never
// sent buys nothing.
func TestUnconfiguredProviderSkipsSystemTransform(t *testing.T) {
	hooks := &fakeHooks{segments: []string{"hook seg"}}
	s := NewSession(Config{
		Providers: provider.Registry{},
		Model:     message.ModelRef{Provider: "nope", Model: "m1"},
		Hooks:     hooks,
	})
	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("Prompt succeeded with an unconfigured provider, want an error")
	}
	if hooks.systemCalls != 0 {
		t.Fatalf("system.transform fired %d times on a failed-provider turn, want 0", hooks.systemCalls)
	}
	if hooks.paramCalls != 1 {
		t.Fatalf("chat.params fired %d times, want 1 (provider resolution needs its model)", hooks.paramCalls)
	}
}
