package shimwire

import (
	"bytes"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

func TestControlBodiesRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("hello", func(t *testing.T) {
		t.Parallel()
		want := Hello{
			Protocol: ProtocolName, Min: 1, Max: 1,
			OrgID: "org-1", SessionID: "sess-1", ShimID: "shim-1",
			ProcessEpoch: 3, PID: 4242, ProcessStartedAt: 1700000000,
			HarnessPID: 4243, HarnessStartedAt: 1700000001,
			WorkareaPath: "/w/a", Phase: PhaseRunning, Generation: 7,
			FirstSeq: 10, LastSeq: 99, OrphanDeadlineUnixNano: 1700000090,
			Extensions: Extensions{Values: map[string]string{ExtCarrierEpoch: "5"}},
		}
		body, err := EncodeHello(want)
		if err != nil {
			t.Fatalf("EncodeHello: %v", err)
		}
		got, err := DecodeHello(body)
		if err != nil {
			t.Fatalf("DecodeHello: %v", err)
		}
		if got.OrgID != want.OrgID || got.SessionID != want.SessionID ||
			got.HarnessPID != want.HarnessPID || got.HarnessStartedAt != want.HarnessStartedAt ||
			got.Generation != want.Generation || got.Phase != want.Phase {
			t.Fatalf("DecodeHello = %+v, want %+v", got, want)
		}
		if v, ok := got.Extensions.Get(ExtCarrierEpoch); !ok || v != "5" {
			t.Fatalf("carrier_epoch extension did not survive: %q %v", v, ok)
		}
	})

	t.Run("welcome and adopted", func(t *testing.T) {
		t.Parallel()
		wBody, err := EncodeWelcome(Welcome{
			Protocol: ProtocolName, Selected: 1, ControllerID: "d-1",
			ProposedGeneration: 8, ResumeFrom: 42,
		})
		if err != nil {
			t.Fatalf("EncodeWelcome: %v", err)
		}
		w, err := DecodeWelcome(wBody)
		if err != nil {
			t.Fatalf("DecodeWelcome: %v", err)
		}
		if w.ProposedGeneration != 8 || w.ResumeFrom != 42 || w.Selected != 1 {
			t.Fatalf("DecodeWelcome = %+v", w)
		}

		aBody, err := EncodeAdopted(Adopted{Generation: 8, Contiguous: true, ReplayFrom: 42, ReplayTo: 99, Phase: PhaseRunning})
		if err != nil {
			t.Fatalf("EncodeAdopted: %v", err)
		}
		a, err := DecodeAdopted(aBody)
		if err != nil {
			t.Fatalf("DecodeAdopted: %v", err)
		}
		if !a.Contiguous || a.Generation != 8 || a.ReplayFrom != 42 {
			t.Fatalf("DecodeAdopted = %+v", a)
		}
	})

	t.Run("heartbeat exit error snapshot resize", func(t *testing.T) {
		t.Parallel()
		hb, _ := EncodeHeartbeat(HeartbeatMsg{Generation: 3, AckedSeq: 77, Phase: PhaseRunning})
		if got, err := DecodeHeartbeat(hb); err != nil || got.AckedSeq != 77 {
			t.Fatalf("heartbeat round trip = %+v, %v", got, err)
		}
		ex, _ := EncodeExit(ExitMsg{Seq: 5, ExitCode: 130, Signal: "SIGINT"})
		if got, err := DecodeExit(ex); err != nil || got.Signal != "SIGINT" || got.ExitCode != 130 {
			t.Fatalf("exit round trip = %+v, %v", got, err)
		}
		er, _ := EncodeError(ErrorMsg{Code: CodeStaleGeneration, Detail: "nope"})
		if got, err := DecodeError(er); err != nil || got.Code != CodeStaleGeneration {
			t.Fatalf("error round trip = %+v, %v", got, err)
		}
		sn, _ := EncodeSnapshot(SnapshotMsg{AtSeq: 12, Screen: []byte{1, 2, 3}})
		if got, err := DecodeSnapshot(sn); err != nil || got.AtSeq != 12 || !bytes.Equal(got.Screen, []byte{1, 2, 3}) {
			t.Fatalf("snapshot round trip = %+v, %v", got, err)
		}
		rz, _ := EncodeResize(ResizeMsg{Generation: 4, Cols: 120, Rows: 40})
		if got, err := DecodeResize(rz); err != nil || got.Cols != 120 || got.Generation != 4 {
			t.Fatalf("resize round trip = %+v, %v", got, err)
		}
	})
}

