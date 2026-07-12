package attachwire

import "encoding/binary"

// MaxVarintLen is the maximum width of a protocol varint, in bytes — a full
// uint64 (§2.1). Identical to encoding/binary.MaxVarintLen64.
const MaxVarintLen = binary.MaxVarintLen64

// AppendUvarint appends the unsigned LEB128 (§2.1) encoding of x to dst and
// returns the extended slice. Byte-for-byte identical to
// encoding/binary.AppendUvarint (no ZigZag; little-endian base-128; the 0x80
// continuation bit set on every byte but the last).
func AppendUvarint(dst []byte, x uint64) []byte {
	return binary.AppendUvarint(dst, x)
}

// PutUvarint writes the unsigned LEB128 (§2.1) encoding of x into buf and
// returns the number of bytes written. buf must be at least MaxVarintLen long
// to hold every value. Byte-for-byte identical to
// encoding/binary.PutUvarint.
func PutUvarint(buf []byte, x uint64) int {
	return binary.PutUvarint(buf, x)
}

// Uvarint decodes an unsigned LEB128 (§2.1) varint from the front of buf. It
// returns the decoded value and the number of bytes consumed. Decoding is
// byte-for-byte identical to encoding/binary.Uvarint, with the two
// error dispositions the protocol requires broken out:
//
//   - a value that does not terminate within MaxVarintLen bytes, or whose final
//     byte carries an illegal high bit, is an overflow → ErrVarintOverflow;
//   - a buffer that ends while a continuation bit is still set (including an
//     empty buffer where a varint was expected) is a truncation →
//     ErrVarintTruncated.
//
// Both are FramingErrors: the receiver MUST close the connection with
// error.code = framing (§2.1).
func Uvarint(buf []byte) (value uint64, n int, err error) {
	v, m := binary.Uvarint(buf)
	switch {
	case m > 0:
		return v, m, nil
	case m < 0:
		return 0, 0, ErrVarintOverflow
	default: // m == 0: buffer too small / ended mid-varint
		return 0, 0, ErrVarintTruncated
	}
}
