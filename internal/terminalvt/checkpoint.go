package vt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"sort"

	checkpointparser "github.com/RenseiAI/donmai/internal/terminalparser"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// CheckpointSchema identifies the complete built-in terminal continuation schema.
// Handlers, callbacks, logger and I/O pipes are receiver-owned configuration;
// embedders must restore their own handler state alongside this checkpoint.
const (
	CheckpointSchema   = "charm-x-vt/b16d026a9d2e+ansi0.11.7+uvf5a850f9c2b7/continuation-v1"
	MaxCheckpointBytes = 16 << 20
	maxCheckpointCells = 2 << 20
)

// These wire structs deliberately contain no interface-typed fields. Indexed
// colors must retain their identity, rather than becoming resolved RGB colors.
// Terminal-originated strings use []byte on the wire: OSC 7/8/title handlers
// can retain invalid UTF-8 as raw Go string bytes, which JSON string encoding
// would replace with U+FFFD and make a reachable checkpoint unrestorable.
type checkpointColor struct {
	Kind  uint8
	Value uint32
	RGBA  [4]uint16
}
type checkpointStyle struct {
	Fg, Bg, UnderlineColor checkpointColor
	Underline              uv.Underline
	Attrs                  uint8
}
type checkpointCell struct {
	Content []byte
	Style   checkpointStyle
	Link    checkpointLink
	Width   int
}
type checkpointLink struct {
	URL, Params []byte
}
type checkpointCellKey struct {
	Content string
	Style   checkpointStyle
	Link    uv.Link
	Width   int
}
type checkpointCursor struct {
	Pen            checkpointStyle
	Link           checkpointLink
	Position       uv.Position
	Style          CursorStyle
	Steady, Hidden bool
}
type checkpointScreen struct {
	Lines             [][]uint32
	Touched           []*uv.LineData
	Cursor, Saved     checkpointCursor
	Scroll            uv.Rectangle
	Scrollback        [][]uint32
	ScrollbackEnabled bool
	ScrollbackLimit   int
}
type checkpointMode struct {
	Number  int
	DEC     bool
	Setting ansi.ModeSetting
}
type checkpointState struct {
	Cells                []checkpointCell
	Schema               string
	Width, Height        int
	Screens              [2]checkpointScreen
	Active               int
	Colors               [256]checkpointColor
	Defaults, Current    [3]checkpointColor
	Charsets             [4]CharSet
	Modes                []checkpointMode
	LastChar             rune
	Grapheme             []rune
	Parser               checkpointparser.ParserCheckpoint
	LastState            byte
	IconName, Title, CWD []byte
	Tabs                 []bool
	GL, GR, GSingle      int
	Phantom              bool
}

func encodeCheckpointColor(c color.Color) checkpointColor {
	switch c := c.(type) {
	case nil:
		return checkpointColor{}
	case ansi.BasicColor:
		return checkpointColor{Kind: 1, Value: uint32(c)}
	case ansi.IndexedColor:
		return checkpointColor{Kind: 2, Value: uint32(c)}
	default:
		r, g, b, a := c.RGBA()
		return checkpointColor{Kind: 4, RGBA: [4]uint16{uint16(r & 0xffff), uint16(g & 0xffff), uint16(b & 0xffff), uint16(a & 0xffff)}}
	}
}

func (c checkpointColor) decode() color.Color {
	switch c.Kind {
	case 0:
		return nil
	case 1:
		return ansi.BasicColor(c.Value & 0xff)
	case 2:
		return ansi.IndexedColor(c.Value & 0xff)
	default:
		return color.RGBA64{R: c.RGBA[0], G: c.RGBA[1], B: c.RGBA[2], A: c.RGBA[3]}
	}
}

func (c checkpointColor) valid() bool {
	return c.Kind <= 4 && c.Kind != 3 && (c.Kind != 1 || c.Value < 16) && (c.Kind != 2 || c.Value < 256) && (c.Kind != 3 || c.Value <= 0xffffff)
}

func encodeCheckpointStyle(s uv.Style) checkpointStyle {
	return checkpointStyle{encodeCheckpointColor(s.Fg), encodeCheckpointColor(s.Bg), encodeCheckpointColor(s.UnderlineColor), s.Underline, s.Attrs}
}

