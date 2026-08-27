package engine

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// captureLogs redirects the default slog logger into a buffer for the test.
// slog.SetDefault is process-global, so a test that calls captureLogs must
// not call t.Parallel().
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestInstructionsTruncationIsLoud pins the loud-truncation contract: an
// oversize AGENTS.md keeps its head, carries an in-band marker that names the
// path and both byte sizes, and writes one WARN log line. A silent cut once
// dropped 344 KiB of a 408 KiB AGENTS.md with the model never told.
func TestInstructionsTruncationIsLoud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	body := strings.Repeat("x", 70*1024)
	writeInstr(t, path, body)

	buf := captureLogs(t)
	content, _, err := loadInstructions(dir, defaultMaxInstructionsBytes)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}

	head := strings.Repeat("x", defaultMaxInstructionsBytes)
	if !strings.HasPrefix(content, head) {
		t.Errorf("truncated content lost the head: got %d bytes", len(content))
	}
	marker := strings.TrimPrefix(content, head)
	for _, want := range []string{
		"truncated",
		path,
		strconv.Itoa(len(body)), // original size
		strconv.Itoa(defaultMaxInstructionsBytes),             // kept size
		strconv.Itoa(len(body) - defaultMaxInstructionsBytes), // dropped size
		"read_file",
	} {
		if !strings.Contains(marker, want) {
			t.Errorf("marker %q does not contain %q", marker, want)
		}
	}

	if !strings.HasPrefix(marker, "\n[... truncated:") || !strings.HasSuffix(marker, "...]") {
		t.Errorf("marker %q does not use the [... ... ...] bracket form", marker)
	}

	out := buf.String()
	if !strings.Contains(out, "WARN") {
		t.Errorf("expected a WARN log line, got:\n%s", out)
	}
	for _, want := range []string{"instructions", path, strconv.Itoa(len(body)), strconv.Itoa(defaultMaxInstructionsBytes)} {
		if !strings.Contains(out, want) {
			t.Errorf("log line %q does not contain %q", out, want)
		}
	}
}

// TestInstructionsUnderCapUntouched verifies an under-cap file gets no marker
// and no log line.
func TestInstructionsUnderCapUntouched(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "small and complete")

	buf := captureLogs(t)
	content, _, err := loadInstructions(dir, defaultMaxInstructionsBytes)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if content != "small and complete" {
		t.Errorf("content = %q, want the file verbatim", content)
	}
	if out := buf.String(); out != "" {
		t.Errorf("under-cap file logged: %s", out)
	}
}

// TestInstructionsMaxBytesConfigurable pins InstructionsConfig.MaxBytes: zero
// takes the 64 KiB default, a positive value sets the cap, and a negative
// value disables truncation for a deployment that wants the whole file.
func TestInstructionsMaxBytesConfigurable(t *testing.T) {
	tests := []struct {
		name     string
		maxBytes int
		size     int
		want     int // kept body bytes before the marker
	}{
		{"zero takes the default", 0, 70 * 1024, defaultMaxInstructionsBytes},
		{"positive sets the cap", 1024, 4096, 1024},
		{"negative disables the cap", -1, 70 * 1024, 70 * 1024},
		{"cap of one keeps one byte", 1, 100, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := strings.Repeat("x", tc.size)
			writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
			captureLogs(t)

			content, _, err := loadInstructions(dir, resolveInstructionsMaxBytes(&InstructionsConfig{MaxBytes: tc.maxBytes}))
			if err != nil {
				t.Fatalf("loadInstructions: %v", err)
			}
			kept := len(content)
			if i := strings.Index(content, "[..."); i >= 0 {
				kept = len(strings.TrimSuffix(content[:i], "\n"))
			}
			if kept != tc.want {
				t.Errorf("kept %d body bytes, want %d", kept, tc.want)
			}
			if tc.want == tc.size && strings.Contains(content, "[...") {
				t.Errorf("an untruncated file must carry no marker: %q", content[len(content)-100:])
			}
		})
	}
}

// TestInstructionsTruncationRuneBoundary verifies a cap that lands inside a
// multi-byte rune trims back to a rune boundary, and that the marker reports
// the KEPT byte count after that trim, not the cap.
func TestInstructionsTruncationRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	// Each "é" is 2 bytes, so a cap of 5 lands mid-rune: 4 bytes are kept.
	body := strings.Repeat("é", 8)
	writeInstr(t, path, body)
	captureLogs(t)

	content, _, err := loadInstructions(dir, 5)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	kept, _, ok := strings.Cut(content, "\n[...")
	if !ok {
		t.Fatalf("content carries no marker: %q", content)
	}
	if kept != strings.Repeat("é", 2) {
		t.Errorf("kept = %q, want 2 whole runes", kept)
	}
	if !strings.Contains(content, "The first 4 bytes are above") {
		t.Errorf("marker must report 4 kept bytes, not the cap of 5: %q", content)
	}
	if !strings.Contains(content, "12 bytes are not shown") {
		t.Errorf("marker must report 12 dropped bytes: %q", content)
	}

	// A cap below the first rune keeps nothing, and still says so.
	degenerate, _, err := loadInstructions(dir, 1)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if !strings.HasPrefix(degenerate, "\n[... truncated:") {
		t.Errorf("cap below one rune must keep no content: %q", degenerate)
	}
	if !strings.Contains(degenerate, "The first 0 bytes are above") {
		t.Errorf("marker must report 0 kept bytes: %q", degenerate)
	}
}

// TestInstructionsSessionCapFromConfig drives the cap through a real session:
// the injected system segment must carry the marker when Config.Instructions
// sets a small MaxBytes.
func TestInstructionsSessionCapFromConfig(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), strings.Repeat("y", 4096))
	captureLogs(t)

	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{MaxBytes: 512}}, 1)
	seg := prov.requests[0].System[2]
	if !strings.Contains(seg, "[...") {
		t.Errorf("segment carries no truncation marker: %q", seg)
	}
	if !strings.Contains(seg, strings.Repeat("y", 512)) || strings.Contains(seg, strings.Repeat("y", 513)) {
		t.Errorf("segment does not carry exactly 512 kept body bytes: %q", seg)
	}
}

// TestInstructionsPathOverrideTruncationIsLoud verifies the explicit-path
// branch shares the cap and the marker with auto-discovery.
func TestInstructionsPathOverrideTruncationIsLoud(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "custom.md")
	if err := os.WriteFile(override, []byte(strings.Repeat("z", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	buf := captureLogs(t)

	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{Path: override, MaxBytes: 256}}, 1)
	seg := prov.requests[0].System[2]
	if !strings.Contains(seg, "[...") {
		t.Errorf("override segment carries no truncation marker: %q", seg)
	}
	if !strings.Contains(buf.String(), "custom.md") {
		t.Errorf("expected a WARN line naming custom.md, got:\n%s", buf.String())
	}
}
