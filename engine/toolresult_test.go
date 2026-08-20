package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// retainCfg builds a Config with retention enabled at the given inline
// limit, wired to a scripted provider and a session dir (retention requires
// one — see Session.toolResultInlineLimit).
func retainCfg(dir string, prov *scriptedProvider, inline, retained int) Config {
	return Config{
		Providers:               provider.Registry{prov.name: prov},
		Model:                   message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir:              dir,
		ToolResultInlineBytes:   inline,
		ToolResultRetainedBytes: retained,
	}
}

// bigOutputTool is a built-in test tool returning exactly the text it is
// configured with, so a test can drive retention through the REAL
// runToolCalls path rather than calling maybeRetainToolResult directly.
func bigOutputTool(name, text string) Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        name,
			Description: "test tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Run: func(_ context.Context, _ *Session, _ json.RawMessage) (message.Parts, error) {
			return message.Parts{&message.Text{Text: text}}, nil
		},
	}
}

// lines builds n numbered lines, each "line-<i>", newline-terminated.
func linesText(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line-%d\n", i)
	}
	return b.String()
}

// runOneToolTurn drives a single tool-calling turn through Session.Prompt
// against a scripted provider, returning the session and the tool-role
// message's single ToolResult.
func runOneToolTurn(t *testing.T, cfg Config, prov *scriptedProvider, toolName string) (*Session, *message.ToolResult) {
	t.Helper()
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	h := s.History()
	for _, m := range h {
		for _, p := range m.Parts {
			if tr, ok := p.(*message.ToolResult); ok {
				return s, tr
			}
		}
	}
	t.Fatalf("no ToolResult in history: %+v", h)
	return nil, nil
}

// oneToolTurnProvider scripts: turn 1 calls toolName, turn 2 ends.
func oneToolTurnProvider(toolName string) *scriptedProvider {
	return &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", toolName, `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
}

// TestToolResultRetainedAboveInlineLimit is the core behavior: a text tool
// result larger than ToolResultInlineBytes is replaced in canonical history
// by a preview carrying a handle, the sidecar file holds the full original
// bytes, and the handle's metadata is registered.
func TestToolResultRetainedAboveInlineLimit(t *testing.T) {
	dir := t.TempDir()
	big := linesText(5000) // well over the 1KB limit below
	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 1024, 0)
	cfg.Tools = []Tool{bigOutputTool("bigtool", big)}

	s, tr := runOneToolTurn(t, cfg, prov, "bigtool")

	if len(tr.Content) != 2 {
		t.Fatalf("retained content parts = %d, want 2 (header + preview): %+v", len(tr.Content), tr.Content)
	}
	header := tr.Content[0].(*message.Text).Text
	if !strings.Contains(header, "handle=trh_1") {
		t.Errorf("header missing handle: %q", header)
	}
	if !strings.Contains(header, fmt.Sprintf("bytes=%d", len(big))) {
		t.Errorf("header missing total bytes %d: %q", len(big), header)
	}
	preview := tr.Content[1].(*message.Text).Text
	if len(preview) > 1024 {
		t.Errorf("preview len = %d, want <= 1024", len(preview))
	}
	if !strings.HasPrefix(big, preview) {
		t.Errorf("preview is not a prefix of the original text")
	}

	// The sidecar file holds the FULL original bytes, verbatim.
	got, err := os.ReadFile(filepath.Join(dir, toolResultsDirName, s.ID, "trh_1.txt"))
	if err != nil {
		t.Fatalf("sidecar file: %v", err)
	}
	if string(got) != big {
		t.Errorf("sidecar bytes = %d, want %d (verbatim)", len(got), len(big))
	}

	meta, ok := s.lookupToolResult("trh_1")
	if !ok {
		t.Fatal("trh_1 not registered")
	}
	if meta.Tool != "bigtool" || meta.Bytes != len(big) || meta.Lines != 5000 {
		t.Errorf("meta = %+v, want tool=bigtool bytes=%d lines=5000", meta, len(big))
	}
}

