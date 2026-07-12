package attachwire

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
)

// fixtureScreen is the §12.1 conformance screen: a 2×1 primary-active screen in
// epoch 1, echo-on; cursor at row 0, col 1, visible, block; no modes, no mouse;
// saved primary cursor == active cursor; cell 1 = "A" bold, indexed-256 fg 1,
// default bg; cell 2 = " " all-default; no alt; no scrollback.
func fixtureScreen() Screen {
	return Screen{
		Epoch:          1,
		EchoMode:       EchoOn,
		Cols:           2,
		Rows:           1,
		ActiveBuffer:   BufferPrimary,
		CursorRow:      0,
		CursorCol:      1,
		CursorVisible:  true,
		CursorShape:    CursorShapeBlock,
		Modes:          0,
		MouseProto:     0,
		SavedCursorRow: 0,
		SavedCursorCol: 1,
		Primary: []Cell{
			{RuneBytes: []byte("A"), Style: StyleBold, FG: IndexedColor(1), BG: DefaultColor},
			{RuneBytes: []byte(" "), Style: 0, FG: DefaultColor, BG: DefaultColor},
		},
		AltPresent: false,
		Scrollback: nil,
	}
}

// fixture26 is the authoritative 26-byte §12.1 snap serialization.
var fixture26 = []byte{
	0x01, 0x01, 0x02, 0x01, 0x00, 0x00, 0x01, 0x01, 0x01, // epoch, echo, cols, rows, activeBuf, curRow, curCol, curVis, curShape
	0x00, 0x00, // modes, mouseProto
	0x00, 0x01, // savedCursor row, col
	0x01, 0x41, 0x01, 0x01, 0x01, 0x00, // cell "A": runeLen 'A' style=bold fg=indexed(1) bg=default
	0x01, 0x20, 0x00, 0x00, 0x00, // cell " ": runeLen ' ' style=0 fg=default bg=default
	0x00, // altPresent=0
	0x00, // sbLines=0
}

// TestSnapFormat01ConformanceFixture26Bytes proves the §12.1 snapFormat 0x01
// serialization is byte-exact: it encodes the described screen to the literal
// 26 bytes and decodes the literal 26 bytes back to a structurally-equal screen.
func TestSnapFormat01ConformanceFixture26Bytes(t *testing.T) {
	if len(fixture26) != 26 {
		t.Fatalf("fixture is %d bytes, must be 26", len(fixture26))
	}
	s := fixtureScreen()
	enc, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(enc, fixture26) {
		t.Fatalf("encode not byte-exact:\n got % X\nwant % X", enc, fixture26)
	}
	dec, err := DecodeScreen(fixture26)
	if err != nil {
		t.Fatalf("DecodeScreen: %v", err)
	}
	if !reflect.DeepEqual(dec, s) {
		t.Fatalf("decode not structurally equal:\n got %#v\nwant %#v", dec, s)
	}
}

// TestSnapshotEnvelopeConformanceFixtureAtSeq42 proves the full frozen envelope
// wrap at atSeq=42: 2A 01 1A + the 26 snap bytes (§12.1).
func TestSnapshotEnvelopeConformanceFixtureAtSeq42(t *testing.T) {
	env := SnapshotEnvelope{AtSeq: 42, SnapFormat: SnapFormatScreen, Snap: fixture26}
	enc := env.Encode()
	want := append([]byte{0x2A, 0x01, 0x1A}, fixture26...)
	if !bytes.Equal(enc, want) {
		t.Fatalf("envelope not byte-exact:\n got % X\nwant % X", enc, want)
	}
	dec, err := DecodeSnapshotEnvelope(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec.AtSeq != 42 || dec.SnapFormat != SnapFormatScreen || !bytes.Equal(dec.Snap, fixture26) {
		t.Fatalf("envelope decode mismatch: %#v", dec)
	}
	// And the inner snap decodes to the fixture screen.
	scr, err := DecodeScreen(dec.Snap)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scr, fixtureScreen()) {
		t.Fatalf("inner snap decode mismatch")
	}
}

