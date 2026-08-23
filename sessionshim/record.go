package sessionshim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// RecordSchemaVersion is the discovery-record schema version. A reader that
// does not recognise it quarantines rather than guessing at the fields.
const RecordSchemaVersion = 1

// MaxRecordBytes bounds one discovery record on disk. §D6 requires records to be
// BOUNDED; the bound is enforced on read before decoding so a corrupted or
// hostile file cannot drive an unbounded allocation during a startup scan.
const MaxRecordBytes = 8 << 10 // 8 KiB

// ErrRecordInvalid reports a record that fails the §D6 schema contract.
var ErrRecordInvalid = errors.New("sessionshim: invalid discovery record")

// Record is the complete on-disk discovery record. The field set is CLOSED and
// deliberately minimal (§D6).
//
// What is absent is as much a part of the contract as what is present: no
// bearer, no provider credential, no environment snapshot, no prompt, no
// terminal byte, no workarea secret, no serialized adaptation plan. A daemon
// that restarts re-mints carrier and heartbeat credentials from the lifecycle
// identity instead of reading them here, so this file can sit on disk for the
// life of a session without being a secret at rest.
//
// WorkareaPath is the one path-shaped field, and it is here because adoption
// must verify WORKAREA identity as well as process identity: a shim whose
// harness is running against a different workarea than the session's record says
// is exactly the ambiguity §D7 quarantines. It is a location, not a credential.
type Record struct {
	SchemaVersion int `json:"schemaVersion"`

	OrgID     string `json:"orgId"`
	SessionID string `json:"sessionId"`

	ShimID       string `json:"shimId"`
	ProcessEpoch uint64 `json:"processEpoch"`

	PID int `json:"pid"`
	// ProcessStartedAt is the OS-reported start identity in Unix nanoseconds.
	// It is never optional: a bare PID is not evidence, because PID reuse is
	// normal and signalling a reused PID is how cleanup code kills the wrong
	// process (§D2, §D10).
	ProcessStartedAt int64 `json:"processStartedAt"`

	SocketPath string `json:"socketPath"`
	// SocketDevice / SocketInode bind the record to the exact socket file that
	// was live when it was written. A path alone can be replaced underneath us;
	// the (dev, ino) pair cannot be forged by re-creating a file at the path.
	SocketDevice uint64 `json:"socketDevice"`
	SocketInode  uint64 `json:"socketInode"`

	ProtocolMin uint32 `json:"protocolMin"`
	ProtocolMax uint32 `json:"protocolMax"`

	Phase shimwire.Phase `json:"phase"`

	// WorkareaPath is the workarea the harness runs against; compared at
	// adoption against the session's own expectation.
	WorkareaPath string `json:"workareaPath,omitempty"`
	// WorkareaRoot is the optional session-owned lifecycle root. It is a
	// secret-free integrity cross-check; old records omit it and remain valid.
	WorkareaRoot string `json:"workarea_root,omitempty"`

	CreatedAtUnixNano      int64 `json:"createdAt"`
	OrphanDeadlineUnixNano int64 `json:"orphanDeadlineAt,omitempty"`
}

// Identity returns the record's lifecycle identity.
func (r Record) Identity() Identity {
	return Identity{OrgID: r.OrgID, SessionID: r.SessionID}
}

// CreatedAt returns the record's creation time.
func (r Record) CreatedAt() time.Time { return time.Unix(0, r.CreatedAtUnixNano) }

// OrphanDeadline returns the armed orphan deadline, or the zero time when none
// is armed.
func (r Record) OrphanDeadline() time.Time {
	if r.OrphanDeadlineUnixNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, r.OrphanDeadlineUnixNano)
}

// Validate enforces the §D6 schema contract on a decoded record.
func (r Record) Validate() error {
	if r.SchemaVersion != RecordSchemaVersion {
		return fmt.Errorf("%w: schemaVersion %d, want %d", ErrRecordInvalid, r.SchemaVersion, RecordSchemaVersion)
	}
	if err := r.Identity().Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrRecordInvalid, err)
	}
	if r.ShimID == "" {
		return fmt.Errorf("%w: shimId is empty", ErrRecordInvalid)
	}
	if r.PID <= 0 {
		return fmt.Errorf("%w: pid %d is not positive", ErrRecordInvalid, r.PID)
	}
	if r.ProcessStartedAt <= 0 {
		// A record without start identity would force the reader to trust a bare
		// PID, which §D2 forbids outright.
		return fmt.Errorf("%w: processStartedAt is missing", ErrRecordInvalid)
	}
	if r.SocketPath == "" {
		return fmt.Errorf("%w: socketPath is empty", ErrRecordInvalid)
	}
	if r.ProtocolMin == 0 || r.ProtocolMin > r.ProtocolMax {
		return fmt.Errorf("%w: inverted protocol range [%d,%d]", ErrRecordInvalid, r.ProtocolMin, r.ProtocolMax)
	}
	if !r.Phase.Known() {
		return fmt.Errorf("%w: unknown phase %q", ErrRecordInvalid, r.Phase)
	}
	if r.CreatedAtUnixNano <= 0 {
		return fmt.Errorf("%w: createdAt is missing", ErrRecordInvalid)
	}
	if r.WorkareaRoot != "" {
		if !filepath.IsAbs(r.WorkareaRoot) || !filepath.IsAbs(r.WorkareaPath) {
			return fmt.Errorf("%w: workarea root/path must be absolute", ErrRecordInvalid)
		}
		rel, err := filepath.Rel(filepath.Clean(r.WorkareaRoot), filepath.Clean(r.WorkareaPath))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: workareaPath is outside workareaRoot", ErrRecordInvalid)
		}
	}
	return nil
}

