package typeid

import "testing"

// TestConsoleGoldenMessageIDParses pins a cross-language compatibility
// fixture for meetneptune/boxes' web console, which mints a "msg" TypeID
// by hand in TypeScript (mintClientMessageID, web/src/lib/agent-events.ts)
// rather than depending on this package. The golden string was generated
// FROM a fixed 16-byte UUIDv7-shaped input via this package's own
// encodeSuffix, so it proves harness's decoder accepts what harness's own
// encoder produces for that input; the console's own test
// (web/src/lib/agent-events.test.ts) encodes the IDENTICAL input bytes
// independently and asserts it produces this exact same string — the pair
// is the actual cross-language round trip, not just an isolated
// self-check on either side.
func TestConsoleGoldenMessageIDParses(t *testing.T) {
	const golden = "msg_01hf7yat00e41861050r3gg28a"
	want := [16]byte{1, 139, 207, 229, 104, 0, 113, 2, 131, 4, 5, 6, 7, 8, 9, 10}

	parsed, err := Parse(golden)
	if err != nil {
		t.Fatalf("Parse(%q): %v", golden, err)
	}
	if parsed.Prefix() != "msg" {
		t.Errorf("Prefix() = %q, want %q", parsed.Prefix(), "msg")
	}
	if parsed.UUID() != want {
		t.Errorf("UUID() = %v, want %v", parsed.UUID(), want)
	}
	if parsed.String() != golden {
		t.Errorf("String() = %q, want %q", parsed.String(), golden)
	}
}
