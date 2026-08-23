package attachwirev2

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/RenseiAI/donmai/attachwire"
)

// The v2-only control registry. Inherited v1 controls retain their original
// concrete types and codec in the parent attachwire package.
const (
	CtrlHostGap         attachwire.ControlType = "host_gap"
	CtrlCarrierActivate attachwire.ControlType = "carrier_activate"
	CtrlCarrierActive   attachwire.ControlType = "carrier_active"
	CtrlHostAck         attachwire.ControlType = "host_ack"
)

// V2-only typed error codes.
const (
	CodeCarrierNotActive        attachwire.ErrorCode = "carrier-not-active"
	CodeCarrierActivationOrder  attachwire.ErrorCode = "carrier-activation-order"
	CodeCarrierProofUnavailable attachwire.ErrorCode = "carrier-proof-unavailable"
	CodeCarrierProofDrift       attachwire.ErrorCode = "carrier-proof-drift"
	CodeCarrierCursorRegression attachwire.ErrorCode = "carrier-cursor-regression"
	CodeHostDurability          attachwire.ErrorCode = "host-durability-unavailable"
	CodeHostFrameConflict       attachwire.ErrorCode = "host-frame-conflict"
)

var (
	// ErrMalformedControl reports a v2 control that is not its exact closed
	// bounded shape.
	ErrMalformedControl = errors.New("attachwire/v2: malformed control")
	// ErrControlMismatch reports an acknowledgement for another exact leg.
	ErrControlMismatch = errors.New("attachwire/v2: control correlation mismatch")
)

// DecimalUint64 is a canonical JSON decimal string. It prevents uint64 host
// cursors and epochs from being rounded by JSON-number consumers.
type DecimalUint64 uint64

// MarshalJSON writes the value as one canonical quoted decimal string.
func (v DecimalUint64) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(v), 10))
}

