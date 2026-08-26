package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// runTool invokes a built-in tool on a throwaway session rooted at workDir.
func runTool(t *testing.T, tool Tool, workDir, args string) (string, error) {
	t.Helper()
	s := NewSession(Config{WorkDir: workDir})
	out, err := tool.Run(context.Background(), s, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	return out.Text(), nil
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertFileEdited fails the test unless events contains exactly one
// file.edited event for wantPath.
func assertFileEdited(t *testing.T, events []plugin.Event, wantPath string) {
	t.Helper()
	var found int
	for _, ev := range events {
		if ev.Type != plugin.EventFileEdited {
			continue
		}
		found++
		var props plugin.FileEditedProperties
		if err := json.Unmarshal(ev.Properties, &props); err != nil {
			t.Fatal(err)
		}
		if props.Path != wantPath {
			t.Errorf("file.edited path = %q, want %q", props.Path, wantPath)
		}
	}
	if found != 1 {
		t.Fatalf("file.edited events = %d, want 1: %+v", found, events)
	}
}

func TestWriteFileEmitsFileEdited(t *testing.T) {
	dir := t.TempDir()
	hooks := &fakeHooks{}
	s := NewSession(Config{WorkDir: dir, Hooks: hooks})

	if _, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","content":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	assertFileEdited(t, hooks.events, filepath.Join(dir, "a.txt"))
}

func TestEditFileEmitsFileEdited(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "alpha\n")
	hooks := &fakeHooks{}
	s := NewSession(Config{WorkDir: dir, Hooks: hooks})

	if _, err := editFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","old_string":"alpha","new_string":"beta"}`)); err != nil {
		t.Fatal(err)
	}
	assertFileEdited(t, hooks.events, filepath.Join(dir, "a.txt"))
}

func TestEditFileFailureEmitsNoFileEdited(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "alpha\n")
	hooks := &fakeHooks{}
	s := NewSession(Config{WorkDir: dir, Hooks: hooks})

	if _, err := editFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","old_string":"nope","new_string":"beta"}`)); err == nil {
		t.Fatal("expected error")
	}
	for _, ev := range hooks.events {
		if ev.Type == plugin.EventFileEdited {
			t.Errorf("file.edited emitted on failed edit: %+v", ev)
		}
	}
}

func TestReadFileBasic(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\ngamma\n")

	out, err := runTool(t, readFileTool(), dir, `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "1→alpha\n2→beta\n3→gamma"
	if out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestReadFileEmpty(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "empty.txt"), "")

	out, err := runTool(t, readFileTool(), dir, `{"path":"empty.txt"}`)
	if err != nil {
		t.Fatalf("read_file on empty file: %v", err)
	}
	if out != "(empty file)" {
		t.Errorf("out = %q, want %q", out, "(empty file)")
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	writeTestFile(t, filepath.Join(dir, "a.txt"), strings.Join(lines, "\n")+"\n")

	out, err := runTool(t, readFileTool(), dir, `{"path":"a.txt","offset":3,"limit":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3→line3") || !strings.Contains(out, "4→line4") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "line5") || strings.Contains(out, "2→") {
		t.Errorf("out includes lines outside window: %q", out)
	}
	if !strings.Contains(out, "[truncated: showing lines 3-4 of 10]") {
		t.Errorf("missing truncation footer: %q", out)
	}
}

func TestReadFileNoFooterWhenComplete(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "one\ntwo\n")

	out, err := runTool(t, readFileTool(), dir, `{"path":"a.txt","limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[truncated") {
		t.Errorf("unexpected footer: %q", out)
	}
}

func TestReadFileLongLineTruncated(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 2500)
	writeTestFile(t, filepath.Join(dir, "a.txt"), long+"\nshort\n")

	out, err := runTool(t, readFileTool(), dir, `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasSuffix(first, "…") {
		t.Errorf("long line not marked truncated: %q…", first[:50])
	}
	if got := len([]rune(strings.TrimPrefix(first, "1→"))); got != 2001 {
		t.Errorf("truncated line rune length = %d, want 2001", got)
	}
	if !strings.Contains(out, "2→short") {
		t.Errorf("out = %q", out)
	}
}

func TestReadFileMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := runTool(t, readFileTool(), dir, `{"path":"nope.txt"}`); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestReadFileDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := runTool(t, readFileTool(), dir, `{"path":"."}`); err == nil {
		t.Fatal("want error for directory")
	}
}

func TestReadFileAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "abs.txt")
	writeTestFile(t, p, "hello\n")
	// WorkDir deliberately different from the file's directory.
	out, err := runTool(t, readFileTool(), t.TempDir(), fmt.Sprintf(`{"path":%q}`, p))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1→hello") {
		t.Errorf("out = %q", out)
	}
}

// runToolParts invokes a built-in tool on a throwaway session rooted at
// workDir and returns the raw message.Parts, not just the Text()
// concatenation runTool returns — needed to inspect a returned Blob part.
func runToolParts(t *testing.T, tool Tool, workDir, args string) (message.Parts, error) {
	t.Helper()
	s := NewSession(Config{WorkDir: workDir})
	return tool.Run(context.Background(), s, json.RawMessage(args))
}

// tinyPNG builds a real, compliant, tiny PNG — a 2x2 solid image — so image
// tests exercise genuine image bytes without a committed binary fixture
// (AGENTS.md's fixture-size lesson from #101: keep test images tiny).
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// TestSniffMediaTypeSurvivesShortReads red-verifies the io.ReadFull sniff
// in sniffMediaType: iotest.OneByteReader wraps the source so every Read
// call returns exactly one byte, the shape a pipe, a FUSE/network mount, or
// a signal-interrupted read(2) can produce. A single plain Read against
// such a source would see only the first byte and misclassify almost every
// real image; io.ReadFull is what makes classification correct regardless
// of how many underlying reads it takes.
func TestSniffMediaTypeSurvivesShortReads(t *testing.T) {
	data := tinyPNG(t)
	mediaType, sniff, err := sniffMediaType(iotest.OneByteReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "image/png" {
		t.Errorf("mediaType = %q, want image/png", mediaType)
	}
	if !bytes.Equal(sniff, data) {
		t.Errorf("sniffed %d bytes, want all %d source bytes (file is under imageSniffLen)", len(sniff), len(data))
	}
}

func TestReadFileImagePNGReturnsTextAndBlob(t *testing.T) {
	dir := t.TempDir()
	data := tinyPNG(t)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	parts, err := runToolParts(t, readFileTool(), dir, `{"path":"shot.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (Text, Blob): %+v", len(parts), parts)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("parts[0] = %T, want *message.Text", parts[0])
	}
	for _, want := range []string{"image/png", fmt.Sprintf("%d bytes", len(data)), "2x2"} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("summary %q missing %q", text.Text, want)
		}
	}
	blob, ok := parts[1].(*message.Blob)
	if !ok {
		t.Fatalf("parts[1] = %T, want *message.Blob", parts[1])
	}
	if blob.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", blob.MediaType)
	}
	if !bytes.Equal(blob.Data, data) {
		t.Errorf("Blob.Data does not round-trip the source file bytes")
	}
}

// TestReadFileImageExtensionLieTextNamedPNGStaysText is the surplus-direction
// half of the extension-lies pair: a file NAMED .png that actually holds
// plain text bytes must NOT be sniffed as an image. Trusting the extension
// alone would wrongly wrap plain source text in an image Blob.
func TestReadFileImageExtensionLieTextNamedPNGStaysText(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "notes.png"), "just some text\nsecond line\n")

	parts, err := runToolParts(t, readFileTool(), dir, `{"path":"notes.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1 (Text only): %+v", len(parts), parts)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("parts[0] = %T, want *message.Text", parts[0])
	}
	if !strings.Contains(text.Text, "1→just some text") {
		t.Errorf("out = %q, want ordinary line-numbered text", text.Text)
	}
}

// TestReadFileImageExtensionLiePNGNamedTxtIsSniffedAsImage is the missing-
// direction half: a file named .txt that actually holds PNG magic bytes must
// still be recognized as an image. Extension is a hint only; magic bytes are
// authoritative (AGENTS.md: read_file classifies "never by its extension").
func TestReadFileImageExtensionLiePNGNamedTxtIsSniffedAsImage(t *testing.T) {
	dir := t.TempDir()
	data := tinyPNG(t)
	if err := os.WriteFile(filepath.Join(dir, "disguised.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	parts, err := runToolParts(t, readFileTool(), dir, `{"path":"disguised.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (Text, Blob): %+v", len(parts), parts)
	}
	blob, ok := parts[1].(*message.Blob)
	if !ok {
		t.Fatalf("parts[1] = %T, want *message.Blob", parts[1])
	}
	if blob.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", blob.MediaType)
	}
}

func TestReadFileImageOverCapReturnsTextErrorNoBlob(t *testing.T) {
	dir := t.TempDir()
	data := tinyPNG(t)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	orig := readFileMaxImageBytes
	wantCap := len(data) - 1
	readFileMaxImageBytes = wantCap // force this exact file over cap
	t.Cleanup(func() { readFileMaxImageBytes = orig })

	parts, err := runToolParts(t, readFileTool(), dir, `{"path":"shot.png"}`)
	if err == nil {
		t.Fatal("want error for over-cap image")
	}
	if parts != nil {
		t.Errorf("parts = %+v, want nil (no Blob) on an over-cap image error", parts)
	}
	wantSubstr := fmt.Sprintf("%d-byte read_file image limit", wantCap)
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want it to contain %q", err, wantSubstr)
	}
}

