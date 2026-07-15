// Package viewertest is an OSS, reusable viewer-side test harness for the D3
// attach wire protocol: a headless screen-assert toolkit that decodes a received
// Snapshot into an attachwire.Screen and exposes ergonomic, deterministic
// assertions (cell text, cursor position, alt-screen state) plus a Driver that
// sends input, requests snapshots, and returns decoded screens with timeouts.
//
// It is transport-agnostic: the assertion helpers work on any attachwire.Screen
// (decoded from a Snapshot frame), and the Driver drives an attachtest.Viewer
// over the abstract relay wire ("attach to a relay URL with a bearer"). There
// are no platform, brand, or service references here — this is standalone core.
//
// Two paths reconstruct a viewer screen:
//
//   - Snapshot-decode (authoritative): DecodeSnapshotFrame turns a Snapshot
//     frame into a Screen. This is what a snapshot_request answers.
//   - VT-feed (convenience): NewScreenBuilder feeds relay Output byte-frames
//     into a fresh terminal emulator seeded to the fixture geometry so a caller
//     can assert between snapshots without forcing one. The snapshot-decode path
//     is authoritative; the VT-feed path is a best-effort mirror.
package viewertest

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/attachwire"
)

// DecodeSnapshotFrame decodes a received Snapshot frame (§3.1 envelope + §12.1
// snapFormat 0x01 screen) into an attachwire.Screen. It returns a clear error
// when the frame is not a Snapshot or carries an unexpected snapFormat, so a
// caller never silently asserts against a zero-value Screen.
func DecodeSnapshotFrame(f attachwire.Frame) (attachwire.Screen, error) {
	if f.Type != attachwire.TypeSnapshot {
		return attachwire.Screen{}, fmt.Errorf("viewertest: frame is %v, not a Snapshot", f.Type)
	}
	env, err := attachwire.DecodeSnapshotEnvelope(f.Payload)
	if err != nil {
		return attachwire.Screen{}, fmt.Errorf("viewertest: decoding snapshot envelope: %w", err)
	}
	if env.SnapFormat != attachwire.SnapFormatScreen {
		return attachwire.Screen{}, fmt.Errorf("viewertest: snapshot snapFormat 0x%02X is not the VT screen format (0x%02X)", env.SnapFormat, attachwire.SnapFormatScreen)
	}
	scr, err := attachwire.DecodeScreen(env.Snap)
	if err != nil {
		return attachwire.Screen{}, fmt.Errorf("viewertest: decoding screen: %w", err)
	}
	return scr, nil
}

// IsAltScreen reports whether the screen's active buffer is the alternate buffer
// (a full-screen TUI that entered ESC[?1049h). It is the exact, redraw-proof
// signal that distinguishes a real alt-screen enter from garbled byte output.
func IsAltScreen(s attachwire.Screen) bool {
	return s.ActiveBuffer == attachwire.BufferAlt
}

// CursorAt returns the cursor position (row, col), 0-indexed, row-major — the
// same coordinate space as the cell grid.
func CursorAt(s attachwire.Screen) (row, col int) {
	//nolint:gosec // G115: cursor coords are VT grid coordinates from a decoded snapshot, bounded by the grid geometry
	return int(s.CursorRow), int(s.CursorCol)
}

// CursorVisible reports whether the cursor is currently shown.
func CursorVisible(s attachwire.Screen) bool { return s.CursorVisible }

// activeGrid returns the cell grid of the active buffer (alt when alt-screen is
// active, else primary).
func activeGrid(s attachwire.Screen) []attachwire.Cell {
	if s.ActiveBuffer == attachwire.BufferAlt {
		return s.Alt
	}
	return s.Primary
}

// CellText returns the text of the cell at (row, col) in the ACTIVE buffer,
// joining the cell's rune bytes (a cell MAY carry a multi-codepoint grapheme).
// A wide-glyph continuation cell (the trailing half of a double-width glyph)
// returns "". An out-of-range coordinate returns "" — callers asserting exact
// text will see a mismatch rather than a panic.
func CellText(s attachwire.Screen, row, col int) string {
	return cellTextIn(activeGrid(s), s, row, col)
}

// CellTextIn returns the text of a cell in an explicitly chosen buffer
// (attachwire.BufferPrimary or attachwire.BufferAlt), regardless of which is
// active. Useful to assert the preserved primary screen while alt is active.
func CellTextIn(s attachwire.Screen, buffer uint8, row, col int) string {
	grid := s.Primary
	if buffer == attachwire.BufferAlt {
		grid = s.Alt
	}
	return cellTextIn(grid, s, row, col)
}

func cellTextIn(grid []attachwire.Cell, s attachwire.Screen, row, col int) string {
	if row < 0 || col < 0 || uint64(row) >= s.Rows || uint64(col) >= s.Cols {
		return ""
	}
	//nolint:gosec // G115: Cols is a VT grid width from a decoded snapshot; row/col are bounds-checked above
	idx := row*int(s.Cols) + col
	if idx < 0 || idx >= len(grid) {
		return ""
	}
	c := grid[idx]
	if c.Style&attachwire.StyleWideContinuation != 0 {
		return ""
	}
	return string(c.RuneBytes)
}

// RowText returns the ACTIVE-buffer row's text, concatenating each cell (wide
// continuations skipped) and trimming trailing spaces — the natural form for
// asserting a line's visible content.
func RowText(s attachwire.Screen, row int) string {
	return rowTextIn(activeGrid(s), s, row)
}

// RowTextIn is RowText for an explicitly chosen buffer.
func RowTextIn(s attachwire.Screen, buffer uint8, row int) string {
	grid := s.Primary
	if buffer == attachwire.BufferAlt {
		grid = s.Alt
	}
	return rowTextIn(grid, s, row)
}

func rowTextIn(grid []attachwire.Cell, s attachwire.Screen, row int) string {
	if row < 0 || uint64(row) >= s.Rows {
		return ""
	}
	cols := int(s.Cols) //nolint:gosec // G115: Cols is a VT grid width from a decoded snapshot (small, bounded)
	var b strings.Builder
	for col := 0; col < cols; col++ {
		idx := row*cols + col
		if idx < 0 || idx >= len(grid) {
			break
		}
		c := grid[idx]
		if c.Style&attachwire.StyleWideContinuation != 0 {
			continue
		}
		if len(c.RuneBytes) == 0 {
			b.WriteByte(' ')
			continue
		}
		b.Write(c.RuneBytes)
	}
	return strings.TrimRight(b.String(), " \t\x00")
}

// Dump renders the ACTIVE buffer as newline-joined rows (trailing blanks
// trimmed per row) for diagnostics in a failing assertion. It is prefixed with a
// one-line header describing geometry, active buffer, and cursor.
func Dump(s attachwire.Screen) string {
	buf := "primary"
	if s.ActiveBuffer == attachwire.BufferAlt {
		buf = "alt"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[screen %dx%d buffer=%s cursor=(%d,%d) visible=%t epoch=%d]\n",
		s.Cols, s.Rows, buf, s.CursorRow, s.CursorCol, s.CursorVisible, s.Epoch)
	for row := 0; uint64(row) < s.Rows; row++ {
		b.WriteString(RowText(s, row))
		b.WriteByte('\n')
	}
	return b.String()
}