// encode marshals a record for durable publication.
func (r Record) encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: encode record: %w", err)
	}
	if len(b) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: encoded record is %d bytes, max %d", ErrRecordInvalid, len(b), MaxRecordBytes)
	}
	return b, nil
}

// decodeRecord decodes a record STRICTLY.
//
// DisallowUnknownFields is the mechanical guard behind "the record contains no
// secrets": a future writer that adds a token field produces a record this
// reader REFUSES rather than silently carries. The schema is closed, so an
// unknown field is a contract violation and quarantining it is correct.
func decodeRecord(data []byte) (Record, error) {
	var r Record
	if len(data) > MaxRecordBytes {
		return r, fmt.Errorf("%w: record is %d bytes, max %d", ErrRecordInvalid, len(data), MaxRecordBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("%w: %v", ErrRecordInvalid, err)
	}
	if dec.More() {
		return r, fmt.Errorf("%w: trailing bytes after record", ErrRecordInvalid)
	}
	return r, r.Validate()
}

// Tombstone is the durable terminal observation a shim leaves behind after it
// has reaped its own harness process group (§D8).
//
// It is the ONLY artifact that can prove workload death to a later daemon. That
// is why it is written after the reap rather than before, and why it persists
// until a daemon durably reports the terminal outcome or an operator performs an
// audited disposal: destroying it early would turn a proven death back into an
// unresolved one.
type Tombstone struct {
	SchemaVersion int `json:"schemaVersion"`

	OrgID     string `json:"orgId"`
	SessionID string `json:"sessionId"`

	ShimID       string `json:"shimId"`
	ProcessEpoch uint64 `json:"processEpoch"`

	// HarnessPID and HarnessStartedAt record WHICH process was reaped, so a
	// later janitor can tell "this group is gone" from "a new process reused the
	// pid".
	HarnessPID       int   `json:"harnessPid"`
	HarnessStartedAt int64 `json:"harnessStartedAt"`

	ExitCode uint64 `json:"exitCode"`
	Signal   string `json:"signal,omitempty"`
	// LastSeq is the final host output sequence the shim allocated.
	LastSeq uint64 `json:"lastSeq"`
	// GroupReaped records whether the shim PROVED the harness process group was
	// gone. False means the terminal observation exists but death is not proven,
	// and the session stays in reconciliation rather than releasing a claim.
	GroupReaped bool `json:"groupReaped"`

	ObservedAtUnixNano int64 `json:"observedAt"`
}

// Identity returns the tombstone's lifecycle identity.
func (t Tombstone) Identity() Identity {
	return Identity{OrgID: t.OrgID, SessionID: t.SessionID}
}

// ObservedAt returns when the terminal observation was made.
func (t Tombstone) ObservedAt() time.Time { return time.Unix(0, t.ObservedAtUnixNano) }

// Validate enforces the tombstone contract.
func (t Tombstone) Validate() error {
	if t.SchemaVersion != RecordSchemaVersion {
		return fmt.Errorf("%w: tombstone schemaVersion %d, want %d", ErrRecordInvalid, t.SchemaVersion, RecordSchemaVersion)
	}
	if err := t.Identity().Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrRecordInvalid, err)
	}
	if t.ShimID == "" {
		return fmt.Errorf("%w: tombstone shimId is empty", ErrRecordInvalid)
	}
	if t.ObservedAtUnixNano <= 0 {
		return fmt.Errorf("%w: tombstone observedAt is missing", ErrRecordInvalid)
	}
	return nil
}

func (t Tombstone) encode() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: encode tombstone: %w", err)
	}
	if len(b) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: encoded tombstone is %d bytes, max %d", ErrRecordInvalid, len(b), MaxRecordBytes)
	}
	return b, nil
}

func decodeTombstone(data []byte) (Tombstone, error) {
	var t Tombstone
	if len(data) > MaxRecordBytes {
		return t, fmt.Errorf("%w: tombstone is %d bytes, max %d", ErrRecordInvalid, len(data), MaxRecordBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, fmt.Errorf("%w: %v", ErrRecordInvalid, err)
	}
	if dec.More() {
		return t, fmt.Errorf("%w: trailing bytes after tombstone", ErrRecordInvalid)
	}
	return t, t.Validate()
}
