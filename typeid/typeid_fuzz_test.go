package typeid

import (
	"strings"
	"testing"
)

// FuzzParse fuzzes Parse with arbitrary strings. Parse's own doc comment
// says it "strictly validat[es] both the prefix and the suffix against the
// spec grammar" — so any string it accepts is already in the spec's unique
// canonical form (TestValidVectors pins this for the hand-transcribed
// conformance vectors: "Round-trip: String() must reproduce the canonical
// input"). This fuzz target checks that same invariant against arbitrary,
// not just hand-picked, inputs: no panic, and Parse-accept implies
// re-encoding reproduces the identical string (not merely an equivalent
// one), which in turn must re-parse to the same prefix and UUID.
func FuzzParse(f *testing.F) {
	for _, v := range validVectors {
		f.Add(v.typeid)
	}
	for _, v := range invalidVectors {
		f.Add(v.typeid)
	}
	f.Add("")
	f.Add("_")
	f.Add("a_")
	f.Add("__")
	f.Add(strings.Repeat("a", 100))
	f.Add(strings.Repeat("0", 26))
	f.Add("prefix_" + strings.Repeat("0", 26))
	f.Add("préfix_" + strings.Repeat("0", 26))

	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<16 {
			t.Skip()
		}

		id, err := Parse(s)
		if err != nil {
			return // rejected input: a valid terminal outcome.
		}

		if got := id.String(); got != s {
			t.Fatalf("Parse(%q) succeeded but String() = %q, want the identical canonical string back (Parse strictly validates canonical form)", s, got)
		}

		reencoded := id.String()
		reparsed, err := Parse(reencoded)
		if err != nil {
			t.Fatalf("Parse(%q) succeeded but re-parsing its own String() output %q failed: %v", s, reencoded, err)
		}
		if reparsed.Prefix() != id.Prefix() || reparsed.UUID() != id.UUID() {
			t.Fatalf("Parse(%q) round trip mismatch: reparsed prefix=%q uuid=%x, want prefix=%q uuid=%x",
				s, reparsed.Prefix(), reparsed.UUID(), id.Prefix(), id.UUID())
		}
	})
}
