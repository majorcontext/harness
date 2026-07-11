package process

import (
	"strings"
	"testing"
	"testing/iotest"
)

// TestLastLinesShortReads encodes the PR#71 review finding: the tail path
// used a single f.Read, but io.Reader may legally short-read — the
// unfilled remainder of the buffer stayed NUL and was emitted as part of
// the "last N lines". lastLines must fill deterministically (io.ReadFull
// semantics) regardless of how the reader chunks.
func TestLastLinesShortReads(t *testing.T) {
	content := "one\ntwo\nthree\nfour\n"
	for name, r := range map[string]interface{ Read([]byte) (int, error) }{
		"one-byte-reads": iotest.OneByteReader(strings.NewReader(content)),
		"half-reads":     iotest.HalfReader(strings.NewReader(content)),
		"clean-reads":    strings.NewReader(content),
	} {
		got, err := lastLines(r, int64(len(content)), 2)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != "three\nfour" {
			t.Errorf("%s: lastLines = %q, want %q", name, got, "three\nfour")
		}
		if strings.ContainsRune(got, 0) {
			t.Errorf("%s: result contains NUL bytes — buffer not filled", name)
		}
	}
	// A reader that ends early (file truncated between Stat and read)
	// returns what it got rather than erroring or emitting NULs.
	got, err := lastLines(strings.NewReader("only\n"), 64, 3)
	if err != nil {
		t.Fatalf("truncated: %v", err)
	}
	if got != "only" || strings.ContainsRune(got, 0) {
		t.Fatalf("truncated: lastLines = %q, want %q with no NULs", got, "only")
	}
}