// UnmarshalJSON accepts only one canonical quoted decimal string.
func (v *DecimalUint64) UnmarshalJSON(data []byte) error {
	if v == nil {
		return fmt.Errorf("%w: nil decimal destination", ErrMalformedControl)
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: uint64 must be a JSON string", ErrMalformedControl)
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != raw {
		return fmt.Errorf("%w: non-canonical uint64 %q", ErrMalformedControl, raw)
	}
	*v = DecimalUint64(n)
	return nil
}

// GapReason is the closed host replay-gap reason set.
type GapReason string

const (
	// GapRingEvicted is the source-compatible ordinary replay-gap disposition.
	GapRingEvicted GapReason = "ring_evicted"
	// GapControllerUnforwarded is reserved to proof-bound N+1..K recovery.
	GapControllerUnforwarded GapReason = "controller_unforwarded"
)

// HostGap declares the exact unavailable shimwire range before its recovery
// Snapshot. It is outside the host sequence namespace.
type HostGap struct {
	Type    attachwire.ControlType `json:"type"`
	FromSeq DecimalUint64          `json:"fromSeq"`
	ToSeq   DecimalUint64          `json:"toSeq"`
	Reason  GapReason              `json:"reason"`
}

// ControlType returns CtrlHostGap.
func (HostGap) ControlType() attachwire.ControlType { return CtrlHostGap }

// CarrierActivate asks the exact prepared candidate leg to become active after
// local adoption publication.
type CarrierActivate struct {
	Type         attachwire.ControlType `json:"type"`
	PTYEpoch     DecimalUint64          `json:"ptyEpoch"`
	CarrierEpoch DecimalUint64          `json:"carrierEpoch"`
}

// ControlType returns CtrlCarrierActivate.
func (CarrierActivate) ControlType() attachwire.ControlType { return CtrlCarrierActivate }

// CarrierActive acknowledges the exact promotion and its contiguous durable
// cursor.
type CarrierActive struct {
	Type         attachwire.ControlType `json:"type"`
	PTYEpoch     DecimalUint64          `json:"ptyEpoch"`
	CarrierEpoch DecimalUint64          `json:"carrierEpoch"`
	AckSeq       DecimalUint64          `json:"ackSeq"`
}

// ControlType returns CtrlCarrierActive.
func (CarrierActive) ControlType() attachwire.ControlType { return CtrlCarrierActive }

// HostAck acknowledges the highest contiguous durable host disposition.
type HostAck struct {
	Type         attachwire.ControlType `json:"type"`
	PTYEpoch     DecimalUint64          `json:"ptyEpoch"`
	CarrierEpoch DecimalUint64          `json:"carrierEpoch"`
	AckSeq       DecimalUint64          `json:"ackSeq"`
}

// ControlType returns CtrlHostAck.
func (HostAck) ControlType() attachwire.ControlType { return CtrlHostAck }

// MarshalControl encodes either an inherited v1 control or a v2-only control,
// stamping its concrete discriminator. V1's encoder and registry are untouched.
func MarshalControl(message attachwire.ControlMessage) ([]byte, error) {
	if inheritedV1Control(message) {
		return attachwire.MarshalControl(message)
	}
	if err := validateV2Control(message); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	discriminator, err := json.Marshal(message.ControlType())
	if err != nil {
		return nil, err
	}
	object["type"] = discriminator
	return json.Marshal(object)
}

// BuildControlFrame wraps a v2 control in the inherited byte-identical binary
// Control frame with seq=0 and rel_time=0.
func BuildControlFrame(message attachwire.ControlMessage) (attachwire.Frame, error) {
	raw, err := MarshalControl(message)
	if err != nil {
		return attachwire.Frame{}, err
	}
	return attachwire.NewControlFrame(attachwire.EncodeControlPayload(raw)), nil
}

// DecodeControl recognizes the four v2 additions and delegates every inherited
// discriminator to the frozen v1 decoder. V2 additions use exact closed object
// shapes: duplicate, unknown, missing, trailing, or non-canonical members fail.
func DecodeControl(data []byte) (attachwire.ControlMessage, error) {
	var discriminator attachwire.ControlType
	var probe struct {
		Type attachwire.ControlType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: type is required", ErrMalformedControl)
	}
	discriminator = probe.Type
	if discriminator != CtrlHostGap && discriminator != CtrlCarrierActivate &&
		discriminator != CtrlCarrierActive && discriminator != CtrlHostAck {
		return attachwire.DecodeControl(data)
	}
	fields, err := strictObject(data)
	if err != nil {
		return nil, err
	}
	switch discriminator {
	case CtrlHostGap:
		var message HostGap
		if err := decodeExact(data, fields, []string{"type", "fromSeq", "toSeq", "reason"}, &message); err != nil {
			return nil, err
		}
		if err := validateV2Control(message); err != nil {
			return nil, err
		}
		return message, nil
	case CtrlCarrierActivate:
		var message CarrierActivate
		if err := decodeExact(data, fields, []string{"type", "ptyEpoch", "carrierEpoch"}, &message); err != nil {
			return nil, err
		}
		if err := validateV2Control(message); err != nil {
			return nil, err
		}
		return message, nil
	case CtrlCarrierActive:
		var message CarrierActive
		if err := decodeExact(data, fields, []string{"type", "ptyEpoch", "carrierEpoch", "ackSeq"}, &message); err != nil {
			return nil, err
		}
		if err := validateV2Control(message); err != nil {
			return nil, err
		}
		return message, nil
	case CtrlHostAck:
		var message HostAck
		if err := decodeExact(data, fields, []string{"type", "ptyEpoch", "carrierEpoch", "ackSeq"}, &message); err != nil {
			return nil, err
		}
		if err := validateV2Control(message); err != nil {
			return nil, err
		}
		return message, nil
	}
	return nil, fmt.Errorf("%w: unknown v2 control", ErrMalformedControl)
}

func validateV2Control(message attachwire.ControlMessage) error {
	if message == nil {
		return fmt.Errorf("%w: nil control", ErrMalformedControl)
	}
	switch typed := message.(type) {
	case HostGap:
		if typed.FromSeq == 0 || typed.ToSeq < typed.FromSeq ||
			(typed.Reason != GapRingEvicted && typed.Reason != GapControllerUnforwarded) {
			return fmt.Errorf("%w: invalid host_gap", ErrMalformedControl)
		}
	case CarrierActivate:
		if typed.CarrierEpoch == 0 {
			return fmt.Errorf("%w: carrier epoch must be positive", ErrMalformedControl)
		}
	case CarrierActive:
		if typed.CarrierEpoch == 0 {
			return fmt.Errorf("%w: carrier epoch must be positive", ErrMalformedControl)
		}
	case HostAck:
		if typed.CarrierEpoch == 0 {
			return fmt.Errorf("%w: carrier epoch must be positive", ErrMalformedControl)
		}
	case *HostGap:
		if typed == nil {
			return fmt.Errorf("%w: nil host_gap", ErrMalformedControl)
		}
		return validateV2Control(*typed)
	case *CarrierActivate:
		if typed == nil {
			return fmt.Errorf("%w: nil carrier_activate", ErrMalformedControl)
		}
		return validateV2Control(*typed)
	case *CarrierActive:
		if typed == nil {
			return fmt.Errorf("%w: nil carrier_active", ErrMalformedControl)
		}
		return validateV2Control(*typed)
	case *HostAck:
		if typed == nil {
			return fmt.Errorf("%w: nil host_ack", ErrMalformedControl)
		}
		return validateV2Control(*typed)
	default:
		return fmt.Errorf("%w: unknown control type %q", ErrMalformedControl, message.ControlType())
	}
	return nil
}

func inheritedV1Control(message attachwire.ControlMessage) bool {
	switch message.(type) {
	case attachwire.Subscribe, *attachwire.Subscribe,
		attachwire.ResumeFrom, *attachwire.ResumeFrom,
		attachwire.SnapshotRequest, *attachwire.SnapshotRequest,
		attachwire.Kill, *attachwire.Kill,
		attachwire.Grab, *attachwire.Grab,
		attachwire.Release, *attachwire.Release,
		attachwire.Presence, *attachwire.Presence,
		attachwire.InputAck, *attachwire.InputAck,
		attachwire.PenGranted, *attachwire.PenGranted,
		attachwire.PenRevoked, *attachwire.PenRevoked,
		attachwire.PenState, *attachwire.PenState,
		attachwire.RoomState, *attachwire.RoomState,
		attachwire.ControlError, *attachwire.ControlError:
		return true
	default:
		return false
	}
}

func strictObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%w: control must be an object", ErrMalformedControl)
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: field name: %v", ErrMalformedControl, err)
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, fmt.Errorf("%w: field name is not a string", ErrMalformedControl)
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate field %q", ErrMalformedControl, name)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: field %q: %v", ErrMalformedControl, name, err)
		}
		fields[name] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("%w: object end: %v", ErrMalformedControl, err)
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, fmt.Errorf("%w: trailing data", ErrMalformedControl)
	}
	return fields, nil
}

func decodeExact(data []byte, fields map[string]json.RawMessage, allowed []string, destination any) error {
	if len(fields) != len(allowed) {
		return fmt.Errorf("%w: field set is not exact", ErrMalformedControl)
	}
	for _, name := range allowed {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("%w: missing field %q", ErrMalformedControl, name)
		}
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedControl, err)
	}
	return nil
}
