package attachwire

import (
	"fmt"
	"math/bits"
	"unicode/utf8"
)

// §12.1 snapFormat 0x01 — VT-serialized screen. This payload layout is v1-draft
// (owned by the host VT) and format-tagged so it can evolve behind a new
// snapFormat value without a protocol bump. It is escape-safe by construction:
// the cell grid cannot carry C0/DEL/C1/ESC bytes in runeBytes, validated on
// BOTH encode and decode (§9).

// echoMode values (§10, §12.1).
const (
	EchoOff     uint8 = 0x00 // echo-off / raw (e.g. a password prompt)
	EchoOn      uint8 = 0x01 // echo-on / cooked
	EchoUnknown uint8 = 0xFF // unknown — prediction is SUPPRESSED (§10)
)

// activeBuffer values (§12.1).
const (
	BufferPrimary uint8 = 0x00
	BufferAlt     uint8 = 0x01
)

// cursorShape values (§12.1).
const (
	CursorShapeDefault   uint8 = 0x00
	CursorShapeBlock     uint8 = 0x01
	CursorShapeUnderline uint8 = 0x02
	CursorShapeBar       uint8 = 0x03
)

// modes bitmap (§12.1): the terminal modes a viewer must know to render and to
// route driver input correctly. Bits 0x20–0x80 are reserved and MUST be 0.
const (
	ModeBracketedPaste uint8 = 0x01 // ?2004
	ModeAppCursorKeys  uint8 = 0x02 // DECCKM
	ModePendingWrap    uint8 = 0x04 // autowrap "wrap on next glyph" state
	ModeMouseTracking  uint8 = 0x08 // mouse tracking enabled
	ModeFocusEvent     uint8 = 0x10 // ?1004 focus-event reporting
	ModesReservedMask  uint8 = 0xE0 // 0x20 | 0x40 | 0x80 — MUST be 0
)

// mouseProto low-nibble tracking modes (§12.1); meaningful only when
// ModeMouseTracking is set.
const (
	MouseTrackNormal    uint8 = 0x1 // ?1000
	MouseTrackHighlight uint8 = 0x2 // ?1001
	MouseTrackButton    uint8 = 0x3 // ?1002
	MouseTrackAny       uint8 = 0x4 // ?1003
)

// mouseProto high-nibble coordinate encodings (§12.1).
const (
	MouseEncX10  uint8 = 0x0 // legacy X10
	MouseEncUTF8 uint8 = 0x1 // ?1005
	MouseEncSGR  uint8 = 0x2 // ?1006
)

// MouseProto packs a tracking mode (low nibble) and coordinate encoding (high
// nibble) into the §12.1 mouseProto byte. Example: SGR any-event = 0x24.
func MouseProto(track, enc uint8) uint8 { return (enc << 4) | (track & 0x0F) }

// MouseTrack extracts the low-nibble tracking mode from a mouseProto byte.
func MouseTrack(p uint8) uint8 { return p & 0x0F }

// MouseEnc extracts the high-nibble coordinate encoding from a mouseProto byte.
func MouseEnc(p uint8) uint8 { return p >> 4 }

// style bit flags (§12.1). Bit 0x80 is reserved and MUST be 0.
const (
	StyleBold             uint8 = 0x01
	StyleItalic           uint8 = 0x02
	StyleUnderline        uint8 = 0x04
	StyleReverse          uint8 = 0x08
	StyleDim              uint8 = 0x10
	StyleStrikethrough    uint8 = 0x20
	StyleWideContinuation uint8 = 0x40 // wide-glyph continuation cell (runeLen == 0)
	StyleReserved         uint8 = 0x80 // MUST be 0
)

// ColorMode discriminates the §12.1 cell color union.
type ColorMode uint8

// The §12.1 ColorMode discriminant values.
const (
	ColorDefault   ColorMode = 0x00 // no operand bytes
	ColorIndexed   ColorMode = 0x01 // [idx:u8]
	ColorTruecolor ColorMode = 0x02 // [r:u8][g:u8][b:u8]
)

// Color is a §12.1 discriminated color union so indexed-256 and truecolor cells
// coexist losslessly.
type Color struct {
	Mode    ColorMode
	Idx     uint8 // ColorIndexed
	R, G, B uint8 // ColorTruecolor
}

// DefaultColor is the zero-value default color (no operand bytes).
var DefaultColor = Color{Mode: ColorDefault}

// IndexedColor builds an indexed-256 color.
func IndexedColor(idx uint8) Color { return Color{Mode: ColorIndexed, Idx: idx} }

