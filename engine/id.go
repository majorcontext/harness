package engine

import (
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/typeid"
)

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

// usableClientMessageID reports whether id is safe to use verbatim as a
// user message's ID: non-empty, and not one of the reserved provenance
// prefixes engine itself mints for a DIFFERENT kind of synthetic message —
// compactionSummaryIDTag ("cmpsum", compact.go) for a compaction summary, or
// message.SyntheticOrphanIDPrefix ("synthetic-orphan-tool-result-") for a
// synthesized orphaned tool result. Both prefixes are load-bearing markers
// elsewhere in this package (isCompactionSummaryID,
// message.IsSyntheticOrphanID) that assume only engine itself ever mints an
// ID with that shape; a client-supplied ID colliding with one would make a
// genuine user message indistinguishable from that synthetic kind.
//
// This is cheap insurance against an accidental collision, not attacker
// defense: PromptWithOrigin's caller (server.Server, this repo's only HTTP
// surface for it) is reached exclusively by a trusted, authenticated
// first-party client, so an id that fails this check is silently ignored
// in favor of a fresh server-minted one — see ResolveMessageID — never a
// rejected request.
func usableClientMessageID(id string) bool {
	if id == "" {
		return false
	}
	if strings.HasPrefix(id, compactionSummaryIDTag) {
		return false
	}
	if strings.HasPrefix(id, message.SyntheticOrphanIDPrefix) {
		return false
	}
	return true
}

// ResolveMessageID returns id unchanged when usableClientMessageID accepts
// it, or mints a fresh "msg" TypeID otherwise. This is the exact rule
// PromptWithOrigin applies at each of its two mint sites (the claude-code-
// delegated and native branches) to the id a caller supplies for the
// appended user message; it is exported so a caller that must know a
// prompt's resolved message ID before the turn actually runs — a queued
// prompt's synchronous accept response, notably, dispatched into
// PromptWithOrigin only later, asynchronously, once its turn is drained —
// can compute the SAME value PromptWithOrigin will use, once, without
// duplicating the reserved-prefix rule or risking a second, different mint
// for the same logical prompt.
func ResolveMessageID(id string) string {
	if usableClientMessageID(id) {
		return id
	}
	return newID("msg")
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