// TestToolResultPreviewHeaderExactFormat pins the documented header, byte
// for byte (docs/plans/2026-08-19-tool-result-handles.md §2.1). It compares
// against a hand-written literal, NOT against the same fmt verbs the
// producer uses — a test that reassembled the format string would pass no
// matter what the format became, which is the exact drift this guards.
func TestToolResultPreviewHeaderExactFormat(t *testing.T) {
	got := toolResultPreviewHeader("trh_1", "bash", 123456, 4201, 16384)
	want := `[tool result retained: handle=trh_1 tool=bash bytes=123456 lines=4201 preview_bytes=16384 — read the rest with read_tool_result(handle="trh_1")]`
	if got != want {
		t.Errorf("header mismatch\n got: %s\nwant: %s", got, want)
	}

	gotCap := toolResultCapHeader("bash", 123456, 16384)
	wantCap := `[tool result truncated: tool=bash bytes=123456 preview_bytes=16384 — retention cap reached for this session, the remainder is discarded]`
	if gotCap != wantCap {
		t.Errorf("cap header mismatch\n got: %s\nwant: %s", gotCap, wantCap)
	}
}

// TestToolResultUnderLimitUntouched: an under-limit result passes through
// completely unchanged — same parts, same text, no header, no sidecar
// directory.
func TestToolResultUnderLimitUntouched(t *testing.T) {
	dir := t.TempDir()
	small := "just a little output\n"
	prov := oneToolTurnProvider("smalltool")
	cfg := retainCfg(dir, prov, 1024, 0)
	cfg.Tools = []Tool{bigOutputTool("smalltool", small)}

	s, tr := runOneToolTurn(t, cfg, prov, "smalltool")

	if len(tr.Content) != 1 {
		t.Fatalf("content parts = %d, want 1 (untouched): %+v", len(tr.Content), tr.Content)
	}
	if got := tr.Content[0].(*message.Text).Text; got != small {
		t.Errorf("text = %q, want %q", got, small)
	}
	if _, err := os.Stat(filepath.Join(dir, toolResultsDirName, s.ID)); !os.IsNotExist(err) {
		t.Errorf("sidecar dir created for an under-limit result (err=%v)", err)
	}
}

// TestToolResultRetentionDisabledByZeroLimit: a non-positive inline limit
// disables retention entirely, however large the output — the contract an
// embedder building a bare engine.Config relies on. No sidecar directory is
// created, and the read_tool_result tool is not registered.
func TestToolResultRetentionDisabledByZeroLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit%d", limit), func(t *testing.T) {
			dir := t.TempDir()
			big := linesText(5000)
			prov := oneToolTurnProvider("bigtool")
			cfg := retainCfg(dir, prov, limit, 0)
			cfg.Tools = []Tool{bigOutputTool("bigtool", big)}

			s, tr := runOneToolTurn(t, cfg, prov, "bigtool")

			if len(tr.Content) != 1 || tr.Content[0].(*message.Text).Text != big {
				t.Errorf("output was modified with retention disabled: %d parts", len(tr.Content))
			}
			if _, err := os.Stat(filepath.Join(dir, toolResultsDirName, s.ID)); !os.IsNotExist(err) {
				t.Errorf("sidecar dir created with retention disabled (err=%v)", err)
			}
		})
	}
}

// TestToolResultRetentionRequiresSessionDir: with no SessionDir there is
// nowhere durable to put the bytes, so retention is off whatever the byte
// limits say — a preview naming an unreadable handle would be worse than no
// preview.
func TestToolResultRetentionRequiresSessionDir(t *testing.T) {
	big := linesText(5000)
	prov := oneToolTurnProvider("bigtool")
	cfg := Config{
		Providers:             provider.Registry{prov.name: prov},
		Model:                 message.ModelRef{Provider: prov.name, Model: "m1"},
		ToolResultInlineBytes: 1024,
		Tools:                 []Tool{bigOutputTool("bigtool", big)},
	}

	_, tr := runOneToolTurn(t, cfg, prov, "bigtool")

	if len(tr.Content) != 1 || tr.Content[0].(*message.Text).Text != big {
		t.Errorf("retention ran without a SessionDir: %d parts", len(tr.Content))
	}
}