// TrueColor builds a 24-bit truecolor.
func TrueColor(r, g, b uint8) Color { return Color{Mode: ColorTruecolor, R: r, G: g, B: b} }

// Cell is one §12.1 grid cell. RuneBytes is length-prefixed UTF-8 (§12.1): it
// MAY carry a multi-codepoint grapheme cluster, is empty (runeLen == 0) only on
// a wide-glyph continuation cell (StyleWideContinuation set), and MUST contain
// no C0/DEL/C1/ESC content (escape-safe by construction, §9).
type Cell struct {
	RuneBytes []byte
	Style     uint8
	FG        Color
	BG        Color
}

// Screen is a snapFormat 0x01 serialized terminal screen (§12.1).
type Screen struct {
	Epoch          uint64 // host stream epoch this snapshot belongs to (§4.1)
	EchoMode       uint8  // EchoOff | EchoOn | EchoUnknown
	Cols           uint64
	Rows           uint64
	ActiveBuffer   uint8 // BufferPrimary | BufferAlt
	CursorRow      uint64
	CursorCol      uint64
	CursorVisible  bool
	CursorShape    uint8 // CursorShape*
	Modes          uint8 // modes bitmap
	MouseProto     uint8 // mouse tracking protocol + encoding
	SavedCursorRow uint64
	SavedCursorCol uint64
	Primary        []Cell   // Rows × Cols cells, row-major
	AltPresent     bool     // whether an alt buffer follows
	Alt            []Cell   // Rows × Cols cells if AltPresent
	Scrollback     [][]Cell // scrollback-tail lines, oldest first (primary only)
}

// validateHeader enforces the §12.1 header invariants that hold on both encode
// and decode.
func (s Screen) validateHeader() error {
	switch s.EchoMode {
	case EchoOff, EchoOn, EchoUnknown:
	default:
		return newFramingf("snapshot echoMode invalid: 0x%02X", s.EchoMode)
	}
	switch s.ActiveBuffer {
	case BufferPrimary, BufferAlt:
	default:
		return newFramingf("snapshot activeBuffer invalid: 0x%02X", s.ActiveBuffer)
	}
	if s.CursorShape > CursorShapeBar {
		return newFramingf("snapshot cursorShape invalid: 0x%02X", s.CursorShape)
	}
	if s.Modes&ModesReservedMask != 0 {
		return newFramingf("snapshot modes reserved bits set: 0x%02X", s.Modes)
	}
	if s.Modes&ModeMouseTracking == 0 && s.MouseProto != 0 {
		return newFramingf("snapshot mouseProto must be 0 when mouse tracking disabled: 0x%02X", s.MouseProto)
	}
	return nil
}

// gridLen returns Rows × Cols, rejecting a multiplication overflow.
func (s Screen) gridLen() (uint64, error) {
	hi, lo := bits.Mul64(s.Rows, s.Cols)
	if hi != 0 {
		return 0, newFramingf("snapshot grid dimensions overflow uint64: rows=%d cols=%d", s.Rows, s.Cols)
	}
	return lo, nil
}

// Encode serializes the screen to snapFormat 0x01 bytes (§12.1). It validates
// the header, the grid sizes (Primary and, if present, Alt each hold exactly
// Rows × Cols cells), and every cell's escape-safety and flag invariants. A
// grid-size mismatch is a plain error (a producer bug); a wire-level invariant
// violation (reserved bit set, disallowed rune) is a FramingError.
func (s Screen) Encode() ([]byte, error) {
	if err := s.validateHeader(); err != nil {
		return nil, err
	}
	n, err := s.gridLen()
	if err != nil {
		return nil, err
	}
	if uint64(len(s.Primary)) != n {
		return nil, fmt.Errorf("attachwire: primary grid has %d cells, want rows*cols=%d", len(s.Primary), n)
	}
	if s.AltPresent && uint64(len(s.Alt)) != n {
		return nil, fmt.Errorf("attachwire: alt grid has %d cells, want rows*cols=%d", len(s.Alt), n)
	}

	buf := make([]byte, 0, 32+int(min64(n, 1024))*5) //nolint:gosec // G115: min64(n, 1024) is capped to 1024, always fits int
	buf = AppendUvarint(buf, s.Epoch)
	buf = append(buf, s.EchoMode)
	buf = AppendUvarint(buf, s.Cols)
	buf = AppendUvarint(buf, s.Rows)
	buf = append(buf, s.ActiveBuffer)
	buf = AppendUvarint(buf, s.CursorRow)
	buf = AppendUvarint(buf, s.CursorCol)
	buf = append(buf, boolByte(s.CursorVisible))
	buf = append(buf, s.CursorShape)
	buf = append(buf, s.Modes)
	buf = append(buf, s.MouseProto)
	buf = AppendUvarint(buf, s.SavedCursorRow)
	buf = AppendUvarint(buf, s.SavedCursorCol)

	for i := range s.Primary {
		buf, err = encodeCell(buf, s.Primary[i])
		if err != nil {
			return nil, err
		}
	}
	buf = append(buf, boolByte(s.AltPresent))
	if s.AltPresent {
		for i := range s.Alt {
			buf, err = encodeCell(buf, s.Alt[i])
			if err != nil {
				return nil, err
			}
		}
	}
	buf = AppendUvarint(buf, uint64(len(s.Scrollback)))
	for _, line := range s.Scrollback {
		buf = AppendUvarint(buf, uint64(len(line)))
		for i := range line {
			buf, err = encodeCell(buf, line[i])
			if err != nil {
				return nil, err
			}
		}
	}
	return buf, nil
}

