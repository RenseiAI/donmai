package attachwire

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func i64(v int64) *int64    { return &v }
func strp(s string) *string { return &s }

func TestControlRoundTripAllTypes(t *testing.T) {
	msgs := []ControlMessage{
		Subscribe{Type: CtrlSubscribe, SessionID: "s1", AsRole: RoleViewer, ResumeFrom: nil, ResumeEpoch: nil, Viewport: &Viewport{Cols: 80, Rows: 24}},
		Subscribe{Type: CtrlSubscribe, SessionID: "s1", AsRole: RoleHost, Epoch: i64(3), ResumeFrom: i64(10), ResumeEpoch: i64(3)},
		ResumeFrom{Type: CtrlResumeFrom, Seq: 100, Epoch: i64(2)},
		ResumeFrom{Type: CtrlResumeFrom, Seq: 0, Epoch: nil},
		SnapshotRequest{Type: CtrlSnapshotRequest, Reason: ReasonBackpressure},
		Kill{Type: CtrlKill, Reason: KillRevoked, Signal: strp("SIGTERM")},
		Kill{Type: CtrlKill, Reason: KillStopped, Signal: nil},
		Grab{Type: CtrlGrab},
		Release{Type: CtrlRelease},
		Presence{Type: CtrlPresence, Op: PresenceList, Members: []PresenceMember{{UserID: "u", ConnID: "c", Role: "driver", Driving: true}}},
		InputAck{Type: CtrlInputAck, AckInputSeq: 55},
		PenGranted{Type: CtrlPenGranted, UserID: "u", ConnID: "c", PenGeneration: 4},
		PenRevoked{Type: CtrlPenRevoked, UserID: "u", ConnID: "c", PenGeneration: 5},
		PenState{Type: CtrlPenState, HolderUserID: strp("u"), HolderConnID: strp("c"), PenGeneration: 6},
		PenState{Type: CtrlPenState, HolderUserID: nil, HolderConnID: nil, PenGeneration: 0},
		RoomState{Type: CtrlRoomState, State: RoomDegraded, SinceSeq: i64(42)},
		RoomState{Type: CtrlRoomState, State: RoomEnded, SinceSeq: nil},
		ControlError{Type: CtrlError, Code: CodeBackpressure, Message: "slow consumer", Retryable: false},
	}
	for _, want := range msgs {
		raw, err := MarshalControl(want)
		if err != nil {
			t.Fatalf("MarshalControl(%T): %v", want, err)
		}
		got, err := DecodeControl(raw)
		if err != nil {
			t.Fatalf("DecodeControl(%s): %v", raw, err)
		}
		if got.ControlType() != want.ControlType() {
			t.Fatalf("type mismatch: got %q want %q", got.ControlType(), want.ControlType())
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip %T:\n got %#v\nwant %#v\n json: %s", want, got, want, raw)
		}
	}
}

func TestMarshalControlStampsType(t *testing.T) {
	// A struct whose Type field is unset (or wrong) still marshals with the
	// canonical discriminator.
	raw, err := MarshalControl(Grab{})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != "grab" {
		t.Fatalf("MarshalControl stamped type = %v, want \"grab\"", obj["type"])
	}
}

func TestDecodeControlUnknownType(t *testing.T) {
	_, err := DecodeControl([]byte(`{"type":"telepathy"}`))
	if !errors.Is(err, ErrUnknownControlType) {
		t.Fatalf("want ErrUnknownControlType, got %v", err)
	}
	// An unknown control TYPE is a SOFT (forward-compat) case, NOT a framing error.
	if IsFramingErr(err) {
		t.Fatalf("unknown control type must not be a framing error (§6.3)")
	}
}

func TestDecodeControlIgnoresUnknownFields(t *testing.T) {
	// Forward-compatibility (§7): a draft-added field on a known message is ignored.
	raw := []byte(`{"type":"pen_state","holderUserId":"u","holderConnId":"c","penGeneration":9,"futureField":{"nested":true}}`)
	got, err := DecodeControl(raw)
	if err != nil {
		t.Fatal(err)
	}
	ps, ok := got.(PenState)
	if !ok {
		t.Fatalf("got %T, want PenState", got)
	}
	if ps.PenGeneration != 9 || ps.HolderUserID == nil || *ps.HolderUserID != "u" {
		t.Fatalf("unexpected decode: %#v", ps)
	}
}

func TestDecodeControlNonObject(t *testing.T) {
	if _, err := DecodeControl([]byte(`not json`)); err == nil {
		t.Fatalf("want error for non-JSON control message")
	}
}

func TestSubscribeNullResumeFromMarshalsNull(t *testing.T) {
	raw, err := MarshalControl(Subscribe{Type: CtrlSubscribe, SessionID: "s", AsRole: RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if string(obj["resumeFrom"]) != "null" {
		t.Fatalf("resumeFrom = %s, want null (null ≡ 0 ≡ no history, §13)", obj["resumeFrom"])
	}
	// host-only epoch omitted for a viewer subscribe.
	if _, present := obj["epoch"]; present {
		t.Fatalf("epoch must be omitted on a viewer subscribe")
	}
}

func TestBuildControlFrame(t *testing.T) {
	f, err := BuildControlFrame(Grab{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeControl || f.Seq != 0 || f.RelTime != 0 {
		t.Fatalf("control frame headers not zeroed: %#v", f)
	}
	// Decode the frame -> payload -> control message.
	dec, err := DecodeFrame(f.Encode())
	if err != nil {
		t.Fatal(err)
	}
	jsonBytes, err := DecodeControlPayload(dec.Payload)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := DecodeControl(jsonBytes)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ControlType() != CtrlGrab {
		t.Fatalf("got %q, want grab", msg.ControlType())
	}
}

func TestErrorCodeRegistryComplete(t *testing.T) {
	// The §7 v1 code registry must be exactly these nine values.
	want := []ErrorCode{
		CodeFraming, CodeAuth, CodeRoomMismatch, CodePenDenied, CodeRingMiss,
		CodeBackpressure, CodeRateLimited, CodeEpochStale, CodeInternal,
	}
	wantStr := []string{"framing", "auth", "room-mismatch", "pen-denied", "ring-miss", "backpressure", "rate-limited", "epoch-stale", "internal"}
	for i, c := range want {
		if string(c) != wantStr[i] {
			t.Fatalf("code[%d] = %q, want %q", i, c, wantStr[i])
		}
	}
}
