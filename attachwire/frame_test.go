package attachwire

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Type: TypeOutput, Seq: 1, RelTime: 0, Payload: []byte("hello")},
		{Type: TypeOutput, Seq: 0, RelTime: 0, Payload: nil}, // empty payload
		{Type: TypeInput, Seq: 42, RelTime: 999999, Payload: []byte{0x00, 0x01, 0x02}},
		{Type: TypeControl, Seq: 0, RelTime: 0, Payload: []byte(`{"type":"grab"}`)},
		{Type: TypeExit, Seq: 18446744073709551615, RelTime: 18446744073709551615, Payload: []byte{0xFF}},
	}
	for _, want := range cases {
		enc := want.Encode()
		got, err := DecodeFrame(enc)
		if err != nil {
			t.Fatalf("DecodeFrame(%v): %v", want, err)
		}
		// Normalize nil vs empty payload for comparison.
		if len(want.Payload) == 0 {
			want.Payload = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, want)
		}
	}
}

func TestFrameHeaderLayout(t *testing.T) {
	f := Frame{Type: TypeOutput, Seq: 128, RelTime: 1, Payload: []byte("x")}
	enc := f.Encode()
	// type(0x01) seq(128 -> 0x80 0x01) rel_time(1 -> 0x01) payload('x' -> 0x78)
	want := []byte{0x01, 0x80, 0x01, 0x01, 0x78}
	if !bytes.Equal(enc, want) {
		t.Fatalf("frame layout = % X, want % X", enc, want)
	}
}

func TestDecodeFrameUnknownType(t *testing.T) {
	for _, tb := range []byte{0x00, 0x08, 0x7F, 0xFF} {
		buf := []byte{tb, 0x00, 0x00}
		_, err := DecodeFrame(buf)
		if !IsFramingErr(err) {
			t.Fatalf("type 0x%02X: want framing error, got %v", tb, err)
		}
	}
	// Every known type decodes.
	for _, tb := range []EventType{TypeOutput, TypeInput, TypeResize, TypeMarker, TypeExit, TypeSnapshot, TypeControl} {
		if !tb.Known() {
			t.Fatalf("type 0x%02X should be known", byte(tb))
		}
	}
}

func TestDecodeFrameTruncation(t *testing.T) {
	cases := map[string][]byte{
		"empty":              {},
		"type only, no seq":  {0x01},
		"truncated seq":      {0x01, 0x80},       // continuation with no follow
		"truncated rel_time": {0x01, 0x01, 0x80}, // seq ok, rel_time dangles
	}
	for name, buf := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeFrame(buf)
			if !IsFramingErr(err) {
				t.Fatalf("want framing error, got %v", err)
			}
		})
	}
}

func TestOutOfNamespaceConstructors(t *testing.T) {
	cf := NewControlFrame([]byte("x"))
	if cf.Type != TypeControl || cf.Seq != 0 || cf.RelTime != 0 {
		t.Fatalf("NewControlFrame must zero headers, got %#v", cf)
	}
	rf := NewViewportResizeFrame([]byte("y"))
	if rf.Type != TypeResize || rf.Seq != 0 || rf.RelTime != 0 {
		t.Fatalf("NewViewportResizeFrame must zero headers, got %#v", rf)
	}
}

func TestRequiresZeroedHeaders(t *testing.T) {
	cases := []struct {
		t              EventType
		producedByHost bool
		want           bool
	}{
		{TypeControl, true, true},   // Control always out-of-namespace
		{TypeControl, false, true},  //
		{TypeResize, false, true},   // viewer/relay Resize is out-of-namespace
		{TypeResize, true, false},   // host applied-geometry echo is in-namespace
		{TypeOutput, true, false},   // host output is in-namespace
		{TypeExit, true, false},     //
		{TypeSnapshot, true, false}, //
	}
	for _, c := range cases {
		if got := RequiresZeroedHeaders(c.t, c.producedByHost); got != c.want {
			t.Errorf("RequiresZeroedHeaders(%v, host=%v) = %v, want %v", c.t, c.producedByHost, got, c.want)
		}
	}
}

func TestEventTypeString(t *testing.T) {
	if TypeControl.String() != "Control" {
		t.Fatalf("Control String() = %q", TypeControl.String())
	}
	if got := EventType(0x09).String(); got != "Unknown(0x09)" {
		t.Fatalf("unknown String() = %q", got)
	}
}

// TestFramePropertyRoundTrip fuzzes random valid frames through encode/decode.
func TestFramePropertyRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // G404: non-cryptographic randomness for test data
	known := []EventType{TypeOutput, TypeInput, TypeResize, TypeMarker, TypeExit, TypeSnapshot, TypeControl}
	for i := 0; i < 5000; i++ {
		var payload []byte
		if n := rng.Intn(20); n > 0 {
			payload = make([]byte, n)
			_, _ = rng.Read(payload) // math/rand.Rand.Read never returns a non-nil error
		}
		f := Frame{
			Type:    known[rng.Intn(len(known))],
			Seq:     rng.Uint64() >> uint(rng.Intn(64)),
			RelTime: rng.Uint64() >> uint(rng.Intn(64)),
			Payload: payload,
		}
		got, err := DecodeFrame(f.Encode())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Type != f.Type || got.Seq != f.Seq || got.RelTime != f.RelTime || !bytes.Equal(got.Payload, f.Payload) {
			t.Fatalf("round trip mismatch: got %#v want %#v", got, f)
		}
	}
}
