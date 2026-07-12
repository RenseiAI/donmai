package sanitize

import (
	"bytes"
	"testing"
)

// FuzzSanitizer asserts the three security-critical invariants over arbitrary
// input (§9):
//
//  1. the sanitizer never panics on any byte stream;
//  2. the output contains no forbidden sequence — re-scanning the output with a
//     fresh sanitizer is the identity (idempotence);
//  3. chunked delivery equals contiguous delivery for random splits (the
//     split-sequence bypass is closed).
func FuzzSanitizer(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),
		[]byte("hello world\n"),
		[]byte("\x1b[1;31mred\x1b[0m"),
		[]byte("\x1b]52;c;QUJD\x07"),
		[]byte("\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\"),
		[]byte("\x1b]0;title\x07"),
		[]byte("\x1b[6n\x1b[c\x1b[>c"),
		[]byte("\x1b\x50q#1~~\x1b\\"),
		[]byte("\x1b\x50$qm\x1b\\"),
		[]byte("\x1b_kitty\x1b\\"),
		[]byte("\x9b1m\x9d52;c;QQ==\x9c"),
		[]byte("café 日本語 👍🏽 x\xd9\x9by"),
		[]byte("\x1b]0;" + string(bytes.Repeat([]byte("A"), 100))),
		[]byte("\x1b\x1b\x1b[[[???ttt"),
		{0x00, 0x1b, 0x9b, 0x9d, 0x90, 0x9f, 0x9e, 0x98, 0x9c, 0x07, 0x7f},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// (1) never panics — implicit; and produce the contiguous result.
		out := New().Write(data)

		// (2) idempotence: re-sanitizing the output changes nothing. This is the
		// spec-meaningful "output contains no forbidden sequence" check — a
		// surviving strip-disposition sequence would be removed on the second
		// pass and break equality. (Bytes such as NUL/DEL/BEL may legitimately
		// appear INSIDE a passed Sixel or OSC body; idempotence, not a raw byte
		// scan, is the correct invariant.)
		again := New().Write(out)
		if !bytes.Equal(again, out) {
			t.Fatalf("not idempotent:\n in=%q\nout=%q\n re=%q", data, out, again)
		}

		// (3) chunked == contiguous for a few random splits.
		rng := newXRNG(uint64(len(data)) ^ 0x5DEECE66D)
		for trial := 0; trial < 4; trial++ {
			s := New()
			var chunked []byte
			for pos := 0; pos < len(data); {
				n := 1 + rng.intn(5)
				if pos+n > len(data) {
					n = len(data) - pos
				}
				chunked = append(chunked, s.Write(data[pos:pos+n])...)
				pos += n
			}
			if !bytes.Equal(chunked, out) {
				t.Fatalf("chunked != contiguous:\n in=%q\ncontig=%q\nchunk=%q", data, out, chunked)
			}
		}
	})
}