// TestToolResultRetentionPreservesNonTextParts: a Blob survives retention
// untouched and in order, after the preview. Retention is a TEXT-only
// mechanism — image bytes are already bounded by imageclamp at transcode
// time, and dropping one here would silently destroy content.
func TestToolResultRetentionPreservesNonTextParts(t *testing.T) {
	dir := t.TempDir()
	big := linesText(5000)
	blob := &message.Blob{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}

	prov := oneToolTurnProvider("mixed")
	cfg := retainCfg(dir, prov, 1024, 0)
	cfg.Tools = []Tool{{
		Def: provider.ToolDef{Name: "mixed", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(_ context.Context, _ *Session, _ json.RawMessage) (message.Parts, error) {
			return message.Parts{&message.Text{Text: big}, blob}, nil
		},
	}}

	_, tr := runOneToolTurn(t, cfg, prov, "mixed")

	if len(tr.Content) != 3 {
		t.Fatalf("content parts = %d, want 3 (header, preview, blob): %+v", len(tr.Content), tr.Content)
	}
	got, ok := tr.Content[2].(*message.Blob)
	if !ok {
		t.Fatalf("part 3 = %T, want *message.Blob", tr.Content[2])
	}
	if got.MediaType != "image/png" || string(got.Data) != string(blob.Data) {
		t.Errorf("blob mutated: %+v", got)
	}
}

// TestToolResultRetainedBytesCapRefusesRetention: once the per-session
// ceiling is reached, a further oversized result is still previewed but its
// remainder is discarded — and the preview says so with NO handle, since
// there is nothing to read back.
func TestToolResultRetainedBytesCapRefusesRetention(t *testing.T) {
	dir := t.TempDir()
	big := linesText(2000) // ~ 2000 * 8 bytes

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bigtool", `{}`)),
		asstTurn(provider.StopToolUse, toolCall("tc2", "bigtool", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	// Cap admits the first retention but not a second.
	cfg := retainCfg(dir, prov, 512, len(big)+10)
	cfg.Tools = []Tool{bigOutputTool("bigtool", big)}

	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	var results []*message.ToolResult
	for _, m := range s.History() {
		for _, p := range m.Parts {
			if tr, ok := p.(*message.ToolResult); ok {
				results = append(results, tr)
			}
		}
	}
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(results))
	}

	first := results[0].Content[0].(*message.Text).Text
	if !strings.Contains(first, "handle=trh_1") {
		t.Errorf("first result should be retained: %q", first)
	}
	second := results[1].Content[0].(*message.Text).Text
	if !strings.Contains(second, "retention cap reached") {
		t.Errorf("second result should hit the cap: %q", second)
	}
	if strings.Contains(second, "handle=") {
		t.Errorf("cap header must carry no handle: %q", second)
	}
	if _, ok := s.lookupToolResult("trh_2"); ok {
		t.Error("trh_2 registered despite the cap refusing retention")
	}
	if _, err := os.Stat(filepath.Join(dir, toolResultsDirName, s.ID, "trh_2.txt")); !os.IsNotExist(err) {
		t.Errorf("trh_2 sidecar written despite the cap (err=%v)", err)
	}
}

// TestToolResultHandleCounterSurvivesResume is the counter-survives-resume
// requirement: LoadSession folds the durable toolresult.retained record, so
// the next handle continues the series rather than restarting at trh_1 and
// overwriting an existing sidecar file.
func TestToolResultHandleCounterSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	big := linesText(3000)

	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 512, 0)
	cfg.Tools = []Tool{bigOutputTool("bigtool", big)}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.lookupToolResult("trh_1"); !ok {
		t.Fatal("trh_1 not minted in the first process")
	}

	// Resume: a second turn in the reloaded session must mint trh_2.
	prov2 := oneToolTurnProvider("bigtool")
	cfg2 := retainCfg(dir, prov2, 512, 0)
	cfg2.Tools = []Tool{bigOutputTool("bigtool", big)}
	s2, err := LoadSession(cfg2, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	s2.mu.Lock()
	next := s2.toolResultNextID
	s2.mu.Unlock()
	if next != 2 {
		t.Fatalf("toolResultNextID after resume = %d, want 2", next)
	}

	if _, err := s2.Prompt(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.lookupToolResult("trh_2"); !ok {
		t.Error("resumed session did not mint trh_2")
	}
	// The first process's sidecar file must still be intact — the whole
	// point of not restarting the counter.
	got, err := os.ReadFile(filepath.Join(dir, toolResultsDirName, s.ID, "trh_1.txt"))
	if err != nil || string(got) != big {
		t.Errorf("trh_1 sidecar clobbered by the resumed session (err=%v, len=%d)", err, len(got))
	}
}

// TestToolResultHandleMetadataSurvivesResume: read_tool_result on a resumed
// session serves a handle minted by the previous process, with its metadata
// (tool name, byte and line counts) intact.
func TestToolResultHandleMetadataSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	big := linesText(3000)

	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 512, 0)
	cfg.Tools = []Tool{bigOutputTool("bigtool", big)}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	s2, err := LoadSession(retainCfg(dir, oneToolTurnProvider("bigtool"), 512, 0), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := s2.lookupToolResult("trh_1")
	if !ok {
		t.Fatal("trh_1 metadata not restored")
	}
	if meta.Tool != "bigtool" || meta.Bytes != len(big) || meta.Lines != 3000 {
		t.Errorf("restored meta = %+v, want tool=bigtool bytes=%d lines=3000", meta, len(big))
	}
	s2.mu.Lock()
	total := s2.toolResultBytes
	s2.mu.Unlock()
	if total != len(big) {
		t.Errorf("restored toolResultBytes = %d, want %d", total, len(big))
	}

	out, err := runReadToolResult(s2, json.RawMessage(`{"handle":"trh_1","offset":1,"limit":3}`))
	if err != nil {
		t.Fatalf("read on resumed session: %v", err)
	}
	if got := out.Text(); !strings.Contains(got, "line-1") || !strings.Contains(got, "line-3") {
		t.Errorf("resumed read = %q", got)
	}
}

