package attachwire

import (
	"strings"
	"testing"
)

// TestScreenTruncationSweep truncates a valid snap at every interior length and
// asserts each prefix is a framing error — exercising every field's truncation
// branch in DecodeScreen, cell, and color (indexed + truecolor).
func TestScreenTruncationSweep(t *testing.T) {
	// A screen with an indexed cell and a truecolor cell so the sweep hits both
	// color decode paths' truncation branches.
	s := Screen{
		Cols: 2, Rows: 1, EchoMode: EchoOn, ActiveBuffer: BufferPrimary,
		CursorRow: 0, CursorCol: 0, CursorVisible: true, CursorShape: CursorShapeBlock,
		SavedCursorRow: 0, SavedCursorCol: 0,
		Primary: []Cell{
			{RuneBytes: []byte("A"), Style: StyleBold, FG: IndexedColor(3), BG: DefaultColor},
			{RuneBytes: []byte("B"), Style: 0, FG: TrueColor(9, 8, 7), BG: IndexedColor(1)},
		},
		Scrollback: [][]Cell{{{RuneBytes: []byte("z"), FG: DefaultColor, BG: DefaultColor}}},
	}
	enc, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(enc); i++ {
		if _, err := DecodeScreen(enc[:i]); !IsFramingErr(err) {
			t.Fatalf("prefix len %d: want framing error, got %v", i, err)
		}
	}
	if _, err := DecodeScreen(enc); err != nil {
		t.Fatalf("full buffer must decode: %v", err)
	}
	// Trailing byte after a complete screen is a framing error.
	if _, err := DecodeScreen(append(enc, 0x00)); !IsFramingErr(err) {
		t.Fatalf("trailing byte: want framing error, got %v", err)
	}
}

// TestEnvelopeTruncationSweep sweeps the Snapshot envelope decoder.
func TestEnvelopeTruncationSweep(t *testing.T) {
	enc := SnapshotEnvelope{AtSeq: 300, SnapFormat: SnapFormatScreen, Snap: fixture26}.Encode()
	for i := 0; i < len(enc); i++ {
		if _, err := DecodeSnapshotEnvelope(enc[:i]); !IsFramingErr(err) {
			t.Fatalf("prefix len %d: want framing error, got %v", i, err)
		}
	}
}

// TestPayloadTruncationSweep sweeps the structured §3.1 payload decoders.
func TestPayloadTruncationSweep(t *testing.T) {
	input := InputPayload{InputSeq: 300, PenGeneration: 2, UserID: []byte("uid"), Data: []byte("data")}.Encode()
	resize, _ := ResizePayload{Cols: 300, Rows: 24, PxWidth: 1, PxHeight: 2}.Encode()
	marker := MarkerPayload{Label: "checkpoint"}.Encode()
	exit := NewSignalExit("SIGTERM", 15).Encode()

	sweep := func(name string, buf []byte, dec func([]byte) error) {
		for i := 0; i < len(buf); i++ {
			if err := dec(buf[:i]); !IsFramingErr(err) {
				t.Fatalf("%s prefix %d: want framing error, got %v", name, i, err)
			}
		}
		if err := dec(buf); err != nil {
			t.Fatalf("%s full: %v", name, err)
		}
	}
	sweep("input", input, func(b []byte) error { _, e := DecodeInput(b); return e })
	sweep("resize", resize, func(b []byte) error { _, e := DecodeResize(b); return e })
	sweep("marker", marker, func(b []byte) error { _, e := DecodeMarker(b); return e })
	sweep("exit", exit, func(b []byte) error { _, e := DecodeExit(b); return e })
}

func TestOutputAndFrameEncodeForms(t *testing.T) {
	// Package-level EncodeFrame == method form.
	f := Frame{Type: TypeOutput, Seq: 3, Payload: []byte("p")}
	if string(EncodeFrame(f)) != string(f.Encode()) {
		t.Fatalf("EncodeFrame must match Frame.Encode")
	}
	// OutputPayload.Encode method form.
	op := OutputPayload{Data: []byte("abc")}
	if string(op.Encode()) != "abc" {
		t.Fatalf("OutputPayload.Encode mismatch")
	}
}

func TestFramingErrorMessages(t *testing.T) {
	plain := newFraming("boom")
	if !strings.Contains(plain.Error(), "boom") || !strings.Contains(plain.Error(), "framing error") {
		t.Fatalf("plain framing error message = %q", plain.Error())
	}
	wrapped := &FramingError{Reason: "outer", cause: ErrVarintOverflow}
	if !strings.Contains(wrapped.Error(), "outer") || !strings.Contains(wrapped.Error(), "overflow") {
		t.Fatalf("wrapped framing error message = %q", wrapped.Error())
	}
}

func TestEventTypeStringAll(t *testing.T) {
	want := map[EventType]string{
		TypeOutput: "Output", TypeInput: "Input", TypeResize: "Resize",
		TypeMarker: "Marker", TypeExit: "Exit", TypeSnapshot: "Snapshot", TypeControl: "Control",
	}
	for tp, s := range want {
		if tp.String() != s {
			t.Fatalf("%d.String() = %q, want %q", tp, tp.String(), s)
		}
	}
}

func TestInternalHelperBranches(t *testing.T) {
	if min64(3, 5) != 3 || min64(5, 3) != 3 {
		t.Fatalf("min64 wrong")
	}
	if boundedCap(3) != 3 {
		t.Fatalf("boundedCap small wrong")
	}
	if boundedCap(decodePreallocCap+100) != decodePreallocCap {
		t.Fatalf("boundedCap must cap large counts")
	}
}

// TestScreenLargeGridBoundedCap encodes a 32×32 (1024-cell) screen, exercising
// the min64/boundedCap large-count paths on both encode and decode.
func TestScreenLargeGridBoundedCap(t *testing.T) {
	const dim = 32
	cells := make([]Cell, dim*dim)
	for i := range cells {
		cells[i] = Cell{RuneBytes: []byte("."), FG: DefaultColor, BG: DefaultColor}
	}
	s := Screen{
		Cols: dim, Rows: dim, EchoMode: EchoOn, ActiveBuffer: BufferPrimary,
		Primary: cells,
	}
	enc, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeScreen(enc); err != nil {
		t.Fatalf("large grid decode: %v", err)
	}
}
