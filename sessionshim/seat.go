package sessionshim

// seat.go — the secret-free seat launch record a launching daemon writes
// beside a shim's discovery record.
//
// # WHY A SIDECAR AND NOT A RECORD FIELD
//
// The §D6 discovery Record is a CLOSED schema decoded with
// DisallowUnknownFields, and interactive records stay byte-identical to what
// released readers decode. A seat field inside it would make every daemon built
// before that field refuse — and quarantine — every interactive shim launched
// after it, which is exactly the rollback a release must keep safe. The launch
// facts therefore go beside the record, the way the durable acknowledgement
// cursor and the back-pressure state do. Registry.Scan reads only the record
// suffix, so an older daemon never sees this file.
//
// # WHO WRITES IT, AND WHEN
//
// The LAUNCHING DAEMON writes it, synchronously, before it starts the shim
// process: the seat's scope unit and the posture and limits the launch gave
// it. The shim id does not exist yet at that point, so the file is keyed by the
// lifecycle identity and the launch's process epoch — the same pair the scope
// unit name is derived from. A daemon that later adopts the seat reports the
// limits it reads back from the seat's live cgroup and falls back to this
// record, never to its own current configuration.
//
// It is disposed with the incarnation's terminal proof (RemoveTombstoneIncarnation,
// DisposeWithdrawnAbsence), or by the launching daemon when the launch fails
// before the shim ever published a record.

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
	"strings"
	"time"
)

const (
	seatLaunchSchemaVersion = 1
	// seatLaunchSuffix deliberately does NOT end in the record suffix: a name
	// ending in `.json` would be enumerated by Scan as a discovery record.
	seatLaunchSuffix   = ".seat"
	maxSeatLaunchBytes = 1 << 10
	// maxSeatScopeBytes bounds the scope unit name. systemd unit names are at
	// most 255 bytes; the launcher's digest names are far shorter.
	maxSeatScopeBytes = 255
	// maxSeatCPUs and maxSeatMemoryMB bound the recorded limits to values a
	// real host can carry, so a corrupted file cannot report absurd numbers.
	maxSeatCPUs     = 1 << 16
	maxSeatMemoryMB = 1 << 30
	// maxSeatIOWeight is the cgroup v2 IO weight ceiling.
	maxSeatIOWeight = 10000
)

// Seat postures a launch record may carry. They mirror the daemon's seat-budget
// report modes: enforced means the scope's cgroup carries the limits, best-effort
// means cooperative worker caps only, none means no budget applied.
const (
	SeatModeEnforced   = "enforced"
	SeatModeBestEffort = "best-effort"
	SeatModeNone       = "none"
)

// SeatLaunch is one launch's secret-free seat facts: which transient scope owns
// the seat's cgroup and what posture and limits the launch gave it. It carries
// no bearer, credential, or path — only a unit name and numbers.
type SeatLaunch struct {
	SchemaVersion int    `json:"schemaVersion"`
	OrgID         string `json:"orgId"`
	SessionID     string `json:"sessionId"`
	ProcessEpoch  uint64 `json:"processEpoch"`

	// Scope is the transient scope unit that owns the seat's cgroup. Empty when
	// the seat runs in no scope (off Linux, where no scope exists).
	Scope string `json:"scope,omitempty"`
	// Mode is the posture the launch gave the seat: SeatModeEnforced,
	// SeatModeBestEffort or SeatModeNone.
	Mode string `json:"mode"`
	// CPUs, MemoryMB and IOWeight are the limits the launch gave the seat. Zero
	// means no limit of that kind.
	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memoryMb"`
	IOWeight int `json:"ioWeight"`

	CreatedAtUnixNano int64 `json:"createdAt"`
}

// NewSeatLaunch builds the launch record for one launch at the current schema
// version. Validate (and PutSeatLaunch) still decide whether it is well-formed.
func NewSeatLaunch(id Identity, processEpoch uint64, scope, mode string, cpus, memoryMB, ioWeight int, now time.Time) SeatLaunch {
	return SeatLaunch{
		SchemaVersion:     seatLaunchSchemaVersion,
		OrgID:             id.OrgID,
		SessionID:         id.SessionID,
		ProcessEpoch:      processEpoch,
		Scope:             scope,
		Mode:              mode,
		CPUs:              cpus,
		MemoryMB:          memoryMB,
		IOWeight:          ioWeight,
		CreatedAtUnixNano: now.UnixNano(),
	}
}

// Identity returns the lifecycle identity this launch record belongs to.
func (s SeatLaunch) Identity() Identity {
	return Identity{OrgID: s.OrgID, SessionID: s.SessionID}
}

