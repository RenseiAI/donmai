package shimwire

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

// netPipe is one in-memory stream with both ends owned by the test: the
// shim end and the controller end. Cleanup closes both so a stalled peer
// cannot park the test.
func netPipe(t *testing.T) (shim, controller net.Conn) {
	t.Helper()
	shim, controller = net.Pipe()
	t.Cleanup(func() {
		_ = shim.Close()       //nolint:errcheck
		_ = controller.Close() //nolint:errcheck
	})
	return shim, controller
}

// TestV6SpecVectorsPassTheProductionReader is the B1–B3 regression pin: the
// contract's own §3 and §4 vectors framed as wire messages decode through the
// production ReadVersion + typed-decode path. The first version of this codec
// refused every one of them as malformed (strict decodeJSON rejecting the
// contract's own requestId/seq members), so this test fails with exactly the
// review's probe output when the shapes drift from the contract again.
func TestV6SpecVectorsPassTheProductionReader(t *testing.T) {
	t.Parallel()

	vectors := []struct {
		name   string
		typ    MessageType
		raw    string
		decode func(body []byte) error
	}{
		{
			name: "spec §3 update",
			typ:  TypeCredentialUpdate,
			raw:  `{"requestId":7,"generation":3,"workerId":"wrk_9","bearer":"opaque","expiresAt":1760000000000000000}`,
			decode: func(body []byte) error {
				update, err := DecodeCredentialUpdate(body)
				if err != nil {
					return err
				}
				if update.RequestID != 7 || update.Generation != 3 || update.WorkerID != "wrk_9" ||
					update.Bearer != "opaque" || update.ExpiresAt != 1760000000000000000 {
					return errors.New("spec §3 update decoded to the wrong value")
				}
				return nil
			},
		},
		{
			name: "spec §3 result",
			typ:  TypeCredentialResult,
			raw:  `{"requestId":7,"generation":3,"status":"success"}`,
			decode: func(body []byte) error {
				result, err := DecodeCredentialResult(body)
				if err != nil {
					return err
				}
				if result.RequestID != 7 || result.Generation != 3 || result.Status != CredentialSuccess {
					return errors.New("spec §3 result decoded to the wrong value")
				}
				return nil
			},
		},
		{
			name: "spec §4 exit",
			typ:  TypeHeadlessExit,
			raw:  `{"seq":1,"processEpoch":1,"exitCode":0,"signal":"","groupReaped":true,"cause":"completed","outboxKey":"org/sess/attempt-2","outboxState":"delivered","observedAt":1760000000000000000}`,
			decode: func(body []byte) error {
				exit, err := DecodeHeadlessExit(body)
				if err != nil {
					return err
				}
				if exit.Seq != 1 || exit.ProcessEpoch != 1 || exit.Cause != HeadlessExitCompleted ||
					exit.OutboxState != HeadlessExitDelivered || exit.ObservedAt != 1760000000000000000 {
					return errors.New("spec §4 exit decoded to the wrong value")
				}
				return nil
			},
		},
		{
			name: "spec §4 resume marker",
			typ:  TypeHeadlessExit,
			raw:  `{"seq":1,"processEpoch":1,"exitCode":0,"signal":"","groupReaped":false,"cause":"stopped_for_resume","outboxKey":"","outboxState":"none","observedAt":1760000000000000000}`,
			decode: func(body []byte) error {
				exit, err := DecodeHeadlessExit(body)
				if err != nil {
					return err
				}
				if exit.Cause != HeadlessExitStoppedForResume || exit.OutboxState != HeadlessExitNone {
					return errors.New("spec §4 resume marker decoded to the wrong value")
				}
				return nil
			},
		},
	}
	for _, tc := range vectors {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var framed bytes.Buffer
			if err := NewWriter(&framed).WriteVersion(V6, tc.typ, []byte(tc.raw)); err != nil {
				t.Fatalf("v6 WriteVersion(%s): %v", tc.typ, err)
			}
			msg, err := NewReader(bytes.NewReader(framed.Bytes())).ReadVersion(V6)
			if err != nil {
				t.Fatalf("v6 ReadVersion(%s) = %v, want the contract vector admitted", tc.typ, err)
			}
			if msg.Type != tc.typ {
				t.Fatalf("ReadVersion type = %s, want %s", msg.Type, tc.typ)
			}
			if err := tc.decode(msg.Body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestV6ProfileRefusalAnswersOnTheWire is the F1 enforcement pin: the headless
// profile refusal is not a predicate the tests assert — it is a wire answer.
// Every PTY-shaped type sent to a headless-profile connection is answered with
// Error{code:"malformed"} on the same stream and reported as ErrMalformed to
// the reader, while the legal headless vocabulary passes through untouched.
func TestV6ProfileRefusalAnswersOnTheWire(t *testing.T) {
	t.Parallel()

	ptyShaped := []MessageType{
		TypeOutput, TypeGap, TypeSnapshot, TypeInput, TypeResize, TypeExit,
		TypeSnapshotRequest, TypeSnapshotResult, TypeHostFrame,
		TypeAttributedInput, TypeCheckpointRequest, TypeCheckpointResult,
	}
	for _, mt := range ptyShaped {
		t.Run(mt.String(), func(t *testing.T) {
			t.Parallel()
			shim, controller := netPipe(t)
			shimReader, shimWriter := NewReader(shim), NewWriter(shim)
			controllerReader, controllerWriter := NewReader(controller), NewWriter(controller)

			refused := make(chan error, 1)
			go func() {
				_, err := shimReader.ReadProfileVersion(shimWriter, ProfileHeadless, V6)
				refused <- err
			}()

			body := []byte{0x11, 0x22}
			if mt == TypeInput {
				body = EncodeInput(3, []byte("x"))
			}
			if err := controllerWriter.WriteVersion(V6, mt, body); err != nil {
				t.Fatalf("controller WriteVersion(%s): %v", mt, err)
			}
			answer, err := controllerReader.ReadVersion(V6)
			if err != nil {
				t.Fatalf("controller read of the refusal answer: %v", err)
			}
			if answer.Type != TypeError {
				t.Fatalf("refusal answer type = %s, want Error", answer.Type)
			}
			refusal, err := DecodeError(answer.Body)
			if err != nil {
				t.Fatalf("refusal answer did not decode: %v", err)
			}
			if refusal.Code != CodeMalformed {
				t.Fatalf("refusal answer code = %q, want malformed", refusal.Code)
			}
			select {
			case err := <-refused:
				if !errors.Is(err, ErrMalformed) || !errors.Is(err, ErrProfileRefused) {
					t.Fatalf("profile read = %v, want ErrProfileRefused and ErrMalformed", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("profile read did not report the refusal")
			}
		})
	}

	// The legal headless vocabulary passes the same reader untouched.
	for _, mt := range []MessageType{
		TypeHello, TypeWelcome, TypeAdopted, TypeStop, TypeHeartbeat, TypeError,
		TypeCredentialUpdate, TypeCredentialResult, TypeHeadlessExit,
	} {
		t.Run("legal-"+mt.String(), func(t *testing.T) {
			t.Parallel()
			shim, controller := netPipe(t)
			shimReader, shimWriter := NewReader(shim), NewWriter(shim)
			controllerWriter := NewWriter(controller)

			got := make(chan Message, 1)
			readErr := make(chan error, 1)
			go func() {
				msg, err := shimReader.ReadProfileVersion(shimWriter, ProfileHeadless, V6)
				if err != nil {
					readErr <- err
					return
				}
				got <- msg
			}()

			body := []byte{0x11, 0x22}
			if err := controllerWriter.WriteVersion(V6, mt, body); err != nil {
				t.Fatalf("controller WriteVersion(%s): %v", mt, err)
			}
			select {
			case err := <-readErr:
				t.Fatalf("legal %s refused on the headless profile: %v", mt, err)
			case msg := <-got:
				if msg.Type != mt || !bytes.Equal(msg.Body, body) {
					t.Fatalf("legal %s arrived as (%s,%x)", mt, msg.Type, msg.Body)
				}
			}
		})
	}

	// The interactive profile refuses nothing: a released controller's traffic
	// is untouched by the profile existing.
	t.Run("interactive refuses nothing", func(t *testing.T) {
		t.Parallel()
		shim, controller := netPipe(t)
		shimReader, shimWriter := NewReader(shim), NewWriter(shim)
		controllerWriter := NewWriter(controller)

		got := make(chan Message, 1)
		go func() {
			msg, err := shimReader.ReadProfileVersion(shimWriter, ProfileInteractive, V6)
			if err != nil {
				return
			}
			got <- msg
		}()
		if err := controllerWriter.WriteVersion(V6, TypeOutput, []byte{0x11}); err != nil {
			t.Fatalf("controller WriteVersion(Output): %v", err)
		}
		select {
		case msg := <-got:
			if msg.Type != TypeOutput {
				t.Fatalf("interactive Output arrived as %s", msg.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("interactive Output was refused; the profile must refuse nothing")
		}
	})
}

// TestReleasedV1ThroughV5BytesAreIdentical pins the byte-identity half of the
// Done-when going forward: these are the exact bytes the released codec emits
// for its fixed control bodies (encoding/json marshals struct fields in
// declaration order, so the strings below are the wire). Any future change
// that renames, reorders, adds, or re-encodes a released field goes RED here
// before it can ship as a silent downgrade. That the current tree emits these
// bytes for UNCHANGED code paths is proven by the production diff touching
// only the v6 additions; the goldens pin them against every later slice
// (notably the launch-gate lift).
func TestReleasedV1ThroughV5BytesAreIdentical(t *testing.T) {
	t.Parallel()

	t.Run("hello without workload", func(t *testing.T) {
		t.Parallel()
		body, err := EncodeHello(Hello{
			Protocol: ProtocolName, Min: V1, Max: V5,
			OrgID: "org-1", SessionID: "sess-1", ShimID: "shim-1",
			ProcessEpoch: 3, PID: 4242, ProcessStartedAt: 1700000000,
			Phase: PhaseRunning, Generation: 7, FirstSeq: 10, LastSeq: 99,
		})
		if err != nil {
			t.Fatalf("EncodeHello: %v", err)
		}
		want := `{"protocol":"session-shim-v1","protocolMin":1,"protocolMax":5,"orgId":"org-1","sessionId":"sess-1","shimId":"shim-1","processEpoch":3,"pid":4242,"processStartedAt":1700000000,"phase":"running","generation":7,"firstSeq":10,"lastSeq":99,"extensions":{}}`
		if string(body) != want {
			t.Fatalf("released Hello bytes =\n%s\nwant\n%s", body, want)
		}
	})

	t.Run("welcome", func(t *testing.T) {
		t.Parallel()
		body, err := EncodeWelcome(Welcome{
			Protocol: ProtocolName, Selected: V5, ControllerID: "d-1",
			ProposedGeneration: 8, ResumeFrom: 42,
		})
		if err != nil {
			t.Fatalf("EncodeWelcome: %v", err)
		}
		want := `{"protocol":"session-shim-v1","selected":5,"controllerId":"d-1","proposedGeneration":8,"resumeFrom":42,"extensions":{}}`
		if string(body) != want {
			t.Fatalf("released Welcome bytes =\n%s\nwant\n%s", body, want)
		}
	})

	t.Run("adopted", func(t *testing.T) {
		t.Parallel()
		body, err := EncodeAdopted(Adopted{Generation: 8, Contiguous: true, ReplayFrom: 42, ReplayTo: 99, Phase: PhaseRunning})
		if err != nil {
			t.Fatalf("EncodeAdopted: %v", err)
		}
		want := `{"generation":8,"extensions":{},"contiguous":true,"replayFrom":42,"replayTo":99,"phase":"running"}`
		if string(body) != want {
			t.Fatalf("released Adopted bytes =\n%s\nwant\n%s", body, want)
		}
	})

	t.Run("snapshot request and result framing", func(t *testing.T) {
		t.Parallel()
		req, err := EncodeSnapshotRequest(SnapshotRequest{RequestID: 9, Generation: 8, Mode: SnapshotInspect})
		if err != nil {
			t.Fatalf("EncodeSnapshotRequest: %v", err)
		}
		// Fixed binary header: request id, generation, mode — byte for byte
		// what the released v2 codec frames.
		wantReq := []byte{0, 0, 0, 0, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 8, 1}
		if !bytes.Equal(req, wantReq) {
			t.Fatalf("released SnapshotRequest bytes = %x, want %x", req, wantReq)
		}
		res, err := EncodeSnapshotResult(SnapshotResult{RequestID: 9, Generation: 8, Mode: SnapshotInspect, AtSeq: 12, Bytes: []byte{1, 2, 3}})
		if err != nil {
			t.Fatalf("EncodeSnapshotResult: %v", err)
		}
		if got, err := DecodeSnapshotResult(res); err != nil || got.RequestID != 9 || got.AtSeq != 12 || !bytes.Equal(got.Bytes, []byte{1, 2, 3}) {
			t.Fatalf("released SnapshotResult round trip = (%+v,%v)", got, err)
		}
	})

	t.Run("v6 types never frame below v6", func(t *testing.T) {
		t.Parallel()
		// Even byte-shaped, a v6 body is refused by a released reader: the
		// type byte is outside every released closed vocabulary.
		update, err := EncodeCredentialUpdate(CredentialUpdate{RequestID: 7, Generation: 3, WorkerID: "w", Bearer: "b", ExpiresAt: 1760000000000000000})
		if err != nil {
			t.Fatalf("EncodeCredentialUpdate: %v", err)
		}
		var framed bytes.Buffer
		if err := NewWriter(&framed).WriteVersion(V6, TypeCredentialUpdate, update); err != nil {
			t.Fatalf("v6 WriteVersion: %v", err)
		}
		if _, err := NewReader(bytes.NewReader(framed.Bytes())).ReadVersion(V5); !errors.Is(err, ErrMalformed) {
			t.Fatalf("selected v5 admitted a v6-framed credential update: %v", err)
		}
		if _, err := DecodeCredentialUpdate(update); err != nil {
			t.Fatalf("v6 credential update body refused by its own decoder: %v", err)
		}
	})
}

// TestV6CorpusHeadlessReadFrameLevelMalformedIsNotAProfileRefusal pins the
// split between the two refusals a headless profile read can report. A
// decoded PTY-shaped type is a profile refusal, answered on the wire, after
// which the caller keeps reading. A frame-level defect (zero length, an
// unassigned type) is plain ErrMalformed with no answer, which ends the
// connection exactly as it does on the interactive profile; it must never be
// mistaken for an answered refusal and silently skipped.
func TestV6CorpusHeadlessReadFrameLevelMalformedIsNotAProfileRefusal(t *testing.T) {
	t.Parallel()

	frames := map[string][]byte{
		"zero length":     {0, 0, 0, 0},
		"unassigned type": {0, 0, 0, 3, 0x16, '{', '}'},
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			shim, controller := netPipe(t)
			shimReader, shimWriter := NewReader(shim), NewWriter(shim)

			read := make(chan error, 1)
			go func() {
				_, err := shimReader.ReadProfileVersion(shimWriter, ProfileHeadless, V6)
				read <- err
			}()
			if _, err := controller.Write(frame); err != nil {
				t.Fatalf("write %s frame: %v", name, err)
			}
			select {
			case err := <-read:
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("profile read of a %s frame = %v, want ErrMalformed", name, err)
				}
				if errors.Is(err, ErrProfileRefused) {
					t.Fatalf("profile read of a %s frame = %v, reported as an answered profile refusal", name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("profile read of a %s frame did not return", name)
			}
		})
	}
}
