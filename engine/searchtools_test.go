package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGlobToRegexp(t *testing.T) {
	tests := []struct {
		pattern string
		match   string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false},
		{"**/*.go", "sub/main.go", true},
		{"**/*.go", "main.go", true}, // "**/" matches zero directories too
		{"**", "a/b/c.txt", true},
		{"src/?ain.go", "src/main.go", true},
		{"src/?ain.go", "src/maain.go", false},
		{"a.b", "aXb", false}, // literal "." must not act as a wildcard
	}
	for _, tt := range tests {
		re, err := globToRegexp(tt.pattern)
		if err != nil {
			t.Fatalf("globToRegexp(%q): %v", tt.pattern, err)
		}
		if got := re.MatchString(tt.match); got != tt.want {
			t.Errorf("globToRegexp(%q).MatchString(%q) = %v, want %v", tt.pattern, tt.match, got, tt.want)
		}
	}
}

func TestGlobToolFindsNestedMatches(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main")
	writeTestFile(t, filepath.Join(dir, "pkg/sub/util.go"), "package sub")
	writeTestFile(t, filepath.Join(dir, "README.md"), "# hi")

	out, err := runTool(t, globTool(), dir, `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "pkg/sub/util.go") {
		t.Errorf("glob **/*.go = %q, want both matches", out)
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("glob **/*.go matched README.md: %q", out)
	}
}

func TestGlobToolSkipsGitDir(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".git/objects/pack/x.go"), "not real go")
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main")

	out, err := runTool(t, globTool(), dir, `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".git") {
		t.Errorf("glob descended into .git: %q", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("glob missing main.go: %q", out)
	}
}

// TestGlobToolTieBreaksEqualModTimesByPath is the regression test for a
// review finding: sort.Slice is not stable and glob's modTime sort had
// no tie-break, so files sharing a modtime (common for files written
// together — a checkout, a generator run) could appear in either
// relative order across identical calls. Forces three files to the
// EXACT same modtime and asserts the listing is alphabetical among them,
// repeatably.
func TestGlobToolTieBreaksEqualModTimesByPath(t *testing.T) {
	dir := t.TempDir()
	names := []string{"charlie.go", "alpha.go", "bravo.go"}
	same := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		p := filepath.Join(dir, name)
		writeTestFile(t, p, "package main")
		if err := os.Chtimes(p, same, same); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runTool(t, globTool(), dir, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{"alpha.go", "bravo.go", "charlie.go"}
	if len(lines) != len(want) {
		t.Fatalf("glob output = %q, want 3 lines", out)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q (equal-modtime files must tie-break alphabetically): %q", i, lines[i], w, out)
		}
	}
}

func TestGlobToolNoMatches(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "hi")
	out, err := runTool(t, globTool(), dir, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)" {
		t.Errorf("glob no-match output = %q", out)
	}
}

// TestGlobToolNonExistentBasePathReturnsError is the regression test for a
// live review finding: WalkDir's callback swallowed the ROOT path's own
// lstat error via its blanket "err != nil: skip it, don't fail the whole
// search" (correct for a descendant entry, wrong for the root itself), so
// glob("*.go", path="/does/not/exist") used to report "(no matches)" —
// indistinguishable from a real, existing, Go-file-free directory — instead
// of surfacing the caller's bad path, exactly like grep and ls both already
// do for the same input.
func TestGlobToolNonExistentBasePathReturnsError(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{WorkDir: dir})
	_, err := globTool().Run(context.Background(), s, json.RawMessage(`{"pattern":"*.go","path":"does/not/exist"}`))
	if err == nil {
		t.Error("glob against a non-existent base path: want error, got nil")
	}
}

func TestGrepToolFindsMatchingLines(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.go"), "package main\n\nfunc Foo() {}\n")
	writeTestFile(t, filepath.Join(dir, "b.go"), "package main\n\nfunc Bar() {}\n")

	out, err := runTool(t, grepTool(), dir, `{"pattern":"func Foo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:3:func Foo() {}") {
		t.Errorf("grep output = %q, want a.go:3:func Foo() {}", out)
	}
	if strings.Contains(out, "b.go") {
		t.Errorf("grep matched b.go unexpectedly: %q", out)
	}
}

func TestGrepToolGlobFilter(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.go"), "TODO: fix this\n")
	writeTestFile(t, filepath.Join(dir, "a.md"), "TODO: fix this too\n")

	out, err := runTool(t, grepTool(), dir, `{"pattern":"TODO","glob":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go") || strings.Contains(out, "a.md") {
		t.Errorf("grep glob filter output = %q", out)
	}
}

func TestGrepToolCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "Hello World\n")

	out, err := runTool(t, grepTool(), dir, `{"pattern":"hello world","case_insensitive":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Hello World") {
		t.Errorf("case-insensitive grep = %q", out)
	}
}

func TestGrepToolSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("MATCH\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, grepTool(), dir, `{"pattern":"MATCH"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)" {
		t.Errorf("grep should skip binary file, got %q", out)
	}
}

