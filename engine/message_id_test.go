package engine

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestUsableClientMessageID is the RED test for the reserved-prefix guard:
// a client-supplied user-message ID is usable verbatim unless it is empty
// or begins with one of engine's own reserved provenance prefixes for a
// DIFFERENT synthetic message kind (a compaction summary or a synthesized
// orphaned tool result).
func TestUsableClientMessageID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"cmpsum_01abc", false},
		{compactionSummaryIDTag, false},
		{message.SyntheticOrphanIDPrefix + "3-toolcall1", false},
		{"msg_client_supplied", true},
		{"anything-a-trusted-client-picks", true},
		{"cmp", true}, // shares no prefix boundary with "cmpsum"
	}
	for _, c := range cases {
		if got := usableClientMessageID(c.id); got != c.want {
			t.Errorf("usableClientMessageID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestResolveMessageIDUsesSuppliedIDVerbatim is the RED test for
// ResolveMessageID's happy path: a usable client-supplied ID passes
// through unchanged, never replaced by a server mint.
func TestResolveMessageIDUsesSuppliedIDVerbatim(t *testing.T) {
	const supplied = "console-optimistic-id-1"
	got := ResolveMessageID(supplied)
	if got != supplied {
		t.Fatalf("ResolveMessageID(%q) = %q, want the supplied id unchanged", supplied, got)
	}
}

// TestResolveMessageIDMintsOnReservedOrEmpty covers both fail-safe cases:
// an empty id and a reserved-prefix id must NEVER be used verbatim — a
// fresh "msg" TypeID is minted instead, and the prompt is never rejected.
func TestResolveMessageIDMintsOnReservedOrEmpty(t *testing.T) {
	for _, supplied := range []string{"", "cmpsum_hijack", message.SyntheticOrphanIDPrefix + "0-x"} {
		got := ResolveMessageID(supplied)
		if got == supplied {
			t.Fatalf("ResolveMessageID(%q) = %q, want a freshly minted id, not the supplied value", supplied, got)
		}
		if !strings.HasPrefix(got, "msg_") {
			t.Fatalf("ResolveMessageID(%q) = %q, want a msg_-prefixed minted id", supplied, got)
		}
	}
}
