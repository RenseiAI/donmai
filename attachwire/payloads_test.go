package attachwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestOutputRoundTrip(t *testing.T) {
	for _, data := range [][]byte{nil, {}, []byte("raw pty bytes\x1b[0m"), {0x00, 0xFF}} {
		got := DecodeOutput(EncodeOutput(data))
		if !bytes.Equal(got.Data, data) && !(len(got.Data) == 0 && len(data) == 0) {
			t.Fatalf("Output round trip: got % X want % X", got.Data, data)
		}
	}
}

func TestInputRoundTrip(t *testing.T) {
	cases := []InputPayload{
		{InputSeq: 1, PenGeneration: 0, UserID: nil, Data: []byte("ls -la\r")},        // viewer form (unstamped)
		{InputSeq: 99, PenGeneration: 7, UserID: []byte("u-123"), Data: []byte{0x03}}, // relay-stamped
		{InputSeq: 0, PenGeneration: 0, UserID: nil, Data: nil},                       // all empty
	}
	for _, want := range cases {
		got, err := DecodeInput(want.Encode())
		if err != nil {
			t.Fatalf("DecodeInput: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Input round trip: got %#v want %#v", got, want)
		}
	}
}

func TestInputEmptyUserIDForm(t *testing.T) {
	viewer := EncodeViewerInput(5, 2, []byte("x"))
	got, err := DecodeInput(viewer)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stamped() {
		t.Fatalf("viewer input must be unstamped (userIdLen == 0)")
	}
	stamped := InputPayload{InputSeq: 5, PenGeneration: 2, UserID: []byte("u"), Data: []byte("x")}
	got2, _ := DecodeInput(stamped.Encode())
	if !got2.Stamped() {
		t.Fatalf("stamped input must report Stamped()")
	}
}

func TestInputTruncation(t *testing.T) {
	// userIdLen claims 5 but no bytes follow.
	buf := []byte{0x01, 0x00, 0x05}
	if _, err := DecodeInput(buf); !IsFramingErr(err) {
		t.Fatalf("want framing error, got %v", err)
	}
	// trailing bytes after data.
	valid := InputPayload{InputSeq: 1, Data: []byte("a")}.Encode()
	if _, err := DecodeInput(append(valid, 0xAB)); !IsFramingErr(err) {
		t.Fatalf("trailing bytes want framing error, got %v", err)
	}
}

func TestResizeRoundTripAndValidation(t *testing.T) {
	p := ResizePayload{Cols: 80, Rows: 24, PxWidth: 640, PxHeight: 384}
	enc, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeResize(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("Resize round trip: got %#v want %#v", got, p)
	}
	// pxWidth/pxHeight may be zero.
	if _, err := (ResizePayload{Cols: 1, Rows: 1}).Encode(); err != nil {
		t.Fatalf("1x1 with zero pixels must encode: %v", err)
	}
}

func TestResizeZeroGeometryIsFramingError(t *testing.T) {
	for _, p := range []ResizePayload{
		{Cols: 0, Rows: 24},
		{Cols: 80, Rows: 0},
		{Cols: 0, Rows: 0},
	} {
		if _, err := p.Encode(); !IsFramingErr(err) {
			t.Fatalf("encode %v: want framing error, got %v", p, err)
		}
	}
	// Decode of a wire-form 0xN resize is also a framing error.
	buf := []byte{0x00, 0x18, 0x00, 0x00} // cols=0 rows=24 px=0 px=0
	if _, err := DecodeResize(buf); !IsFramingErr(err) {
		t.Fatalf("decode zero-cols: want framing error, got %v", err)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	for _, label := range []string{"", "checkpoint", "unicode: café ☕"} {
		got, err := DecodeMarker(MarkerPayload{Label: label}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		if got.Label != label {
			t.Fatalf("Marker round trip: got %q want %q", got.Label, label)
		}
	}
}

func TestSnapshotEnvelopeRoundTrip(t *testing.T) {
	e := SnapshotEnvelope{AtSeq: 42, SnapFormat: SnapFormatScreen, Snap: []byte{0x01, 0x02, 0x03}}
	got, err := DecodeSnapshotEnvelope(e.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.AtSeq != e.AtSeq || got.SnapFormat != e.SnapFormat || !bytes.Equal(got.Snap, e.Snap) {
		t.Fatalf("envelope round trip: got %#v want %#v", got, e)
	}
}

func TestControlPayloadRoundTrip(t *testing.T) {
	json := []byte(`{"type":"grab"}`)
	got, err := DecodeControlPayload(EncodeControlPayload(json))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, json) {
		t.Fatalf("control payload round trip: got %s want %s", got, json)
	}
	// truncated json length
	if _, err := DecodeControlPayload([]byte{0x05, 0x7B}); !IsFramingErr(err) {
		t.Fatalf("truncated control payload: want framing error, got %v", err)
	}
}
