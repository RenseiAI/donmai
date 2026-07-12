package sanitize

import (
	"strings"
	"unicode/utf8"
)

// UIString implements the frozen §9 "length-capped, control-char-stripped"
// treatment for every protocol string a viewer renders as UI text — Marker
// labels, error.message, presence display names, and the neutralized OSC 0/1/2
// title chip. A protocol string is never rendered raw into viewer UI chrome.
//
// The treatment is exactly, and only (so the relay, web, and iOS producers agree
// byte-for-byte):
//
//   - invalid UTF-8 is dropped (valid-UTF-8 enforced);
//   - every C0 control (0x00-0x1F), DEL (0x7F), and C1 control (0x80-0x9F) rune
//     is stripped;
//   - the result is truncated to at most maxLen runes, on a rune boundary.
//
// maxLen is a maximum rune count; a maxLen <= 0 yields the empty string.
// Truncation is rune-boundary-safe by construction because the builder appends
// whole runes.
func UIString(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // invalid UTF-8 byte — drop and advance one byte
			continue
		}
		i += size
		if isStripRune(r) {
			continue
		}
		b.WriteRune(r)
		n++
		if n >= maxLen {
			break
		}
	}
	return b.String()
}

// isStripRune reports whether r is a C0, DEL, or C1 control that UIString strips.
func isStripRune(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}