func (s checkpointStyle) decode() uv.Style {
	return uv.Style{Fg: s.Fg.decode(), Bg: s.Bg.decode(), UnderlineColor: s.UnderlineColor.decode(), Underline: s.Underline, Attrs: s.Attrs}
}

func (s checkpointStyle) valid() bool {
	return s.Fg.valid() && s.Bg.valid() && s.UnderlineColor.valid() && s.Underline <= 5
}

func encodeCheckpointCursor(c Cursor) checkpointCursor {
	return checkpointCursor{encodeCheckpointStyle(c.Pen), encodeCheckpointLink(c.Link), c.Position, c.Style, c.Steady, c.Hidden}
}

func (c checkpointCursor) decode() Cursor {
	return Cursor{Pen: c.Pen.decode(), Link: c.Link.decode(), Position: c.Position, Style: c.Style, Steady: c.Steady, Hidden: c.Hidden}
}

func encodeCheckpointLink(link uv.Link) checkpointLink {
	return checkpointLink{URL: []byte(link.URL), Params: []byte(link.Params)}
}

func (link checkpointLink) decode() uv.Link {
	return uv.Link{URL: string(link.URL), Params: string(link.Params)}
}

func encodeCheckpointLines(lines []uv.Line, pool map[checkpointCellKey]uint32, cells *[]checkpointCell) [][]uint32 {
	out := make([][]uint32, len(lines))
	for y, line := range lines {
		out[y] = make([]uint32, len(line))
		for x, c := range line {
			key := checkpointCellKey{c.Content, encodeCheckpointStyle(c.Style), c.Link, c.Width}
			index, ok := pool[key]
			if !ok {
				index = uint32(len(*cells) & (maxCheckpointCells - 1))
				pool[key] = index
				*cells = append(*cells, checkpointCell{Content: []byte(c.Content), Style: key.Style, Link: encodeCheckpointLink(c.Link), Width: c.Width})
			}
			out[y][x] = index
		}
	}
	return out
}

func decodeCheckpointLines(lines [][]uint32, cells []checkpointCell) []uv.Line {
	out := make([]uv.Line, len(lines))
	for y, line := range lines {
		out[y] = make(uv.Line, len(line))
		for x, index := range line {
			c := cells[index]
			out[y][x] = uv.Cell{Content: string(c.Content), Style: c.Style.decode(), Link: c.Link.decode(), Width: c.Width}
		}
	}
	return out
}

// ExportCheckpoint returns an owned, bounded, versioned continuation image.
// It must not run concurrently with writes, resize, or other state mutation.
// Oversized state fails explicitly; no state is truncated or inferred.
func (e *Emulator) ExportCheckpoint() ([]byte, error) {
	if e.closed {
		return nil, fmt.Errorf("checkpoint closed terminal: %w", io.ErrClosedPipe)
	}
	// Bound copying before materializing the checkpoint representation.
	cells, textBytes := 0, 0
	for _, screen := range e.scrs {
		lines := screen.buf.Lines
		if screen.scrollback != nil {
			lines = append(append([]uv.Line(nil), lines...), screen.scrollback.lines...)
		}
		for _, line := range lines {
			cells += len(line)
			if cells > maxCheckpointCells {
				return nil, fmt.Errorf("checkpoint exceeds cell limit")
			}
			for _, cell := range line {
				textBytes += len(cell.Content) + len(cell.Link.URL) + len(cell.Link.Params)
				if textBytes > MaxCheckpointBytes {
					return nil, fmt.Errorf("checkpoint exceeds text limit")
				}
			}
		}
	}
	p, err := e.parser.ExportCheckpoint()
	if err != nil {
		return nil, fmt.Errorf("checkpoint parser: %w", err)
	}
	c := checkpointState{Schema: CheckpointSchema, Width: e.Width(), Height: e.Height(), Charsets: e.charsets, LastChar: e.lastChar, Grapheme: e.grapheme, Parser: p, LastState: e.lastState, IconName: []byte(e.iconName), Title: []byte(e.title), CWD: []byte(e.cwd), GL: e.gl, GR: e.gr, GSingle: e.gsingle, Phantom: e.atPhantom}
	if e.scr == &e.scrs[1] {
		c.Active = 1
	}
	pool := make(map[checkpointCellKey]uint32)
	for i, s := range e.scrs {
		c.Screens[i] = checkpointScreen{Lines: encodeCheckpointLines(s.buf.Lines, pool, &c.Cells), Touched: s.buf.Touched, Cursor: encodeCheckpointCursor(s.cur), Saved: encodeCheckpointCursor(s.saved), Scroll: s.scroll}
		if s.scrollback != nil {
			c.Screens[i].ScrollbackEnabled = true
			c.Screens[i].ScrollbackLimit = s.scrollback.maxLines
			c.Screens[i].Scrollback = encodeCheckpointLines(s.scrollback.lines, pool, &c.Cells)
		}
	}
	for i, col := range e.colors {
		c.Colors[i] = encodeCheckpointColor(col)
	}
	c.Defaults = [3]checkpointColor{encodeCheckpointColor(e.defaultFg), encodeCheckpointColor(e.defaultBg), encodeCheckpointColor(e.defaultCur)}
	c.Current = [3]checkpointColor{encodeCheckpointColor(e.fgColor), encodeCheckpointColor(e.bgColor), encodeCheckpointColor(e.curColor)}
	for m, s := range e.modes {
		_, dec := m.(ansi.DECMode)
		c.Modes = append(c.Modes, checkpointMode{m.Mode(), dec, s})
	}
	sort.Slice(c.Modes, func(i, j int) bool {
		if c.Modes[i].DEC != c.Modes[j].DEC {
			return !c.Modes[i].DEC
		}
		return c.Modes[i].Number < c.Modes[j].Number
	})
	c.Tabs = make([]bool, e.tabstops.Width())
	for i := range c.Tabs {
		c.Tabs[i] = e.tabstops.IsStop(i)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode checkpoint: %w", err)
	}
	if len(b) > MaxCheckpointBytes {
		return nil, fmt.Errorf("checkpoint exceeds byte limit")
	}
	return b, nil
}

