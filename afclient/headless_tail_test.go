package afclient

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestHeadlessFailureTailBoundsArePinned pins the operator tail bounds
// literally: 4096 bytes and 50 lines. Comparing against the constants
// themselves would stay green when the constants drift (M3/M3b), so the
// literals appear here. A single-line 5000-byte tail keeps exactly the
// last 4096 bytes; 200 short lines keep exactly the last 50.
func TestHeadlessFailureTailBoundsArePinned(t *testing.T) {
	t.Parallel()
	if MaxHeadlessFailureTailBytes != 4096 {
		t.Fatalf("MaxHeadlessFailureTailBytes = %d; want 4096", MaxHeadlessFailureTailBytes)
	}
	if MaxHeadlessFailureTailLines != 50 {
		t.Fatalf("MaxHeadlessFailureTailLines = %d; want 50", MaxHeadlessFailureTailLines)
	}
	single := TruncateHeadlessFailureTail(strings.Repeat("x", 5000))
	if len(single) != 4096 {
		t.Fatalf("single-line tail is %d bytes; want exactly 4096", len(single))
	}
	var b strings.Builder
	for range 200 {
		b.WriteString("pad-line\n")
	}
	msg := b.String()
	if len(msg) >= 4096 {
		t.Fatalf("fixture is %d bytes; want under 4096 so only the line bound applies", len(msg))
	}
	trimmed := TruncateHeadlessFailureTail(msg)
	lines := strings.Split(trimmed, "\n")
	if len(lines) != 50 {
		t.Fatalf("line-bound tail has %d lines; want exactly 50", len(lines))
	}
}

// TestTruncateHeadlessFailureTail_MultibyteRuneSafe pins rune-safe
// truncation: a tail whose 4096-byte cut lands mid-rune must start at a
// rune boundary with valid UTF-8. Disabling the leading partial-rune
// trim leaves a continuation byte at the head (invalid UTF-8), so this
// test goes red without the trim.
func TestTruncateHeadlessFailureTail_MultibyteRuneSafe(t *testing.T) {
	t.Parallel()
	// "あ" is 3 bytes in UTF-8. 2000 of them are 6000 bytes, so the
	// 4096-byte suffix slice starts 2 bytes inside a rune; the trim must
	// drop the partial head.
	s := strings.Repeat("あ", 2000)
	tail := TruncateHeadlessFailureTail(s)
	if len(tail) > 4096 {
		t.Fatalf("multibyte tail is %d bytes; want at most 4096", len(tail))
	}
	if !utf8.ValidString(tail) {
		t.Fatalf("multibyte tail is not valid UTF-8; want rune-aligned head")
	}
	if !strings.HasPrefix(tail, "あ") {
		t.Fatalf("multibyte tail head = %q; want a full rune", tail[:4])
	}
	if tail != strings.Repeat("あ", len(tail)/3) {
		t.Fatalf("multibyte tail is not a whole-rune suffix of the input")
	}
}

// TestTruncateHeadlessFailureTail_InvalidUTF8StaysWireBound pins the
// transport boundary: raw subprocess diagnostics can carry invalid UTF-8,
// and encoding/json expands each invalid byte to U+FFFD on decode. The
// helper sanitizes before measuring so the decoded wire tail stays within
// the 4096-byte bound. Without the sanitize-first step the Go string
// measures 4096 bytes but decodes to 12288.
func TestTruncateHeadlessFailureTail_InvalidUTF8StaysWireBound(t *testing.T) {
	t.Parallel()
	raw := strings.Repeat("\xff", 5000)
	tail := TruncateHeadlessFailureTail(raw)
	if !utf8.ValidString(tail) {
		t.Fatalf("malformed tail is not valid UTF-8 after truncation")
	}
	if len(tail) > 4096 {
		t.Fatalf("malformed tail is %d bytes; want at most 4096", len(tail))
	}
	payload, err := json.Marshal(tail)
	if err != nil {
		t.Fatalf("marshal tail: %v", err)
	}
	var decoded string
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal tail: %v", err)
	}
	if len(decoded) > 4096 {
		t.Fatalf("decoded tail is %d bytes; want at most 4096", len(decoded))
	}
	if !utf8.ValidString(decoded) {
		t.Fatalf("decoded tail is not valid UTF-8")
	}
}
