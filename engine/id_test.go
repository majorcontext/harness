package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/typeid"
)

// TestNewIDGeneratesTypeID is the RED test for switching newID from raw
// crypto/rand hex to the typeid package: a new session/message ID must be a
// well-formed TypeID (UUIDv7-backed, spec grammar) with the requested prefix,
// and must round-trip through typeid.Parse.
func TestNewIDGeneratesTypeID(t *testing.T) {
	for _, prefix := range []string{"ses", "msg"} {
		id := newID(prefix)
		if !strings.HasPrefix(id, prefix+"_") {
			t.Fatalf("newID(%q) = %q, want prefix %q", prefix, id, prefix)
		}
		tid, err := typeid.Parse(id)
		if err != nil {
			t.Fatalf("typeid.Parse(%q) = %v, want valid TypeID", id, err)
		}
		if tid.Prefix() != prefix {
			t.Errorf("parsed prefix = %q, want %q", tid.Prefix(), prefix)
		}
		if tid.String() != id {
			t.Errorf("round-trip = %q, want %q", tid.String(), id)
		}
	}
}

// TestNewIDIsUniquePrefixedAndParseable checks what two back-to-back newID
// calls actually guarantee: distinct values, each still bearing the
// requested prefix and parseable as a TypeID. It does NOT assert ordering —
// typeid.New cannot guarantee intra-millisecond ordering (its tail bits are
// crypto/rand, per its doc comment), so two IDs minted back to back within
// the same millisecond may legitimately sort in either lexical order. The
// cross-millisecond ordering guarantee typeid.New DOES make is exercised
// separately by TestNewIDOrderedAcrossMillisecond.
func TestNewIDIsUniquePrefixedAndParseable(t *testing.T) {
	a := newID("ses")
	b := newID("ses")
	if a == b {
		t.Fatalf("newID produced a duplicate: %q", a)
	}
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "ses_") {
			t.Errorf("newID(%q) = %q, want prefix %q", "ses", id, "ses")
		}
		if _, err := typeid.Parse(id); err != nil {
			t.Errorf("typeid.Parse(%q) = %v, want valid TypeID", id, err)
		}
	}
}

// TestNewIDOrderedAcrossMillisecond verifies newID's real, end-to-end
// cross-millisecond ordering guarantee: two IDs minted in different
// milliseconds sort lexicographically in timestamp order (typeid.New's doc
// comment: "IDs generated in different milliseconds always sort in
// timestamp order"). newID has no injectable clock (it calls typeid.New,
// which uses the real wall clock), so this test waits for a real
// millisecond boundary — deterministically, by polling the freshly minted
// ID's own embedded TypeID timestamp until it advances past the first ID's,
// never with a raw sleep. The poll is bounded by an explicit deadline so a
// wedged clock fails the test instead of hanging it.
func TestNewIDOrderedAcrossMillisecond(t *testing.T) {
	a := newID("ses")
	tidA, err := typeid.Parse(a)
	if err != nil {
		t.Fatalf("typeid.Parse(%q) = %v", a, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var b string
	var tidB typeid.TypeID
	for {
		b = newID("ses")
		tidB, err = typeid.Parse(b)
		if err != nil {
			t.Fatalf("typeid.Parse(%q) = %v", b, err)
		}
		if tidB.Time().After(tidA.Time()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("millisecond never advanced after %v of polling", 5*time.Second)
		}
	}

	if !(a < b) {
		t.Errorf("a, b = %q, %q: want a < b (earlier millisecond must sort first)", a, b)
	}
}

// TestValidSessionIDAcceptsLegacyHex is the RED test for the two-format
// compat rule: pre-TypeID session logs on disk use "ses_" + 16 lowercase hex
// digits, and that shape must remain valid forever, alongside new TypeIDs.
func TestValidSessionIDAcceptsLegacyHex(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"ses_0123456789abcdef", true},   // legacy: ses_ + 16 hex
		{"ses_0123456789ABCDEF", false},  // legacy hex is lowercase only
		{"ses_0123456789abcde", false},   // 15 hex chars: too short
		{"ses_0123456789abcdef0", false}, // 17 hex chars: too long
		{"ses_gggggggggggggggg", false},  // not hex
		{newID("ses"), true},             // fresh TypeID
		{newID("msg"), false},            // right shape, wrong prefix
		{"", false},
		{"ses_", false},
		{"not-an-id", false},
		{"../../etc/passwd", false},
		{"ses_../../etc/passwd", false},
	}
	for _, c := range cases {
		if got := ValidSessionID(c.id); got != c.want {
			t.Errorf("ValidSessionID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}
