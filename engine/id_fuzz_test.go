package engine

import (
	"testing"

	"github.com/majorcontext/harness/typeid"
)

// isDocumentedLegacySessionID re-derives ValidSessionID's "legacy" format
// (id.go's doc comment: "'ses_' followed by exactly 16 lowercase hex
// digits") directly from that prose, independently of isLegacyHexID — the
// implementation this fuzz target is checking — rather than calling it.
func isDocumentedLegacySessionID(id string) bool {
	const prefix = "ses_"
	const hexLen = 16
	if len(id) != len(prefix)+hexLen || id[:len(prefix)] != prefix {
		return false
	}
	for _, c := range id[len(prefix):] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// FuzzValidSessionID checks ValidSessionID's documented two-format contract
// (id.go: legacy "ses_" + 16 lowercase hex digits, or a well-formed "ses"
// TypeID) against arbitrary strings: no panic, and any string it accepts
// must independently conform to one of those two shapes. The "current"
// shape's independent check is "a well-formed ses TypeID" restated directly
// from the doc comment (typeid.Parse succeeding with prefix "ses") rather
// than trusting ValidSessionID's own internal branch that does the same.
func FuzzValidSessionID(f *testing.F) {
	f.Add("ses_0123456789abcdef")
	f.Add("ses_0123456789ABCDEF")
	f.Add("")
	f.Add("ses_")
	f.Add("../../etc/passwd")
	f.Add("ses_../../etc/passwd")
	f.Add(newID("ses"))
	f.Add(newID("msg"))
	f.Add("ses_0123456789abcde")
	f.Add("ses_0123456789abcdef0")
	f.Add("ses_gggggggggggggggg")
	f.Add("not-an-id")

	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<16 {
			t.Skip()
		}

		if !ValidSessionID(s) {
			return
		}

		if isDocumentedLegacySessionID(s) {
			return
		}

		tid, err := typeid.Parse(s)
		if err != nil || tid.Prefix() != "ses" {
			t.Fatalf("ValidSessionID(%q) = true, but it matches neither documented format (legacy \"ses_\"+16 lowercase hex, or a well-formed \"ses\" TypeID); typeid.Parse error: %v", s, err)
		}
	})
}
