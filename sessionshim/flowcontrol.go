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
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
)

const (
	streamFlowSchemaVersion = 1
	streamFlowSuffix        = ".flow"
	maxStreamFlowBytes      = 1 << 10
)

// StreamFlowControl is the shim's published back-pressure state: "my consumer
// is not draining me, so I have stopped reading the harness's terminal".
//
// # WHY IT IS A SIDECAR AND NOT A RECORD FIELD
//
// The §D6 discovery Record is a CLOSED schema decoded with
// DisallowUnknownFields: a shim that wrote a new field into it would be
// REFUSED — and therefore quarantined — by every daemon built before that
// field existed. A degraded-state marker that makes an older controller
// quarantine a live shim is the exact failure class this whole change exists to
// remove, so the state goes beside the record instead of inside it, exactly as
// the durable acknowledgement cursor does. Registry.Scan reads only the record
// suffix, so an older daemon does not see this file at all.
//
// It is incarnation-bound (shim id + process epoch), secret-free, and small.
type StreamFlowControl struct {
	SchemaVersion int    `json:"schemaVersion"`
	OrgID         string `json:"orgId"`
	SessionID     string `json:"sessionId"`
	ShimID        string `json:"shimId"`
	ProcessEpoch  uint64 `json:"processEpoch"`

	// Paused reports that the shim has stopped reading the PTY master because a
	// consumer is saturated. The harness is blocked in write(2), which is what a
	// terminal does to a program that outruns its reader; it is NOT stopped, and
	// it loses nothing.
	Paused bool `json:"paused"`
	// PausedSinceUnixNano is when the current pause began.
	PausedSinceUnixNano int64 `json:"pausedSince,omitempty"`
	// PendingBytes is how much host output is queued for delivery.
	PendingBytes int `json:"pendingBytes"`
	// PauseBoundReached reports that the pause ran its whole bound out and
	// reading resumed with the consumer still saturated — the degraded case.
	PauseBoundReached bool `json:"pauseBoundReached,omitempty"`
	// ObservedAtUnixNano is when this state was published.
	ObservedAtUnixNano int64 `json:"observedAt"`
}

// Identity returns the lifecycle identity this state belongs to.
func (f StreamFlowControl) Identity() Identity {
	return Identity{OrgID: f.OrgID, SessionID: f.SessionID}
}

// PausedSince returns when the pause began, or the zero time.
func (f StreamFlowControl) PausedSince() time.Time {
	if f.PausedSinceUnixNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, f.PausedSinceUnixNano)
}

// Degraded reports whether an operator should be looking at this session: the
// reader is paused, or it was resumed only because the pause bound ran out.
func (f StreamFlowControl) Degraded() bool { return f.Paused || f.PauseBoundReached }

func (f StreamFlowControl) validate() error {
	if f.SchemaVersion != streamFlowSchemaVersion {
		return fmt.Errorf("sessionshim: stream flow schemaVersion %d, want %d",
			f.SchemaVersion, streamFlowSchemaVersion)
	}
	if err := f.Identity().Validate(); err != nil {
		return err
	}
	if f.ShimID == "" {
		return errors.New("sessionshim: stream flow state is missing its shim id")
	}
	if f.ObservedAtUnixNano <= 0 {
		return errors.New("sessionshim: stream flow state is missing observedAt")
	}
	return nil
}

func (f StreamFlowControl) encode() ([]byte, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: encode stream flow state: %w", err)
	}
	if len(raw) > maxStreamFlowBytes {
		return nil, fmt.Errorf("sessionshim: stream flow state is %d bytes, max %d", len(raw), maxStreamFlowBytes)
	}
	return raw, nil
}

func decodeStreamFlow(data []byte) (StreamFlowControl, error) {
	var f StreamFlowControl
	if len(data) > maxStreamFlowBytes {
		return f, fmt.Errorf("sessionshim: stream flow state is %d bytes, max %d", len(data), maxStreamFlowBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return f, fmt.Errorf("sessionshim: decode stream flow state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return f, errors.New("sessionshim: stream flow state has trailing data")
	}
	return f, f.validate()
}

func streamFlowName(id Identity, shimID string, processEpoch uint64) string {
	correlation := id.Key() + "\x1f" + shimID + "\x1f" + strconv.FormatUint(processEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:]) + streamFlowSuffix
}

// PutStreamFlow publishes one incarnation's back-pressure state.
func (r *Registry) PutStreamFlow(state StreamFlowControl) error {
	raw, err := state.encode()
	if err != nil {
		return err
	}
	return r.publish(streamFlowName(state.Identity(), state.ShimID, state.ProcessEpoch), raw)
}

// StreamFlow reads the back-pressure state a live shim incarnation published.
//
// A missing file is reported as the zero value with a nil error: no state
// published means no back-pressure observed, which is the ordinary case and is
// not a diagnostic failure.
func (r *Registry) StreamFlow(id Identity, shimID string, processEpoch uint64) (StreamFlowControl, error) {
	raw, err := r.readEntry(streamFlowName(id, shimID, processEpoch))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StreamFlowControl{}, nil
		}
		return StreamFlowControl{}, err
	}
	state, err := decodeStreamFlow(raw)
	if err != nil {
		return StreamFlowControl{}, err
	}
	if state.Identity() != id || state.ShimID != shimID || state.ProcessEpoch != processEpoch {
		return StreamFlowControl{}, errors.New("sessionshim: stream flow state does not match the live shim incarnation")
	}
	return state, nil
}

// RemoveStreamFlow deletes one incarnation's published back-pressure state.
// Idempotent: both the shim's own teardown and a janitor may reach it.
func (r *Registry) RemoveStreamFlow(id Identity, shimID string, processEpoch uint64) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(streamFlowName(id, shimID, processEpoch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sessionshim: remove stream flow state: %w", err)
	}
	return nil
}

// DefaultOutputFlowControl is the back-pressure configuration a shim installs on
// the PTY host it owns.
//
// A shim is the one ptyhost owner for which unbounded queueing is the wrong
// answer. Its consumer is a REPLACEABLE controller that may be persisting every
// frame through something slow, so "behind" is measured in minutes rather than
// milliseconds; without a gate the queue grows without bound while the ring
// evicts underneath it, and the consumer that eventually catches up is served a
// Gap plus a replay of everything it missed. Stopping the read instead makes the
// harness wait, which costs nothing and loses nothing.
func DefaultOutputFlowControl() ptyhost.OutputFlowControl {
	return ptyhost.OutputFlowControl{
		HighWaterBytes: ptyhost.DefaultOutputHighWaterBytes,
		PauseBound:     ptyhost.DefaultOutputPauseBound,
	}
}
