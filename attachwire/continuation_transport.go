package attachwire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// ContinuationSubprotocol is separate from every legacy attach subprotocol.
const ContinuationSubprotocol = "interactive-continuation-v1"

// ContinuationChunkBytes bounds one opaque checkpoint chunk.
const ContinuationChunkBytes = 256 << 10

// ContinuationChunkJSONLimit includes base64 expansion and bounded metadata.
const ContinuationChunkJSONLimit = 4*((ContinuationChunkBytes+2)/3) + 2048

// ContinuationHello selects one supported opaque continuation schema.
type ContinuationHello struct {
	Schema string `json:"schema"`
}

// ContinuationRequest is optional metadata on an ordinary snapshot request.
// It is sent only after the active host positively advertises support.
type ContinuationRequest struct {
	RequestID string `json:"requestId"`
	Schema    string `json:"schema"`
}

// ContinuationChunk uploads a piece of one immutable complete checkpoint.
// The bearer token supplies host/carrier authority; fields do not grant it.
type ContinuationChunk struct {
	Schema    string `json:"schema"`
	RequestID string `json:"requestId"`
	HostEpoch uint64 `json:"hostEpoch"`
	Offset    uint32 `json:"offset"`
	Total     uint32 `json:"total"`
	SHA256    string `json:"sha256"`
	Data      []byte `json:"data"`
}

// ContinuationChunkAck acknowledges only the exact next upload offset.
type ContinuationChunkAck struct {
	NextOffset uint32 `json:"nextOffset"`
	Complete   bool   `json:"complete"`
}

// ContinuationAccepted is sent only after checkpoint and contiguous tail are
// selected together. The client still must validate and restore before commit.
type ContinuationAccepted struct {
	Schema    string `json:"schema"`
	RequestID string `json:"requestId"`
	HostEpoch uint64 `json:"hostEpoch"`
	AtSeq     uint64 `json:"atSeq"`
	Total     uint32 `json:"total"`
	SHA256    string `json:"sha256"`
}

// ContinuationKind is the closed opaque rail phase registry.
type ContinuationKind uint8

// Opaque continuation rail phase tags.
const (
	ContinuationAcceptedKind        ContinuationKind = 1
	ContinuationCheckpointChunkKind ContinuationKind = 2
	ContinuationHostFrameKind       ContinuationKind = 3
	ContinuationErrorKind           ContinuationKind = 4
)

// ContinuationMessage belongs only to ContinuationSubprotocol. Data is opaque
// checkpoint bytes for Chunk and exact encoded canonical bytes for HostFrame.
// It is never encoded or interpreted as a legacy attach Frame.
type ContinuationMessage struct {
	Kind      ContinuationKind
	Accepted  ContinuationAccepted
	Offset    uint32
	Data      []byte
	ErrorCode string
}

// Validate enforces the exact schema and URL-safe bounded request identity.
func (r ContinuationRequest) Validate() error {
	if r.Schema != ContinuationSchema || len(r.RequestID) == 0 || len(r.RequestID) > 128 {
		return fmt.Errorf("attachwire: unsupported continuation request")
	}
	for _, c := range r.RequestID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Errorf("attachwire: invalid continuation request identity")
		}
	}
	return nil
}

func validContinuationDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Validate checks bounds and complete chunk geometry without allocating state.
func (c ContinuationChunk) Validate() error {
	if err := (ContinuationRequest{RequestID: c.RequestID, Schema: c.Schema}).Validate(); err != nil {
		return err
	}
	if c.Total == 0 || c.Total > MaxContinuationBytes || c.Offset >= c.Total || c.Offset%ContinuationChunkBytes != 0 || !validContinuationDigest(c.SHA256) || uint64(len(c.Data)) != min(uint64(ContinuationChunkBytes), uint64(c.Total-c.Offset)) {
		return fmt.Errorf("attachwire: invalid continuation chunk")
	}
	return nil
}

// Validate checks the immutable checkpoint boundary metadata.
func (a ContinuationAccepted) Validate() error {
	if err := (ContinuationRequest{RequestID: a.RequestID, Schema: a.Schema}).Validate(); err != nil {
		return err
	}
	if a.Total == 0 || a.Total > MaxContinuationBytes || !validContinuationDigest(a.SHA256) {
		return fmt.Errorf("attachwire: invalid accepted checkpoint metadata")
	}
	return nil
}

// DecodeContinuationHello requires a positive explicit schema selection.
func DecodeContinuationHello(b []byte) (ContinuationHello, error) {
	var h ContinuationHello
	if err := decodeContinuationJSON(b, &h, 1024, "schema"); err != nil {
		return h, err
	}
	if h.Schema != ContinuationSchema {
		return h, fmt.Errorf("attachwire: unsupported continuation schema")
	}
	return h, nil
}

