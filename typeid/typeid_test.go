package typeid

import (
	"strings"
	"testing"
	"time"
)

// --- Conformance vectors -----------------------------------------------
//
// Embedded by hand from the official TypeID spec conformance suite
// (github.com/jetify-com/typeid, spec v0.3), fetched from:
//   https://raw.githubusercontent.com/jetify-com/typeid/main/spec/valid.yml
//   https://raw.githubusercontent.com/jetify-com/typeid/main/spec/invalid.yml
//
// This package has no YAML dependency, so the vectors are transcribed
// directly as Go table entries rather than parsed at runtime.

type validVector struct {
	name   string
	typeid string
	prefix string
	uuid   string // canonical 8-4-4-4-12 hex form
}

var validVectors = []validVector{
	{"nil", "00000000000000000000000000", "", "00000000-0000-0000-0000-000000000000"},
	{"one", "00000000000000000000000001", "", "00000000-0000-0000-0000-000000000001"},
	{"ten", "0000000000000000000000000a", "", "00000000-0000-0000-0000-00000000000a"},
	{"sixteen", "0000000000000000000000000g", "", "00000000-0000-0000-0000-000000000010"},
	{"thirty-two", "00000000000000000000000010", "", "00000000-0000-0000-0000-000000000020"},
	{"max-valid", "7zzzzzzzzzzzzzzzzzzzzzzzzz", "", "ffffffff-ffff-ffff-ffff-ffffffffffff"},
	{"valid-alphabet", "prefix_0123456789abcdefghjkmnpqrs", "prefix", "0110c853-1d09-52d8-d73e-1194e95b5f19"},
	{"valid-uuidv7", "prefix_01h455vb4pex5vsknk084sn02q", "prefix", "01890a5d-ac96-774b-bcce-b302099a8057"},
	{"prefix-underscore", "pre_fix_00000000000000000000000000", "pre_fix", "00000000-0000-0000-0000-000000000000"},
}

type invalidVector struct {
	name        string
	typeid      string
	description string
}

var invalidVectors = []invalidVector{
	{"prefix-uppercase", "PREFIX_00000000000000000000000000", "The prefix should be lowercase with no uppercase letters"},
	{"prefix-numeric", "12345_00000000000000000000000000", "The prefix can't have numbers, it needs to be alphabetic"},
	{"prefix-period", "pre.fix_00000000000000000000000000", "The prefix can't have symbols, it needs to be alphabetic"},
	{"prefix-non-ascii", "préfix_00000000000000000000000000", "The prefix can only have ascii letters"},
	{"prefix-spaces", "  prefix_00000000000000000000000000", "The prefix can't have any spaces"},
	{"prefix-64-chars", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl_00000000000000000000000000", "The prefix can't be 64 characters, it needs to be 63 characters or less"},
	{"separator-empty-prefix", "_00000000000000000000000000", "If the prefix is empty, the separator should not be there"},
	{"separator-empty", "_", "A separator by itself should not be treated as the empty string"},
	{"suffix-short", "prefix_1234567890123456789012345", "The suffix can't be 25 characters, it needs to be exactly 26 characters"},
	{"suffix-long", "prefix_123456789012345678901234567", "The suffix can't be 27 characters, it needs to be exactly 26 characters"},
	{"suffix-spaces", "prefix_1234567890123456789012345 ", "The suffix can't have any spaces"},
	{"suffix-uppercase", "prefix_0123456789ABCDEFGHJKMNPQRS", "The suffix should be lowercase with no uppercase letters"},
	{"suffix-hyphens", "prefix_123456789-123456789-123456", "The suffix can't have any hyphens"},
	{"suffix-wrong-alphabet", "prefix_ooooooiiiiiiuuuuuuulllllll", "The suffix should only have letters from the spec's alphabet"},
	{"suffix-ambiguous-crockford", "prefix_i23456789ol23456789oi23456", "The suffix should not have any ambiguous characters from the crockford encoding"},
	{"suffix-hyphens-crockford", "prefix_123456789-0123456789-0123456", "The suffix can't ignore hyphens as in the crockford encoding"},
	{"suffix-overflow", "prefix_8zzzzzzzzzzzzzzzzzzzzzzzzz", "The suffix should encode at most 128-bits"},
	{"prefix-underscore-start", "_prefix_00000000000000000000000000", "The prefix can't start with an underscore"},
	{"prefix-underscore-end", "prefix__00000000000000000000000000", "The prefix can't end with an underscore"},
	{"empty", "", "The empty string is not a valid typeid"},
	{"prefix-empty", "prefix_", "The suffix can't be the empty string"},
}

// mustHexUUID parses a canonical 8-4-4-4-12 hex UUID string into its 16
// raw bytes. It is a test helper only: production code never needs to
// parse hyphenated UUID strings, only the TypeID suffix encoding.
func mustHexUUID(t *testing.T, s string) [16]byte {
	t.Helper()
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) != 32 {
		t.Fatalf("mustHexUUID: %q is not a 32-hex-digit UUID", s)
	}
	nibble := func(c byte) byte {
		switch {
		case c >= '0' && c <= '9':
			return c - '0'
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10
		}
		t.Fatalf("mustHexUUID: invalid hex digit %q in %q", c, s)
		return 0
	}
	var out [16]byte
	for i := 0; i < 16; i++ {
		out[i] = nibble(hex[2*i])<<4 | nibble(hex[2*i+1])
	}
	return out
}