// TestReadFileImageTruncatedPNGFallsBackToText proves the DecodeConfig gate
// in readPathContent: a file whose first 8 bytes are a genuine PNG
// signature, but whose body is not a real PNG (a truncated download, a
// corrupt write), fails image.DecodeConfig and is read as ordinary text
// instead of shipping a Blob the model cannot use.
func TestReadFileImageTruncatedPNGFallsBackToText(t *testing.T) {
	dir := t.TempDir()
	pngSignature := []byte("\x89PNG\r\n\x1a\n")
	body := append(pngSignature, []byte("not a real IHDR chunk")...)
	if err := os.WriteFile(filepath.Join(dir, "broken.png"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	parts, err := runToolParts(t, readFileTool(), dir, `{"path":"broken.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1 (Text only, no Blob for an undecodable image): %+v", len(parts), parts)
	}
	if _, ok := parts[0].(*message.Text); !ok {
		t.Fatalf("parts[0] = %T, want *message.Text", parts[0])
	}
}

// TestReadFileImageToolCallProducesBlobToolResult drives read_file through
// the SAME production dispatch path a real turn uses — an assistant
// ToolCall executed by Session.runToolCalls, not a direct Tool.Run call —
// closing the gap between the engine-level Tool.Run tests above and the
// transcode-level golden test in provider/anthropic/transcode_test.go,
// which hand-builds a ToolResult shaped like read_file's output rather than
// obtaining one from read_file itself (AGENTS.md's "verification drives
// the production entry point" rule).
func TestReadFileImageToolCallProducesBlobToolResult(t *testing.T) {
	dir := t.TempDir()
	data := tinyPNG(t)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSession(Config{WorkDir: dir})
	asst := &message.Message{
		Role: message.RoleAssistant,
		Parts: message.Parts{
			&message.ToolCall{CallID: "call1", Name: "read_file", Arguments: json.RawMessage(`{"path":"shot.png"}`)},
		},
	}

	results := s.runToolCalls(context.Background(), asst)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	tr, ok := results[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("results[0] = %T, want *message.ToolResult", results[0])
	}
	if tr.IsError {
		t.Fatalf("ToolResult.IsError = true, content: %+v", tr.Content)
	}
	if len(tr.Content) != 2 {
		t.Fatalf("ToolResult.Content = %d parts, want 2 (Text, Blob): %+v", len(tr.Content), tr.Content)
	}
	blob, ok := tr.Content[1].(*message.Blob)
	if !ok {
		t.Fatalf("ToolResult.Content[1] = %T, want *message.Blob", tr.Content[1])
	}
	if blob.MediaType != "image/png" || !bytes.Equal(blob.Data, data) {
		t.Errorf("Blob = %+v, want MediaType image/png and Data matching the source file", blob)
	}
}

func TestWriteFileCreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	out, err := runTool(t, writeFileTool(), dir, `{"path":"a/b/c.txt","content":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote 5 bytes to ") {
		t.Errorf("out = %q", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q", got)
	}
}

// TestWriteFileUnreadExistingFileErrors is the red-verified regression guard
// for the core defect this feature closes: before the read-before-overwrite
// guard existed, write_file overwrote ANY existing file unconditionally — a
// model could destroy a file it never opened. Reverting the os.Stat/
// readHashFor block in writeFileTool's Run (engine/filetools.go) makes this
// test fail (write_file silently succeeds and "new" clobbers "old"),
// confirming the guard, not something else, is what this test exercises.
func TestWriteFileUnreadExistingFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "old")

	_, err := runTool(t, writeFileTool(), dir, `{"path":"a.txt","content":"new"}`)
	if err == nil {
		t.Fatal("want error overwriting an existing file never read this session")
	}
	wantSubstr := "exists and has not been read this session; read it first (or use edit_file)"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want it to contain %q", err, wantSubstr)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "old" {
		t.Errorf("content = %q, want unchanged %q", got, "old")
	}
}

// TestWriteFileReadThenWriteSucceeds is the happy path the guard must not
// block: read_file the existing content, then write_file succeeds and
// overwrites it. runTool builds a fresh session per call, so both tool
// invocations must share the SAME session for the guard to see the read.
func TestWriteFileReadThenWriteSucceeds(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "old")
	s := NewSession(Config{WorkDir: dir})

	if _, err := readFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt"}`)); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if _, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","content":"new"}`)); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

// TestWriteFileChangedSinceReadErrors proves the guard's second check: even
// with a recorded read, write_file refuses to overwrite a file that changed
// on disk since that read (a concurrent writer, another tool, an external
// process) — trusting only the "was it ever read" bit would let a write
// through against content the session's last read no longer describes.
func TestWriteFileChangedSinceReadErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeTestFile(t, path, "old")
	s := NewSession(Config{WorkDir: dir})

	if _, err := readFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt"}`)); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	// Simulate an external change landing after the read, bypassing every
	// tool this session tracks.
	writeTestFile(t, path, "externally changed")

	_, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","content":"new"}`))
	if err == nil {
		t.Fatal("want error overwriting a file changed on disk since it was read")
	}
	wantSubstr := "changed on disk since it was read; read it again before overwriting"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want it to contain %q", err, wantSubstr)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "externally changed" {
		t.Errorf("content = %q, want unchanged %q", got, "externally changed")
	}
}