func TestScreenRoundTripRich(t *testing.T) {
	// A screen exercising alt buffer, wide-glyph continuation, truecolor,
	// indexed color, multi-codepoint grapheme, scrollback (incl. an empty line).
	wide := Cell{RuneBytes: []byte("世"), Style: 0, FG: TrueColor(10, 20, 30), BG: DefaultColor}
	cont := Cell{RuneBytes: nil, Style: StyleWideContinuation, FG: TrueColor(10, 20, 30), BG: DefaultColor}
	zwj := Cell{RuneBytes: []byte("👩‍💻"), Style: StyleBold | StyleUnderline, FG: IndexedColor(200), BG: TrueColor(1, 2, 3)}
	blank := Cell{RuneBytes: []byte(" "), Style: 0, FG: DefaultColor, BG: DefaultColor}
	s := Screen{
		Epoch:          77,
		EchoMode:       EchoUnknown,
		Cols:           2,
		Rows:           2,
		ActiveBuffer:   BufferAlt,
		CursorRow:      1,
		CursorCol:      0,
		CursorVisible:  false,
		CursorShape:    CursorShapeBar,
		Modes:          ModeBracketedPaste | ModeMouseTracking | ModeFocusEvent,
		MouseProto:     MouseProto(MouseTrackAny, MouseEncSGR), // 0x24
		SavedCursorRow: 0,
		SavedCursorCol: 1,
		Primary:        []Cell{wide, cont, blank, blank},
		AltPresent:     true,
		Alt:            []Cell{zwj, blank, blank, blank},
		Scrollback: [][]Cell{
			{blank, wide, cont},
			nil, // empty scrollback line
		},
	}
	enc, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeScreen(enc)
	if err != nil {
		t.Fatalf("DecodeScreen: %v", err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("rich round trip mismatch:\n got %#v\nwant %#v", got, s)
	}
	// mouseProto nibble helpers
	if MouseTrack(0x24) != MouseTrackAny || MouseEnc(0x24) != MouseEncSGR {
		t.Fatalf("mouseProto nibble helpers wrong for 0x24")
	}
}

func TestScreenEscapeSafetyRejectedOnEncode(t *testing.T) {
	bad := []struct {
		name string
		r    []byte
	}{
		{"ESC", []byte{0x1B}},
		{"C0 NUL", []byte{0x00}},
		{"DEL", []byte{0x7F}},
		{"C1", []byte{0xC2, 0x85}},  // U+0085 NEL, valid UTF-8, C1 control -> reject
		{"bad UTF-8", []byte{0xFF}}, // invalid encoding
	}
	for _, c := range bad {
		s := Screen{
			Cols: 1, Rows: 1,
			EchoMode:     EchoOn,
			ActiveBuffer: BufferPrimary,
			Primary:      []Cell{{RuneBytes: c.r, FG: DefaultColor, BG: DefaultColor}},
		}
		if _, err := s.Encode(); !IsFramingErr(err) {
			t.Fatalf("%s: encode must be a framing error, got %v", c.name, err)
		}
	}
	// A legitimate multibyte printable rune (é) must be accepted.
	s := Screen{
		Cols: 1, Rows: 1, EchoMode: EchoOn, ActiveBuffer: BufferPrimary,
		Primary: []Cell{{RuneBytes: []byte("é"), FG: DefaultColor, BG: DefaultColor}},
	}
	if _, err := s.Encode(); err != nil {
		t.Fatalf("printable multibyte rune must encode: %v", err)
	}
}

func TestScreenReservedBitsRejected(t *testing.T) {
	base := func() Screen {
		return Screen{
			Cols: 1, Rows: 1, EchoMode: EchoOn, ActiveBuffer: BufferPrimary,
			Primary: []Cell{{RuneBytes: []byte("x"), FG: DefaultColor, BG: DefaultColor}},
		}
	}
	// reserved modes bit
	s := base()
	s.Modes = 0x20
	if _, err := s.Encode(); !IsFramingErr(err) {
		t.Fatalf("reserved modes bit must be rejected, got %v", err)
	}
	// mouseProto set with tracking disabled
	s = base()
	s.MouseProto = 0x24
	if _, err := s.Encode(); !IsFramingErr(err) {
		t.Fatalf("mouseProto without tracking must be rejected, got %v", err)
	}
	// reserved style bit
	s = base()
	s.Primary[0].Style = StyleReserved
	if _, err := s.Encode(); !IsFramingErr(err) {
		t.Fatalf("reserved style bit must be rejected, got %v", err)
	}
	// invalid cursor shape
	s = base()
	s.CursorShape = 0x04
	if _, err := s.Encode(); !IsFramingErr(err) {
		t.Fatalf("invalid cursorShape must be rejected, got %v", err)
	}
}

func TestScreenContinuationCellInvariant(t *testing.T) {
	// runeLen == 0 without the continuation flag is illegal.
	s := Screen{
		Cols: 1, Rows: 1, EchoMode: EchoOn, ActiveBuffer: BufferPrimary,
		Primary: []Cell{{RuneBytes: nil, Style: 0, FG: DefaultColor, BG: DefaultColor}},
	}
	if _, err := s.Encode(); !IsFramingErr(err) {
		t.Fatalf("runeLen==0 non-continuation must be rejected, got %v", err)
	}
	// continuation flag WITH a rune is illegal.
	s.Primary[0] = Cell{RuneBytes: []byte("x"), Style: StyleWideContinuation, FG: DefaultColor, BG: DefaultColor}
	if _, err := s.Encode(); !IsFramingErr(err) {
		t.Fatalf("continuation with runeLen!=0 must be rejected, got %v", err)
	}
}

func TestScreenGridSizeMismatchOnEncode(t *testing.T) {
	s := Screen{
		Cols: 2, Rows: 2, EchoMode: EchoOn, ActiveBuffer: BufferPrimary,
		Primary: []Cell{{RuneBytes: []byte("x"), FG: DefaultColor, BG: DefaultColor}}, // only 1 of 4
	}
	if _, err := s.Encode(); err == nil {
		t.Fatalf("grid size mismatch must error")
	}
}

func TestDecodeScreenNegative(t *testing.T) {
	// Grid dimensions that overflow uint64 on multiply.
	// epoch=0 echo=on cols=2^40 rows=2^40 ... (huge product) -> framing error
	huge := AppendUvarint(nil, 0)         // epoch
	huge = append(huge, EchoOn)           // echoMode
	huge = AppendUvarint(huge, 1<<40)     // cols
	huge = AppendUvarint(huge, 1<<40)     // rows -> product overflows? 2^80 > 2^64 -> yes
	huge = append(huge, BufferPrimary)    // activeBuffer
	huge = AppendUvarint(huge, 0)         // cursorRow
	huge = AppendUvarint(huge, 0)         // cursorCol
	huge = append(huge, 1)                // cursorVisible
	huge = append(huge, CursorShapeBlock) // cursorShape
	huge = append(huge, 0)                // modes
	huge = append(huge, 0)                // mouseProto
	huge = AppendUvarint(huge, 0)         // savedRow
	huge = AppendUvarint(huge, 0)         // savedCol
	if _, err := DecodeScreen(huge); !IsFramingErr(err) {
		t.Fatalf("grid overflow must be a framing error, got %v", err)
	}

	// Truncated grid: says 1x1 but no cell bytes.
	short := AppendUvarint(nil, 0)
	short = append(short, EchoOn)
	short = AppendUvarint(short, 1)
	short = AppendUvarint(short, 1)
	short = append(short, BufferPrimary, 0x00, 0x00, 0x01, CursorShapeBlock, 0x00, 0x00)
	short = AppendUvarint(short, 0) // savedRow
	short = AppendUvarint(short, 0) // savedCol -> grid should follow but buffer ends
	if _, err := DecodeScreen(short); !IsFramingErr(err) {
		t.Fatalf("truncated grid must be a framing error, got %v", err)
	}

	// Invalid boolean byte for cursorVisible (value 2).
	badBool := AppendUvarint(nil, 0)
	badBool = append(badBool, EchoOn)
	badBool = AppendUvarint(badBool, 1)
	badBool = AppendUvarint(badBool, 1)
	badBool = append(badBool, BufferPrimary, 0x00, 0x00, 0x02 /*cursorVisible=2*/, CursorShapeBlock, 0x00, 0x00)
	badBool = AppendUvarint(badBool, 0)
	badBool = AppendUvarint(badBool, 0)
	if _, err := DecodeScreen(badBool); !IsFramingErr(err) {
		t.Fatalf("invalid boolean byte must be a framing error, got %v", err)
	}

	// Unknown color mode (0x03) in a cell.
	unk := AppendUvarint(nil, 0)
	unk = append(unk, EchoOn)
	unk = AppendUvarint(unk, 1)
	unk = AppendUvarint(unk, 1)
	unk = append(unk, BufferPrimary, 0x00, 0x00, 0x01, CursorShapeBlock, 0x00, 0x00)
	unk = AppendUvarint(unk, 0)
	unk = AppendUvarint(unk, 0)
	unk = append(unk, 0x01, 0x41, 0x00, 0x03) // cell: runeLen=1 'A' style=0 fgMode=0x03(unknown)
	if _, err := DecodeScreen(unk); !IsFramingErr(err) {
		t.Fatalf("unknown color mode must be a framing error, got %v", err)
	}
}

// TestScreenPropertyRoundTrip fuzzes random valid screens.
func TestScreenPropertyRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(123))
	glyphs := []string{"a", "Z", "0", " ", "é", "世", "🚀", "~"}
	randColor := func() Color {
		switch rng.Intn(3) {
		case 0:
			return DefaultColor
		case 1:
			return IndexedColor(uint8(rng.Intn(256)))
		default:
			return TrueColor(uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)))
		}
	}
	randCell := func() Cell {
		style := uint8(rng.Intn(64)) & 0x3F // no reserved / no forced continuation here
		style &^= StyleWideContinuation
		return Cell{RuneBytes: []byte(glyphs[rng.Intn(len(glyphs))]), Style: style, FG: randColor(), BG: randColor()}
	}
	for iter := 0; iter < 400; iter++ {
		cols := uint64(rng.Intn(4) + 1)
		rows := uint64(rng.Intn(4) + 1)
		n := cols * rows
		s := Screen{
			Epoch:          rng.Uint64() >> uint(rng.Intn(64)),
			EchoMode:       []uint8{EchoOff, EchoOn, EchoUnknown}[rng.Intn(3)],
			Cols:           cols,
			Rows:           rows,
			ActiveBuffer:   uint8(rng.Intn(2)),
			CursorRow:      uint64(rng.Intn(int(rows))),
			CursorCol:      uint64(rng.Intn(int(cols))),
			CursorVisible:  rng.Intn(2) == 0,
			CursorShape:    uint8(rng.Intn(4)),
			SavedCursorRow: uint64(rng.Intn(int(rows))),
			SavedCursorCol: uint64(rng.Intn(int(cols))),
		}
		for i := uint64(0); i < n; i++ {
			s.Primary = append(s.Primary, randCell())
		}
		if rng.Intn(2) == 0 {
			s.AltPresent = true
			for i := uint64(0); i < n; i++ {
				s.Alt = append(s.Alt, randCell())
			}
		}
		if lines := rng.Intn(3); lines > 0 {
			for l := 0; l < lines; l++ {
				var line []Cell
				for c := rng.Intn(int(cols) + 1); c > 0; c-- {
					line = append(line, randCell())
				}
				s.Scrollback = append(s.Scrollback, line)
			}
		}
		enc, err := s.Encode()
		if err != nil {
			t.Fatalf("iter %d Encode: %v", iter, err)
		}
		got, err := DecodeScreen(enc)
		if err != nil {
			t.Fatalf("iter %d Decode: %v", iter, err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Fatalf("iter %d round trip mismatch:\n got %#v\nwant %#v", iter, got, s)
		}
	}
}