// DecodeScreen parses snapFormat 0x01 bytes into a Screen (§12.1), enforcing
// every escape-safety and flag invariant. Any shortfall, disallowed rune,
// reserved bit, or unknown color mode is a FramingError.
func DecodeScreen(snap []byte) (Screen, error) {
	r := newReader(snap)
	var s Screen
	var err error

	if s.Epoch, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if s.EchoMode, err = r.readByte(); err != nil {
		return Screen{}, err
	}
	if s.Cols, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if s.Rows, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if s.ActiveBuffer, err = r.readByte(); err != nil {
		return Screen{}, err
	}
	if s.CursorRow, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if s.CursorCol, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if s.CursorVisible, err = r.boolByte(); err != nil {
		return Screen{}, err
	}
	if s.CursorShape, err = r.readByte(); err != nil {
		return Screen{}, err
	}
	if s.Modes, err = r.readByte(); err != nil {
		return Screen{}, err
	}
	if s.MouseProto, err = r.readByte(); err != nil {
		return Screen{}, err
	}
	if s.SavedCursorRow, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if s.SavedCursorCol, err = r.uvarint(); err != nil {
		return Screen{}, err
	}
	if err = s.validateHeader(); err != nil {
		return Screen{}, err
	}

	n, err := s.gridLen()
	if err != nil {
		return Screen{}, err
	}
	if s.Primary, err = r.cells(n); err != nil {
		return Screen{}, err
	}
	if s.AltPresent, err = r.boolByte(); err != nil {
		return Screen{}, err
	}
	if s.AltPresent {
		if s.Alt, err = r.cells(n); err != nil {
			return Screen{}, err
		}
	}
	sbLines, err := r.uvarint()
	if err != nil {
		return Screen{}, err
	}
	if s.Scrollback, err = r.scrollback(sbLines); err != nil {
		return Screen{}, err
	}
	if err := r.expectDone(); err != nil {
		return Screen{}, err
	}
	return s, nil
}

// encodeCell appends one §12.1 cell, enforcing the escape-safety and flag
// invariants on encode.
func encodeCell(dst []byte, c Cell) ([]byte, error) {
	if c.Style&StyleReserved != 0 {
		return nil, newFramingf("snapshot cell style reserved bit 0x80 set: 0x%02X", c.Style)
	}
	cont := c.Style&StyleWideContinuation != 0
	if cont && len(c.RuneBytes) != 0 {
		return nil, newFraming("snapshot continuation cell must carry runeLen == 0")
	}
	if !cont && len(c.RuneBytes) == 0 {
		return nil, newFraming("snapshot runeLen == 0 permitted only on continuation cells")
	}
	if err := validateRuneBytes(c.RuneBytes); err != nil {
		return nil, err
	}
	dst = AppendUvarint(dst, uint64(len(c.RuneBytes)))
	dst = append(dst, c.RuneBytes...)
	dst = append(dst, c.Style)
	var err error
	if dst, err = encodeColor(dst, c.FG); err != nil {
		return nil, err
	}
	if dst, err = encodeColor(dst, c.BG); err != nil {
		return nil, err
	}
	return dst, nil
}

