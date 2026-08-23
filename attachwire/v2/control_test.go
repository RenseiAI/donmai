package attachwirev2

import (
	"bytes"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

type unknownControl struct{}

func (unknownControl) ControlType() attachwire.ControlType { return "future_custom" }

func TestV2VersionAuthorityIsDistinct(t *testing.T) {
	t.Parallel()
	if ProtocolVersion != "interactive-attach-v2" || SubprotocolVersion != ProtocolVersion || VersionPathSegment != "v2" {
		t.Fatalf("v2 version authority = %q/%q/%q", ProtocolVersion, SubprotocolVersion, VersionPathSegment)
	}
	if ProtocolVersion == attachwire.ProtocolVersion {
		t.Fatal("v2 reused the frozen v1 protocol token")
	}
}

func TestInheritedV1ControlBytesAreIdentical(t *testing.T) {
	t.Parallel()
	messages := []attachwire.ControlMessage{
		attachwire.Subscribe{SessionID: "session", AsRole: attachwire.RoleHost, ResumeFrom: nil},
		attachwire.SnapshotRequest{Reason: attachwire.ReasonResync},
		attachwire.Kill{Reason: attachwire.KillStopped},
		attachwire.ControlError{Code: attachwire.CodeFraming, Message: "bad", Retryable: false},
	}
	for _, message := range messages {
		message := message
		t.Run(string(message.ControlType()), func(t *testing.T) {
			t.Parallel()
			v1, err := attachwire.MarshalControl(message)
			if err != nil {
				t.Fatalf("v1 MarshalControl: %v", err)
			}
			v2, err := MarshalControl(message)
			if err != nil {
				t.Fatalf("v2 MarshalControl inherited v1: %v", err)
			}
			if !bytes.Equal(v1, v2) {
				t.Fatalf("inherited v1 bytes changed:\nv1=%s\nv2=%s", v1, v2)
			}
		})
	}
}

func TestV2ControlVocabularyRoundTripsCanonicalStrings(t *testing.T) {
	t.Parallel()
	messages := []attachwire.ControlMessage{
		HostGap{FromSeq: 7, ToSeq: 9, Reason: GapRingEvicted},
		HostGap{FromSeq: 10, ToSeq: 12, Reason: GapControllerUnforwarded},
		CarrierActivate{PTYEpoch: 4, CarrierEpoch: 8},
		CarrierActive{PTYEpoch: 4, CarrierEpoch: 8, AckSeq: 10},
		HostAck{PTYEpoch: 4, CarrierEpoch: 8, AckSeq: 11},
	}
	for _, message := range messages {
		message := message
		t.Run(string(message.ControlType()), func(t *testing.T) {
			t.Parallel()
			frame, err := BuildControlFrame(message)
			if err != nil {
				t.Fatalf("BuildControlFrame: %v", err)
			}
			if frame.Type != attachwire.TypeControl || frame.Seq != 0 || frame.RelTime != 0 {
				t.Fatalf("v2 control header = %+v", frame)
			}
			jsonBytes, err := attachwire.DecodeControlPayload(frame.Payload)
			if err != nil {
				t.Fatalf("DecodeControlPayload: %v", err)
			}
			decoded, err := DecodeControl(jsonBytes)
			if err != nil {
				t.Fatalf("DecodeControl(%s): %v", jsonBytes, err)
			}
			if decoded.ControlType() != message.ControlType() {
				t.Fatalf("round trip type = %q, want %q", decoded.ControlType(), message.ControlType())
			}
			if bytes.Contains(jsonBytes, []byte(`"ackSeq":10`)) || bytes.Contains(jsonBytes, []byte(`"carrierEpoch":8`)) {
				t.Fatalf("uint64 control field encoded as JSON number: %s", jsonBytes)
			}
		})
	}
}

func TestV2ClosedControlsRejectUnknownDuplicateAndNumericFields(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		[]byte(`{"type":"host_ack","ptyEpoch":"1","carrierEpoch":"2","ackSeq":3}`),
		[]byte(`{"type":"host_ack","ptyEpoch":"1","carrierEpoch":"2","ackSeq":"3","extra":true}`),
		[]byte(`{"type":"host_ack","ptyEpoch":"1","carrierEpoch":"2","ackSeq":"3","ackSeq":"3"}`),
		[]byte(`{"type":"host_gap","fromSeq":"01","toSeq":"2","reason":"ring_evicted"}`),
		[]byte(`{"type":"host_gap","fromSeq":"1","toSeq":"2","reason":"unknown"}`),
	}
	for _, raw := range cases {
		if _, err := DecodeControl(raw); !errors.Is(err, ErrMalformedControl) {
			t.Errorf("DecodeControl(%s) error = %v, want ErrMalformedControl", raw, err)
		}
	}
	if _, err := MarshalControl(unknownControl{}); !errors.Is(err, ErrMalformedControl) {
		t.Fatalf("MarshalControl(unknown) error = %v, want ErrMalformedControl", err)
	}
	if _, err := attachwire.DecodeControl([]byte(`{"type":"host_ack","ptyEpoch":"1","carrierEpoch":"2","ackSeq":"3"}`)); !errors.Is(err, attachwire.ErrUnknownControlType) {
		t.Fatalf("frozen v1 decoder accepted v2 host_ack: %v", err)
	}
}