// TestWriteFileCreateNewUnguarded proves the guard is scoped to EXISTING
// files only: creating a brand-new path never requires a prior read_file,
// since there is no existing content to protect. TestWriteFileCreatesNestedDirs
// above already covers this same path with a fresh runTool session per
// call; this test makes the "never read this session" condition explicit.
func TestWriteFileCreateNewUnguarded(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{WorkDir: dir})

	if _, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"new.txt","content":"hello"}`)); err != nil {
		t.Fatalf("write_file on a new path: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

// TestEditFileUpdatesReadGuardHash proves requirement 3 of the guard design:
// a successful edit_file updates the tracked hash to the post-edit content,
// so an edit-then-write sequence on the SAME path never has to read_file
// again — edit_file's own exact-match requirement already proves the model
// saw the pre-edit content, and the file now holds exactly what this
// session just wrote.
func TestEditFileUpdatesReadGuardHash(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "hello world\n")
	s := NewSession(Config{WorkDir: dir})

	if _, err := readFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt"}`)); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if _, err := editFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","old_string":"world","new_string":"there"}`)); err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	// No read_file call between edit_file and write_file: the guard must
	// trust edit_file's own hash update, not require a fresh read.
	if _, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","content":"replaced entirely"}`)); err != nil {
		t.Fatalf("write_file after edit_file with no intervening read_file: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "replaced entirely" {
		t.Errorf("content = %q, want %q", got, "replaced entirely")
	}
}

// TestReloadClearsReadGuardSet proves requirement 4: the read set is
// runtime-only and never persisted. A session that read a path, then was
// persisted and reloaded via LoadSession (a fresh process resuming a
// session, or this same process after a restart), must NOT remember that
// read — write_file on the same path in the reloaded session requires a
// fresh read_file, exactly as if the path had never been read at all.
func TestReloadClearsReadGuardSet(t *testing.T) {
	dir := t.TempDir()
	sessionDir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "old")

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := Config{
		WorkDir:    dir,
		SessionDir: sessionDir,
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: prov.name, Model: "m1"},
	}
	s := NewSession(cfg)
	// A real turn so LoadSession below has a persisted log to find.
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	if _, err := readFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt"}`)); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	// Confirm the guard is actually satisfied on the live session before
	// reloading, so the reload assertion below means what it claims.
	if _, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","content":"live session can overwrite"}`)); err != nil {
		t.Fatalf("write_file on live session after read_file: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, "a.txt"), "old again") // restore for the reloaded check below

	reloaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writeFileTool().Run(context.Background(), reloaded, json.RawMessage(`{"path":"a.txt","content":"new"}`))
	if err == nil {
		t.Fatal("want error: reloaded session's read set must be empty")
	}
	wantSubstr := "exists and has not been read this session"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want it to contain %q", err, wantSubstr)
	}
}

