package shimwire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// CheckpointChunkBytes keeps each v5 message beneath the unchanged1MiB limit.
const (
	CheckpointChunkBytes  = 256 << 10
	checkpointChunkHeader = 57
)

// CheckpointCapability binds the supported schema to the PTY stream epoch.
type CheckpointCapability struct {
	Schema    string `json:"schema"`
	HostEpoch uint64 `json:"hostEpoch"`
}

// CheckpointRequest selects one complete-state schema at a fenced controller.
type CheckpointRequest struct {
	RequestID  uint64
	Generation Generation
	Schema     string
}

// CheckpointResult is a bounded chunk of one immutable checkpoint. Its digest
// covers the entire encoded checkpoint, including schema, PTY epoch and AtSeq.
// Chunks are contiguous, begin at offset0, and cannot change correlation.
type CheckpointResult struct {
	RequestID     uint64
	Generation    Generation
	Total, Offset uint32
	Digest        [32]byte
	Code          ErrorCode
	Bytes         []byte
}

// EncodeCheckpointRequest encodes only the explicitly supported schema.
func EncodeCheckpointRequest(r CheckpointRequest) ([]byte, error) {
	if r.RequestID == 0 || r.Generation == 0 || r.Schema != attachwire.ContinuationSchema {
		return nil, fmt.Errorf("shimwire: %w: invalid checkpoint request", ErrMalformed)
	}
	b := make([]byte, 16, len(r.Schema)+16)
	binary.BigEndian.PutUint64(b, r.RequestID)
	binary.BigEndian.PutUint64(b[8:], uint64(r.Generation))
	return append(b, r.Schema...), nil
}

// DecodeCheckpointRequest rejects unsupported schemas and trailing data.
func DecodeCheckpointRequest(b []byte) (CheckpointRequest, error) {
	if len(b) != 16+len(attachwire.ContinuationSchema) {
		return CheckpointRequest{}, fmt.Errorf("shimwire: %w: checkpoint request length", ErrMalformed)
	}
	r := CheckpointRequest{binary.BigEndian.Uint64(b), Generation(binary.BigEndian.Uint64(b[8:])), string(b[16:])}
	if _, err := EncodeCheckpointRequest(r); err != nil {
		return CheckpointRequest{}, err
	}
	return r, nil
}

// EncodeCheckpointResult validates all lengths before allocation.
func EncodeCheckpointResult(r CheckpointResult) ([]byte, error) {
	code, ok := snapshotResultCodes[r.Code]
	if !ok || r.RequestID == 0 || r.Generation == 0 || r.Total > attachwire.MaxContinuationBytes || r.Offset > r.Total || len(r.Bytes) > CheckpointChunkBytes || uint64(len(r.Bytes)) > uint64(r.Total-r.Offset) {
		return nil, fmt.Errorf("shimwire: %w: invalid checkpoint chunk", ErrMalformed)
	}
	if r.Code != "" {
		if r.Total != 0 || r.Offset != 0 || len(r.Bytes) != 0 || r.Digest != [32]byte{} {
			return nil, fmt.Errorf("shimwire: %w: checkpoint refusal contains state", ErrMalformed)
		}
	} else if r.Total == 0 || len(r.Bytes) == 0 || r.Offset%CheckpointChunkBytes != 0 || uint64(len(r.Bytes)) != min(uint64(CheckpointChunkBytes), uint64(r.Total-r.Offset)) {
		return nil, fmt.Errorf("shimwire: %w: empty checkpoint chunk", ErrMalformed)
	}
	b := make([]byte, checkpointChunkHeader, len(r.Bytes)+checkpointChunkHeader)
	binary.BigEndian.PutUint64(b, r.RequestID)
	binary.BigEndian.PutUint64(b[8:], uint64(r.Generation))
	binary.BigEndian.PutUint32(b[16:], r.Total)
	binary.BigEndian.PutUint32(b[20:], r.Offset)
	copy(b[24:56], r.Digest[:])
	b[56] = code
	return append(b, r.Bytes...), nil
}

// DecodeCheckpointResult returns owned bytes only after bounded validation.
func DecodeCheckpointResult(b []byte) (CheckpointResult, error) {
	if len(b) < checkpointChunkHeader || len(b) > checkpointChunkHeader+CheckpointChunkBytes {
		return CheckpointResult{}, fmt.Errorf("shimwire: %w: checkpoint chunk length", ErrMalformed)
	}
	r := CheckpointResult{RequestID: binary.BigEndian.Uint64(b), Generation: Generation(binary.BigEndian.Uint64(b[8:])), Total: binary.BigEndian.Uint32(b[16:]), Offset: binary.BigEndian.Uint32(b[20:]), Bytes: b[57:]}
	copy(r.Digest[:], b[24:56])
	found := false
	for code, value := range snapshotResultCodes {
		if value == b[56] {
			r.Code = code
			found = true
			break
		}
	}
	if !found {
		return CheckpointResult{}, fmt.Errorf("shimwire: %w: checkpoint refusal code", ErrMalformed)
	}
	if _, err := EncodeCheckpointResult(r); err != nil {
		return CheckpointResult{}, err
	}
	r.Bytes = append([]byte(nil), r.Bytes...)
	return r, nil
}

// UnmarshalJSON requires an explicit host epoch, including when its value is
// zero; absence/null must never be interpreted as the default stream epoch.
func (c *CheckpointCapability) UnmarshalJSON(b []byte) error {
	var fields struct {
		Schema    *string `json:"schema"`
		HostEpoch *uint64 `json:"hostEpoch"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		return fmt.Errorf("shimwire: checkpoint capability: %w", err)
	}
	if fields.Schema == nil || fields.HostEpoch == nil {
		return fmt.Errorf("shimwire: %w: incomplete checkpoint capability", ErrMalformed)
	}
	c.Schema, c.HostEpoch = *fields.Schema, *fields.HostEpoch
	return nil
}
