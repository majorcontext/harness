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
	// F6(a): a plain HasPrefix(big, preview) check is a mutation escape —
	// it is vacuously true for an EMPTY preview too (every string is a
	// prefix of "" trivially satisfying HasPrefix in the other direction,
	// and "" is a prefix of everything), so a regression that silently
	// zeroed the preview would sail through it. Pin both the exact length
	// and the exact bytes.
	if len(preview) != 1024 {
		t.Fatalf("preview len = %d, want exactly 1024 (the inline limit)", len(preview))
	}
	if preview != big[:1024] {
		t.Errorf("preview is not the exact head of the original text")
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

// TestToolResultGateMeasuresMaskedLength is a round-5 review finding's red
// test. The retention gate (`len(text) <= limit`) measured the UNMASKED
// original length, while everything downstream — the preview, meta.Bytes,
// the retention-ceiling accounting — measures the MASKED length. A result
// that masks down to well under the limit (a long secret value collapses
// to "***") still triggered retention on its pre-mask size: a handle got
// burned and a sidecar file written for content that fit inline all along
// once masked, with a "read the rest with read_tool_result" header
// pointing at nothing left to read.
func TestToolResultGateMeasuresMaskedLength(t *testing.T) {
	dir := t.TempDir()
	secretValue := strings.Repeat("x", 300)
	text := "TOKEN=" + secretValue // 306 bytes pre-mask; masks down to "TOKEN=***" (9 bytes)

	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 100, 0) // limit=100: pre-mask (306) exceeds it, post-mask (9) doesn't
	cfg.Tools = []Tool{bigOutputTool("bigtool", text)}

	s, tr := runOneToolTurn(t, cfg, prov, "bigtool")

	if len(tr.Content) != 1 {
		t.Fatalf("expected the result to stay inline (1 part), got %d: %+v", len(tr.Content), tr.Content)
	}
	got := tr.Content[0].(*message.Text).Text
	if strings.Contains(got, "handle=") {
		t.Errorf("a handle was burned for a result that fits inline once masked: %q", got)
	}
	if !strings.Contains(got, "TOKEN=***") {
		t.Errorf("masked text was not returned inline: %q", got)
	}
	if strings.Contains(got, secretValue) {
		t.Errorf("secret value leaked unmasked: %q", got)
	}
	if _, ok := s.lookupToolResult("trh_1"); ok {
		t.Error("a handle was registered despite the result fitting inline once masked")
	}
}