func TestEditFileSingleOccurrence(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "hello world\n")

	out, err := runTool(t, editFileTool(), dir, `{"path":"a.txt","old_string":"world","new_string":"there"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced 1 occurrence(s) in ") {
		t.Errorf("out = %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "hello there\n" {
		t.Errorf("content = %q", got)
	}
}

func TestEditFileAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "foo foo foo\n")

	_, err := runTool(t, editFileTool(), dir, `{"path":"a.txt","old_string":"foo","new_string":"bar"}`)
	if err == nil {
		t.Fatal("want ambiguity error")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error should name the count: %v", err)
	}
	if !strings.Contains(err.Error(), "replace_all") {
		t.Errorf("error should suggest replace_all: %v", err)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "foo foo foo\n")

	out, err := runTool(t, editFileTool(), dir, `{"path":"a.txt","old_string":"foo","new_string":"bar","replace_all":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced 3 occurrence(s) in ") {
		t.Errorf("out = %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "bar bar bar\n" {
		t.Errorf("content = %q", got)
	}
}

func TestEditFileNotFound(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "hello\n")

	_, err := runTool(t, editFileTool(), dir, `{"path":"a.txt","old_string":"nope","new_string":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "old_string not found") {
		t.Errorf("err = %v", err)
	}
}

func TestEditFileSameStrings(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "hello\n")

	_, err := runTool(t, editFileTool(), dir, `{"path":"a.txt","old_string":"hello","new_string":"hello"}`)
	if err == nil {
		t.Fatal("want error when old_string == new_string")
	}
}

func TestFileToolsOfferedToProvider(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range prov.requests[0].Tools {
		names = append(names, d.Name)
	}
	for _, want := range []string{"read_file", "write_file", "edit_file"} {
		if !contains(names, want) {
			t.Errorf("tool %q not offered; got %v", want, names)
		}
	}
}

// TestWriteFileFailedReadDoesNotUnlock proves a read_file that ERRORED
// (offset past end-of-file) does not authorize an overwrite: the guard
// records a hash only at a return that handed the model content.
func TestWriteFileFailedReadDoesNotUnlock(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "line1\nline2\n")
	s := NewSession(Config{WorkDir: dir})

	if _, err := readFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","offset":99}`)); err == nil {
		t.Fatal("want read_file error for offset past end of file")
	}
	_, err := writeFileTool().Run(context.Background(), s, json.RawMessage(`{"path":"a.txt","content":"new"}`))
	if err == nil {
		t.Fatal("want write_file refusal: the only read of this file errored")
	}
	if !strings.Contains(err.Error(), "has not been read this session") {
		t.Errorf("error = %q, want the unread-file refusal", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "line1\nline2\n" {
		t.Errorf("content = %q, want unchanged", got)
	}
}

// TestWriteFileStatErrorRefuses proves a stat failure OTHER than not-exist
// refuses the write: an unreadable path cannot prove no protected file
// exists there, so falling through to unguarded-create would be a hole.
func TestWriteFileStatErrorRefuses(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission-based stat failure cannot be produced as root")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(locked, "a.txt"), "old")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, err := writeFileTool().Run(context.Background(), NewSession(Config{WorkDir: dir}), json.RawMessage(`{"path":"locked/a.txt","content":"new"}`))
	if err == nil {
		t.Fatal("want write_file refusal on a stat error that is not not-exist")
	}
	if !strings.Contains(err.Error(), "cannot stat") {
		t.Errorf("error = %q, want the cannot-stat refusal", err)
	}
}

// TestWriteFileSpecialFileUnguarded proves the guard's scope is REGULAR
// files, as documented: writing to a device file like /dev/null (a common
// discard idiom) needs no prior read.
func TestWriteFileSpecialFileUnguarded(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("/dev/null unavailable")
	}
	dir := t.TempDir()
	if _, err := writeFileTool().Run(context.Background(), NewSession(Config{WorkDir: dir}), json.RawMessage(`{"path":"/dev/null","content":"discard"}`)); err != nil {
		t.Fatalf("write_file to /dev/null: %v", err)
	}
}
