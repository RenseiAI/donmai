package sessionshim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"

	"github.com/RenseiAI/donmai/shimwire"
)

const (
	durableAckSchemaVersion = 1
	durableAckSuffix        = ".ack"
	maxDurableAckBytes      = 1 << 10
)

// durableAckCursor is the secret-free, incarnation-bound acknowledgement a
// shim persists after a controller proves an external carrier durably accepted
// every disposition through AckedSeq. It is deliberately separate from the
// frozen v1 discovery Record so released readers keep byte-exact behavior.
type durableAckCursor struct {
	SchemaVersion        int                 `json:"schemaVersion"`
	OrgID                string              `json:"orgId"`
	SessionID            string              `json:"sessionId"`
	ShimID               string              `json:"shimId"`
	ProcessEpoch         uint64              `json:"processEpoch"`
	ControllerGeneration shimwire.Generation `json:"controllerGeneration"`
	AckedSeq             uint64              `json:"ackedSeq"`
}

func (a durableAckCursor) Identity() Identity {
	return Identity{OrgID: a.OrgID, SessionID: a.SessionID}
}

func (a durableAckCursor) validate() error {
	if a.SchemaVersion != durableAckSchemaVersion {
		return fmt.Errorf("sessionshim: durable ack schemaVersion %d, want %d", a.SchemaVersion, durableAckSchemaVersion)
	}
	if err := a.Identity().Validate(); err != nil {
		return err
	}
	if a.ShimID == "" || a.ControllerGeneration == 0 || a.AckedSeq == 0 {
		return errors.New("sessionshim: durable ack is missing shim, generation, or positive cursor")
	}
	return nil
}

func (a durableAckCursor) encode() ([]byte, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: encode durable ack: %w", err)
	}
	if len(raw) > maxDurableAckBytes {
		return nil, fmt.Errorf("sessionshim: durable ack is %d bytes, max %d", len(raw), maxDurableAckBytes)
	}
	return raw, nil
}

func decodeDurableAck(data []byte) (durableAckCursor, error) {
	var ack durableAckCursor
	if len(data) > maxDurableAckBytes {
		return ack, fmt.Errorf("sessionshim: durable ack is %d bytes, max %d", len(data), maxDurableAckBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ack); err != nil {
		return ack, fmt.Errorf("sessionshim: decode durable ack: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ack, errors.New("sessionshim: durable ack has trailing data")
	}
	return ack, ack.validate()
}

func durableAckName(id Identity, shimID string, processEpoch uint64) string {
	correlation := id.Key() + "\x1f" + shimID + "\x1f" + strconv.FormatUint(processEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:]) + durableAckSuffix
}

func (r *Registry) putDurableAck(ack durableAckCursor) error {
	raw, err := ack.encode()
	if err != nil {
		return err
	}
	return r.publish(durableAckName(ack.Identity(), ack.ShimID, ack.ProcessEpoch), raw)
}

func (r *Registry) getDurableAck(rec Record) (durableAckCursor, error) {
	raw, err := r.readEntry(durableAckName(rec.Identity(), rec.ShimID, rec.ProcessEpoch))
	if err != nil {
		return durableAckCursor{}, err
	}
	ack, err := decodeDurableAck(raw)
	if err != nil {
		return durableAckCursor{}, err
	}
	if ack.Identity() != rec.Identity() || ack.ShimID != rec.ShimID || ack.ProcessEpoch != rec.ProcessEpoch {
		return durableAckCursor{}, errors.New("sessionshim: durable ack does not match the live shim incarnation")
	}
	return ack, nil
}

func (r *Registry) removeDurableAck(id Identity, shimID string, processEpoch uint64) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(durableAckName(id, shimID, processEpoch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sessionshim: remove durable ack: %w", err)
	}
	return nil
}