func (c checkpointState) validate() error {
	if c.Schema != CheckpointSchema {
		return fmt.Errorf("unsupported checkpoint schema %q", c.Schema)
	}
	if c.Width < 1 || c.Height < 1 || c.Width > 4096 || c.Height > 4096 || c.Width > maxCheckpointCells/c.Height || c.Active < 0 || c.Active > 1 {
		return fmt.Errorf("invalid checkpoint geometry")
	}
	if len(c.Tabs) != c.Width || c.GL < 0 || c.GL > 3 || c.GR < 0 || c.GR > 3 || c.GSingle < 0 || c.GSingle > 3 || c.LastState > parser.Utf8State || len(c.Grapheme) > 4096 {
		return fmt.Errorf("invalid terminal continuation state")
	}
	if err := c.Parser.Validate(); err != nil {
		return fmt.Errorf("invalid checkpoint parser: %w", err)
	}
	if len(c.Cells) == 0 || len(c.Cells) > maxCheckpointCells {
		return fmt.Errorf("invalid cell dictionary")
	}
	for _, cell := range c.Cells {
		if cell.Width < 0 || cell.Width > 4096 || !cell.Style.valid() {
			return fmt.Errorf("invalid dictionary cell")
		}
	}
	cells := 0
	for _, s := range c.Screens {
		if len(s.Lines) != c.Height || len(s.Touched) > c.Height || s.Scroll.Min.X < 0 || s.Scroll.Min.Y < 0 || s.Scroll.Max.X > c.Width || s.Scroll.Max.Y > c.Height || s.Scroll.Empty() {
			return fmt.Errorf("invalid screen state")
		}
		for _, line := range s.Touched {
			if line != nil && (line.FirstCell < 0 || line.LastCell < line.FirstCell || line.LastCell > c.Width) {
				return fmt.Errorf("invalid touched line range")
			}
		}
		for _, cur := range []checkpointCursor{s.Cursor, s.Saved} {
			// Saved cursors may legitimately lie beyond the current dimensions after resize.
			if cur.Position.X < 0 || cur.Position.Y < 0 || cur.Position.X > 4096 || cur.Position.Y > 4096 || cur.Style < 0 || cur.Style > CursorBar || !cur.Pen.valid() {
				return fmt.Errorf("invalid cursor state")
			}
		}
		if s.ScrollbackLimit < 0 || s.ScrollbackLimit > 100000 || len(s.Scrollback) > s.ScrollbackLimit || (!s.ScrollbackEnabled && (len(s.Scrollback) > 0 || s.ScrollbackLimit != 0)) {
			return fmt.Errorf("invalid scrollback state")
		}
		for _, line := range s.Lines {
			if len(line) != c.Width {
				return fmt.Errorf("invalid screen line")
			}
		}
		for _, lines := range [][][]uint32{s.Lines, s.Scrollback} {
			for _, line := range lines {
				cells += len(line)
				if cells > maxCheckpointCells {
					return fmt.Errorf("checkpoint exceeds cell limit")
				}
				for _, index := range line {
					if uint64(index) >= uint64(len(c.Cells)) {
						return fmt.Errorf("invalid cell reference")
					}
				}
			}
		}
	}
	for _, cols := range [][]checkpointColor{c.Colors[:], c.Defaults[:], c.Current[:]} {
		for _, col := range cols {
			if !col.valid() {
				return fmt.Errorf("invalid color")
			}
		}
	}
	if len(c.Modes) > 1024 {
		return fmt.Errorf("too many terminal modes")
	}
	seen := map[[2]int]bool{}
	for _, m := range c.Modes {
		dec := 0
		if m.DEC {
			dec = 1
		}
		key := [2]int{dec, m.Number}
		if seen[key] || m.Number < 0 || m.Number > parser.ParamMask || m.Setting > ansi.ModePermanentlyReset {
			return fmt.Errorf("invalid mode state")
		}
		seen[key] = true
	}
	for _, cs := range c.Charsets {
		if len(cs) > 256 {
			return fmt.Errorf("invalid charset")
		}
	}
	return nil
}

