// Package typeid implements the TypeID specification
// (https://github.com/jetify-com/typeid, spec v0.3): a type-safe, K-sortable,
// globally unique identifier built on top of a UUIDv7.
//
// A TypeID's canonical string form is an optional lowercase prefix, an
// underscore separator (omitted when the prefix is empty), and a 26-character
// suffix that encodes a 128-bit UUID as lowercase Crockford base32.
//
// This package has zero non-stdlib dependencies; both the string grammar
// and the base32 suffix codec are implemented by hand from the spec, not by
// pulling in a YAML/UUID/base32 library.
package typeid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// TypeID is a parsed, validated TypeID: a prefix plus the 16 raw bytes of
// the UUID it encodes. The zero value is not a valid TypeID (its string
// form, "00000000000000000000000000", is well-formed but callers should
// construct instances via New or Parse).
type TypeID struct {
	prefix string
	id     [16]byte
}

// maxPrefixLen is the maximum number of characters allowed in a prefix, per
// spec.
const maxPrefixLen = 63

// suffixLen is the fixed length, in characters, of the base32-encoded UUID
// suffix.
const suffixLen = 26

// nowFunc returns the current time and is used to generate the UUIDv7
// timestamp. It is a package variable so tests can inject deterministic
// clocks without sleeping; production code must never override it.
var nowFunc = time.Now

// New generates a fresh TypeID with the given prefix, using a freshly
// generated UUIDv7 (48-bit Unix millisecond timestamp, version and variant
// bits per RFC 9562, and a cryptographically random tail from crypto/rand).
//
// New does not guarantee monotonic ordering of IDs generated within the same
// millisecond: the spec does not require this, and the random tail bits mean
// two IDs sharing a millisecond may sort in either order. IDs generated in
// different milliseconds always sort in timestamp order.
func New(prefix string) (TypeID, error) {
	if err := validatePrefix(prefix); err != nil {
		return TypeID{}, err
	}
	id, err := generateV7(nowFunc())
	if err != nil {
		return TypeID{}, fmt.Errorf("typeid: generating uuidv7: %w", err)
	}
	return TypeID{prefix: prefix, id: id}, nil
}

// MustNew is like New but panics on error. Intended for tests and other
// contexts where a bad prefix is a programmer error.
func MustNew(prefix string) TypeID {
	id, err := New(prefix)
	if err != nil {
		panic(err)
	}
	return id
}

// Parse parses a canonical TypeID string, strictly validating both the
// prefix and the suffix against the spec grammar. Errors name the exact
// violation (bad character, wrong length, out-of-range value, etc.).
func Parse(s string) (TypeID, error) {
	if s == "" {
		return TypeID{}, errors.New("typeid: empty string is not a valid typeid")
	}

	prefix, suffix, hasSeparator := cutLastUnderscore(s)
	if hasSeparator && prefix == "" {
		return TypeID{}, errors.New("typeid: separator '_' present but prefix is empty")
	}

	if err := validatePrefix(prefix); err != nil {
		return TypeID{}, err
	}

	if len(suffix) != suffixLen {
		return TypeID{}, fmt.Errorf("typeid: suffix must be exactly %d characters, got %d", suffixLen, len(suffix))
	}

	id, err := decodeSuffix(suffix)
	if err != nil {
		return TypeID{}, err
	}

	return TypeID{prefix: prefix, id: id}, nil
}

// cutLastUnderscore splits s on its last '_' into a prefix and suffix,
// mirroring the grammar prefix_suffix (the suffix itself never contains an
// underscore, so the last one is always the separator). hasSeparator
// reports whether an underscore was found at all.
func cutLastUnderscore(s string) (prefix, suffix string, hasSeparator bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '_' {
			return s[:i], s[i+1:], true
		}
	}
	return "", s, false
}

// validatePrefix checks a prefix against the spec grammar: 0-63 characters,
// each an ASCII lowercase letter or underscore, and (when non-empty) not
// starting or ending with an underscore.
func validatePrefix(prefix string) error {
	if len(prefix) > maxPrefixLen {
		return fmt.Errorf("typeid: prefix must be at most %d characters, got %d", maxPrefixLen, len(prefix))
	}
	if prefix == "" {
		return nil
	}
	if prefix[0] == '_' {
		return errors.New("typeid: prefix must not start with an underscore")
	}
	if prefix[len(prefix)-1] == '_' {
		return errors.New("typeid: prefix must not end with an underscore")
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if c == '_' || (c >= 'a' && c <= 'z') {
			continue
		}
		return fmt.Errorf("typeid: prefix must contain only lowercase ascii letters and underscores, found %q at position %d", c, i)
	}
	return nil
}

// String returns the canonical string form of the TypeID: prefix + "_" +
// suffix, or just the suffix when the prefix is empty.
func (t TypeID) String() string {
	suffix := encodeSuffix(t.id)
	if t.prefix == "" {
		return suffix
	}
	return t.prefix + "_" + suffix
}

// Prefix returns the TypeID's prefix (the empty string if none).
func (t TypeID) Prefix() string {
	return t.prefix
}

// UUID returns the 16 raw bytes of the UUID encoded by the TypeID's suffix.
func (t TypeID) UUID() [16]byte {
	return t.id
}

// Time returns the timestamp encoded in the TypeID's UUIDv7 suffix, decoded
// from its 48-bit big-endian Unix millisecond timestamp field. It is valid
// to call on any TypeID whose UUID happens to carry a v7-shaped timestamp in
// its first 6 bytes, regardless of the version bits actually stored there.
func (t TypeID) Time() time.Time {
	ms := uint64(t.id[0])<<40 | uint64(t.id[1])<<32 | uint64(t.id[2])<<24 |
		uint64(t.id[3])<<16 | uint64(t.id[4])<<8 | uint64(t.id[5])
	return time.UnixMilli(int64(ms)).UTC()
}

// generateV7 builds a UUIDv7 (RFC 9562) for the given timestamp: a 48-bit
// big-endian Unix millisecond count, a 4-bit version field set to 7, a
// 12-bit random "rand_a", a 2-bit variant field set to 0b10, and a 62-bit
// random "rand_b". rand_a and rand_b are drawn from crypto/rand.
func generateV7(t time.Time) ([16]byte, error) {
	var id [16]byte

	ms := uint64(t.UnixMilli())
	id[0] = byte(ms >> 40)
	id[1] = byte(ms >> 32)
	id[2] = byte(ms >> 24)
	id[3] = byte(ms >> 16)
	id[4] = byte(ms >> 8)
	id[5] = byte(ms)

	if _, err := rand.Read(id[6:16]); err != nil {
		return id, err
	}

	// Version: top 4 bits of byte 6 = 0111.
	id[6] = (id[6] & 0x0F) | 0x70
	// Variant: top 2 bits of byte 8 = 10.
	id[8] = (id[8] & 0x3F) | 0x80

	return id, nil
}
