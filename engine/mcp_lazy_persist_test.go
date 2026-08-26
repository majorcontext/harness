package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// lazyPersistCfg is a lazy-deferral session config backed by a session
// store, so a selection can be journaled and reloaded.
func lazyPersistCfg(dir string, reg MCPRegistry) Config {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	return Config{
		Providers:      provider.Registry{prov.name: prov},
		Model:          message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir:     dir,
		MCP:            reg,
		MCPToolLoading: MCPToolLoadingLazy,
	}
}

// startedSession returns a session whose log exists, since every persist
// path is a no-op until the first record has been written.
func startedSession(t *testing.T, cfg Config) *Session {
	t.Helper()
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}
	return s
}

func sessionLog(t *testing.T, dir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// countRecords counts log lines of one record type.
func countRecords(t *testing.T, dir, id, kind string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(sessionLog(t, dir, id), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec.Type == kind {
			n++
		}
	}
	return n
}

// appendRawRecord appends one record to a session log by hand. The live
// path cannot write a malformed name, so a log holding one is corruption or
// an older binary's output -- which is exactly what the replay guard has to
// survive.
func appendRawRecord(t *testing.T, dir, id string, rec record) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// TestSelectionSurvivesReload is the core recovery property: a tool the
// model loaded before a restart is loaded again on the first request after
// it, with no second select call.
func TestSelectionSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 3)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	name := mcpToolName("a", "tool01")
	var res mcpSelectResult
	runMCPAction(t, s, `{"action":"select","tools":["`+name+`"]}`, &res)
	if len(res.Selected) != 1 {
		t.Fatalf("selected = %v, want the one tool", res.Selected)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}
	if n := countRecords(t, dir, s.ID, recMCPToolsSelected); n != 1 {
		t.Fatalf("log holds %d %s records, want 1", n, recMCPToolsSelected)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.mcpToolSelected(name) {
		t.Fatalf("%q did not survive the reload", name)
	}
	got := mcpDefNames(loaded.toolDefs(context.Background()))
	if len(got) != 1 || got[0] != name {
		t.Fatalf("reloaded defs = %v, want the restored selection [%s]", got, name)
	}
}

// TestRoutedCallSelectionSurvivesReload covers the SECOND writer of the
// record. An implementation that journals only from select loses this: the
// tool the model actually used comes back unloaded.
func TestRoutedCallSelectionSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 3)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	name := mcpToolName("a", "tool02")
	if _, isErr := s.executeTool(context.Background(), &message.ToolCall{CallID: "c1", Name: name, Arguments: []byte(`{}`)}, []byte(`{}`)); isErr {
		t.Fatal("routed call reported an error")
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.mcpToolSelected(name) {
		t.Fatalf("the used tool %q did not survive the reload", name)
	}
}

// TestRepeatSelectionWritesOneRecord keeps the log from growing per call: a
// name already in the set changes nothing and journals nothing.
func TestRepeatSelectionWritesOneRecord(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 2)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	name := mcpToolName("a", "tool00")
	for i := 0; i < 3; i++ {
		runMCPAction(t, s, `{"action":"select","tools":["`+name+`"]}`, &mcpSelectResult{})
	}
	// The same tool called repeatedly must not journal either.
	for i := 0; i < 3; i++ {
		s.executeTool(context.Background(), &message.ToolCall{CallID: "c", Name: name, Arguments: []byte(`{}`)}, []byte(`{}`))
	}
	if n := countRecords(t, dir, s.ID, recMCPToolsSelected); n != 1 {
		t.Fatalf("log holds %d %s records, want exactly 1", n, recMCPToolsSelected)
	}
}

