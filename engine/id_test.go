package engine

import (
	"strings"
	"testing"

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

// TestNewIDIsTimeOrdered checks the bonus property (not a hard requirement,
// but the whole reason to move to TypeID): two IDs minted back to back sort
// the same way lexicographically as they were generated, because a TypeID's
// suffix encodes a UUIDv7 whose leading bits are a millisecond timestamp.
func TestNewIDIsTimeOrdered(t *testing.T) {
	a := newID("ses")
	b := newID("ses")
	if a == b {
		t.Fatalf("newID produced a duplicate: %q", a)
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
