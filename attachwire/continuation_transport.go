package attachwire

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// ContinuationSubprotocol is separate from every legacy attach subprotocol.
const ContinuationSubprotocol = "interactive-continuation-v1"

// ContinuationUploadGrantHeader carries only the request-scoped upload grant.
const ContinuationUploadGrantHeader = "X-Continuation-Upload-Grant"

// ContinuationChunkBytes bounds one opaque checkpoint chunk.
const ContinuationChunkBytes = 256 << 10

// ContinuationChunkJSONLimit includes base64 expansion and bounded metadata.
const ContinuationChunkJSONLimit = 4*((ContinuationChunkBytes+2)/3) + 2048

// ContinuationHello selects one supported opaque continuation schema.
type ContinuationHello struct {
	Schema string `json:"schema"`
}

// ContinuationRequest carries host-only request authority as an optional
// snapshot-request field. It is never a viewer-message DTO and is sent only
// after the active host positively advertises support.
type ContinuationRequest struct {
	RequestID   string `json:"requestId"`
	Schema      string `json:"schema"`
	UploadGrant string `json:"uploadGrant"`
}

// ContinuationChunk uploads a piece of one immutable complete checkpoint.
// A short-lived grant supplies request-scoped authority in a header; payload
// fields never grant authority or carry the grant to viewers.
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
	if err := validateContinuationIdentity(r.Schema, r.RequestID); err != nil {
		return err
	}
	if len(r.UploadGrant) != 43 {
		return fmt.Errorf("attachwire: missing or invalid continuation upload grant")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(r.UploadGrant)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != r.UploadGrant {
		return fmt.Errorf("attachwire: missing or invalid continuation upload grant")
	}
	return nil
}

// Format redacts the grant even when a containing control is formatted.
func (r ContinuationRequest) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "{requestId:%q schema:%q uploadGrant:<redacted>}", r.RequestID, r.Schema)
}

// LogValue prevents structured logging from disclosing the upload grant.
func (r ContinuationRequest) LogValue() slog.Value {
	return slog.GroupValue(slog.String("requestId", r.RequestID), slog.String("schema", r.Schema), slog.String("uploadGrant", "<redacted>"))
}

func validateContinuationIdentity(schema, requestID string) error {
	if schema != ContinuationSchema || len(requestID) == 0 || len(requestID) > 128 {
		return fmt.Errorf("attachwire: unsupported continuation request")
	}
	for _, c := range requestID {
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
	if err := validateContinuationIdentity(c.Schema, c.RequestID); err != nil {
		return err
	}
	if c.Total == 0 || c.Total > MaxContinuationBytes || c.Offset >= c.Total || c.Offset%ContinuationChunkBytes != 0 || !validContinuationDigest(c.SHA256) || uint64(len(c.Data)) != min(uint64(ContinuationChunkBytes), uint64(c.Total-c.Offset)) {
		return fmt.Errorf("attachwire: invalid continuation chunk")
	}
	return nil
}

// Validate checks the immutable checkpoint boundary metadata.
func (a ContinuationAccepted) Validate() error {
	if err := validateContinuationIdentity(a.Schema, a.RequestID); err != nil {
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

// LogValue also protects the grant when the containing host control is logged.
func (r SnapshotRequest) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("type", string(r.ControlType())), slog.String("reason", string(r.Reason))}
	if r.Continuation != nil {
		attrs = append(attrs, slog.Any("continuation", r.Continuation))
	}
	return slog.GroupValue(attrs...)
}