// TestSelectJournalsSelectedAndPendingTogether pins the record's payload:
// one record per call, carrying both buckets that entered the set, and
// neither of the two that did not.
func TestSelectJournalsSelectedAndPendingTogether(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{
		names:     []string{"up", "down"},
		connected: map[string]bool{"up": true, "down": false},
		tools:     lazyTools("up", 1),
	}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	live := mcpToolName("up", "tool00")
	pending := mcpToolName("down", "later")
	missing := mcpToolName("up", "no_such_tool")
	runMCPAction(t, s, `{"action":"select","tools":["`+live+`","`+pending+`","`+missing+`"]}`, &mcpSelectResult{})

	var recs [][]string
	for _, line := range strings.Split(sessionLog(t, dir, s.ID), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Type     string   `json:"type"`
			MCPTools []string `json:"mcp_tools"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Type == recMCPToolsSelected {
			recs = append(recs, rec.MCPTools)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("wrote %d records, want 1 per call", len(recs))
	}
	got := strings.Join(recs[0], ",")
	if got != live+","+pending {
		t.Fatalf("record names = %q, want %q (missing must never be recorded)", got, live+","+pending)
	}
}

// TestReplaySkipsMalformedAndDedupes is the replay guard: a name no server
// can own must not enter the restored set, matching what select itself
// refuses to record, and a duplicate must collapse rather than double.
func TestReplaySkipsMalformedAndDedupes(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 2)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	good := mcpToolName("a", "tool00")
	// Append records by hand: the live path cannot write these shapes, so a
	// log holding them is corruption or an older binary's output.
	appendRawRecord(t, dir, s.ID, record{Type: recMCPToolsSelected, MCPTools: []string{good, "not_namespaced", "mcp__", "mcp__a__", good}})

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession failed on a log with malformed names: %v", err)
	}
	if !loaded.mcpToolSelected(good) {
		t.Fatalf("the well-formed name %q was not restored", good)
	}
	for _, bad := range []string{"not_namespaced", "mcp__", "mcp__a__"} {
		if loaded.mcpToolSelected(bad) {
			t.Fatalf("malformed name %q entered the restored set", bad)
		}
	}
	if n := len(loaded.mcpSelected); n != 1 {
		t.Fatalf("restored set holds %d names, want 1 (duplicates collapse)", n)
	}
}

// TestPendingSelectionArmsAfterReload and its sibling below are the pair
// that proves the reap closes the hallucinated-name hole WITHOUT breaking
// self-heal. Both start from the same log shape: a name recorded while its
// server was unconnected.
func TestPendingSelectionArmsAfterReload(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a", "down"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 1)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	pending := mcpToolName("down", "later")
	runMCPAction(t, s, `{"action":"select","tools":["`+pending+`"]}`, &mcpSelectResult{})

	// The server connects, and it DOES hold the tool.
	reg.connected["down"] = true
	reg.tools = append(reg.tools, provider.ToolDef{Name: pending, Description: "later", InputSchema: json.RawMessage(`{"type":"object"}`)})

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := mcpDefNames(loaded.toolDefs(context.Background()))
	if len(got) != 1 || got[0] != pending {
		t.Fatalf("defs = %v, want the armed pending tool [%s] with no second select", got, pending)
	}
}

func TestInventedPendingSelectionIsReapedAfterReload(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a", "down"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 1)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	invented := mcpToolName("down", "never_existed")
	runMCPAction(t, s, `{"action":"select","tools":["`+invented+`"]}`, &mcpSelectResult{})

	// The server connects and its catalog is now visible WITHOUT the tool:
	// the name was invented. It must expose at least one tool of its own,
	// because that is what proves the snapshot represents this server --
	// the reap deliberately spares a server that reports connected but
	// contributes nothing to the snapshot yet, since a connect racing the
	// catalog read looks exactly the same (see reapMCPSelections).
	reg.connected["down"] = true
	reg.tools = append(reg.tools, provider.ToolDef{Name: mcpToolName("down", "real_tool")})

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.mcpToolSelected(invented) {
		t.Fatal("test setup: the name should be restored before the first plan reaps it")
	}
	loaded.toolDefs(context.Background())
	if loaded.mcpToolSelected(invented) {
		t.Fatalf("invented name %q survived the first plan after a reload", invented)
	}
}

// TestRestoredSelectionOfAbsentServerIsKept covers the third recovery
// shape: the server never comes back during this session, so the name is
// neither armed nor reaped, and nothing errors.
func TestRestoredSelectionOfAbsentServerIsKept(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a", "down"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 1)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)

	pending := mcpToolName("down", "later")
	runMCPAction(t, s, `{"action":"select","tools":["`+pending+`"]}`, &mcpSelectResult{})

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := mcpDefNames(loaded.toolDefs(context.Background()))
	for _, name := range got {
		if name == pending {
			t.Fatalf("%q contributed a def while its server is still unconnected", name)
		}
	}
	if !loaded.mcpToolSelected(pending) {
		t.Fatalf("%q was dropped while its server was still unconnected; it must wait", pending)
	}
}

// TestSelectionIsInertWhenReloadedEager keeps a restored set harmless for a
// session whose config no longer defers anything.
func TestSelectionIsInertWhenReloadedEager(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 3)}
	cfg := lazyPersistCfg(dir, reg)
	s := startedSession(t, cfg)
	runMCPAction(t, s, `{"action":"select","tools":["`+mcpToolName("a", "tool00")+`"]}`, &mcpSelectResult{})

	eager := cfg
	eager.MCPToolLoading = MCPToolLoadingEager
	loaded, err := LoadSession(eager, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defs, catalog := loaded.toolDefsWithCatalog(context.Background())
	if catalog != "" {
		t.Fatalf("an eager reload rendered a catalog:\n%s", catalog)
	}
	if got := len(mcpDefNames(defs)); got != 3 {
		t.Fatalf("eager reload registered %d MCP defs, want all 3", got)
	}
}

// TestPreLogSelectionSurvivesReload closes the gap review found on this
// PR: a selection made BEFORE the log exists (persistMCPToolsSelected
// no-ops until logStarted) must still reach disk. ensureLog captures the
// live selected set into the header write, exactly as it captures the
// live model and effort, so the first turn's log creation carries the
// earlier selection.
func TestPreLogSelectionSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	reg := &lazyFakeRegistry{names: []string{"a"}, connected: map[string]bool{"a": true}, tools: lazyTools("a", 3)}
	cfg := lazyPersistCfg(dir, reg)

	s := NewSession(cfg)
	want := mcpToolName("a", "tool01")
	// Select while no log exists yet: the persist path is a no-op here.
	if added := s.markMCPToolsSelected(want); len(added) != 1 {
		t.Fatalf("markMCPToolsSelected = %v", added)
	}
	// First prompt creates the log; the header write must carry the set.
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	re, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !re.mcpSelected[want] {
		t.Fatalf("reloaded session lost the pre-log selection %q; mcpSelected = %v", want, re.mcpSelected)
	}
}
