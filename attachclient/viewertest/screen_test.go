package viewertest_test

import (
	"testing"

	"github.com/RenseiAI/donmai/attachclient/viewertest"
	"github.com/RenseiAI/donmai/attachwire"
)

// mkScreen builds a cols x rows attachwire.Screen with all-blank cells, then lets
// the caller overwrite specific cells.
func mkScreen(cols, rows int, alt bool) attachwire.Screen {
	blank := func() []attachwire.Cell {
		g := make([]attachwire.Cell, cols*rows)
		for i := range g {
			g[i] = attachwire.Cell{RuneBytes: []byte(" ")}
		}
		return g
	}
	s := attachwire.Screen{
		Cols: uint64(cols), Rows: uint64(rows), //nolint:gosec // G115: small test grid dimensions
		ActiveBuffer: attachwire.BufferPrimary,
		Primary:      blank(),
	}
	if alt {
		s.ActiveBuffer = attachwire.BufferAlt
		s.AltPresent = true
		s.Alt = blank()
	}
	return s
}

func put(s attachwire.Screen, grid []attachwire.Cell, row, col int, text string) {
	grid[row*int(s.Cols)+col] = attachwire.Cell{RuneBytes: []byte(text)} //nolint:gosec // G115: small test grid width
}

func TestScreenAssertions(t *testing.T) {
	s := mkScreen(20, 5, false)
	put(s, s.Primary, 0, 0, "H")
	put(s, s.Primary, 0, 1, "i")
	put(s, s.Primary, 2, 4, "R")
	s.CursorRow, s.CursorCol, s.CursorVisible = 3, 7, true

	if viewertest.IsAltScreen(s) {
		t.Error("IsAltScreen=true, want false")
	}
	if got := viewertest.CellText(s, 0, 0); got != "H" {
		t.Errorf("CellText(0,0)=%q want H", got)
	}
	if got := viewertest.CellText(s, 2, 4); got != "R" {
		t.Errorf("CellText(2,4)=%q want R", got)
	}
	if got := viewertest.CellText(s, 99, 99); got != "" {
		t.Errorf("out-of-range CellText=%q want empty", got)
	}
	if got := viewertest.RowText(s, 0); got != "Hi" {
		t.Errorf("RowText(0)=%q want %q", got, "Hi")
	}
	if r, c := viewertest.CursorAt(s); r != 3 || c != 7 {
		t.Errorf("CursorAt=(%d,%d) want (3,7)", r, c)
	}
	if !viewertest.CursorVisible(s) {
		t.Error("CursorVisible=false, want true")
	}
}

func TestActiveBufferSelection(t *testing.T) {
	s := mkScreen(10, 3, true)
	put(s, s.Primary, 0, 0, "P")
	put(s, s.Alt, 0, 0, "A")

	if !viewertest.IsAltScreen(s) {
		t.Fatal("IsAltScreen=false, want true")
	}
	if got := viewertest.CellText(s, 0, 0); got != "A" {
		t.Errorf("active CellText(0,0)=%q want A (alt active)", got)
	}
	if got := viewertest.CellTextIn(s, attachwire.BufferPrimary, 0, 0); got != "P" {
		t.Errorf("CellTextIn(primary,0,0)=%q want P", got)
	}
}

func TestWideContinuationReturnsEmpty(t *testing.T) {
	s := mkScreen(10, 1, false)
	put(s, s.Primary, 0, 0, "世")
	s.Primary[1] = attachwire.Cell{Style: attachwire.StyleWideContinuation}
	if got := viewertest.CellText(s, 0, 0); got != "世" {
		t.Errorf("CellText(0,0)=%q want 世", got)
	}
	if got := viewertest.CellText(s, 0, 1); got != "" {
		t.Errorf("continuation CellText(0,1)=%q want empty", got)
	}
	if got := viewertest.RowText(s, 0); got != "世" {
		t.Errorf("RowText(0)=%q want 世", got)
	}
}

func TestDecodeSnapshotFrameRoundTrip(t *testing.T) {
	src := mkScreen(8, 2, false)
	put(src, src.Primary, 0, 0, "X")
	src.CursorRow, src.CursorCol = 1, 3
	snap, err := src.Encode()
	if err != nil {
		t.Fatalf("encode screen: %v", err)
	}
	env := attachwire.SnapshotEnvelope{AtSeq: 9, SnapFormat: attachwire.SnapFormatScreen, Snap: snap}
	frame := attachwire.Frame{Type: attachwire.TypeSnapshot, Payload: env.Encode()}

	got, err := viewertest.DecodeSnapshotFrame(frame)
	if err != nil {
		t.Fatalf("DecodeSnapshotFrame: %v", err)
	}
	if viewertest.CellText(got, 0, 0) != "X" {
		t.Errorf("decoded CellText(0,0)=%q want X", viewertest.CellText(got, 0, 0))
	}
	if r, c := viewertest.CursorAt(got); r != 1 || c != 3 {
		t.Errorf("decoded CursorAt=(%d,%d) want (1,3)", r, c)
	}

	// Non-Snapshot frame is a clear error, not a zero-value screen.
	if _, err := viewertest.DecodeSnapshotFrame(attachwire.Frame{Type: attachwire.TypeOutput}); err == nil {
		t.Error("DecodeSnapshotFrame(Output) = nil error, want error")
	}
}

func TestVTFeedReconstructsScreen(t *testing.T) {
	b := viewertest.NewScreenBuilder(40, 6, 1)
	// Move to (row2,col4) 1-indexed=3;5, print READY; then enter alt and print.
	b.Feed([]byte("\x1b[2J\x1b[3;5HREADY"))
	s := b.Screen()
	if viewertest.IsAltScreen(s) {
		t.Error("expected primary after non-alt feed")
	}
	if got := viewertest.CellText(s, 2, 4); got != "R" {
		t.Errorf("vtfeed CellText(2,4)=%q want R\n%s", got, viewertest.Dump(s))
	}
	if got := viewertest.RowText(s, 2); got != "    READY" {
		t.Errorf("vtfeed RowText(2)=%q want %q", got, "    READY")
	}

	b.Feed([]byte("\x1b[?1049h\x1b[2J\x1b[1;1HALT"))
	alt := b.Screen()
	if !viewertest.IsAltScreen(alt) {
		t.Errorf("expected alt after ?1049h\n%s", viewertest.Dump(alt))
	}
	if got := viewertest.RowText(alt, 0); got != "ALT" {
		t.Errorf("vtfeed alt RowText(0)=%q want ALT", got)
	}
}
