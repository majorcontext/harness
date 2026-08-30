package claudecode

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestClientNameMatchesFamily proves Name() reports the same string other
// packages route by — a divergence here would silently break
// provider.Registry.For for every claude-code model ref.
func TestClientNameMatchesFamily(t *testing.T) {
	if got := (Client{}).Name(); got != Family {
		t.Errorf("Name() = %q, want %q", got, Family)
	}
}

// TestClientStreamAlwaysErrors pins the deliberate backstop behavior: see
// the package doc comment for why Client.Stream is never expected to run
// in normal operation, and must fail loudly (never panic, never silently
// no-op) if something ever calls it anyway.
func TestClientStreamAlwaysErrors(t *testing.T) {
	ref := message.ModelRef{Provider: Family, Model: "sonnet"}
	_, err := (Client{}).Stream(context.Background(), &provider.Request{Model: ref})
	if err == nil {
		t.Fatal("Stream returned no error")
	}
	if !strings.Contains(err.Error(), ref.String()) {
		t.Errorf("error %q does not name the offending model ref %q", err, ref.String())
	}
}
