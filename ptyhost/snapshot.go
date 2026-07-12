package ptyhost

import (
	"image/color"
	"log/slog"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/attachwire"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// buildScreen serializes a materialized VT state into a snapFormat-0x01
// attachwire.Screen (§12.1). It is escape-safe by construction: every extracted
// rune is validated and any non-printable content is replaced with U+FFFD (and
// logged) rather than failing the snapshot (§9/§12.1 conformance rule). The
// grids are emitted exactly cols*rows cells, with wide glyphs followed by a
// continuation cell (runeLen 0, wide-continuation flag).
func buildScreen(raw vtRaw, epoch uint64, echoMode uint8, logger *slog.Logger) attachwire.Screen {
	cols, rows := raw.cols, raw.rows
	activeBuf := attachwire.BufferPrimary
	if raw.altActive {
		activeBuf = attachwire.BufferAlt
	}
	s := attachwire.Screen{
		Epoch:          epoch,
		EchoMode:       echoMode,
		Cols:           uint64(cols),
		Rows:           uint64(rows),
		ActiveBuffer:   activeBuf,
		CursorRow:      uint64(raw.cursorY),
		CursorCol:      uint64(raw.cursorX),
		CursorVisible:  raw.cursorVisible,
		CursorShape:    raw.cursorShape,
		Modes:          raw.modes,
		MouseProto:     raw.mouseProto,
		SavedCursorRow: uint64(raw.savedY),
		SavedCursorCol: uint64(raw.savedX),
		Primary:        convertGrid(raw.primary, cols, rows, logger),
		AltPresent:     raw.altActive,
	}
	if raw.altActive {
		s.Alt = convertGrid(raw.alt, cols, rows, logger)
	}
	for _, line := range raw.scrollback {
		s.Scrollback = append(s.Scrollback, convertRow(line, len(line), logger))
	}
	return s
}

// convertGrid converts a row-major []*uv.Cell of cols*rows into exactly
// cols*rows attachwire cells, handling wide-glyph continuation.
func convertGrid(grid []*uv.Cell, cols, rows int, logger *slog.Logger) []attachwire.Cell {
	out := make([]attachwire.Cell, 0, cols*rows)
	for y := 0; y < rows; y++ {
		row := grid[y*cols : (y+1)*cols]
		out = append(out, convertRow(row, cols, logger)...)
	}
	return out
}

// convertRow converts one row of up to width columns into exactly width
// attachwire cells (§12.1). A wide glyph (Width>1) is emitted as one base cell
// followed by continuation cells; blanks become a space cell.
func convertRow(row []*uv.Cell, width int, logger *slog.Logger) []attachwire.Cell {
	out := make([]attachwire.Cell, 0, width)
	x := 0
	for x < len(row) && len(out) < width {
		c := row[x]
		base := convertCell(c, logger)
		out = append(out, base)
		w := 1
		if c != nil && c.Width > 1 {
			w = c.Width
		}
		for k := 1; k < w && len(out) < width; k++ {
			out = append(out, attachwire.Cell{
				RuneBytes: nil,
				Style:     attachwire.StyleWideContinuation,
				FG:        base.FG,
				BG:        base.BG,
			})
		}
		x += w
	}
	// Normalize to exactly width cells (pad blanks / truncate).
	for len(out) < width {
		out = append(out, blankCell())
	}
	if len(out) > width {
		out = out[:width]
	}
	return out
}

func blankCell() attachwire.Cell {
	return attachwire.Cell{RuneBytes: []byte(" "), FG: attachwire.DefaultColor, BG: attachwire.DefaultColor}
}

// convertCell converts a single uv.Cell to an escape-safe attachwire.Cell.
func convertCell(c *uv.Cell, logger *slog.Logger) attachwire.Cell {
	if c == nil || c.Content == "" {
		return blankCell()
	}
	rb, replaced := safeRunes(c.Content)
	if replaced && logger != nil {
		logger.Debug("ptyhost: snapshot cell content had non-printable runes; replaced with U+FFFD")
	}
	return attachwire.Cell{
		RuneBytes: rb,
		Style:     convertStyle(c.Style),
		FG:        convertColor(c.Style.Fg),
		BG:        convertColor(c.Style.Bg),
	}
}

// safeRunes returns printable-only UTF-8 for a cell's grapheme content,
// replacing any C0/DEL/C1/ESC rune (or invalid UTF-8) with U+FFFD (§12.1
// escape-safe). It never returns empty for non-empty input; an all-control
// input collapses to a single space.
func safeRunes(content string) (out []byte, replaced bool) {
	out = make([]byte, 0, len(content))
	for i := 0; i < len(content); {
		r, size := utf8.DecodeRuneInString(content[i:])
		if r == utf8.RuneError && size == 1 {
			out = utf8.AppendRune(out, '�')
			replaced = true
			i++
			continue
		}
		i += size
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
			out = utf8.AppendRune(out, '�')
			replaced = true
			continue
		}
		out = utf8.AppendRune(out, r)
	}
	if len(out) == 0 {
		return []byte(" "), replaced
	}
	return out, replaced
}

// convertStyle maps ultraviolet cell attributes to the §12.1 style bitmap.
func convertStyle(st uv.Style) uint8 {
	var out uint8
	a := st.Attrs
	if a&uv.AttrBold != 0 {
		out |= attachwire.StyleBold
	}
	if a&uv.AttrItalic != 0 {
		out |= attachwire.StyleItalic
	}
	if st.Underline != uv.UnderlineNone {
		out |= attachwire.StyleUnderline
	}
	if a&uv.AttrReverse != 0 {
		out |= attachwire.StyleReverse
	}
	if a&uv.AttrFaint != 0 {
		out |= attachwire.StyleDim
	}
	if a&uv.AttrStrikethrough != 0 {
		out |= attachwire.StyleStrikethrough
	}
	return out
}

// convertColor maps a color.Color to the §12.1 discriminated color union.
func convertColor(c color.Color) attachwire.Color {
	switch v := c.(type) {
	case nil:
		return attachwire.DefaultColor
	case ansi.BasicColor:
		return attachwire.IndexedColor(uint8(v))
	case ansi.IndexedColor:
		return attachwire.IndexedColor(uint8(v))
	default:
		// Truecolor / RGBColor / any other color.Color: read the 16-bit channels
		// and scale to 8-bit.
		r, g, b, _ := c.RGBA()
		return attachwire.TrueColor(uint8(r>>8), uint8(g>>8), uint8(b>>8))
	}
}
