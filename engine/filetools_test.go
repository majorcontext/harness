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
// authoritative (AGENTS.md: "never trust extension alone").
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
	readFileMaxImageBytes = len(data) - 1 // force this exact file over cap
	t.Cleanup(func() { readFileMaxImageBytes = orig })

	_, err := runToolParts(t, readFileTool(), dir, `{"path":"shot.png"}`)
	if err == nil {
		t.Fatal("want error for over-cap image")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error = %q, want it to name the image/limit", err)
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

func TestWriteFileOverwrites(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "old")
	if _, err := runTool(t, writeFileTool(), dir, `{"path":"a.txt","content":"new"}`); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "new" {
		t.Errorf("content = %q", got)
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