func TestV3HostFramePreservesExactCanonicalBytesForEveryHostType(t *testing.T) {
	t.Parallel()
	resize, err := (attachwire.ResizePayload{Cols: 120, Rows: 40, PxWidth: 1200, PxHeight: 800}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frames := []attachwire.Frame{
		{Type: attachwire.TypeOutput, Seq: 1, RelTime: 2, Payload: []byte{0x00, 0xff, '\r', '\n'}},
		{Type: attachwire.TypeResize, Seq: 2, RelTime: 3, Payload: resize},
		{Type: attachwire.TypeMarker, Seq: 3, RelTime: 4, Payload: (attachwire.MarkerPayload{Label: "mark"}).Encode()},
		{Type: attachwire.TypeSnapshot, Seq: 4, RelTime: 5, Payload: (attachwire.SnapshotEnvelope{AtSeq: 3, SnapFormat: attachwire.SnapFormatScreen, Snap: []byte{1, 2, 3}}).Encode()},
		{Type: attachwire.TypeExit, Seq: 5, RelTime: 6, Payload: attachwire.NewNormalExit(0).Encode()},
	}
	for _, frame := range frames {
		frame := frame
		t.Run(frame.Type.String(), func(t *testing.T) {
			t.Parallel()
			requestID := uint64(0)
			if frame.Type == attachwire.TypeSnapshot {
				requestID = 77
			}
			want := HostFrame{RequestID: requestID, FrameBytes: frame.Encode()}
			body, err := EncodeHostFrame(want)
			if err != nil {
				t.Fatalf("EncodeHostFrame: %v", err)
			}
			got, err := DecodeHostFrame(body)
			if err != nil || got.RequestID != want.RequestID || !bytes.Equal(got.FrameBytes, want.FrameBytes) {
				t.Fatalf("HostFrame round trip = (%+v,%v)", got, err)
			}
		})
	}
}

func TestV3HostFrameRejectsIllegalTypeSequenceRequestAndEncoding(t *testing.T) {
	t.Parallel()
	valid := attachwire.Frame{Type: attachwire.TypeOutput, Seq: 1, Payload: []byte("x")}.Encode()
	cases := []HostFrame{
		{FrameBytes: attachwire.Frame{Type: attachwire.TypeOutput, Seq: 0, Payload: []byte("x")}.Encode()},
		{RequestID: 9, FrameBytes: valid},
		{FrameBytes: attachwire.Frame{Type: attachwire.TypeInput, Seq: 1, Payload: []byte{1}}.Encode()},
		{FrameBytes: []byte{byte(attachwire.TypeOutput), 0x81, 0x00, 0x00, 'x'}},
	}
	for _, hostFrame := range cases {
		if _, err := EncodeHostFrame(hostFrame); !errors.Is(err, ErrMalformed) {
			t.Errorf("EncodeHostFrame(%+v) = %v, want ErrMalformed", hostFrame, err)
		}
	}
	if _, err := DecodeHostFrame(make([]byte, hostFrameHeaderLen)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeHostFrame(empty) = %v, want ErrMalformed", err)
	}
}

func TestStrictDecodeRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	// Strictness is the mechanical half of "a protocol mismatch is never a silent
	// downgrade": a field this build does not understand is a capability it does
	// not have, and accepting it would hide that.
	if _, err := DecodeHello([]byte(`{"protocol":"session-shim-v1","surpriseField":1}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeHello with unknown field = %v, want ErrMalformed", err)
	}
	if _, err := DecodeWelcome([]byte(`{"protocol":"session-shim-v1","bearerToken":"secret"}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeWelcome with unknown field = %v, want ErrMalformed", err)
	}
}

func TestStrictDecodeRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	// Two concatenated documents in one body would otherwise decode as the first
	// alone, silently discarding whatever the second said.
	if _, err := DecodeAdopted([]byte(`{"generation":1}{"generation":2}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeAdopted with trailing document = %v, want ErrMalformed", err)
	}
}

func TestStrictDecodeProvesEOFAfterFirstDocument(t *testing.T) {
	t.Parallel()
	for _, body := range [][]byte{
		[]byte(`{"generation":1}{"generation":2}`),
		[]byte("{\"generation\":1}\n[]"),
		[]byte("{\"generation\":1}\x00"),
	} {
		if _, err := DecodeAdopted(body); !errors.Is(err, ErrMalformed) {
			t.Fatalf("DecodeAdopted(%q) = %v, want ErrMalformed", body, err)
		}
	}
	if _, err := DecodeAdopted([]byte("{\"generation\":1}\n\t")); err != nil {
		t.Fatalf("trailing whitespace should be legal: %v", err)
	}
}

func TestV2SnapshotBodiesPreserveEveryByteAndValidateCorrelation(t *testing.T) {
	t.Parallel()
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	req := SnapshotRequest{RequestID: 9, Generation: 17, Mode: SnapshotEmit}
	body, err := EncodeSnapshotRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeSnapshotRequest(body); err != nil || got != req {
		t.Fatalf("request round trip = (%+v,%v), want %+v", got, err, req)
	}
	result := SnapshotResult{RequestID: 9, Generation: 17, Mode: SnapshotEmit, AtSeq: 41, InStream: true, Bytes: all}
	body, err = EncodeSnapshotResult(result)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSnapshotResult(body)
	if err != nil || got.RequestID != result.RequestID || got.Generation != result.Generation || got.Mode != result.Mode || got.AtSeq != result.AtSeq || !got.InStream || !bytes.Equal(got.Bytes, all) {
		t.Fatalf("result round trip = (%+v,%v)", got, err)
	}
	for _, bad := range []SnapshotRequest{{Generation: 1, Mode: SnapshotInspect}, {RequestID: 1, Mode: SnapshotInspect}, {RequestID: 1, Generation: 1, Mode: 99}} {
		if _, err := EncodeSnapshotRequest(bad); !errors.Is(err, ErrMalformed) {
			t.Fatalf("EncodeSnapshotRequest(%+v) = %v, want ErrMalformed", bad, err)
		}
	}
}

func TestGapDecodeRejectsUnknownReasonAndInvertedRange(t *testing.T) {
	t.Parallel()

	t.Run("unknown reason", func(t *testing.T) {
		t.Parallel()
		if _, err := DecodeGap([]byte(`{"fromSeq":1,"toSeq":2,"reason":"because"}`)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("DecodeGap unknown reason = %v, want ErrMalformed", err)
		}
	})
	t.Run("inverted range", func(t *testing.T) {
		t.Parallel()
		// An inverted range describes no gap at all; accepting it would let a
		// consumer render a nonsensical loss window as if it were real.
		if _, err := DecodeGap([]byte(`{"fromSeq":9,"toSeq":2,"reason":"ring_evicted"}`)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("DecodeGap inverted range = %v, want ErrMalformed", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		g, err := DecodeGap([]byte(`{"fromSeq":2,"toSeq":9,"reason":"ahead_of_stream"}`))
		if err != nil || g.Reason != GapAheadOfStream || g.FromSeq != 2 || g.ToSeq != 9 {
			t.Fatalf("DecodeGap = %+v, %v", g, err)
		}
	})
}

func TestStopDecodeRejectsUnknownReason(t *testing.T) {
	t.Parallel()

	if _, err := DecodeStop([]byte(`{"generation":1,"reason":"vibes"}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeStop unknown reason = %v, want ErrMalformed", err)
	}
	s, err := DecodeStop([]byte(`{"generation":1,"reason":"policy"}`))
	if err != nil || s.Reason != StopPolicy {
		t.Fatalf("DecodeStop = %+v, %v", s, err)
	}
}

func TestOutputCarriesRawBytesVerbatim(t *testing.T) {
	t.Parallel()

	// Terminal bytes are arbitrary binary. Round-tripping a payload that is
	// invalid UTF-8 and full of control characters proves nothing re-encodes it.
	raw := []byte{0x00, 0x1b, 0x5b, 0x32, 0x4a, 0xff, 0xfe, 0x80, 0x0a}
	body := EncodeOutput(1234, 5678, raw)
	seq, rel, got, err := DecodeOutput(body)
	if err != nil {
		t.Fatalf("DecodeOutput: %v", err)
	}
	if seq != 1234 || rel != 5678 {
		t.Fatalf("DecodeOutput header = (%d, %d), want (1234, 5678)", seq, rel)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("DecodeOutput data = %v, want %v", got, raw)
	}
}

func TestInputCarriesTheFenceInItsHeader(t *testing.T) {
	t.Parallel()

	// The generation rides the header precisely so a shim cannot accept the bytes
	// without also seeing the fence.
	raw := []byte("ls -la\r")
	body := EncodeInput(99, raw)
	gen, got, err := DecodeInput(body)
	if err != nil {
		t.Fatalf("DecodeInput: %v", err)
	}
	if gen != 99 {
		t.Fatalf("DecodeInput generation = %d, want 99", gen)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("DecodeInput data = %q, want %q", got, raw)
	}
}

func TestByteCarryingDecodersRejectShortBodies(t *testing.T) {
	t.Parallel()

	if _, _, _, err := DecodeOutput([]byte{1, 2, 3}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeOutput short body = %v, want ErrMalformed", err)
	}
	if _, _, err := DecodeInput([]byte{1, 2, 3}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("DecodeInput short body = %v, want ErrMalformed", err)
	}
}

func TestClosedRegistriesRejectUnassignedValues(t *testing.T) {
	t.Parallel()

	if Phase("recovering").Known() {
		t.Error(`Phase("recovering").Known() = true; the phase registry is closed`)
	}
	if GapReason("dropped").Known() {
		t.Error(`GapReason("dropped").Known() = true; the reason registry is closed`)
	}
	if StopReason("shrug").Known() {
		t.Error(`StopReason("shrug").Known() = true; the reason registry is closed`)
	}
	if ErrorCode("teapot").Known() {
		t.Error(`ErrorCode("teapot").Known() = true; the code registry is closed`)
	}
	for _, p := range []Phase{PhaseStarting, PhaseRunning, PhaseOrphaned, PhaseExited} {
		if !p.Known() {
			t.Errorf("Phase(%q).Known() = false; it is an assigned v1 phase", p)
		}
	}
	for _, c := range []ErrorCode{
		CodeVersionMismatch, CodeExtensionRequired, CodeMalformed, CodeStaleGeneration,
		CodeGenerationRequired, CodeIdentityMismatch, CodeUnauthenticated,
		CodePhaseUnknown, CodeExited, CodeInternal,
	} {
		if !c.Known() {
			t.Errorf("ErrorCode(%q).Known() = false; it is an assigned v1 code", c)
		}
	}
}