// TestGrepToolSkipsOversizedFiles proves grep never reads a file over
// maxGrepFileBytes whole into memory — a live review flagged this as an
// OOM risk (a default, no-path search walking into an unexpectedly huge
// file). The oversized file is skipped entirely, bounded via
// io.LimitReader(f, maxGrepFileBytes+1) over one open handle (not a
// separately captured os.Stat size — an earlier revision used exactly
// that TOCTOU-prone shape, which AGENTS.md's read_file guidance forbids,
// and a second review round caught it); a small file alongside it still
// matches normally.
func TestGrepToolSkipsOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	huge := make([]byte, maxGrepFileBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	copy(huge, []byte("MATCH\n"))
	if err := os.WriteFile(filepath.Join(dir, "huge.txt"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "small.txt"), "MATCH\n")

	out, err := runTool(t, grepTool(), dir, `{"pattern":"MATCH"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "huge.txt") {
		t.Errorf("grep read the oversized file: %q", out)
	}
	if !strings.Contains(out, "small.txt") {
		t.Errorf("grep missed the small file: %q", out)
	}
}

// TestGrepToolSkipsBinaryFileWithTextPrefix is the regression test for a
// live review finding: looksBinary only sniffed the first imageSniffLen
// (512) bytes, but grep then line-searches the WHOLE file regardless (up
// to maxGrepFileBytes) — a file whose first 512 bytes are plain ASCII text
// but whose body turns binary (a NUL well past the old sniff window) used
// to pass the guard entirely, leaking raw binary bytes into the tool
// result on any matching "line". The NUL here sits at ~4000 bytes: past
// the old 512-byte window, comfortably inside the new grepBinarySniffLen
// (64 KiB) one.
func TestGrepToolSkipsBinaryFileWithTextPrefix(t *testing.T) {
	dir := t.TempDir()
	body := make([]byte, 0, 4100)
	body = append(body, []byte("MATCH this is a plausible-looking text prefix\n")...)
	for len(body) < 4000 {
		body = append(body, 'x')
	}
	body = append(body, 0x00) // the binary marker, well past a 512-byte sniff
	body = append(body, []byte("\nMATCH again after the NUL\n")...)
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, grepTool(), dir, `{"pattern":"MATCH"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)" {
		t.Errorf("grep should skip a file that is binary past the first 512 bytes, got %q", out)
	}
}

func TestGrepToolInvalidPattern(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{WorkDir: dir})
	_, err := grepTool().Run(context.Background(), s, json.RawMessage(`{"pattern":"("}`))
	if err == nil {
		t.Error("grep with unbalanced paren: want error, got nil")
	}
}

func TestLsToolListsDirsFirst(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "b.txt"), "x")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "x")
	if err := os.Mkdir(filepath.Join(dir, "zdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runTool(t, lsTool(), dir, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[0] != "zdir/" || lines[1] != "a.txt" || lines[2] != "b.txt" {
		t.Errorf("ls output = %v, want [zdir/ a.txt b.txt]", lines)
	}
}

func TestLsToolEmptyDir(t *testing.T) {
	dir := t.TempDir()
	out, err := runTool(t, lsTool(), dir, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(empty directory)" {
		t.Errorf("ls empty dir = %q", out)
	}
}

func TestLsToolRelativePathResolvesAgainstWorkDir(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "sub/inner.txt"), "x")
	out, err := runTool(t, lsTool(), dir, `{"path":"sub"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "inner.txt" {
		t.Errorf("ls sub = %q, want inner.txt", out)
	}
}

// TestLsToolCapsHugeDirectoryListing is the regression test for a live
// review finding: unlike glob/grep, ls had no maxSearchResults bound at
// all, so a read-only explore/plan subagent listing a huge directory
// (node_modules, a data dir, a build-output tree) could flood the tool
// result / session context with every entry, unbounded.
func TestLsToolCapsHugeDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxSearchResults+50; i++ {
		writeTestFile(t, filepath.Join(dir, fmt.Sprintf("file-%04d.txt", i)), "x")
	}
	out, err := runTool(t, lsTool(), dir, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	// maxSearchResults entry lines plus the trailing "[truncated: ...]"
	// marker line.
	if len(lines) != maxSearchResults+1 {
		t.Errorf("ls output has %d lines, want %d (%d entries + truncation marker)", len(lines), maxSearchResults+1, maxSearchResults)
	}
	if !strings.Contains(out, "[truncated:") {
		t.Errorf("ls output missing truncation marker (last line: %q)", lines[len(lines)-1])
	}
}

func TestSearchToolsRegisteredByDefault(t *testing.T) {
	s := NewSession(Config{WorkDir: t.TempDir()})
	for _, name := range []string{"glob", "grep", "ls"} {
		if _, ok := s.tools[name]; !ok {
			t.Errorf("tool %q not registered by default", name)
		}
	}
}
