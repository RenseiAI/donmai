package attachwire

import (
	"bytes"
	"reflect"
	"testing"
)

// FuzzDecodeFrame exercises the §2 frame decoder. The T10 lane extends this;
// the invariant here is: decoding never panics, and any frame that decodes
// cleanly re-encodes and re-decodes to the same structure (canonical
// idempotence — the header varints may be non-minimal in the raw input, so we
// do NOT assert byte-equality with the seed).
func FuzzDecodeFrame(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x01},
		{0x01, 0x00, 0x00},
		Frame{Type: TypeOutput, Seq: 1, RelTime: 0, Payload: []byte("hi")}.Encode(),
		Frame{Type: TypeInput, Seq: 42, RelTime: 7, Payload: EncodeViewerInput(1, 0, []byte("x"))}.Encode(),
		Frame{Type: TypeControl, Payload: EncodeControlPayload([]byte(`{"type":"grab"}`))}.Encode(),
		Frame{Type: TypeExit, Seq: 9, Payload: NewSignalExit("SIGKILL", 9).Encode()}.Encode(),
		append([]byte{0x2A, 0x01, 0x1A}, fixture26...), // NOT a frame, but interesting bytes
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := DecodeFrame(data)
		if err != nil {
			return
		}
		re, err := DecodeFrame(fr.Encode())
		if err != nil {
			t.Fatalf("re-decode of a valid frame failed: %v", err)
		}
		if re.Type != fr.Type || re.Seq != fr.Seq || re.RelTime != fr.RelTime || !bytes.Equal(re.Payload, fr.Payload) {
			t.Fatalf("frame not canonical-idempotent: %#v vs %#v", fr, re)
		}
	})
}

// FuzzDecodeSnap exercises the snapFormat 0x01 (§12.1) screen decoder. Invariant:
// no panic; a screen that decodes cleanly re-encodes and re-decodes to an equal
// structure; and re-encoded bytes decode identically (escape-safety and reserved
// bits always hold on the re-encode).
func FuzzDecodeSnap(f *testing.F) {
	seeds := [][]byte{
		fixture26,
		{},
		{0x00},
	}
	if rich, err := fixtureScreen().Encode(); err == nil {
		seeds = append(seeds, rich)
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := DecodeScreen(data)
		if err != nil {
			return
		}
		enc, err := s.Encode()
		if err != nil {
			t.Fatalf("re-encode of a decoded screen failed: %v", err)
		}
		s2, err := DecodeScreen(enc)
		if err != nil {
			t.Fatalf("re-decode of a re-encoded screen failed: %v", err)
		}
		if !reflect.DeepEqual(s, s2) {
			t.Fatalf("screen not canonical-idempotent")
		}
	})
}

// FuzzDecodeControl exercises the §7 control-message decoder. Invariant: no
// panic; a message that decodes cleanly re-marshals and re-decodes to the same
// concrete type.
func FuzzDecodeControl(f *testing.F) {
	seeds := []string{
		`{"type":"grab"}`,
		`{"type":"subscribe","sessionId":"s","asRole":"viewer","resumeFrom":null,"resumeEpoch":null}`,
		`{"type":"room_state","state":"degraded","sinceSeq":42}`,
		`{"type":"error","code":"framing","message":"x","retryable":false}`,
		`{"type":"pen_state","holderUserId":null,"holderConnId":null,"penGeneration":0,"future":1}`,
		`{"type":"telepathy"}`,
		`not json`,
		``,
		`{}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := DecodeControl(data)
		if err != nil {
			return
		}
		raw, err := MarshalControl(msg)
		if err != nil {
			t.Fatalf("re-marshal of a decoded control failed: %v", err)
		}
		msg2, err := DecodeControl(raw)
		if err != nil {
			t.Fatalf("re-decode of a re-marshaled control failed: %v", err)
		}
		if msg.ControlType() != msg2.ControlType() {
			t.Fatalf("control type changed across round trip: %q -> %q", msg.ControlType(), msg2.ControlType())
		}
	})
}
