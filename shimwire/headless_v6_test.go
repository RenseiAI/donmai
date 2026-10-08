package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// TestV6HeadlessVocabularyIsSelectedV6Only pins the additive v6 registry the
// way TestV5CheckpointVocabularyAndBounds pins v5: the three headless types
// are illegal at every selected version below v6 (including v5), legal at v6,
// outside the v1-frozen Known registry, and refused on the plain v1 framer.
// CredentialUpdate carries the controller fence, so it is mutating; the
// result and the terminal observation are not.
func TestV6HeadlessVocabularyIsSelectedV6Only(t *testing.T) {
	t.Parallel()

	for _, mt := range []MessageType{TypeCredentialUpdate, TypeCredentialResult, TypeHeadlessExit} {
		if mt.Known() {
			t.Errorf("%s.Known() = true; it is outside the v1-frozen registry", mt)
		}
		for _, v := range []uint32{V1, V2, V3, V4, V5} {
			if mt.AllowedIn(v) {
				t.Errorf("%s leaked into selected v%d", mt, v)
			}
		}
		if !mt.AllowedIn(V6) {
			t.Errorf("%s missing from selected v6", mt)
		}
		var framed bytes.Buffer
		if err := NewWriter(&framed).Write(mt, []byte{1}); err == nil {
			t.Errorf("plain v1 Write(%s) succeeded, want ErrMalformed", mt)
		}
	}

	if !TypeCredentialUpdate.Mutating() {
		t.Error("TypeCredentialUpdate.Mutating() = false; it carries the controller generation fence")
	}
	for _, mt := range []MessageType{TypeCredentialResult, TypeHeadlessExit} {
		if mt.Mutating() {
			t.Errorf("%s.Mutating() = true; an observation carries no authority", mt)
		}
	}

	for _, tc := range []struct {
		name string
		typ  MessageType
		body func() []byte
	}{
		{"credential update", TypeCredentialUpdate, func() []byte {
			b, err := EncodeCredentialUpdate(CredentialUpdate{Generation: 3, WorkerID: "w-1", Bearer: "bearer-1", ExpiresAt: 1700000000})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
		{"credential result", TypeCredentialResult, func() []byte {
			b, err := EncodeCredentialResult(CredentialResult{Generation: 3})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
		{"headless exit", TypeHeadlessExit, func() []byte {
			b, err := EncodeHeadlessExit(HeadlessExit{Cause: HeadlessExitCompleted, OutboxKey: "sess/1", OutboxState: HeadlessExitDelivered, GroupReaped: true})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var framed bytes.Buffer
			if err := NewWriter(&framed).WriteVersion(V6, tc.typ, tc.body()); err != nil {
				t.Fatalf("v6 WriteVersion(%s): %v", tc.typ, err)
			}
			if _, err := NewReader(bytes.NewReader(framed.Bytes())).ReadVersion(V5); !errors.Is(err, ErrMalformed) {
				t.Fatalf("selected v5 admitted %s: %v", tc.typ, err)
			}
			var replay bytes.Buffer
			if err := NewWriter(&replay).WriteVersion(V6, tc.typ, tc.body()); err != nil {
				t.Fatalf("v6 WriteVersion(%s): %v", tc.typ, err)
			}
			msg, err := NewReader(bytes.NewReader(replay.Bytes())).ReadVersion(V6)
			if err != nil || msg.Type != tc.typ {
				t.Fatalf("v6 ReadVersion(%s) = (%+v,%v)", tc.typ, msg, err)
			}
		})
	}

	// Selected v6 keeps the inherited vocabulary too: v5's checkpoint exchange
	// and the v1-frozen registry stay legal, so the headless connection still
	// speaks Hello/Welcome/Adopted/Stop/Heartbeat/Error framing.
	for _, mt := range []MessageType{TypeHello, TypeCheckpointRequest, TypeCheckpointResult} {
		if !mt.AllowedIn(V6) {
			t.Errorf("%s: AllowedIn(V6) = false, want true", mt)
		}
	}
}

// TestHeadlessProfileRefusesEveryPTYShapedType pins the v6 profile split:
// under the headless profile the live terminal vocabulary (output, input,
// resize, every snapshot shape, host frames, attributed input, checkpoints)
// is refused, while the ownership/fence/liveness/stop/credential/terminal
// set stays legal. The interactive profile refuses nothing — a released
// controller's selected v1–v5 traffic is untouched by the profile existing.
func TestHeadlessProfileRefusesEveryPTYShapedType(t *testing.T) {
	t.Parallel()

	ptyShaped := []MessageType{
		TypeOutput, TypeGap, TypeSnapshot, TypeInput, TypeResize, TypeExit,
		TypeSnapshotRequest, TypeSnapshotResult, TypeHostFrame,
		TypeAttributedInput, TypeCheckpointRequest, TypeCheckpointResult,
	}
	headlessLegal := []MessageType{
		TypeHello, TypeWelcome, TypeAdopted, TypeStop, TypeHeartbeat, TypeError,
		TypeCredentialUpdate, TypeCredentialResult, TypeHeadlessExit,
	}
	for _, mt := range ptyShaped {
		if !mt.RefusedIn(ProfileHeadless, V6) {
			t.Errorf("headless profile admits PTY-shaped %s at v6", mt)
		}
		if mt.RefusedIn(ProfileInteractive, V6) {
			t.Errorf("interactive profile refuses %s at v6", mt)
		}
	}
	for _, mt := range headlessLegal {
		if mt.RefusedIn(ProfileHeadless, V6) {
			t.Errorf("headless profile refuses %s at v6", mt)
		}
	}
	// A headless profile never speaks a pre-v6 selected version: the profile
	// exists only at v6, so refusing everything below it keeps a mis-selected
	// headless connection from degrading into a terminal-shaped one.
	for _, mt := range headlessLegal {
		if !mt.RefusedIn(ProfileHeadless, V5) {
			t.Errorf("headless profile admits %s below v6", mt)
		}
	}
	if Profile("terminal").Known() {
		t.Error(`Profile("terminal").Known() = true; the profile registry is closed`)
	}
	for _, p := range []Profile{ProfileInteractive, ProfileHeadless} {
		if !p.Known() {
			t.Errorf("Profile(%q).Known() = false; it is an assigned profile", p)
		}
	}
}

// TestHelloWorkloadAdvertisesHeadlessExclusively pins the optional extension
// advertisement: a headless Hello carries workload=headless with no new
// top-level field and no caller-map mutation, an interactive Hello omits the
// key, every released decoder still reads both, and a conflicting or unknown
// value fails closed.
func TestHelloWorkloadAdvertisesHeadlessExclusively(t *testing.T) {
	t.Parallel()

	original := map[string]string{ExtCarrierEpoch: "carrier-17"}
	h := Hello{Min: HeadlessMin, Max: HeadlessMax, Workload: ProfileHeadless, Extensions: Extensions{Values: original}}
	raw, err := EncodeHello(h)
	if err != nil {
		t.Fatalf("EncodeHello headless: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["workload"]; ok {
		t.Fatal("headless Hello has a new top-level field; the advertisement must stay in the extension map")
	}
	if _, ok := original[ExtWorkload]; ok {
		t.Fatal("encoding mutated the caller's extension map")
	}
	decoded, err := DecodeHello(raw)
	if err != nil {
		t.Fatalf("DecodeHello headless: %v", err)
	}
	if decoded.Workload != ProfileHeadless {
		t.Fatalf("DecodeHello workload = %q, want headless", decoded.Workload)
	}
	if decoded.Extensions.Values[ExtCarrierEpoch] != "carrier-17" {
		t.Fatal("carrier extension did not survive beside the workload advertisement")
	}
	again, err := EncodeHello(decoded)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("headless Hello round trip: %v", err)
	}

	plain, err := EncodeHello(Hello{Min: V1, Max: V5})
	if err != nil {
		t.Fatalf("EncodeHello interactive: %v", err)
	}
	decodedPlain, err := DecodeHello(plain)
	if err != nil {
		t.Fatalf("DecodeHello interactive: %v", err)
	}
	if decodedPlain.Workload != ProfileInteractive {
		t.Fatalf("omitted workload decoded as %q, want the interactive default", decodedPlain.Workload)
	}
	var plainFields map[string]json.RawMessage
	if err := json.Unmarshal(plain, &plainFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := plainFields["workload"]; ok {
		t.Fatal("interactive Hello advertises a workload key it should omit")
	}

	conflict := h
	conflict.Extensions = Extensions{Values: map[string]string{ExtWorkload: "pty"}}
	if _, err := EncodeHello(conflict); err == nil {
		t.Fatal("conflicting workload sources accepted")
	}
	if _, err := EncodeHello(Hello{Workload: "terminal"}); err == nil {
		t.Fatal("unassigned workload value accepted")
	}
	if _, err := DecodeHello([]byte(`{"extensions":{"values":{"workload":"terminal"}}}`)); err == nil {
		t.Fatal("unassigned workload advertisement accepted")
	}
	if _, err := DecodeHello([]byte(`{"workload":"headless"}`)); err == nil {
		t.Fatal("top-level workload field accepted; released decoders would reject it before selection")
	}
	if err := (Extensions{Required: []string{ExtWorkload}}).CheckRequired(); err == nil {
		t.Fatal("informational workload advertisement became a requirement")
	}
}

// TestHeadlessRangeNeverOverlapsAnInteractivePeer pins the Done-when
// quarantine: a headless shim advertising [6,6] has no overlap with a
// released controller range, so Negotiate fails closed with the version
// mismatch the adoption path classifies as protocol_mismatch — the seat is
// never treated as a terminal session and never ended. The build's own
// advertised ProtocolMax stays at v5 until the launch gate lands, so no peer
// built from this source selects v6 yet.
func TestHeadlessRangeNeverOverlapsAnInteractivePeer(t *testing.T) {
	t.Parallel()

	if HeadlessMin != V6 || HeadlessMax != V6 {
		t.Fatalf("headless range = [%d,%d], want [6,6] exclusively", HeadlessMin, HeadlessMax)
	}
	if ProtocolMax != V5 {
		t.Fatalf("ProtocolMax = %d, want 5: the v6 codec ships before any peer advertises or selects it", ProtocolMax)
	}
	for _, tc := range [][2]uint32{{V1, V1}, {V1, V2}, {V1, V5}, {V1, V3}} {
		if _, err := Negotiate(HeadlessMin, HeadlessMax, tc[0], tc[1]); !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("headless [6,6] vs controller [%d,%d] = %v, want ErrVersionMismatch", tc[0], tc[1], err)
		}
	}
	if selected, err := Negotiate(HeadlessMin, HeadlessMax, V1, V6); err != nil || selected != V6 {
		t.Fatalf("v6-capable overlap = (%d,%v), want selected v6", selected, err)
	}
}

// TestCredentialBodiesValidateTheFenceAndTheSecret pins the strict JSON
// bodies: a CredentialUpdate without a generation, worker id, bearer or
// positive expiry is malformed (an empty bearer must never blank the runner's
// credential), a CredentialResult ack carries no refusal detail while a
// refusal carries exactly one assigned result code, and unknown fields or
// trailing documents are refused like every other control body.
func TestCredentialBodiesValidateTheFenceAndTheSecret(t *testing.T) {
	t.Parallel()

	update := CredentialUpdate{Generation: 9, WorkerID: "worker-2", Bearer: "bearer-2", ExpiresAt: 1700003600}
	body, err := EncodeCredentialUpdate(update)
	if err != nil {
		t.Fatalf("EncodeCredentialUpdate: %v", err)
	}
	got, err := DecodeCredentialUpdate(body)
	if err != nil || got != update {
		t.Fatalf("credential update round trip = (%+v,%v), want %+v", got, err, update)
	}
	for _, bad := range []CredentialUpdate{
		{WorkerID: "w", Bearer: "b", ExpiresAt: 1},
		{Generation: 9, Bearer: "b", ExpiresAt: 1},
		{Generation: 9, WorkerID: "w", ExpiresAt: 1},
		{Generation: 9, WorkerID: "w", Bearer: "b"},
		{Generation: 9, WorkerID: "w", Bearer: "b", ExpiresAt: -1},
	} {
		raw, err := EncodeCredentialUpdate(bad)
		if err != nil {
			t.Fatalf("EncodeCredentialUpdate(%+v): %v", bad, err)
		}
		if _, err := DecodeCredentialUpdate(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeCredentialUpdate(%+v) accepted, want ErrMalformed", bad)
		}
	}

	ack, err := EncodeCredentialResult(CredentialResult{Generation: 9})
	if err != nil {
		t.Fatalf("EncodeCredentialResult ack: %v", err)
	}
	if got, err := DecodeCredentialResult(ack); err != nil || got.Generation != 9 || got.Code != "" {
		t.Fatalf("credential ack round trip = (%+v,%v)", got, err)
	}
	refusal, err := EncodeCredentialResult(CredentialResult{Generation: 9, Code: CodeStaleGeneration, Detail: "resurfaced controller"})
	if err != nil {
		t.Fatalf("EncodeCredentialResult refusal: %v", err)
	}
	if got, err := DecodeCredentialResult(refusal); err != nil || got.Code != CodeStaleGeneration {
		t.Fatalf("credential refusal round trip = (%+v,%v)", got, err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"generation":9,"code":"teapot"}`),
		[]byte(`{"generation":0,"code":"stale_generation"}`),
		[]byte(`{"generation":9,"detail":"without a code"}`),
		[]byte(`{"generation":9,"surprise":1}`),
		[]byte(`{"generation":9}{"generation":9}`),
	} {
		if _, err := DecodeCredentialResult(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeCredentialResult(%s) accepted, want ErrMalformed", raw)
		}
	}
}

// TestHeadlessExitNamesTheOutboxRecord pins the immutable terminal
// observation: every cause in the closed registry round-trips with its outbox
// key, state and group proof, while an unknown cause, an unknown state, a
// missing key, an unknown field or a trailing document is malformed. The key
// is required in both states — pending names what the replacement controller
// replays, delivered names what it must not send again.
func TestHeadlessExitNamesTheOutboxRecord(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		cause HeadlessExitCause
		state HeadlessExitState
	}{
		{HeadlessExitCompleted, HeadlessExitDelivered},
		{HeadlessExitFailed, HeadlessExitPending},
		{HeadlessExitCancelled, HeadlessExitPending},
		{HeadlessExitLostOwnership, HeadlessExitPending},
		{HeadlessExitOrphaned, HeadlessExitPending},
		{HeadlessExitWorkerGone, HeadlessExitPending},
	} {
		want := HeadlessExit{ExitCode: 3, Signal: "SIGTERM", Cause: tc.cause, OutboxKey: "org/sess/attempt-2", OutboxState: tc.state, GroupReaped: true}
		body, err := EncodeHeadlessExit(want)
		if err != nil {
			t.Fatalf("EncodeHeadlessExit(%s): %v", tc.cause, err)
		}
		got, err := DecodeHeadlessExit(body)
		if err != nil || got != want {
			t.Fatalf("headless exit round trip = (%+v,%v), want %+v", got, err, want)
		}
	}
	for _, raw := range [][]byte{
		[]byte(`{"cause":" vaporized ","outboxKey":"k","outboxState":"pending"}`),
		[]byte(`{"cause":"completed","outboxKey":"k","outboxState":"maybe"}`),
		[]byte(`{"cause":"completed","outboxState":"delivered"}`),
		[]byte(`{"cause":"completed","outboxKey":"k","outboxState":"delivered","surprise":1}`),
		[]byte(`{"cause":"completed","outboxKey":"k","outboxState":"delivered"}{}`),
	} {
		if _, err := DecodeHeadlessExit(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeHeadlessExit(%s) accepted, want ErrMalformed", raw)
		}
	}
	if HeadlessExitCause("vaporized").Known() || HeadlessExitState("maybe").Known() {
		t.Error("unassigned headless cause/state reported known; the registries are closed")
	}
}

// TestSelectedV1ThroughV5TrafficIsByteIdentical pins the other Done-when
// half: every byte the released versions frame — the v1 control registry, the
// v2 snapshot pair, the v3 host frame, the v4 attributed input and the v5
// checkpoint pair — stays legal at exactly the versions that admitted it
// before, and the v6 additions leak into none of them. A released controller
// replaying its recorded traffic against this codec sees identical admission.
func TestSelectedV1ThroughV5TrafficIsByteIdentical(t *testing.T) {
	t.Parallel()

	v1Frozen := []MessageType{
		TypeHello, TypeWelcome, TypeAdopted, TypeOutput, TypeGap, TypeSnapshot,
		TypeInput, TypeResize, TypeStop, TypeHeartbeat, TypeExit, TypeError,
	}
	for _, mt := range v1Frozen {
		for _, v := range []uint32{V1, V2, V3, V4, V5} {
			if !mt.AllowedIn(v) {
				t.Errorf("%s: AllowedIn(v%d) = false; the v1-frozen registry is legal at every released version", mt, v)
			}
		}
	}
	for _, mt := range []MessageType{TypeSnapshotRequest, TypeSnapshotResult} {
		for _, v := range []uint32{V2, V3, V4, V5} {
			if !mt.AllowedIn(v) {
				t.Errorf("%s: AllowedIn(v%d) = false; released admission unchanged", mt, v)
			}
		}
		if mt.AllowedIn(V1) {
			t.Errorf("%s: AllowedIn(v1) = true; selected v1 stays closed", mt)
		}
	}
	if !TypeHostFrame.AllowedIn(V3) || !TypeHostFrame.AllowedIn(V4) || !TypeHostFrame.AllowedIn(V5) {
		t.Error("HostFrame admission changed at v3/v4/v5")
	}
	if TypeHostFrame.AllowedIn(V1) || TypeHostFrame.AllowedIn(V2) {
		t.Error("HostFrame leaked below v3")
	}
	if !TypeAttributedInput.AllowedIn(V4) || !TypeAttributedInput.AllowedIn(V5) {
		t.Error("AttributedInput admission changed at v4/v5")
	}
	if TypeAttributedInput.AllowedIn(V1) || TypeAttributedInput.AllowedIn(V2) || TypeAttributedInput.AllowedIn(V3) {
		t.Error("AttributedInput leaked below v4")
	}
	for _, mt := range []MessageType{TypeCheckpointRequest, TypeCheckpointResult} {
		if !mt.AllowedIn(V5) {
			t.Errorf("%s: AllowedIn(v5) = false; released admission unchanged", mt)
		}
		for _, v := range []uint32{V1, V2, V3, V4} {
			if mt.AllowedIn(v) {
				t.Errorf("%s leaked into v%d", mt, v)
			}
		}
	}
	for _, mt := range []MessageType{TypeCredentialUpdate, TypeCredentialResult, TypeHeadlessExit} {
		for _, v := range []uint32{V1, V2, V3, V4, V5} {
			if mt.AllowedIn(v) {
				t.Errorf("%s leaked into selected v%d; released traffic must not admit it", mt, v)
			}
		}
	}
}
