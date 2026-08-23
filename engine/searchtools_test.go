package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestSearchToolsRegisteredByDefault(t *testing.T) {
	s := NewSession(Config{WorkDir: t.TempDir()})
	for _, name := range []string{"glob", "grep", "ls"} {
		if _, ok := s.tools[name]; !ok {
			t.Errorf("tool %q not registered by default", name)
		}
	}
}
