package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// runMCPAction runs one mcp tool action against s and decodes its JSON
// result into out.
func runMCPAction(t *testing.T, s *Session, args string, out any) {
	t.Helper()
	tool, ok := s.tools[mcpSessionToolName]
	if !ok {
		t.Fatal("mcp tool absent")
	}
	parts, err := tool.Run(context.Background(), s, []byte(args))
	if err != nil {
		t.Fatalf("mcp %s: %v", args, err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("mcp result not text: %#v", parts[0])
	}
	if err := json.Unmarshal([]byte(text.Text), out); err != nil {
		t.Fatalf("decoding mcp result %q: %v", text.Text, err)
	}
}

func runMCPActionErr(t *testing.T, s *Session, args string) error {
	t.Helper()
	tool, ok := s.tools[mcpSessionToolName]
	if !ok {
		t.Fatal("mcp tool absent")
	}
	_, err := tool.Run(context.Background(), s, []byte(args))
	return err
}

// searchSession builds a lazy session over one "github" server with a small
// hand-written catalog, so a ranking assertion can name exact scores.
func searchSession(t *testing.T) (*Session, *lazyFakeRegistry) {
	t.Helper()
	reg := &lazyFakeRegistry{
		names:     []string{"github"},
		connected: map[string]bool{"github": true},
		tools: []provider.ToolDef{
			{Name: "mcp__github__create_issue", Description: "Create a new issue in a repository", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "mcp__github__list_issues", Description: "List issues in a repository", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "mcp__github__merge_pr", Description: "Merge a pull request", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	return NewSession(Config{MCP: reg, MCPToolLoading: MCPToolLoadingLazy}), reg
}

// # Scoring

// TestMCPSearchScoreWorkedExample pins the arithmetic the design's worked
// example states: query "create issue" against mcp__github__create_issue
// scores 120 — both tokens inside the remote name (50 each) plus both
// inside the description (10 each), with no exact-name bonus, because the
// whole query "create issue" does not equal "create_issue".
func TestMCPSearchScoreWorkedExample(t *testing.T) {
	def := provider.ToolDef{Name: "mcp__github__create_issue", Description: "Create a new issue in a repository"}
	if got := mcpSearchScore(def, "create issue", mcpSearchTokens("create issue")); got != 120 {
		t.Fatalf("score = %d, want 120", got)
	}
}

func TestMCPSearchScoreRules(t *testing.T) {
	tests := []struct {
		name  string
		def   provider.ToolDef
		query string
		want  int
	}{
		{
			name:  "exact remote name",
			def:   provider.ToolDef{Name: "mcp__github__create_issue"},
			query: "create_issue",
			// The exact bonus fires once for the whole query. The query
			// then tokenizes on the underscore, so BOTH tokens are also
			// substrings of the remote name.
			want: mcpSearchScoreExactName + mcpSearchScoreRemoteName*2,
		},
		{
			name:  "exact full name",
			def:   provider.ToolDef{Name: "mcp__github__merge_pr"},
			query: "mcp__github__merge_pr",
			// exact bonus, plus tokens mcp/github/merge/pr scored per field
			want: mcpSearchScoreExactName +
				mcpSearchScoreRemoteName*2 + // "merge", "pr"
				mcpSearchScoreServerName*1, // "github"
		},
		{
			name:  "a repeated token scores once",
			def:   provider.ToolDef{Name: "mcp__a__issue", Description: "issue"},
			query: "issue issue issue",
			want:  mcpSearchScoreRemoteName + mcpSearchScoreDescription,
		},
		{
			name:  "substring inside a compound name",
			def:   provider.ToolDef{Name: "mcp__a__createIssue"},
			query: "issue",
			want:  mcpSearchScoreRemoteName,
		},
		{
			name:  "case insensitive",
			def:   provider.ToolDef{Name: "mcp__a__createIssue", Description: "Creates ISSUES"},
			query: "ISSUE",
			want:  mcpSearchScoreRemoteName + mcpSearchScoreDescription,
		},
		{
			name:  "server name match",
			def:   provider.ToolDef{Name: "mcp__github__ping"},
			query: "github",
			want:  mcpSearchScoreServerName,
		},
		{
			name:  "no match scores zero",
			def:   provider.ToolDef{Name: "mcp__a__ping", Description: "pings"},
			query: "database",
			want:  0,
		},
		{
			name:  "a malformed name scores zero",
			def:   provider.ToolDef{Name: "bash", Description: "run a command"},
			query: "command",
			want:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpSearchScore(tc.def, tc.query, mcpSearchTokens(tc.query)); got != tc.want {
				t.Fatalf("score = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMCPSearchTokensDeduplicateAndSplit(t *testing.T) {
	got := mcpSearchTokens("Create__ISSUE, create!!! 42")
	want := []string{"create", "issue", "42"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	if got := mcpSearchTokens("   ...  "); got != nil {
		t.Fatalf("tokens for a blank query = %v, want nil", got)
	}
}

// # The search action

func TestMCPSearchRanksAndReportsLoaded(t *testing.T) {
	s, _ := searchSession(t)
	var res mcpSearchResult
	runMCPAction(t, s, `{"action":"search","query":"issue"}`, &res)

	if len(res.Matches) != 2 {
		t.Fatalf("matches = %+v, want the two issue tools", res.Matches)
	}
	// create_issue and list_issues both score 50 (remote name) + 10
	// (description); the tie breaks by name, so create_issue comes first.
	if res.Matches[0].Name != "mcp__github__create_issue" || res.Matches[1].Name != "mcp__github__list_issues" {
		t.Fatalf("order = %s, %s; want create_issue then list_issues (score tie broken by name)",
			res.Matches[0].Name, res.Matches[1].Name)
	}
	if res.Matches[0].Server != "github" {
		t.Fatalf("server = %q, want github", res.Matches[0].Server)
	}
	if res.Total != 2 || res.Truncated {
		t.Fatalf("total/truncated = %d/%v, want 2/false", res.Total, res.Truncated)
	}
	for _, m := range res.Matches {
		if m.Loaded {
			t.Fatalf("%q reports loaded before any select", m.Name)
		}
	}

	runMCPAction(t, s, `{"action":"select","tools":["mcp__github__create_issue"]}`, &mcpSelectResult{})
	runMCPAction(t, s, `{"action":"search","query":"issue"}`, &res)
	for _, m := range res.Matches {
		want := m.Name == "mcp__github__create_issue"
		if m.Loaded != want {
			t.Fatalf("%q loaded = %v, want %v", m.Name, m.Loaded, want)
		}
	}
}

// TestMCPSearchLoadedIsCallabilityNotMembership is the auto-below-threshold
// case: nothing defers, so every tool is callable and search must say so.
// A membership flag would answer false and send the model into select calls
// it does not need.
func TestMCPSearchLoadedIsCallabilityNotMembership(t *testing.T) {
	s, _ := searchSession(t)
	s.cfg.MCPToolLoading = MCPToolLoadingAuto
	s.cfg.MCPToolLoadingThreshold = 10 // catalog of 3 stays under it

	var res mcpSearchResult
	runMCPAction(t, s, `{"action":"search","query":"issue"}`, &res)
	for _, m := range res.Matches {
		if !m.Loaded {
			t.Fatalf("%q reports loaded=false while nothing is deferred", m.Name)
		}
	}
}

func TestMCPSearchLimitAndTruncation(t *testing.T) {
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 30)}
	s := NewSession(Config{MCP: reg, MCPToolLoading: MCPToolLoadingLazy})

	var res mcpSearchResult
	runMCPAction(t, s, `{"action":"search","query":"tool","limit":5}`, &res)
	if len(res.Matches) != 5 || res.Total != 30 || !res.Truncated {
		t.Fatalf("matches/total/truncated = %d/%d/%v, want 5/30/true", len(res.Matches), res.Total, res.Truncated)
	}

	// A limit below 1 falls back to the default rather than erroring.
	runMCPAction(t, s, `{"action":"search","query":"tool","limit":0}`, &res)
	if len(res.Matches) != mcpSearchDefaultLimit {
		t.Fatalf("matches with limit 0 = %d, want the default %d", len(res.Matches), mcpSearchDefaultLimit)
	}
	// A limit above the cap is clamped.
	runMCPAction(t, s, `{"action":"search","query":"tool","limit":500}`, &res)
	if len(res.Matches) != 30 {
		t.Fatalf("matches with limit 500 = %d, want all 30 (cap %d)", len(res.Matches), mcpSearchMaxLimit)
	}
}

func TestMCPSearchBlankQueryErrors(t *testing.T) {
	s, _ := searchSession(t)
	for _, args := range []string{`{"action":"search"}`, `{"action":"search","query":"   "}`, `{"action":"search","query":"!!!"}`} {
		err := runMCPActionErr(t, s, args)
		if err == nil {
			t.Fatalf("%s succeeded, want an error rather than a whole-catalog dump", args)
		}
		if !strings.Contains(err.Error(), "query") {
			t.Fatalf("error %q does not name the query argument", err)
		}
	}
}

// # The select action

func TestMCPSelectBuckets(t *testing.T) {
	s, reg := searchSession(t)
	reg.names = append(reg.names, "down")
	reg.connected["down"] = false

	var res mcpSelectResult
	runMCPAction(t, s, `{"action":"select","tools":[
		"mcp__github__create_issue",
		"mcp__github__no_such_tool",
		"mcp__down__later",
		"mcp__nowhere__thing",
		"not_namespaced"
	]}`, &res)

	if len(res.Selected) != 1 || res.Selected[0] != "mcp__github__create_issue" {
		t.Fatalf("selected = %v, want the one live tool", res.Selected)
	}
	if len(res.Pending) != 1 || res.Pending[0] != "mcp__down__later" {
		t.Fatalf("pending = %v, want the unconnected server's tool", res.Pending)
	}
	wantMissing := []string{"mcp__github__no_such_tool", "mcp__nowhere__thing", "not_namespaced"}
	if strings.Join(res.Missing, ",") != strings.Join(wantMissing, ",") {
		t.Fatalf("missing = %v, want %v", res.Missing, wantMissing)
	}
	if len(res.Already) != 0 {
		t.Fatalf("already = %v, want empty on a first select", res.Already)
	}
	if res.Note == "" {
		t.Fatal("note is empty; the model needs to be told when the selection takes effect")
	}

	// Selected and pending entered the set; missing did not.
	for _, name := range append(res.Selected, res.Pending...) {
		if !s.mcpToolSelected(name) {
			t.Fatalf("%q did not enter the selected set", name)
		}
	}
	for _, name := range res.Missing {
		if s.mcpToolSelected(name) {
			t.Fatalf("missing name %q entered the selected set", name)
		}
	}

	// A second call reports already, and the bucket order puts already
	// first even for the pending name whose server is still down.
	runMCPAction(t, s, `{"action":"select","tools":["mcp__github__create_issue","mcp__down__later"]}`, &res)
	if len(res.Already) != 2 {
		t.Fatalf("already = %v, want both names on the repeat call", res.Already)
	}
	if len(res.Selected) != 0 || len(res.Pending) != 0 {
		t.Fatalf("repeat select re-reported selected=%v pending=%v, want both empty", res.Selected, res.Pending)
	}
}

// TestMCPSelectOfLoadedToolIsRecorded keeps a tool the model is using
// loaded across an auto flip: a tool whose server resolves eager TODAY
// (catalog under the threshold) but whose mode is auto can flip, so
// selecting it is recorded, and the record carries it across the flip. The
// array does not move for such a select, so it invalidates no cached
// prefix.
func TestMCPSelectOfLoadedToolIsRecorded(t *testing.T) {
	s, _ := searchSession(t)
	s.cfg.MCPToolLoading = MCPToolLoadingAuto
	s.cfg.MCPToolLoadingThreshold = 10 // 3 tools: nothing defers yet
	ctx := context.Background()

	before, err := json.Marshal(s.toolDefs(ctx))
	if err != nil {
		t.Fatal(err)
	}
	var res mcpSelectResult
	runMCPAction(t, s, `{"action":"select","tools":["mcp__github__create_issue"]}`, &res)
	if len(res.Selected) != 1 {
		t.Fatalf("selected = %v, want the already-loaded tool recorded", res.Selected)
	}
	if !s.mcpToolSelected("mcp__github__create_issue") {
		t.Fatal("a tool whose server can still flip was not recorded")
	}
	after, err := json.Marshal(s.toolDefs(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("selecting an already-loaded tool changed the tools array; it must invalidate no cached prefix")
	}

	// The flip: the catalog crosses the threshold, so the server defers —
	// and the recorded selection keeps the tool loaded.
	s.cfg.MCPToolLoadingThreshold = 1
	got := mcpDefNames(s.toolDefs(ctx))
	if len(got) != 1 || got[0] != "mcp__github__create_issue" {
		t.Fatalf("after the flip defs = %v, want the recorded tool still loaded", got)
	}
}

// TestMCPSelectOfPinnedEagerToolIsNotRecorded is the other half of the
// per-server gate, and the reason both writers of the record must agree: a
// server pinned eager by MCPToolLoadingByServer can NEVER flip, so a record
// for its tools could never pay for itself. The select still reports the
// tool as selected — it is loaded and callable, which is what the model
// asked for — but nothing enters the set.
func TestMCPSelectOfPinnedEagerToolIsNotRecorded(t *testing.T) {
	s, _ := lazySession(t, Config{
		MCPToolLoading:         MCPToolLoadingLazy,
		MCPToolLoadingByServer: map[string]MCPToolLoading{"pinned": MCPToolLoadingEager},
	}, map[string]int{"pinned": 1, "deferred": 1})
	ctx := context.Background()
	pinned := mcpToolName("pinned", "tool00")

	var res mcpSelectResult
	runMCPAction(t, s, `{"action":"select","tools":["`+pinned+`"]}`, &res)
	if len(res.Selected) != 1 || res.Selected[0] != pinned {
		t.Fatalf("selected = %v, want [%s] — the tool IS loaded", res.Selected, pinned)
	}
	if s.mcpToolSelected(pinned) {
		t.Fatalf("%q entered the selected set, but its server can never flip", pinned)
	}
	// It is callable either way, which is the point.
	if got := mcpDefNames(s.toolDefs(ctx)); len(got) != 1 || got[0] != pinned {
		t.Fatalf("defs = %v, want the pinned server's tool loaded", got)
	}
	// A repeat select reports selected again, never already: no set
	// membership was ever needed to make that tool callable.
	runMCPAction(t, s, `{"action":"select","tools":["`+pinned+`"]}`, &res)
	if len(res.Selected) != 1 || len(res.Already) != 0 {
		t.Fatalf("repeat select = selected %v already %v, want it reported selected again", res.Selected, res.Already)
	}
}

// TestMCPSelectNoteIsConditional pins the note to the batch's real outcome.
// An unconditional "callable from the next request" lies for a pending-only
// batch — those tools' server is down — and a model acting on it calls a
// tool that cannot be there.
func TestMCPSelectNoteIsConditional(t *testing.T) {
	s, reg := searchSession(t)
	reg.names = append(reg.names, "down")
	reg.connected["down"] = false

	var res mcpSelectResult
	runMCPAction(t, s, `{"action":"select","tools":["mcp__github__create_issue"]}`, &res)
	if res.Note != mcpSelectNoteCallable {
		t.Fatalf("note for a selected-bearing batch = %q, want the callable note", res.Note)
	}

	runMCPAction(t, s, `{"action":"select","tools":["mcp__down__later"]}`, &res)
	if len(res.Pending) != 1 {
		t.Fatalf("pending = %v, want the unconnected server's tool", res.Pending)
	}
	if res.Note != mcpSelectNotePending {
		t.Fatalf("note for a pending-only batch = %q, want the reconnect note", res.Note)
	}
	if strings.Contains(res.Note, "callable from the next request") {
		t.Fatalf("a pending-only batch claims its tools are callable next request: %q", res.Note)
	}

	runMCPAction(t, s, `{"action":"select","tools":["mcp__github__no_such_tool"]}`, &res)
	if res.Note != mcpSelectNoteNone {
		t.Fatalf("note for a missing-only batch = %q, want the no-tool-loaded note", res.Note)
	}
}

func TestMCPSelectEmptyToolsErrors(t *testing.T) {
	s, _ := searchSession(t)
	for _, args := range []string{`{"action":"select"}`, `{"action":"select","tools":[]}`} {
		if err := runMCPActionErr(t, s, args); err == nil {
			t.Fatalf("%s succeeded, want an error", args)
		}
	}
}

// TestMCPSelectTakesEffectNextRoundOfTheSameTurn drives the production
// entry point: the model calls select on round 1, and round 2 of the SAME
// turn carries the schema. runAgenticLoop rebuilds the request per tool
// round, which is what makes this work.
func TestMCPSelectTakesEffectNextRoundOfTheSameTurn(t *testing.T) {
	reg := &lazyFakeRegistry{
		names:     []string{"github"},
		connected: map[string]bool{"github": true},
		tools: []provider.ToolDef{
			{Name: "mcp__github__create_issue", Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)},
		},
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", mcpSessionToolName, `{"action":"select","tools":["mcp__github__create_issue"]}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	var rounds [][]string
	s := NewSession(Config{
		Providers:      provider.Registry{"test": prov},
		Model:          message.ModelRef{Provider: "test", Model: "m1"},
		MCP:            reg,
		MCPToolLoading: MCPToolLoadingLazy,
		OnRequest: func(_ string, _ int, req *provider.Request) {
			var names []string
			for _, d := range req.Tools {
				if isMCPToolName(d.Name) {
					names = append(names, d.Name)
				}
			}
			rounds = append(rounds, names)
		},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("saw %d requests, want 2", len(rounds))
	}
	if len(rounds[0]) != 0 {
		t.Fatalf("round 1 carried MCP tools %v, want none (deferred)", rounds[0])
	}
	if len(rounds[1]) != 1 || rounds[1][0] != "mcp__github__create_issue" {
		t.Fatalf("round 2 carried %v, want the selected tool", rounds[1])
	}
}

// # Use implies selection

// TestRoutedCallImpliesSelection proves a tool the model actually used
// stays loaded across an auto flip. Without it, an eager server's tool --
// which needs no select, and which the model is told not to select -- would
// lose its schema the moment a second server pushed the catalog over the
// threshold.
func TestRoutedCallImpliesSelection(t *testing.T) {
	s, _ := searchSession(t)
	s.cfg.MCPToolLoading = MCPToolLoadingAuto
	s.cfg.MCPToolLoadingThreshold = 10 // 3 tools: nothing defers yet
	ctx := context.Background()

	name := "mcp__github__merge_pr"
	out, isErr := s.executeTool(ctx, &message.ToolCall{CallID: "c1", Name: name, Arguments: []byte(`{}`)}, []byte(`{}`))
	if isErr {
		t.Fatalf("routed call reported an error: %v", out)
	}
	if !s.mcpToolSelected(name) {
		t.Fatalf("%q was not recorded by its own routed call", name)
	}

	// The flip: the threshold drops below the catalog size, so the server
	// defers — and the used tool is still loaded.
	s.cfg.MCPToolLoadingThreshold = 1
	got := mcpDefNames(s.toolDefs(ctx))
	if len(got) != 1 || got[0] != name {
		t.Fatalf("after the flip defs = %v, want the used tool [%s]", got, name)
	}
}

// TestRoutedCallRecordsNothingWhenNothingCanDefer is the default-path cost
// guard: a plain eager config can never flip, so the record could never pay
// for itself and must not be written at all.
func TestRoutedCallRecordsNothingWhenNothingCanDefer(t *testing.T) {
	s, _ := searchSession(t)
	s.cfg.MCPToolLoading = MCPToolLoadingEager
	name := "mcp__github__merge_pr"
	s.executeTool(context.Background(), &message.ToolCall{CallID: "c1", Name: name, Arguments: []byte(`{}`)}, []byte(`{}`))
	if s.mcpToolSelected(name) {
		t.Fatalf("%q was recorded in a session that can never defer", name)
	}
}

// TestUnroutedCallRecordsNothing keeps an invented name out of the set: a
// call that never resolved a binding returns an error and records nothing.
func TestUnroutedCallRecordsNothing(t *testing.T) {
	reg := &erroringMCPRegistry{lazyFakeRegistry: lazyFakeRegistry{
		names: []string{"github"}, connected: map[string]bool{"github": true}, tools: lazyTools("github", 1),
	}}
	s := NewSession(Config{MCP: reg, MCPToolLoading: MCPToolLoadingLazy})
	name := "mcp__github__invented"
	_, isErr := s.executeTool(context.Background(), &message.ToolCall{CallID: "c1", Name: name, Arguments: []byte(`{}`)}, []byte(`{}`))
	if !isErr {
		t.Fatal("an unroutable call reported success")
	}
	if s.mcpToolSelected(name) {
		t.Fatalf("unroutable name %q entered the selected set", name)
	}
}

// erroringMCPRegistry fails every CallTool, the shape MCPManager returns
// for an unknown tool name or an unconnected server.
type erroringMCPRegistry struct{ lazyFakeRegistry }

func (r *erroringMCPRegistry) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, errors.New("engine: mcp: unknown tool")
}

// # The action gate

// TestMCPToolAdvertisesSearchAndSelectOnlyWhenItCanDefer pins the gate onto
// the tool def itself, in both directions.
func TestMCPToolAdvertisesSearchAndSelectOnlyWhenItCanDefer(t *testing.T) {
	tests := []struct {
		name     string
		global   MCPToolLoading
		override map[string]MCPToolLoading
		want     bool
	}{
		{name: "eager advertises two actions", global: MCPToolLoadingEager, want: false},
		{name: "lazy advertises four", global: MCPToolLoadingLazy, want: true},
		{
			name: "global eager with one per-server lazy advertises four", global: MCPToolLoadingEager,
			override: map[string]MCPToolLoading{"b": MCPToolLoadingLazy}, want: true,
		},
		{
			name: "global lazy with every server pinned eager advertises two", global: MCPToolLoadingLazy,
			override: map[string]MCPToolLoading{"a": MCPToolLoadingEager, "b": MCPToolLoadingEager}, want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := lazySession(t, Config{MCPToolLoading: tc.global, MCPToolLoadingByServer: tc.override}, map[string]int{"a": 1, "b": 1})
			def := s.tools[mcpSessionToolName].Def
			schema := string(def.InputSchema)
			hasSearch := strings.Contains(schema, `"search"`)
			hasSelect := strings.Contains(schema, `"select"`)
			if hasSearch != tc.want || hasSelect != tc.want {
				t.Fatalf("schema advertises search=%v select=%v, want %v:\n%s", hasSearch, hasSelect, tc.want, schema)
			}
			describesSelect := strings.Contains(def.Description, "select(tools)")
			if describesSelect != tc.want {
				t.Fatalf("description describes select = %v, want %v", describesSelect, tc.want)
			}
		})
	}
}

// TestMCPToolDefByteStableAcrossRequests keeps the gated def out of the
// prompt-cache problem: policy is fixed for a session's life, so the def
// must not move between requests.
func TestMCPToolDefByteStableAcrossRequests(t *testing.T) {
	s, _ := lazySession(t, Config{
		MCPToolLoading:          MCPToolLoadingAuto,
		MCPToolLoadingThreshold: 1,
	}, map[string]int{"a": 3})
	ctx := context.Background()
	first, err := json.Marshal(s.tools[mcpSessionToolName].Def)
	if err != nil {
		t.Fatal(err)
	}
	s.toolDefs(ctx)
	s.markMCPToolsSelected(mcpToolName("a", "tool00"))
	s.toolDefs(ctx)
	got, err := json.Marshal(s.tools[mcpSessionToolName].Def)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(first) {
		t.Fatalf("mcp tool def moved:\nfirst: %s\ngot:   %s", first, got)
	}
}

// TestRoutedCallRecordsNothingForAPinnedEagerServer pins the gate to the
// SERVER rather than the session. A server pinned eager can never flip to
// deferred, so a record for its tools could never pay for itself — even in
// a session that defers some OTHER server.
func TestRoutedCallRecordsNothingForAPinnedEagerServer(t *testing.T) {
	s, _ := lazySession(t, Config{
		MCPToolLoading:         MCPToolLoadingLazy,
		MCPToolLoadingByServer: map[string]MCPToolLoading{"pinned": MCPToolLoadingEager},
	}, map[string]int{"pinned": 1, "deferred": 1})
	ctx := context.Background()

	pinned := mcpToolName("pinned", "tool00")
	deferred := mcpToolName("deferred", "tool00")

	if _, isErr := s.executeTool(ctx, &message.ToolCall{CallID: "c1", Name: pinned, Arguments: []byte(`{}`)}, []byte(`{}`)); isErr {
		t.Fatal("routed call to the pinned server reported an error")
	}
	if s.mcpToolSelected(pinned) {
		t.Fatalf("%q was recorded, but its server can never flip to deferred", pinned)
	}

	// The same session still records a call to a server that CAN defer.
	if _, isErr := s.executeTool(ctx, &message.ToolCall{CallID: "c2", Name: deferred, Arguments: []byte(`{}`)}, []byte(`{}`)); isErr {
		t.Fatal("routed call to the deferred server reported an error")
	}
	if !s.mcpToolSelected(deferred) {
		t.Fatalf("%q was not recorded by its own routed call", deferred)
	}
}