// TestReadToolResultOutputIsNeverRetained is review finding F2's red test.
// read_tool_result's OWN output must be exempt from retention: without the
// exemption, a read whose returned text exceeds the inline limit — which is
// the ordinary case, since the documented default max_bytes (16384) sits
// right at a typical inline limit and the tool's own max (65536) is well
// above it — mints a NEW handle instead of returning inline. That makes
// the documented max_bytes ceiling unreachable in practice and doubles the
// on-disk bytes for content that is already durably retained under its
// source handle.
func TestReadToolResultOutputIsNeverRetained(t *testing.T) {
	dir := t.TempDir()
	// A retained result big enough that a full-window read_tool_result call
	// against it returns MORE than the inline limit below.
	big := linesText(3000)

	s := NewSession(Config{
		Providers:             provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:                 message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir:            dir,
		ToolResultInlineBytes: 100, // deliberately tiny: read_tool_result's own output will exceed this
	})
	handle, err := s.writeRetainedToolResult("bash", big)
	if err != nil {
		t.Fatal(err)
	}

	before := s.toolResultNextID
	out := s.maybeRetainToolResult(readToolResultToolName, message.Parts{&message.Text{Text: linesText(500)}})
	after := s.toolResultNextID

	if after != before {
		t.Errorf("toolResultNextID advanced from %d to %d — read_tool_result's own output was retained", before, after)
	}
	if len(out) != 1 {
		t.Fatalf("read_tool_result output was rewritten: %d parts, want 1 (untouched): %+v", len(out), out)
	}
	if _, ok := s.lookupToolResult(handle); !ok {
		t.Fatal("the source handle itself should still be registered")
	}
	if _, ok := s.lookupToolResult(toolResultHandlePrefix + "2"); ok {
		t.Error("a second handle was minted from read_tool_result's own output")
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
	wantCap := `[tool result truncated: tool=bash bytes=123456 preview_bytes=16384 — retaining this result would exceed the per-session retention budget; its remainder is discarded irrecoverably, though a smaller result later this session may still be retained]`
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
	if !strings.Contains(second, "exceed the per-session retention budget") || !strings.Contains(second, "irrecoverably") {
		t.Errorf("second result should hit the cap with the honest budget/irrecoverable wording: %q", second)
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

// TestToolResultCapHeaderDoesNotOverstatePermanence is a round-3 review
// finding's red test. toolResultBytes is NOT incremented on a refusal
// (only writeRetainedToolResult increments it, and that never runs on the
// cap-refused path), so a later, SMALLER oversized result can still fit
// under the SAME ceiling and be retained successfully — directly
// contradicting a header that claims "no further tool result will be
// retained this session". This drives that exact scenario end to end:
// fill most of a small cap with one retained result, get refused on a
// medium one, then successfully retain a small one afterward.
func TestToolResultCapHeaderDoesNotOverstatePermanence(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{
		SessionDir:              dir,
		ToolResultInlineBytes:   10,
		ToolResultRetainedBytes: 1000,
	})

	// First: retained, uses ~900 of the 1000-byte budget.
	if _, err := s.writeRetainedToolResult("bash", strings.Repeat("a", 900)); err != nil {
		t.Fatal(err)
	}

	// Second: refused. 900 + 150 > 1000.
	refused := s.maybeRetainToolResult("bash", message.Parts{&message.Text{Text: strings.Repeat("b", 150)}})
	if len(refused) < 1 {
		t.Fatal("expected cap-refusal content")
	}
	refusedHeader := refused[0].(*message.Text).Text
	if !strings.Contains(refusedHeader, "tool result truncated") {
		t.Fatalf("expected the cap-refusal header, got: %q", refusedHeader)
	}
	if strings.Contains(refusedHeader, "no further tool result will be retained") {
		t.Errorf("header claims permanence that is not true — a later smaller result can still be retained:\n%s", refusedHeader)
	}
	if strings.Contains(refusedHeader, "cap has been reached") {
		t.Errorf("header claims the cap was reached via accumulation, which overstates a single oversized result:\n%s", refusedHeader)
	}

	// Third: SUCCEEDS. 900 + 60 <= 1000 — proving the second call's
	// refusal was NOT "no further tool result will be retained this
	// session."
	third := s.maybeRetainToolResult("bash", message.Parts{&message.Text{Text: strings.Repeat("c", 60)}})
	thirdHeader := third[0].(*message.Text).Text
	if !strings.Contains(thirdHeader, "tool result retained") {
		t.Fatalf("a smaller result after a refusal should still be retained — got: %q", thirdHeader)
	}
}

// TestToolResultRetainedFileIsMaskedAndPrivate is review finding F4's red
// test. A retained result routinely contains a command's raw output —
// including an env dump or a leaked credential in a log line — and the
// sidecar file must not sit on disk in cleartext, group- and world-
// readable. This asserts both halves: the obvious secret-shaped line is
// masked in the bytes actually written to disk, and the file/directory
// permissions are private (0600/0700).
func TestToolResultRetainedFileIsMaskedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	secretValue := "AKIAABCDEFGHIJKLMNOP"
	text := "starting build\n" +
		"AWS_SECRET_ACCESS_KEY=" + secretValue + "\n" +
		"DB_PASSWORD=hunter2hunter2\n" +
		"build ok\n"

	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 10, 0) // tiny inline limit: force retention
	cfg.Tools = []Tool{bigOutputTool("bigtool", text)}

	s, _ := runOneToolTurn(t, cfg, prov, "bigtool")

	path := filepath.Join(dir, toolResultsDirName, s.ID, "trh_1.txt")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("sidecar file: %v", err)
	}
	onDisk := string(got)
	if strings.Contains(onDisk, secretValue) {
		t.Errorf("secret value landed on disk unmasked:\n%s", onDisk)
	}
	if strings.Contains(onDisk, "hunter2hunter2") {
		t.Errorf("password value landed on disk unmasked:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "AWS_SECRET_ACCESS_KEY=***") {
		t.Errorf("masked key/separator not preserved:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "DB_PASSWORD=***") {
		t.Errorf("masked key/separator not preserved:\n%s", onDisk)
	}
	// Benign lines survive verbatim.
	if !strings.Contains(onDisk, "starting build") || !strings.Contains(onDisk, "build ok") {
		t.Errorf("benign lines were altered:\n%s", onDisk)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("sidecar file mode = %o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Join(dir, toolResultsDirName, s.ID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("sidecar directory mode = %o, want 0700", perm)
	}
}

// TestMaskSecretsPreservesBenignText: the masker must not touch text that
// merely mentions a sensitive-sounding word without the key[=:]value shape
// — over-masking would silently corrupt ordinary output (a sentence
// containing the word "password", a variable named "token_count").
func TestMaskSecretsPreservesBenignText(t *testing.T) {
	in := "the password reset flow sends a token_count of 3\nAPI_KEY=abc123def456\n"
	got := maskSecrets(in)
	if !strings.Contains(got, "the password reset flow sends a token_count of 3") {
		t.Errorf("benign prose was altered: %q", got)
	}
	if !strings.Contains(got, "API_KEY=***") {
		t.Errorf("key=value shape was not masked: %q", got)
	}
	if strings.Contains(got, "abc123def456") {
		t.Errorf("secret value survived masking: %q", got)
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

// TestLoadSessionRegistersReadToolResultForExistingHandles is review
// finding F12's red test. newSession decides whether to register
// read_tool_result BEFORE LoadSession's record fold populates
// s.toolResults — against an empty map, every time, regardless of what the
// log actually holds. A session resumed after tool_result_inline_bytes was
// set to 0 (retention disabled going forward) can still carry real,
// replayed handles from before that change. Without this fix, the model
// sees preview lines naming a trh_N handle and a tool it cannot call:
// read_tool_result was never registered, so the call fails as an unknown
// tool rather than the tool's own clean "unknown handle" error.
func TestLoadSessionRegistersReadToolResultForExistingHandles(t *testing.T) {
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

	// Resume with retention now DISABLED (tool_result_inline_bytes: 0):
	// newSession's own gate would refuse to register read_tool_result.
	cfg2 := Config{
		Providers:             provider.Registry{prov.name: prov},
		Model:                 message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir:            dir,
		ToolResultInlineBytes: 0,
	}
	s2, err := LoadSession(cfg2, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.tools[readToolResultToolName]; !ok {
		t.Fatal("read_tool_result not registered on a resumed session carrying a replayed handle")
	}
	// And it must actually work — not just be present.
	out, err := runReadToolResult(s2, json.RawMessage(`{"handle":"trh_1","offset":1,"limit":3}`))
	if err != nil {
		t.Fatalf("read on a resumed, retention-disabled session: %v", err)
	}
	if !strings.Contains(out.Text(), "line-1") {
		t.Errorf("resumed read = %q", out.Text())
	}

	// Counterweight: a session with NO replayed handles and retention
	// disabled must still NOT register the tool — this fix must not
	// regress the ordinary disabled case into always-on.
	emptyID := newID("ses")
	log := `{"type":"session","id":"` + emptyID + `","created_at":"2026-08-19T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, emptyID+".jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	s3, err := LoadSession(Config{SessionDir: dir, ToolResultInlineBytes: 0}, emptyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s3.tools[readToolResultToolName]; ok {
		t.Error("read_tool_result registered for a resumed session with no replayed handles and retention disabled")
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

// TestLoadSessionAdvancesNextIDPastHandlesSeenInHistoryText is a round-5
// review finding's red test for silent handle reuse after resume.
//
// toolResultNextID was rebuilt ONLY from toolresult.retained pointer
// records — but persistToolResultRetainedLocked is best-effort
// (writeRetainedToolResult still returns the handle successfully even if
// the pointer-record WRITE fails, landing the error in lastPersistErr).
// So a crash between "the sidecar file is written and the preview text
// naming it is durably appended to history" and "the pointer record makes
// it to the log" leaves a resumed session's toolResultNextID pointing at
// the SAME number the crashed process already handed to the model in a
// preview the model trusts. The next retention in the resumed process then
// silently reuses that handle, overwriting the sidecar file — and
// read_tool_result serves the NEW content under the name the model
// believes still describes the OLD content.
//
// This constructs exactly that: a message record durably carrying a
// "handle=trh_7" preview header in its text, with NO corresponding
// toolresult.retained record for trh_7 anywhere in the log (simulating the
// lost pointer-record write). Resuming and then retaining a new oversized
// result must NOT mint trh_7 again.
func TestLoadSessionAdvancesNextIDPastHandlesSeenInHistoryText(t *testing.T) {
	dir := t.TempDir()
	id := "ses_01m0g96daxegnaqwtqe135ah4x"
	previewHeader := toolResultPreviewHeader("trh_7", "bash", 5000, 100, 500)
	log := strings.Join([]string{
		`{"type":"session","id":"` + id + `","created_at":"2026-08-19T00:00:00Z"}`,
		// A durable message carrying the trh_7 preview header — but NO
		// toolresult.retained record for trh_7 anywhere in this log: the
		// pointer-record write is the one that's missing.
		`{"type":"message","message":{"id":"msg_1","role":"tool","parts":[{"type":"text","text":` + jsonQuote(t, previewHeader) + `}]}}`,
		// A lower, genuinely-persisted handle, so the ordinary pointer-record
		// fold path still has something to do (and this test also confirms
		// it doesn't regress).
		`{"type":"toolresult.retained","tool_result":{"handle":"trh_2","tool":"bash","bytes":50,"lines":1}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir, ToolResultInlineBytes: 10}, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.toolResultNextID <= 7 {
		t.Fatalf("toolResultNextID = %d after resume, want > 7 (a trh_7 preview is durably in history)", s.toolResultNextID)
	}

	handle, err := s.writeRetainedToolResult("bash", "some new content, unrelated to whatever trh_7 used to mean")
	if err != nil {
		t.Fatal(err)
	}
	if handle == "trh_7" {
		t.Fatalf("resumed session re-minted trh_7 — this SILENTLY OVERWRITES the sidecar file a durable preview in history still names")
	}
}

// jsonQuote returns s as a Go/JSON string literal (quotes included), for
// building raw JSONL fixture lines inline without a separate marshal step.
func jsonQuote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