// RestoreCheckpoint validates the entire image before replacing terminal state.
// It keeps handlers/callbacks/logger/pipes and emits no callbacks or output while
// restoring. On success existing screen pointers remain stable. A closed
// terminal cannot be restored. Configuration must match the exporting embedder.
func (e *Emulator) RestoreCheckpoint(b []byte) error {
	if e.closed {
		return fmt.Errorf("restore closed terminal: %w", io.ErrClosedPipe)
	}
	if len(b) == 0 || len(b) > MaxCheckpointBytes {
		return fmt.Errorf("invalid checkpoint length")
	}
	var c checkpointState
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return fmt.Errorf("decode checkpoint: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing checkpoint data")
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, b) {
		return fmt.Errorf("noncanonical or incomplete checkpoint")
	}
	if err := c.validate(); err != nil {
		return err
	}
	// Parser validation above makes this restore non-failing before state commit.
	if err := e.parser.RestoreCheckpoint(c.Parser); err != nil {
		return err
	}
	for i, s := range c.Screens {
		buf := &uv.RenderBuffer{Buffer: &uv.Buffer{Lines: decodeCheckpointLines(s.Lines, c.Cells)}, Touched: s.Touched}
		e.scrs[i] = Screen{cb: &e.cb, buf: buf, cur: s.Cursor.decode(), saved: s.Saved.decode(), scroll: s.Scroll}
		if s.ScrollbackEnabled {
			e.scrs[i].scrollback = &Scrollback{lines: decodeCheckpointLines(s.Scrollback, c.Cells), maxLines: s.ScrollbackLimit}
		}
	}
	e.scr = &e.scrs[c.Active]
	for i, col := range c.Colors {
		e.colors[i] = col.decode()
	}
	e.defaultFg, e.defaultBg, e.defaultCur = c.Defaults[0].decode(), c.Defaults[1].decode(), c.Defaults[2].decode()
	e.fgColor, e.bgColor, e.curColor = c.Current[0].decode(), c.Current[1].decode(), c.Current[2].decode()
	e.charsets = c.Charsets
	e.modes = make(ansi.Modes, len(c.Modes))
	for _, m := range c.Modes {
		var mode ansi.Mode = ansi.ANSIMode(m.Number)
		if m.DEC {
			mode = ansi.DECMode(m.Number)
		}
		e.modes[mode] = m.Setting
	}
	e.lastChar, e.grapheme, e.lastState = c.LastChar, c.Grapheme, c.LastState
	e.iconName, e.title, e.cwd = string(c.IconName), string(c.Title), string(c.CWD)
	e.tabstops = uv.DefaultTabStops(c.Width)
	e.tabstops.Clear()
	for i, on := range c.Tabs {
		if on {
			e.tabstops.Set(i)
		}
	}
	e.gl, e.gr, e.gsingle, e.atPhantom = c.GL, c.GR, c.GSingle, c.Phantom
	return nil
}
