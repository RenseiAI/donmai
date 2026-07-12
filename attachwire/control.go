package attachwire

import (
	"encoding/json"
	"fmt"
)

// §7 control-plane messages. The SET of message "type" values is v1-frozen
// (adding/removing a message is a protocol-version bump); adding an optional
// FIELD to an existing message is v1-draft, and receivers MUST ignore unknown
// fields (forward-compatibility) — encoding/json does this by default, so decode
// is forward-compatible out of the box.
//
// Arbitration semantics (grab/release/pen_*) are platform-defined; these types
// are normative for ROUTING AND PARSING ONLY. Which leg may legitimately emit
// which message is the §6.3 admission matrix — a caller concern. DecodeControl
// parses a known message regardless of leg; a mis-routed but known message is a
// soft (ignore) case for the router, not a decode error.

// ControlType is the §7 "type" discriminator.
type ControlType string

// The v1-frozen control-message type registry (§7).
const (
	CtrlSubscribe       ControlType = "subscribe"
	CtrlResumeFrom      ControlType = "resume_from"
	CtrlSnapshotRequest ControlType = "snapshot_request"
	CtrlKill            ControlType = "kill"
	CtrlGrab            ControlType = "grab"
	CtrlRelease         ControlType = "release"
	CtrlPresence        ControlType = "presence"
	CtrlInputAck        ControlType = "input_ack"
	CtrlPenGranted      ControlType = "pen_granted"
	CtrlPenRevoked      ControlType = "pen_revoked"
	CtrlPenState        ControlType = "pen_state"
	CtrlRoomState       ControlType = "room_state"
	CtrlError           ControlType = "error"
)

// Role is the §15 role claim / §7 asRole request value. role is a ceiling: a
// connection can never escalate over the wire (§6, §11.1, §15).
type Role string

const (
	RoleHost   Role = "host"
	RoleDriver Role = "driver"
	RoleViewer Role = "viewer"
)

// SnapshotReason is the §7 snapshot_request.reason enum.
type SnapshotReason string

const (
	ReasonJoin         SnapshotReason = "join"
	ReasonResync       SnapshotReason = "resync"
	ReasonRingMiss     SnapshotReason = "ring-miss"
	ReasonBackpressure SnapshotReason = "backpressure"
)

// KillReason is the §7 kill.reason enum.
type KillReason string

const (
	KillStopped KillReason = "stopped"
	KillQuota   KillReason = "quota"
	KillRevoked KillReason = "revoked"
)

// PresenceOp is the §7 presence.op enum.
type PresenceOp string

const (
	PresenceJoin  PresenceOp = "join"
	PresenceLeave PresenceOp = "leave"
	PresenceList  PresenceOp = "list"
)

// RoomStateValue is the §7 room_state.state enum. "degraded" is defined: the
// host leg is currently attached via the degraded SSE+POST carrier (§14).
type RoomStateValue string

const (
	RoomLive             RoomStateValue = "live"
	RoomHostReconnecting RoomStateValue = "host-reconnecting"
	RoomDegraded         RoomStateValue = "degraded"
	RoomHostGone         RoomStateValue = "host-gone"
	RoomEnded            RoomStateValue = "ended"
)

// ControlMessage is any §7 control message. ControlType returns the frozen
// discriminator for the concrete type, independent of the struct's Type field —
// MarshalControl uses it to stamp the correct "type" on the wire.
type ControlMessage interface {
	ControlType() ControlType
}

// Viewport is the §7 subscribe.viewport shape.
type Viewport struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Subscribe joins a room (§7). Epoch is host-legs-only and MUST equal the token
// claim. ResumeFrom/ResumeEpoch are int|null (null ≡ 0 ≡ "no applied history",
// §13). Nil pointers marshal as JSON null.
type Subscribe struct {
	Type        ControlType `json:"type"`
	SessionID   string      `json:"sessionId"`
	AsRole      Role        `json:"asRole"`
	Epoch       *int64      `json:"epoch,omitempty"`
	ResumeFrom  *int64      `json:"resumeFrom"`
	ResumeEpoch *int64      `json:"resumeEpoch"`
	Viewport    *Viewport   `json:"viewport,omitempty"`
}

func (Subscribe) ControlType() ControlType { return CtrlSubscribe }

// ResumeFrom requests replay from a sequence (§7, §13). Epoch is int|null.
type ResumeFrom struct {
	Type  ControlType `json:"type"`
	Seq   int64       `json:"seq"`
	Epoch *int64      `json:"epoch"`
}

func (ResumeFrom) ControlType() ControlType { return CtrlResumeFrom }

// SnapshotRequest asks the host to emit a Snapshot (§7). Relay → host.
type SnapshotRequest struct {
	Type   ControlType    `json:"type"`
	Reason SnapshotReason `json:"reason"`
}

func (SnapshotRequest) ControlType() ControlType { return CtrlSnapshotRequest }

// Kill terminates the session (§7). Relay → host. Signal is str|null (a
// non-null signal is a request, not a mandate — §7).
type Kill struct {
	Type   ControlType `json:"type"`
	Reason KillReason  `json:"reason"`
	Signal *string     `json:"signal"`
}

func (Kill) ControlType() ControlType { return CtrlKill }

// Grab takes the pen (§7). Viewer → relay.
type Grab struct {
	Type ControlType `json:"type"`
}

func (Grab) ControlType() ControlType { return CtrlGrab }

// Release drops the pen (§7). Viewer → relay.
type Release struct {
	Type ControlType `json:"type"`
}

func (Release) ControlType() ControlType { return CtrlRelease }