// TestLoadSessionSkipsMalformedRetainedRecord: a malformed or duplicate
// handle in the log is skipped, never folded — but its number still
// advances the counter, so a burned handle can never be reissued.
func TestLoadSessionSkipsMalformedRetainedRecord(t *testing.T) {
	dir := t.TempDir()
	id := "ses_01m0g96daxegnaqwtqe135ah3k"
	log := strings.Join([]string{
		`{"type":"session","id":"` + id + `","created_at":"2026-08-19T00:00:00Z"}`,
		`{"type":"toolresult.retained","tool_result":{"handle":"trh_1","tool":"bash","bytes":100,"lines":5}}`,
		`{"type":"toolresult.retained","tool_result":{"handle":"trh_1","tool":"other","bytes":999,"lines":9}}`, // duplicate
		`{"type":"toolresult.retained","tool_result":{"handle":"bogus","tool":"x","bytes":50,"lines":1}}`,      // malformed
		`{"type":"toolresult.retained","tool_result":{"handle":"trh_0","tool":"x","bytes":50,"lines":1}}`,      // non-positive
		`{"type":"toolresult.retained","tool_result":{"handle":"trh_7","tool":"grep","bytes":200,"lines":8}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}

	if got := len(s.toolResults); got != 2 {
		t.Errorf("folded handles = %d, want 2 (trh_1, trh_7): %+v", got, s.toolResults)
	}
	m1 := s.toolResults["trh_1"]
	if m1.Tool != "bash" || m1.Bytes != 100 {
		t.Errorf("duplicate record overwrote trh_1: %+v", m1)
	}
	if _, ok := s.toolResults["bogus"]; ok {
		t.Error("malformed handle folded")
	}
	if _, ok := s.toolResults["trh_0"]; ok {
		t.Error("non-positive handle folded")
	}
	// Only the two folded records contribute to the byte total; the
	// duplicate must not double-count.
	if s.toolResultBytes != 300 {
		t.Errorf("toolResultBytes = %d, want 300", s.toolResultBytes)
	}
	if s.toolResultNextID != 8 {
		t.Errorf("toolResultNextID = %d, want 8 (past trh_7)", s.toolResultNextID)
	}
}
