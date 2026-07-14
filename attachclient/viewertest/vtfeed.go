package viewertest

import (
	"unicode/utf8"

	"github.com/RenseiAI/donmai/attachwire"
	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"
)

// ScreenBuilder is the optional VT-feed convenience path: it feeds relay-side
// Output byte-frames into a fresh terminal emulator seeded to a fixed geometry
// and materializes an attachwire.Screen, so a caller can assert screen state
// between snapshots without forcing a snapshot_request. The authoritative path
// is snapshot-decode (DecodeSnapshotFrame); this mirror reconstructs only what
// the assertion helpers read — the active buffer's cells, cursor, and
// alt-screen state.
//
// It feeds the same app→terminal Output bytes the host produced (rendering
// escapes and text — never terminal→app query requests), so no query-responder
// wiring is needed. It is single-goroutine; guard it externally if shared.
type ScreenBuilder struct {
	emu   *vt.Emulator
	epoch uint64
}

// NewScreenBuilder builds a VT-feed screen reconstructor at the given geometry.
// Use the same cols/rows the fixture (and host session) run at.
func NewScreenBuilder(cols, rows int, epoch uint64) *ScreenBuilder {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return &ScreenBuilder{emu: vt.NewEmulator(cols, rows), epoch: epoch}
}

// Feed applies one Output frame's bytes to the emulator. Pass the Data of an
// attachwire.TypeOutput frame (DecodeOutput(frame.Payload).Data); non-Output
// frames must not be fed here.
func (b *ScreenBuilder) Feed(data []byte) {
	if len(data) == 0 {
		return
	}
	_, _ = b.emu.Write(data)
}

// FeedFrame feeds an Output frame, ignoring any other frame type. It reports
// whether the frame was an Output frame (and thus fed).
func (b *ScreenBuilder) FeedFrame(f attachwire.Frame) bool {
	if f.Type != attachwire.TypeOutput {
		return false
	}
	b.Feed(attachwire.DecodeOutput(f.Payload).Data)
	return true
}

// Screen materializes the current emulator state into an attachwire.Screen. The
// active buffer's grid is populated (Primary when primary-active, Alt when
// alt-active); the inactive buffer is left empty. This is enough for CellText,
// RowText, CursorAt, CursorVisible, and IsAltScreen.
func (b *ScreenBuilder) Screen() attachwire.Screen {
	e := b.emu
	cols, rows := e.Width(), e.Height()
	alt := e.IsAltScreen()
	pos := e.CursorPosition()

	activeBuf := attachwire.BufferPrimary
	if alt {
		activeBuf = attachwire.BufferAlt
	}
	//nolint:gosec // G115: cols/rows/cursor are emulator grid dimensions and coordinates, always non-negative and small
	s := attachwire.Screen{
		Epoch:         b.epoch,
		EchoMode:      attachwire.EchoUnknown,
		Cols:          uint64(cols),
		Rows:          uint64(rows),
		ActiveBuffer:  activeBuf,
		CursorRow:     uint64(clampNonNeg(pos.Y)),
		CursorCol:     uint64(clampNonNeg(pos.X)),
		CursorVisible: true,
		AltPresent:    alt,
	}
	grid := buildGridFromEmulator(e, cols, rows)
	if alt {
		s.Alt = grid
	} else {
		s.Primary = grid
	}
	return s
}

func clampNonNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// buildGridFromEmulator reads the active screen's cells row-major into exactly
// cols*rows attachwire cells, mirroring wide-glyph continuation handling.
func buildGridFromEmulator(e *vt.Emulator, cols, rows int) []attachwire.Cell {
	out := make([]attachwire.Cell, 0, cols*rows)
	for y := 0; y < rows; y++ {
		rowEnd := (y + 1) * cols
		for x := 0; x < cols && len(out) < rowEnd; {
			c := e.CellAt(x, y)
			base := printableCell(c)
			out = append(out, base)
			w := 1
			if c != nil && c.Width > 1 {
				w = c.Width
			}
			for k := 1; k < w && len(out) < rowEnd; k++ {
				out = append(out, attachwire.Cell{
					RuneBytes: nil,
					Style:     attachwire.StyleWideContinuation,
					FG:        base.FG,
					BG:        base.BG,
				})
			}
			x += w
		}
		for len(out) < rowEnd {
			out = append(out, blankCell())
		}
	}
	return out
}

func blankCell() attachwire.Cell {
	return attachwire.Cell{RuneBytes: []byte(" "), FG: attachwire.DefaultColor, BG: attachwire.DefaultColor}
}

// printableCell converts a uv.Cell to an escape-safe attachwire cell, replacing
// any non-printable rune with U+FFFD (matching the host's §12.1 policy). An
// empty cell becomes a blank space.
func printableCell(c *uv.Cell) attachwire.Cell {
	if c == nil || c.Content == "" {
		return blankCell()
	}
	rb := make([]byte, 0, len(c.Content))
	for i := 0; i < len(c.Content); {
		r, size := utf8.DecodeRuneInString(c.Content[i:])
		if r == utf8.RuneError && size == 1 {
			rb = utf8.AppendRune(rb, '�')
			i++
			continue
		}
		i += size
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
			rb = utf8.AppendRune(rb, '�')
			continue
		}
		rb = utf8.AppendRune(rb, r)
	}
	if len(rb) == 0 {
		return blankCell()
	}
	return attachwire.Cell{RuneBytes: rb, FG: attachwire.DefaultColor, BG: attachwire.DefaultColor}
}