// PresenceMember is one entry in a presence roster (§7). Identity is a
// CONNECTION (userId, connId), not a bare user (§6.1).
type PresenceMember struct {
	UserID  string `json:"userId"`
	ConnID  string `json:"connId"`
	Role    string `json:"role"`
	Driving bool   `json:"driving"`
}

// Presence is a roster change (§7). Relay → viewers.
type Presence struct {
	Type    ControlType      `json:"type"`
	Op      PresenceOp       `json:"op"`
	Members []PresenceMember `json:"members"`
}

func (Presence) ControlType() ControlType { return CtrlPresence }

// InputAck acks the highest contiguous inputSeq accepted for a connection (§5,
// §7). Relay → viewer.
type InputAck struct {
	Type        ControlType `json:"type"`
	AckInputSeq int64       `json:"ackInputSeq"`
}

func (InputAck) ControlType() ControlType { return CtrlInputAck }

// PenGranted announces the pen was assigned (§7). Relay → all.
type PenGranted struct {
	Type          ControlType `json:"type"`
	UserID        string      `json:"userId"`
	ConnID        string      `json:"connId"`
	PenGeneration int64       `json:"penGeneration"`
}

func (PenGranted) ControlType() ControlType { return CtrlPenGranted }

// PenRevoked announces the pen was lost or went stale (§7). Relay → all.
type PenRevoked struct {
	Type          ControlType `json:"type"`
	UserID        string      `json:"userId"`
	ConnID        string      `json:"connId"`
	PenGeneration int64       `json:"penGeneration"`
}

func (PenRevoked) ControlType() ControlType { return CtrlPenRevoked }

// PenState reports the current pen holder, pushed on join/reconnect (§7).
// Holder fields are str|null (null when no holder).
type PenState struct {
	Type          ControlType `json:"type"`
	HolderUserID  *string     `json:"holderUserId"`
	HolderConnID  *string     `json:"holderConnId"`
	PenGeneration int64       `json:"penGeneration"`
}

func (PenState) ControlType() ControlType { return CtrlPenState }

// RoomState reports host-leg / room lifecycle (§7). Relay → viewers. SinceSeq is
// int|null (the last host seq the relay holds).
type RoomState struct {
	Type     ControlType    `json:"type"`
	State    RoomStateValue `json:"state"`
	SinceSeq *int64         `json:"sinceSeq"`
}

func (RoomState) ControlType() ControlType { return CtrlRoomState }

// ControlError is a typed error (§7). Any → any. The message-type itself is
// frozen; the Code set is v1-draft (see the ErrorCode registry).
type ControlError struct {
	Type      ControlType `json:"type"`
	Code      ErrorCode   `json:"code"`
	Message   string      `json:"message"`
	Retryable bool        `json:"retryable"`
}

func (ControlError) ControlType() ControlType { return CtrlError }

// MarshalControl serializes a control message to its JSON object, stamping the
// correct frozen "type" discriminator (§7) regardless of what the struct's Type
// field held — so callers never have to set it by hand.
func MarshalControl(m ControlMessage) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	tb, err := json.Marshal(m.ControlType())
	if err != nil {
		return nil, err
	}
	obj["type"] = tb
	return json.Marshal(obj)
}

// BuildControlFrame marshals a control message and wraps it in a §2
// out-of-namespace Control frame (seq = 0, rel_time = 0) with the §3.1 Control
// payload framing.
func BuildControlFrame(m ControlMessage) (Frame, error) {
	j, err := MarshalControl(m)
	if err != nil {
		return Frame{}, err
	}
	return NewControlFrame(EncodeControlPayload(j)), nil
}

// DecodeControl parses a §7 control-message JSON object, switching on the "type"
// discriminator and returning the concrete typed message as a ControlMessage.
// Unknown JSON FIELDS are ignored (forward-compatibility, §7). An unrecognized
// "type" VALUE returns ErrUnknownControlType — a soft, forward-compatible case
// the router handles per §6.3, distinct from the hard framing error an unknown
// frame TYPE BYTE triggers (§3). Whether a known message is legitimate on the
// leg it arrived on is the caller's routing concern (§6.3), not a decode error.
func DecodeControl(data []byte) (ControlMessage, error) {
	var disc struct {
		Type ControlType `json:"type"`
	}
	if err := json.Unmarshal(data, &disc); err != nil {
		return nil, fmt.Errorf("attachwire: control message not a JSON object: %w", err)
	}
	switch disc.Type {
	case CtrlSubscribe:
		return unmarshalControl[Subscribe](data)
	case CtrlResumeFrom:
		return unmarshalControl[ResumeFrom](data)
	case CtrlSnapshotRequest:
		return unmarshalControl[SnapshotRequest](data)
	case CtrlKill:
		return unmarshalControl[Kill](data)
	case CtrlGrab:
		return unmarshalControl[Grab](data)
	case CtrlRelease:
		return unmarshalControl[Release](data)
	case CtrlPresence:
		return unmarshalControl[Presence](data)
	case CtrlInputAck:
		return unmarshalControl[InputAck](data)
	case CtrlPenGranted:
		return unmarshalControl[PenGranted](data)
	case CtrlPenRevoked:
		return unmarshalControl[PenRevoked](data)
	case CtrlPenState:
		return unmarshalControl[PenState](data)
	case CtrlRoomState:
		return unmarshalControl[RoomState](data)
	case CtrlError:
		return unmarshalControl[ControlError](data)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownControlType, disc.Type)
	}
}

func unmarshalControl[T ControlMessage](data []byte) (ControlMessage, error) {
	var m T
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("attachwire: decoding %T control message: %w", m, err)
	}
	return m, nil
}