// DecodeContinuationChunk rejects missing fields, duplicates and oversized JSON.
func DecodeContinuationChunk(b []byte) (ContinuationChunk, error) {
	var c ContinuationChunk
	if err := decodeContinuationJSON(b, &c, ContinuationChunkJSONLimit, "schema", "requestId", "hostEpoch", "offset", "total", "sha256", "data"); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// EncodeContinuationMessage encodes the versioned rail discriminator followed
// by bounded phase data. It never allocates a canonical host sequence.
func EncodeContinuationMessage(m ContinuationMessage) ([]byte, error) {
	out := []byte{byte(m.Kind)}
	switch m.Kind {
	case ContinuationAcceptedKind:
		if err := m.Accepted.Validate(); err != nil {
			return nil, err
		}
		b, err := json.Marshal(m.Accepted)
		if err != nil {
			return nil, fmt.Errorf("attachwire: encode accepted checkpoint: %w", err)
		}
		return append(out, b...), nil
	case ContinuationCheckpointChunkKind:
		if len(m.Data) == 0 || len(m.Data) > ContinuationChunkBytes || m.Offset >= MaxContinuationBytes || m.Offset%ContinuationChunkBytes != 0 {
			return nil, fmt.Errorf("attachwire: invalid checkpoint chunk phase")
		}
		out = binary.BigEndian.AppendUint32(out, m.Offset)
		return append(out, m.Data...), nil
	case ContinuationHostFrameKind:
		if err := validateContinuationHostFrame(m.Data); err != nil {
			return nil, err
		}
		return append(out, m.Data...), nil
	case ContinuationErrorKind:
		if !validContinuationError(m.ErrorCode) {
			return nil, fmt.Errorf("attachwire: invalid continuation error code")
		}
		return append(out, m.ErrorCode...), nil
	default:
		return nil, fmt.Errorf("attachwire: unknown continuation phase")
	}
}

// DecodeContinuationMessage returns owned phase data. The caller enforces the
// accepted -> complete chunks -> contiguous raw tail state machine.
func DecodeContinuationMessage(b []byte) (ContinuationMessage, error) {
	if len(b) < 2 || len(b) > MaxContinuationBytes+1 {
		return ContinuationMessage{}, fmt.Errorf("attachwire: invalid continuation message size")
	}
	m := ContinuationMessage{Kind: ContinuationKind(b[0])}
	switch m.Kind {
	case ContinuationAcceptedKind:
		if err := decodeContinuationJSON(b[1:], &m.Accepted, 2048, "schema", "requestId", "hostEpoch", "atSeq", "total", "sha256"); err != nil {
			return m, err
		}
	case ContinuationCheckpointChunkKind:
		if len(b) < 6 || len(b) > 5+ContinuationChunkBytes {
			return m, fmt.Errorf("attachwire: short checkpoint chunk")
		}
		m.Offset = binary.BigEndian.Uint32(b[1:5])
		m.Data = append([]byte(nil), b[5:]...)
	case ContinuationHostFrameKind:
		m.Data = append([]byte(nil), b[1:]...)
	case ContinuationErrorKind:
		if len(b) > 33 {
			return m, fmt.Errorf("attachwire: oversized continuation error")
		}
		m.ErrorCode = string(b[1:])
	default:
		return m, fmt.Errorf("attachwire: unknown continuation phase")
	}
	if _, err := EncodeContinuationMessage(m); err != nil {
		return ContinuationMessage{}, err
	}
	return m, nil
}

func validateContinuationHostFrame(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxContinuationBytes {
		return fmt.Errorf("attachwire: invalid opaque host frame size")
	}
	f, err := DecodeFrame(raw)
	if err != nil {
		return err
	}
	if f.Seq == 0 {
		return fmt.Errorf("attachwire: opaque tail must be canonical")
	}
	switch f.Type {
	case TypeOutput, TypeResize, TypeSnapshot, TypeMarker, TypeExit:
		return nil
	default:
		return fmt.Errorf("attachwire: non-host type on opaque tail")
	}
}

func validContinuationError(code string) bool {
	switch code {
	case "unsupported", "unavailable", "timeout", "gap", "invalid", "busy":
		return true
	default:
		return false
	}
}

func decodeContinuationJSON(b []byte, out any, limit int, required ...string) error {
	if len(b) == 0 || len(b) > limit {
		return fmt.Errorf("attachwire: continuation JSON exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("attachwire: continuation JSON must be an object")
	}
	seen := make(map[string]bool, len(required))
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return fmt.Errorf("attachwire: continuation key: %w", err)
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return fmt.Errorf("attachwire: duplicate continuation field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return fmt.Errorf("attachwire: continuation field: %w", err)
		}
		if bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("attachwire: null continuation field")
		}
	}
	if _, err := d.Token(); err != nil {
		return fmt.Errorf("attachwire: continuation object: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("attachwire: trailing continuation JSON")
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("attachwire: missing continuation field %s", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("attachwire: continuation JSON: %w", err)
	}
	return nil
}

// DecodeContinuationChunkAck rejects an incomplete or ambiguous acknowledgement.
func DecodeContinuationChunkAck(b []byte) (ContinuationChunkAck, error) {
	var ack ContinuationChunkAck
	if err := decodeContinuationJSON(b, &ack, 1024, "nextOffset", "complete"); err != nil {
		return ack, err
	}
	if ack.NextOffset > MaxContinuationBytes {
		return ack, fmt.Errorf("attachwire: upload acknowledgement exceeds bound")
	}
	return ack, nil
}
