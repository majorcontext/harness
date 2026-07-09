package typeid

import "fmt"

// alphabet is the TypeID suffix's Crockford base32 alphabet: lowercase,
// with 'i', 'l', 'o' and 'u' removed to avoid visual ambiguity.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// decodeTable maps an alphabet byte to its 5-bit value, or -1 if the byte is
// not part of the alphabet.
var decodeTable = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return t
}()

// encodeSuffix encodes a 128-bit UUID as the TypeID spec's 26-character
// lowercase Crockford base32 suffix.
//
// 128 bits do not divide evenly into 5-bit groups, so the suffix is treated
// as a 130-bit stream whose first 2 bits are an implicit zero pad and whose
// remaining 128 bits are the UUID, most-significant bit first. Each of the
// 26 output characters encodes 5 consecutive bits of that stream.
func encodeSuffix(id [16]byte) string {
	out := make([]byte, suffixLen)
	for i := 0; i < suffixLen; i++ {
		var v byte
		for b := 0; b < 5; b++ {
			v = v<<1 | bitAt(id, i*5+b)
		}
		out[i] = alphabet[v]
	}
	return string(out)
}

// decodeSuffix decodes a 26-character TypeID suffix back into its 128-bit
// UUID, the inverse of encodeSuffix. It rejects characters outside the
// alphabet and suffixes that would encode more than 128 bits (i.e. whose
// first character is not in the range 0-7, per spec).
func decodeSuffix(s string) ([16]byte, error) {
	var id [16]byte

	values := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		v := decodeTable[s[i]]
		if v < 0 {
			return id, fmt.Errorf("typeid: invalid character %q in suffix at position %d", s[i], i)
		}
		values[i] = byte(v)
	}

	// The first character contributes the 2 implicit pad bits plus the top 3
	// data bits; a value above 7 (0b00111) would set a pad bit, overflowing
	// past 128 bits.
	if values[0] > 7 {
		return id, fmt.Errorf("typeid: suffix encodes more than 128 bits (first character must be 0-7, got %q)", s[0])
	}

	for i := 0; i < len(values); i++ {
		for b := 0; b < 5; b++ {
			vpos := i*5 + b
			if vpos < 2 {
				// Pad bit, already confirmed zero above.
				continue
			}
			bit := (values[i] >> (4 - b)) & 1
			if bit == 1 {
				setBitAt(&id, vpos)
			}
		}
	}

	return id, nil
}

// bitAt returns the value (0 or 1) of virtual bit vpos of the 130-bit stream
// formed by 2 implicit zero pad bits followed by id's 128 bits,
// most-significant bit first.
func bitAt(id [16]byte, vpos int) byte {
	if vpos < 2 {
		return 0
	}
	actual := vpos - 2
	byteIndex := actual / 8
	bitFromMSB := actual % 8
	return (id[byteIndex] >> (7 - bitFromMSB)) & 1
}

// setBitAt sets virtual bit vpos (vpos >= 2) of id, using the same 130-bit
// virtual-stream indexing as bitAt.
func setBitAt(id *[16]byte, vpos int) {
	actual := vpos - 2
	byteIndex := actual / 8
	bitFromMSB := actual % 8
	id[byteIndex] |= 1 << (7 - bitFromMSB)
}