func TestValidVectors(t *testing.T) {
	for _, v := range validVectors {
		t.Run(v.name, func(t *testing.T) {
			id, err := Parse(v.typeid)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", v.typeid, err)
			}
			if id.Prefix() != v.prefix {
				t.Errorf("Prefix() = %q, want %q", id.Prefix(), v.prefix)
			}
			wantUUID := mustHexUUID(t, v.uuid)
			if id.UUID() != wantUUID {
				t.Errorf("UUID() = %x, want %x", id.UUID(), wantUUID)
			}
			// Round-trip: String() must reproduce the canonical input.
			if got := id.String(); got != v.typeid {
				t.Errorf("String() = %q, want %q", got, v.typeid)
			}
			// Encoding the expected prefix+UUID directly must also
			// reproduce the canonical TypeID string.
			encoded := TypeID{prefix: v.prefix, id: wantUUID}
			if got := encoded.String(); got != v.typeid {
				t.Errorf("encode(prefix, uuid).String() = %q, want %q", got, v.typeid)
			}
		})
	}
}

func TestInvalidVectors(t *testing.T) {
	for _, v := range invalidVectors {
		t.Run(v.name, func(t *testing.T) {
			id, err := Parse(v.typeid)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded (%v), want error: %s", v.typeid, id, v.description)
			}
		})
	}
}

func TestNewRoundTrip(t *testing.T) {
	id, err := New("user")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := id.String()
	got, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	if got.Prefix() != "user" {
		t.Errorf("Prefix() = %q, want %q", got.Prefix(), "user")
	}
	if got.UUID() != id.UUID() {
		t.Errorf("UUID() = %x, want %x", got.UUID(), id.UUID())
	}
	if got.String() != s {
		t.Errorf("String() = %q, want %q", got.String(), s)
	}
}

func TestNewEmptyPrefix(t *testing.T) {
	id, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.Contains(id.String(), "_") {
		t.Errorf("String() = %q, empty prefix must produce no separator", id.String())
	}
	if _, err := Parse(id.String()); err != nil {
		t.Fatalf("Parse(%q): %v", id.String(), err)
	}
}

// TestNewOrderedAcrossMilliseconds verifies that IDs generated at distinct,
// increasing millisecond timestamps sort lexically in the same order as
// their timestamps, without any real sleeping: nowFunc is swapped out for
// two fixed, known instants.
func TestNewOrderedAcrossMilliseconds(t *testing.T) {
	orig := nowFunc
	t.Cleanup(func() { nowFunc = orig })

	earlier := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	later := earlier.Add(5 * time.Second)

	nowFunc = func() time.Time { return earlier }
	first, err := New("evt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	nowFunc = func() time.Time { return later }
	second, err := New("evt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !(first.String() < second.String()) {
		t.Errorf("expected %q < %q (earlier timestamp must sort first)", first.String(), second.String())
	}
}

// TestNewNotRequiredMonotonicWithinMillisecond documents (and locks in) that
// IDs generated within the same millisecond are NOT required to sort in
// generation order: the spec does not require intra-millisecond
// monotonicity, and this package relies purely on crypto/rand for the tail
// bits. This test only asserts both IDs are valid and share a timestamp; it
// does not assert any particular relative order.
func TestNewNotRequiredMonotonicWithinMillisecond(t *testing.T) {
	orig := nowFunc
	t.Cleanup(func() { nowFunc = orig })

	fixed := time.Date(2024, time.March, 3, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return fixed }

	a, err := New("evt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := New("evt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !a.Time().Equal(fixed) || !b.Time().Equal(fixed) {
		t.Fatalf("expected both IDs to carry timestamp %v, got %v and %v", fixed, a.Time(), b.Time())
	}
}

func TestTimeDecodesKnownVector(t *testing.T) {
	id, err := Parse("prefix_01h455vb4pex5vsknk084sn02q")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := time.Date(2023, time.June, 30, 3, 34, 18, 518_000_000, time.UTC)
	if !id.Time().Equal(want) {
		t.Errorf("Time() = %v, want %v", id.Time(), want)
	}
}

func TestValidatePrefixRules(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		{"empty is ok", "", false},
		{"lowercase letters", "abc", false},
		{"single letter", "a", false},
		{"internal underscore", "pre_fix", false},
		{"63 chars is ok", strings.Repeat("a", 63), false},
		{"64 chars is too long", strings.Repeat("a", 64), true},
		{"leading underscore", "_abc", true},
		{"trailing underscore", "abc_", true},
		{"only underscore", "_", true},
		{"uppercase", "Abc", true},
		{"digit", "abc1", true},
		{"symbol", "ab.c", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.prefix)
			if (err != nil) != c.wantErr {
				t.Errorf("New(%q) error = %v, wantErr %v", c.prefix, err, c.wantErr)
			}
		})
	}
}

func TestMustNewPanicsOnInvalidPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew did not panic on invalid prefix")
		}
	}()
	MustNew("Invalid")
}