// Validate enforces the launch-record contract on an encoded or decoded value.
func (s SeatLaunch) Validate() error {
	if s.SchemaVersion != seatLaunchSchemaVersion {
		return fmt.Errorf("sessionshim: seat launch schemaVersion %d, want %d", s.SchemaVersion, seatLaunchSchemaVersion)
	}
	if err := s.Identity().Validate(); err != nil {
		return err
	}
	switch s.Mode {
	case SeatModeEnforced, SeatModeBestEffort, SeatModeNone:
	default:
		return fmt.Errorf("sessionshim: seat launch mode %q is not one of %s, %s, %s",
			s.Mode, SeatModeEnforced, SeatModeBestEffort, SeatModeNone)
	}
	if s.CPUs < 0 || s.CPUs > maxSeatCPUs || s.MemoryMB < 0 || s.MemoryMB > maxSeatMemoryMB ||
		s.IOWeight < 0 || s.IOWeight > maxSeatIOWeight {
		return errors.New("sessionshim: seat launch limits are not well-formed")
	}
	if err := validateSeatScope(s.Scope); err != nil {
		return err
	}
	if s.Mode == SeatModeEnforced && s.Scope == "" {
		// Enforcement is the scope's cgroup. A record that claims it with no
		// scope would report a confinement nothing provides.
		return errors.New("sessionshim: an enforced seat launch names no scope")
	}
	if s.CreatedAtUnixNano <= 0 {
		return errors.New("sessionshim: seat launch is missing createdAt")
	}
	return nil
}

// validateSeatScope bounds the scope to a systemd scope unit name: the unit
// alphabet only, with the .scope suffix. Empty is allowed (no scope).
func validateSeatScope(scope string) error {
	if scope == "" {
		return nil
	}
	if len(scope) > maxSeatScopeBytes {
		return fmt.Errorf("sessionshim: seat scope is %d bytes, max %d", len(scope), maxSeatScopeBytes)
	}
	if !strings.HasSuffix(scope, ".scope") || len(scope) == len(".scope") {
		return fmt.Errorf("sessionshim: seat scope %q is not a scope unit", scope)
	}
	for i := 0; i < len(scope); i++ {
		c := scope[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '@', c == ':':
		default:
			return fmt.Errorf("sessionshim: seat scope %q carries a byte outside the unit alphabet", scope)
		}
	}
	return nil
}

func (s SeatLaunch) encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: encode seat launch: %w", err)
	}
	if len(raw) > maxSeatLaunchBytes {
		return nil, fmt.Errorf("sessionshim: seat launch is %d bytes, max %d", len(raw), maxSeatLaunchBytes)
	}
	return raw, nil
}

func decodeSeatLaunch(data []byte) (SeatLaunch, error) {
	var s SeatLaunch
	if len(data) > maxSeatLaunchBytes {
		return s, fmt.Errorf("sessionshim: seat launch is %d bytes, max %d", len(data), maxSeatLaunchBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return s, fmt.Errorf("sessionshim: decode seat launch: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return s, errors.New("sessionshim: seat launch has trailing data")
	}
	return s, s.Validate()
}

// seatLaunchName is the per-launch filename, digested from the lifecycle
// identity and the process epoch with the same unit-separator join every other
// incarnation sidecar uses. Two launches of one session get separate files.
func seatLaunchName(id Identity, processEpoch uint64) string {
	correlation := id.Key() + "\x1f" + strconv.FormatUint(processEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:]) + seatLaunchSuffix
}

// PutSeatLaunch durably publishes one launch's seat facts, replacing any
// previous record for the same identity and epoch.
func (r *Registry) PutSeatLaunch(s SeatLaunch) error {
	raw, err := s.encode()
	if err != nil {
		return err
	}
	return r.publish(seatLaunchName(s.Identity(), s.ProcessEpoch), raw)
}

// SeatLaunch reads the launch record for one identity and process epoch.
//
// ok=false with a nil error means no record exists: the seat was launched by a
// daemon that predates launch records, or its record was already disposed. A
// record that exists but fails the size, mode, ownership or schema bounds is an
// error, never silently empty facts.
func (r *Registry) SeatLaunch(id Identity, processEpoch uint64) (SeatLaunch, bool, error) {
	raw, err := r.readEntry(seatLaunchName(id, processEpoch))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SeatLaunch{}, false, nil
		}
		return SeatLaunch{}, false, err
	}
	s, err := decodeSeatLaunch(raw)
	if err != nil {
		return SeatLaunch{}, false, err
	}
	if s.Identity() != id || s.ProcessEpoch != processEpoch {
		return SeatLaunch{}, false, errors.New("sessionshim: seat launch does not match the requested launch")
	}
	return s, true, nil
}

// RemoveSeatLaunch deletes one launch's record. Idempotent: a missing file is
// not an error, because the launch-failure path and the terminal disposal path
// may both reach it.
func (r *Registry) RemoveSeatLaunch(id Identity, processEpoch uint64) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(seatLaunchName(id, processEpoch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sessionshim: remove seat launch: %w", err)
	}
	return nil
}
