package sanitize

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
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
		[]byte("\x1b]0;\xe2\x9c\xb3 name\x07\x1b]8;;x\xc2\x9c\x9b6n"),
		[]byte("\xc2\x9b6n\xc2\x9d11;?\xc2\x9c\x1b]10;\x9c?\x9d8;;u\x9cl\x90q~\x9c"),
		[]byte("\x1b]52;c;ICBGYWJsZSA1LjEgwrcgQ2xhdWRlIE1heA==\x07\x1b]52;c;?\x07\x9d52;p;Gx0=\x9c"),
		[]byte("\x1b]0;" + string(bytes.Repeat([]byte("A"), 100))),
		[]byte("\x1b\x1b\x1b[[[???ttt"),
		{0x00, 0x1b, 0x9b, 0x9d, 0x90, 0x9f, 0x9e, 0x98, 0x9c, 0x07, 0x7f},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// (1) never panics — implicit; and produce the contiguous result.
		// The clipboard hook must never change the stream, and what it offers is
		// valid UTF-8 with no escape or other control besides HT/LF/CR.
		out := NewWithOptions(Options{OnClipboard: func(text string) {
			if !utf8.ValidString(text) || strings.ContainsFunc(text, func(r rune) bool {
				return (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || (r >= 0x7F && r <= 0x9F)
			}) {
				t.Fatalf("clipboard hook offered %q for %q", text, data)
			}
		}}).Write(data)
		if plain := New().Write(data); !bytes.Equal(plain, out) {
			t.Fatalf("clipboard hook changed the stream: %q vs %q", out, plain)
		}

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

		// (4) no C1 control a UTF-8 viewer would act on: neither a C1 encoded
		// as UTF-8 (C2 80..C2 9F) nor a raw 0x9C outside a multibyte sequence.
		if i := c1ForUTF8Viewer(out); i >= 0 {
			t.Fatalf("output carries a C1 control at %d:\n in=%q\nout=%q", i, data, out)
		}
	})
}

// c1ForUTF8Viewer returns the offset of the first C1 control in b that a UTF-8
// terminal would decode and act on, or -1. It follows multibyte sequences the
// way a decoder does: a lead byte opens a sequence and continuation bytes
// extend it, and any other byte ends it.
func c1ForUTF8Viewer(b []byte) int {
	rem := 0
	var lead byte
	for i, c := range b {
		if rem > 0 && c >= 0x80 && c <= 0xBF {
			if lead == 0xC2 && c <= 0x9F {
				return i - 1
			}
			rem--
			continue
		}
		rem = 0
		if c == c1ST {
			return i
		}
		if n := utf8Trail(c); n > 0 {
			rem, lead = n, c
		}
	}
	return -1
}
