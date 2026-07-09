package engine

import "github.com/majorcontext/harness/typeid"

// legacyHexLen is the length, in characters, of the random suffix in a
// pre-TypeID session ID: "ses_" + 16 lowercase hex digits (8 bytes of
// crypto/rand, hex-encoded). Sessions created before the switch to TypeID
// still have logs on disk in this shape, so it must remain valid forever.
const legacyHexLen = 16

// newID mints a fresh, time-sortable TypeID with the given prefix (e.g.
// "ses", "msg"), backed by a UUIDv7. Panics on a crypto/rand failure, which
// is unrecoverable, mirroring the previous crypto/rand-based implementation.
func newID(prefix string) string {
	id, err := typeid.New(prefix)
	if err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return id.String()
}

// ValidSessionID reports whether id is a valid session identifier under
// exactly one of the two shapes the engine ever produces or persists:
//
//   - legacy: "ses_" followed by exactly 16 lowercase hex digits, minted by
//     the pre-TypeID newID and still present in on-disk session logs.
//   - current: a well-formed "ses" TypeID (typeid.Parse succeeds and the
//     parsed prefix is "ses"), minted by the current newID.
//
// Both shapes are accepted everywhere a session ID is read back — from disk
// or off the wire — so existing session logs keep working. Anything else,
// including path-traversal-shaped input like "../../etc/passwd", is
// rejected; callers that build a filesystem path from a session ID (session
// logs are named "<id>.jsonl") should validate with this first.
func ValidSessionID(id string) bool {
	if isLegacyHexID(id, "ses") {
		return true
	}
	tid, err := typeid.Parse(id)
	return err == nil && tid.Prefix() == "ses"
}

// isLegacyHexID reports whether id is prefix + "_" + exactly legacyHexLen
// lowercase hex digits.
func isLegacyHexID(id, prefix string) bool {
	want := prefix + "_"
	if len(id) != len(want)+legacyHexLen || id[:len(want)] != want {
		return false
	}
	for i := len(want); i < len(id); i++ {
		c := id[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}
