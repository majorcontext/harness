package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestBashOutputCappedByDefault reproduces the forensic finding: a command
// that emits megabytes of output (an `apt-get`/`npm install` storm is the
// real-world trigger) must never enter the message log unbounded — one
// runaway command must not be able to poison the whole session. With no
// Config.BashOutputCap set, the tool must fall back to a sane default
// (96KB) and truncate, keeping both head and tail with a marker in between.
func TestBashOutputCappedByDefault(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	// ~5MB of output: a stand-in for an apt-get/npm install log.
	cmd := `for i in $(seq 1 200000); do echo "line-$i-01234567890123456789"; done`
	args, _ := json.Marshal(map[string]string{"command": cmd})

	tool := s.tools["bash"]
	out, err := tool.Run(context.Background(), s, args)
	if err != nil {
		t.Fatalf("bash run error = %v", err)
	}
	text := out.Text()
	if len(text) >= 5*1024*1024 {
		t.Fatalf("output not capped: got %d bytes", len(text))
	}
	if len(text) > defaultBashOutputCap+1024 {
		t.Fatalf("output len = %d, want roughly <= default cap %d", len(text), defaultBashOutputCap)
	}
	if !strings.Contains(text, "line-1-") {
		t.Error("truncated output missing the head (first lines)")
	}
	if !strings.Contains(text, "line-200000-") {
		t.Error("truncated output missing the tail (last lines)")
	}
	if !strings.Contains(text, "truncated") {
		t.Error("truncated output missing a truncation marker")
	}
}

// TestBashOutputCapConfigurable pins Config.BashOutputCap as the knob: a
// custom cap is honored instead of the default.
func TestBashOutputCapConfigurable(t *testing.T) {
	s := NewSession(Config{
		Providers:     provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		BashOutputCap: 200,
	})
	cmd := `printf 'A%.0s' $(seq 1 5000); echo; printf 'B%.0s' $(seq 1 5000)`
	args, _ := json.Marshal(map[string]string{"command": cmd})

	tool := s.tools["bash"]
	out, err := tool.Run(context.Background(), s, args)
	if err != nil {
		t.Fatalf("bash run error = %v", err)
	}
	text := out.Text()
	if len(text) > 400 {
		t.Fatalf("output len = %d, want capped close to the configured 200-byte cap", len(text))
	}
	if !strings.Contains(text, "A") || !strings.Contains(text, "B") {
		t.Errorf("expected head (A) and tail (B) both present, got %q", text)
	}
}

// TestTruncateOutputKeepsHeadAndTail unit-tests the truncation helper
// directly: small inputs pass through untouched, oversized ones keep a head
// and tail slice around a marker, and the result never exceeds the cap.
func TestTruncateOutputKeepsHeadAndTail(t *testing.T) {
	cw := newCappedWriter(10)
	if _, err := cw.Write([]byte("0123456789ABCDEFGHIJ")); err != nil {
		t.Fatal(err)
	}
	got := cw.Bytes()
	if len(got) > 10+40 { // generous slack for the marker text itself
		t.Fatalf("capped output too large: %d bytes: %q", len(got), got)
	}
	if !strings.HasPrefix(string(got), "01234") {
		t.Errorf("capped output = %q, want it to start with the head", got)
	}
	if !strings.HasSuffix(string(got), "FGHIJ") {
		t.Errorf("capped output = %q, want it to end with the tail", got)
	}
	if !strings.Contains(string(got), "truncated") {
		t.Errorf("capped output = %q, want a truncation marker", got)
	}

	small := newCappedWriter(1024)
	if _, err := small.Write([]byte("short output")); err != nil {
		t.Fatal(err)
	}
	if string(small.Bytes()) != "short output" {
		t.Errorf("small output = %q, want it untouched", small.Bytes())
	}
}
