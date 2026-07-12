package ptyhost

import (
	"bytes"
	"io"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

// TestSnapshotConformanceFixture verifies the §12.1 snapFormat-0x01 byte layout
// (the 26-byte authoritative fixture) through the attachwire.Screen shape this
// package emits — a guard that the constants and field order ptyhost relies on
// match the frozen fixture.
func TestSnapshotConformanceFixture(t *testing.T) {
	scr := attachwire.Screen{
		Epoch:          1,
		EchoMode:       attachwire.EchoOn,
		Cols:           2,
		Rows:           1,
		ActiveBuffer:   attachwire.BufferPrimary,
		CursorRow:      0,
		CursorCol:      1,
		CursorVisible:  true,
		CursorShape:    attachwire.CursorShapeBlock,
		SavedCursorRow: 0,
		SavedCursorCol: 1,
		Primary: []attachwire.Cell{
			{RuneBytes: []byte("A"), Style: attachwire.StyleBold, FG: attachwire.IndexedColor(1), BG: attachwire.DefaultColor},
			{RuneBytes: []byte(" "), FG: attachwire.DefaultColor, BG: attachwire.DefaultColor},
		},
	}
	got, err := scr.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []byte{
		0x01, 0x01, 0x02, 0x01, 0x00, 0x00, 0x01, 0x01, 0x01,
		0x00, 0x00,
		0x00, 0x01,
		0x01, 0x41, 0x01, 0x01, 0x01, 0x00,
		0x01, 0x20, 0x00, 0x00, 0x00,
		0x00,
		0x00,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("snap bytes (%d) =\n % x\nwant (%d)\n % x", len(got), got, len(want), want)
	}
}

// TestSnapshotWideGlyph: a width-2 CJK glyph serializes as a base cell carrying
// the grapheme plus a wide-continuation cell (runeLen 0), matching §12.1.
func TestSnapshotWideGlyph(t *testing.T) {
	v := newVTHost(10, 1, DefaultScrollback, io.Discard, nil)
	v.write([]byte("世界")) // two width-2 CJK glyphs → columns 0..3
	scr := serializeScreen(t, v)

	if len(scr.Primary) != 10 {
		t.Fatalf("primary grid has %d cells, want 10", len(scr.Primary))
	}
	base := scr.Primary[0]
	cont := scr.Primary[1]
	if string(base.RuneBytes) != "世" {
		t.Errorf("cell 0 rune = %q, want 世", base.RuneBytes)
	}
	if cont.Style&attachwire.StyleWideContinuation == 0 || len(cont.RuneBytes) != 0 {
		t.Errorf("cell 1 = %+v, want a wide-continuation cell (flag set, runeLen 0)", cont)
	}
	if string(scr.Primary[2].RuneBytes) != "界" {
		t.Errorf("cell 2 rune = %q, want 界", scr.Primary[2].RuneBytes)
	}
}

// TestSnapshotCursorAndModes: cursor placement and a private mode both surface in
// the serialized screen.
func TestSnapshotCursorAndModes(t *testing.T) {
	v := newVTHost(80, 24, DefaultScrollback, io.Discard, nil)
	// Enable bracketed paste + app cursor keys, then place the cursor at (row 3,
	// col 5) 1-based.
	v.write([]byte("\x1b[?2004h\x1b[?1h\x1b[3;5H"))
	scr := serializeScreen(t, v)

	if scr.CursorRow != 2 || scr.CursorCol != 4 {
		t.Errorf("cursor = (row %d, col %d), want (2,4)", scr.CursorRow, scr.CursorCol)
	}
	if scr.Modes&attachwire.ModeBracketedPaste == 0 {
		t.Error("bracketed paste mode bit not set")
	}
	if scr.Modes&attachwire.ModeAppCursorKeys == 0 {
		t.Error("app cursor keys mode bit not set")
	}
}

// TestSafeRunes: the escape-safe extraction replaces control/ESC runes with
// U+FFFD, preserves printable text and multi-rune graphemes (ZWJ), and never
// returns empty.
func TestSafeRunes(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		want         string
		wantReplaced bool
	}{
		{"plain", "abc", "abc", false},
		{"esc", "a\x1bb", "a�b", true},
		{"bel", "\x07", "�", true},
		{"del", "x\x7f", "x�", true},
		{"c1", "y\u0085", "y\uFFFD", true},
		{"zwj-family", "👨‍👩", "👨‍👩", false},
		{"empty-becomes-space", "", " ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, replaced := safeRunes(tc.in)
			if string(got) != tc.want {
				t.Errorf("safeRunes(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if replaced != tc.wantReplaced {
				t.Errorf("safeRunes(%q) replaced = %v, want %v", tc.in, replaced, tc.wantReplaced)
			}
		})
	}
}

// TestSnapshotAlwaysEncodable feeds hostile bytes and asserts the serialized
// screen always encodes (escape-safe by construction, §12.1) — the snapshot must
// never fail on adversarial output.
func TestSnapshotAlwaysEncodable(t *testing.T) {
	v := newVTHost(40, 10, DefaultScrollback, io.Discard, nil)
	// A mix of SGR, control bytes, OSC, and raw high bytes.
	hostile := []byte("\x1b[31mred\x00\x07\x1b]0;title\x07plain\xff\xfe\x1b[1;1H")
	v.write(hostile)
	scr := buildScreen(v.raw(), 7, attachwire.EchoOn, nil)
	if _, err := scr.Encode(); err != nil {
		t.Fatalf("snapshot must be escape-safe by construction, got: %v", err)
	}
	if scr.Epoch != 7 {
		t.Errorf("epoch stamped = %d, want 7", scr.Epoch)
	}
}
