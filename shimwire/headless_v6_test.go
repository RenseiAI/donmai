package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
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
			b, err := EncodeCredentialUpdate(CredentialUpdate{RequestID: 7, Generation: 3, WorkerID: "w-1", Bearer: "bearer-1", ExpiresAt: 1700000000})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
		{"credential result", TypeCredentialResult, func() []byte {
			b, err := EncodeCredentialResult(CredentialResult{RequestID: 7, Generation: 3, Status: CredentialSuccess})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
		{"headless exit", TypeHeadlessExit, func() []byte {
			b, err := EncodeHeadlessExit(HeadlessExit{Seq: 1, ProcessEpoch: 2, Cause: HeadlessExitCompleted, OutboxKey: "sess/1", OutboxState: HeadlessExitDelivered, GroupReaped: true, ObservedAt: 1700000000})
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
	// The F2 direction the first version of this codec missed: a caller map
	// carrying the headless advertisement beside an interactive field is the
	// same ambiguity, and fails closed the same way.
	mapOnly := Hello{Workload: ProfileInteractive, Extensions: Extensions{Values: map[string]string{ExtWorkload: WorkloadHeadless}}}
	if _, err := EncodeHello(mapOnly); err == nil {
		t.Fatal("headless map entry beside an interactive field accepted")
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
// bodies: a CredentialUpdate carries the nonzero strictly-increasing request
// id, the generation fence, a 1–256 byte worker id, a 1–16384 byte bearer and
// a nonzero Unix-nanosecond expiry (an empty bearer must never blank the
// runner's credential), a CredentialResult carries the correlated request id,
// generation and exactly one assigned status (exited is never used), and
// unknown fields or trailing documents are refused like every other control
// body.
func TestCredentialBodiesValidateTheFenceAndTheSecret(t *testing.T) {
	t.Parallel()

	update := CredentialUpdate{RequestID: 7, Generation: 9, WorkerID: "worker-2", Bearer: "bearer-2", ExpiresAt: 1700003600000000000}
	body, err := EncodeCredentialUpdate(update)
	if err != nil {
		t.Fatalf("EncodeCredentialUpdate: %v", err)
	}
	got, err := DecodeCredentialUpdate(body)
	if err != nil || got != update {
		t.Fatalf("credential update round trip = (%+v,%v), want %+v", got, err, update)
	}
	// The review probe that blocked the first version of this codec: the
	// contract's own §3 vector decodes through the production entry point.
	if _, err := DecodeCredentialUpdate([]byte(`{"requestId":7,"generation":3,"workerId":"wrk_9","bearer":"opaque","expiresAt":1760000000000000000}`)); err != nil {
		t.Fatalf("contract §3 update vector refused: %v", err)
	}
	for _, bad := range []CredentialUpdate{
		{Generation: 9, WorkerID: "w", Bearer: "b", ExpiresAt: 1},
		{RequestID: 7, WorkerID: "w", Bearer: "b", ExpiresAt: 1},
		{RequestID: 7, Generation: 9, Bearer: "b", ExpiresAt: 1},
		{RequestID: 7, Generation: 9, WorkerID: "w", ExpiresAt: 1},
		{RequestID: 7, Generation: 9, WorkerID: "w", Bearer: "b"},
		{RequestID: 7, Generation: 9, WorkerID: "w", Bearer: "b", ExpiresAt: -1},
		{RequestID: 7, Generation: 9, WorkerID: strings.Repeat("w", credentialWorkerIDMax+1), Bearer: "b", ExpiresAt: 1},
		{RequestID: 7, Generation: 9, WorkerID: "w", Bearer: strings.Repeat("b", credentialBearerMax+1), ExpiresAt: 1},
	} {
		if _, err := EncodeCredentialUpdate(bad); err == nil {
			t.Errorf("EncodeCredentialUpdate(%+v) accepted, want ErrMalformed", bad)
		}
		raw, err := encodeJSON(bad)
		if err != nil {
			t.Fatalf("encodeJSON(%+v): %v", bad, err)
		}
		if _, err := DecodeCredentialUpdate(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeCredentialUpdate(%+v) accepted, want ErrMalformed", bad)
		}
	}
	// Bound edges: 1 and the maxima are legal; zero and maxima+1 are not.
	for _, ok := range []CredentialUpdate{
		{RequestID: 1, Generation: 1, WorkerID: "w", Bearer: "b", ExpiresAt: 1},
		{RequestID: 1, Generation: 1, WorkerID: strings.Repeat("w", credentialWorkerIDMax), Bearer: strings.Repeat("b", credentialBearerMax), ExpiresAt: 1},
	} {
		raw, err := EncodeCredentialUpdate(ok)
		if err != nil {
			t.Fatalf("EncodeCredentialUpdate(%+v): %v", ok, err)
		}
		if _, err := DecodeCredentialUpdate(raw); err != nil {
			t.Errorf("DecodeCredentialUpdate(%+v): %v", ok, err)
		}
	}

	for _, status := range []CredentialStatus{
		CredentialSuccess, CredentialMalformed, CredentialStaleGeneration,
		CredentialDuplicateChanged, CredentialRequestLedgerFull, CredentialInternal,
		CredentialRequestMismatch, CredentialTimeout,
	} {
		want := CredentialResult{RequestID: 7, Generation: 9, Status: status}
		raw, err := EncodeCredentialResult(want)
		if err != nil {
			t.Fatalf("EncodeCredentialResult(%s): %v", status, err)
		}
		if got, err := DecodeCredentialResult(raw); err != nil || got != want {
			t.Fatalf("credential result round trip = (%+v,%v), want %+v", got, err, want)
		}
	}
	// The contract's own result vector, and the exited refusal the contract
	// explicitly forbids here (a shim past its terminal observation answers
	// internal, never exited).
	if _, err := DecodeCredentialResult([]byte(`{"requestId":7,"generation":3,"status":"success"}`)); err != nil {
		t.Fatalf("contract §3 result vector refused: %v", err)
	}
	if _, err := DecodeCredentialResult([]byte(`{"requestId":7,"generation":3,"status":"exited"}`)); err == nil {
		t.Fatal("exited credential status accepted; the contract forbids it here")
	}
	if _, err := EncodeCredentialResult(CredentialResult{RequestID: 7, Generation: 9, Status: "exited"}); err == nil {
		t.Fatal("EncodeCredentialResult accepted exited; the contract forbids it here")
	}
	if _, err := EncodeCredentialResult(CredentialResult{Generation: 9, Status: CredentialSuccess}); err == nil {
		t.Fatal("EncodeCredentialResult accepted a missing request id")
	}
	for _, raw := range [][]byte{
		[]byte(`{"requestId":7,"generation":9,"status":"teapot"}`),
		[]byte(`{"requestId":0,"generation":9,"status":"success"}`),
		[]byte(`{"requestId":7,"generation":0,"status":"success"}`),
		[]byte(`{"requestId":7,"generation":9}`),
		[]byte(`{"generation":9,"status":"success"}`),
		[]byte(`{"requestId":7,"generation":9,"status":"success","code":"malformed"}`),
		[]byte(`{"requestId":7,"generation":9,"status":"success","surprise":1}`),
		[]byte(`{"requestId":7,"generation":9,"status":"success"}{"requestId":7,"generation":9,"status":"success"}`),
	} {
		if _, err := DecodeCredentialResult(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeCredentialResult(%s) accepted, want ErrMalformed", raw)
		}
	}
	if CredentialStatus("exited").Known() || CredentialStatus("teapot").Known() {
		t.Error("unassigned credential status reported known; the registry is closed")
	}
}

// TestHeadlessExitNamesTheOutboxRecord pins the immutable terminal
// observation: seq is always 1 with the incarnation and observation instant
// named, every cause in the closed registry round-trips with its outbox key,
// state and group proof, while a non-1 sequence, an unknown cause, an unknown
// state, a missing observation instant, none outside stopped_for_resume, an
// unknown field or a trailing document is malformed. None names no record —
// it marks a resume that wrote no terminal — while delivered and pending both
// require the key: pending names what the replacement controller replays,
// delivered names what it must not send again.
func TestHeadlessExitNamesTheOutboxRecord(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		cause HeadlessExitCause
		state HeadlessExitState
	}{
		{HeadlessExitCompleted, HeadlessExitDelivered},
		{HeadlessExitCompleted, HeadlessExitPending},
		{HeadlessExitOrphaned, HeadlessExitDelivered},
		{HeadlessExitOrphaned, HeadlessExitPending},
		{HeadlessExitStoppedForResume, HeadlessExitNone},
		{HeadlessExitShimFailure, HeadlessExitPending},
	} {
		want := HeadlessExit{Seq: 1, ProcessEpoch: 4, ExitCode: 3, Signal: "SIGTERM", Cause: tc.cause, OutboxKey: "org/sess/attempt-2", OutboxState: tc.state, GroupReaped: true, ObservedAt: 1760000000000000000}
		if tc.state == HeadlessExitNone {
			want.OutboxKey = ""
		}
		body, err := EncodeHeadlessExit(want)
		if err != nil {
			t.Fatalf("EncodeHeadlessExit(%s/%s): %v", tc.cause, tc.state, err)
		}
		got, err := DecodeHeadlessExit(body)
		if err != nil || got != want {
			t.Fatalf("headless exit round trip = (%+v,%v), want %+v", got, err, want)
		}
	}
	// The review probes that blocked the first version of this codec: the
	// contract's own §4 vectors — the terminal observation and the
	// stopped_for_resume resume marker — decode through the production entry
	// point.
	for _, raw := range [][]byte{
		[]byte(`{"seq":1,"processEpoch":1,"exitCode":0,"signal":"","groupReaped":true,"cause":"completed","outboxKey":"org/sess/attempt-2","outboxState":"delivered","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"exitCode":0,"signal":"","groupReaped":false,"cause":"stopped_for_resume","outboxKey":"","outboxState":"none","observedAt":1760000000000000000}`),
	} {
		if _, err := DecodeHeadlessExit(raw); err != nil {
			t.Fatalf("contract §4 exit vector refused: %v", err)
		}
	}
	for _, raw := range [][]byte{
		[]byte(`{"seq":2,"processEpoch":1,"cause":"completed","outboxKey":"k","outboxState":"delivered","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":0,"cause":"completed","outboxKey":"k","outboxState":"delivered","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"completed","outboxKey":"k","outboxState":"delivered"}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"failed","outboxKey":"k","outboxState":"pending","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"cancelled","outboxKey":"k","outboxState":"pending","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"lost_ownership","outboxKey":"k","outboxState":"pending","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"worker_exited_without_result","outboxKey":"k","outboxState":"pending","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"completed","outboxKey":"k","outboxState":"none","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"stopped_for_resume","outboxKey":"k","outboxState":"none","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"stopped_for_resume","outboxKey":"","outboxState":"pending","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"completed","outboxState":"delivered","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"completed","outboxKey":"k","outboxState":"maybe","observedAt":1760000000000000000}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"completed","outboxKey":"k","outboxState":"delivered","observedAt":1760000000000000000,"surprise":1}`),
		[]byte(`{"seq":1,"processEpoch":1,"cause":"completed","outboxKey":"k","outboxState":"delivered","observedAt":1760000000000000000}{}`),
	} {
		if _, err := DecodeHeadlessExit(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeHeadlessExit(%s) accepted, want ErrMalformed", raw)
		}
	}
	if HeadlessExitCause("failed").Known() || HeadlessExitCause("vaporized").Known() ||
		HeadlessExitState("maybe").Known() || !HeadlessExitState("none").Known() {
		t.Error("headless cause/state registries disagree with the contract's closed sets")
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
