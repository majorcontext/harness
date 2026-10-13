package acp

import (
	"crypto/rand"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a ULID: 48 bits of millisecond time and 80 random bits in 26
// Crockford base32 characters.
func newULID() string {
	var b [26]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 9; i >= 0; i-- {
		b[i] = crockford[ms&31]
		ms >>= 5
	}
	var r [10]byte
	_, _ = rand.Read(r[:])
	var acc uint64
	var bits uint
	n := 10
	for _, v := range r {
		acc = acc<<8 | uint64(v)
		bits += 8
		for bits >= 5 {
			bits -= 5
			b[n] = crockford[(acc>>bits)&31]
			n++
		}
	}
	return string(b[:])
}
