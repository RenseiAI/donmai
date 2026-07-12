package attachwire

// Low-level decode helpers shared by the frame, payload, and snapshot codecs.
// A reader is a cursor over an immutable input buffer; every read is
// bounds-checked and any shortfall surfaces as a FramingError (§2.1/§3.1).

// decodePreallocCap bounds the initial capacity a decoder reserves for a
// length-driven slice, so a hostile length field cannot force a huge up-front
// allocation. The decode loop still reads exactly the declared count and errors
// out via truncation once the buffer is exhausted, so correctness is unchanged.
const decodePreallocCap = 4096

type reader struct {
	buf []byte
	off int
}

func newReader(buf []byte) *reader { return &reader{buf: buf} }

// remaining returns the number of unread bytes.
func (r *reader) remaining() int { return len(r.buf) - r.off }

// expectDone returns a FramingError if any bytes remain unread. The v1-frozen
// structured payloads (§3.1) have a fixed field list, so trailing bytes signal
// corruption rather than a forward-compatible extension.
func (r *reader) expectDone() error {
	if r.off != len(r.buf) {
		return newFramingf("unexpected trailing bytes in payload (%d left)", r.remaining())
	}
	return nil
}

// readByte reads a single u8.
func (r *reader) readByte() (byte, error) {
	if r.off >= len(r.buf) {
		return 0, newFraming("unexpected end of input reading u8")
	}
	b := r.buf[r.off]
	r.off++
	return b, nil
}

// uvarint reads an unsigned LEB128 varint (§2.1), advancing the cursor.
func (r *reader) uvarint() (uint64, error) {
	v, n, err := Uvarint(r.buf[r.off:])
	if err != nil {
		return 0, err
	}
	r.off += n
	return v, nil
}

// bytes reads exactly n bytes and returns an independent copy (nil when n == 0),
// so the returned slice never aliases the input buffer. A declared length past
// the end of the buffer is a truncation FramingError.
func (r *reader) bytes(n uint64) ([]byte, error) {
	if n > uint64(r.remaining()) { //nolint:gosec // G115: remaining() = len(buf)-off is always >= 0; a single decoded frame is far under MaxInt64
		return nil, newFramingf("declared length %d exceeds %d remaining bytes", n, r.remaining())
	}
	// n <= remaining() (an int) was just proven above, so n fits in int.
	out := cloneBytes(r.buf[r.off : r.off+int(n)]) //nolint:gosec // G115: n bounded by the remaining()-check above
	r.off += int(n)                                //nolint:gosec // G115: n bounded by the remaining()-check above
	return out, nil
}

// lenPrefixed reads a varint length followed by that many bytes (§3.1's
// pervasive length-prefixed idiom), returning an independent copy.
func (r *reader) lenPrefixed() ([]byte, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	return r.bytes(n)
}

// remainingCopy consumes and returns an independent copy of the rest of the
// buffer (nil when empty).
func (r *reader) remainingCopy() []byte {
	out := cloneBytes(r.buf[r.off:])
	r.off = len(r.buf)
	return out
}

// cloneBytes returns an independent copy of b, or nil when b is empty. Empty
// slices are normalized to nil so encode→decode round-trips compare equal.
func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// boundedCap caps a length-driven initial allocation (see decodePreallocCap).
func boundedCap(n uint64) int {
	if n > decodePreallocCap {
		return decodePreallocCap
	}
	return int(n)
}