func encodeColor(dst []byte, c Color) ([]byte, error) {
	switch c.Mode {
	case ColorDefault:
		return append(dst, byte(ColorDefault)), nil
	case ColorIndexed:
		return append(dst, byte(ColorIndexed), c.Idx), nil
	case ColorTruecolor:
		return append(dst, byte(ColorTruecolor), c.R, c.G, c.B), nil
	default:
		return nil, newFramingf("snapshot unknown color mode 0x%02X", byte(c.Mode))
	}
}

// cell decodes one §12.1 cell.
func (r *reader) cell() (Cell, error) {
	runeLen, err := r.uvarint()
	if err != nil {
		return Cell{}, err
	}
	rb, err := r.bytes(runeLen)
	if err != nil {
		return Cell{}, err
	}
	style, err := r.readByte()
	if err != nil {
		return Cell{}, err
	}
	if style&StyleReserved != 0 {
		return Cell{}, newFramingf("snapshot cell style reserved bit 0x80 set: 0x%02X", style)
	}
	cont := style&StyleWideContinuation != 0
	if cont && runeLen != 0 {
		return Cell{}, newFraming("snapshot continuation cell must carry runeLen == 0")
	}
	if !cont && runeLen == 0 {
		return Cell{}, newFraming("snapshot runeLen == 0 permitted only on continuation cells")
	}
	if err := validateRuneBytes(rb); err != nil {
		return Cell{}, err
	}
	fg, err := r.color()
	if err != nil {
		return Cell{}, err
	}
	bg, err := r.color()
	if err != nil {
		return Cell{}, err
	}
	return Cell{RuneBytes: rb, Style: style, FG: fg, BG: bg}, nil
}

func (r *reader) color() (Color, error) {
	m, err := r.readByte()
	if err != nil {
		return Color{}, err
	}
	switch ColorMode(m) {
	case ColorDefault:
		return Color{Mode: ColorDefault}, nil
	case ColorIndexed:
		idx, err := r.readByte()
		if err != nil {
			return Color{}, err
		}
		return Color{Mode: ColorIndexed, Idx: idx}, nil
	case ColorTruecolor:
		rr, err := r.readByte()
		if err != nil {
			return Color{}, err
		}
		g, err := r.readByte()
		if err != nil {
			return Color{}, err
		}
		b, err := r.readByte()
		if err != nil {
			return Color{}, err
		}
		return Color{Mode: ColorTruecolor, R: rr, G: g, B: b}, nil
	default:
		return Color{}, newFramingf("snapshot unknown color mode 0x%02X", m)
	}
}

// cells decodes exactly n cells. The initial capacity is bounded so a hostile n
// cannot force a huge up-front allocation; the loop still errors out via
// truncation once the buffer is exhausted. n == 0 yields a nil slice.
func (r *reader) cells(n uint64) ([]Cell, error) {
	if n == 0 {
		return nil, nil
	}
	out := make([]Cell, 0, boundedCap(n))
	for i := uint64(0); i < n; i++ {
		c, err := r.cell()
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// scrollback decodes the line-framed scrollback tail (oldest first, §12.1).
func (r *reader) scrollback(lines uint64) ([][]Cell, error) {
	if lines == 0 {
		return nil, nil
	}
	out := make([][]Cell, 0, boundedCap(lines))
	for i := uint64(0); i < lines; i++ {
		cnt, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		line, err := r.cells(cnt)
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

// boolByte reads a u8 constrained to 0 or 1.
func (r *reader) boolByte() (bool, error) {
	b, err := r.readByte()
	if err != nil {
		return false, err
	}
	switch b {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, newFramingf("snapshot expected boolean byte (0|1), got 0x%02X", b)
	}
}

// validateRuneBytes enforces §12.1 escape-safety: runeBytes MUST be valid UTF-8
// carrying only printable content — no C0 (U+0000–U+001F, includes ESC), no DEL
// (U+007F), no C1 (U+0080–U+009F). The check is on decoded CODE POINTS, not raw
// bytes, so legitimate multi-byte UTF-8 (whose continuation bytes fall in
// 0x80–0xBF) is accepted.
func validateRuneBytes(b []byte) error {
	for i := 0; i < len(b); {
		rn, size := utf8.DecodeRune(b[i:])
		if rn == utf8.RuneError && size == 1 {
			return newFraming("snapshot runeBytes is not valid UTF-8")
		}
		if rn < 0x20 || rn == 0x7F || (rn >= 0x80 && rn <= 0x9F) {
			return newFramingf("snapshot runeBytes contains disallowed control rune U+%04X", rn)
		}
		i += size
	}
	return nil
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
