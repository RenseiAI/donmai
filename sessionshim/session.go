package sessionshim

// session.go — the secret-free session-identity sidecar a launching daemon
// writes beside a shim's discovery record.
//
// # WHY A SIDECAR AND NOT A RECORD FIELD
//
// The §D6 discovery Record is a CLOSED schema decoded with
// DisallowUnknownFields, and interactive records stay byte-identical to what
// released readers decode. Identity fields inside it would make every daemon
// built before those fields refuse — and quarantine — every interactive shim
// launched after them, which is exactly the rollback a release must keep
// safe. The session facts therefore go beside the record, the way the seat
// launch record and the durable acknowledgement cursor do. Registry.Scan
// reads only the record suffix, so an older daemon never sees this file.
//
// # WHO WRITES IT, AND WHEN
//
// The LAUNCHING DAEMON writes it, synchronously, before it starts the shim
// process: the admitted Repository, ProjectName (the project-allowlist ID
// the session runs under) and Harness (the loop-driver identity) from the
// launch-time spec. A daemon that later adopts the seat reads it back to
// fill the operator-facing handle, so a scoped reader (a repo-scoped watch)
// keeps seeing the session after a daemon restart. Nothing secret rides it:
// identifiers only, exactly as the SessionHandle list already serves.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

const (
	sessionSidecarSchemaVersion = 1
	// sessionSuffix deliberately does NOT end in the record suffix: a name
	// ending in `.json` would be enumerated by Scan as a discovery record.
	sessionSuffix = ".session"
	// maxSessionSidecarBytes bounds one sidecar on disk. Three short
	// identifiers fit comfortably; anything larger is corruption, not a
	// session an operator named.
	maxSessionSidecarBytes = 1 << 10 // 1 KiB
	// maxSessionFieldBytes bounds one identifier field. Repository URLs
	// with embedded credentials are redacted by the serving layer, never
	// stored here — the bound only has to admit the longest legitimate
	// identifier, not an attacker's paste.
	maxSessionFieldBytes = 512
)

// SessionSidecar is one launch's secret-free session identity: the admitted
// repository, project and harness the launch-time spec carried. It carries
// no bearer, credential, environment snapshot, prompt or endpoint secret —
// identifiers only, exactly as the SessionHandle list already serves.
type SessionSidecar struct {
	SchemaVersion int    `json:"schemaVersion"`
	OrgID         string `json:"orgId"`
	SessionID     string `json:"sessionId"`
	ProcessEpoch  uint64 `json:"processEpoch"`

	// Repository is the git URL (or owner/name slug) the session operates
	// on, as carried on the launch-time spec. Empty when the launch ran
	// repository-free.
	Repository string `json:"repository,omitempty"`
	// ProjectName is the allowlist-resolved project identifier the session
	// was dispatched under. Empty when no allowlist entry matched.
	ProjectName string `json:"projectName,omitempty"`
	// Harness is the resolved loop-driver identity. Empty when unreported.
	Harness string `json:"harness,omitempty"`
}

// NewSessionSidecar builds the sidecar for one launch at the current schema
// version. Validate (and PutSessionSidecar) still decide whether it is
// well-formed.
func NewSessionSidecar(id Identity, processEpoch uint64, repository, projectName, harness string) SessionSidecar {
	return SessionSidecar{
		SchemaVersion: sessionSidecarSchemaVersion,
		OrgID:         id.OrgID,
		SessionID:     id.SessionID,
		ProcessEpoch:  processEpoch,
		Repository:    repository,
		ProjectName:   projectName,
		Harness:       harness,
	}
}

// Identity returns the lifecycle identity this sidecar belongs to.
func (s SessionSidecar) Identity() Identity {
	return Identity{OrgID: s.OrgID, SessionID: s.SessionID}
}

// Validate enforces the sidecar contract on an encoded or decoded value.
func (s SessionSidecar) Validate() error {
	if s.SchemaVersion != sessionSidecarSchemaVersion {
		return errors.New("sessionshim: session sidecar schemaVersion is not the current version")
	}
	if err := s.Identity().Validate(); err != nil {
		return err
	}
	for _, field := range []string{s.Repository, s.ProjectName, s.Harness} {
		if len(field) > maxSessionFieldBytes {
			return errors.New("sessionshim: session sidecar field is not well-formed")
		}
		if strings.ContainsRune(field, '\x00') || strings.ContainsRune(field, '\n') {
			return errors.New("sessionshim: session sidecar field is not well-formed")
		}
	}
	return nil
}

func (s SessionSidecar) encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, errors.New("sessionshim: encode session sidecar")
	}
	if len(raw) > maxSessionSidecarBytes {
		return nil, errors.New("sessionshim: session sidecar exceeds the size bound")
	}
	return raw, nil
}

func decodeSessionSidecar(data []byte) (SessionSidecar, error) {
	var s SessionSidecar
	if len(data) > maxSessionSidecarBytes {
		return s, errors.New("sessionshim: session sidecar exceeds the size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return s, errors.New("sessionshim: decode session sidecar")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return s, errors.New("sessionshim: session sidecar has trailing data")
	}
	return s, s.Validate()
}

// sessionSidecarName is the per-launch filename, digested from the lifecycle
// identity and the process epoch with the same unit-separator join every other
// incarnation sidecar uses. Two launches of one session get separate files.
func sessionSidecarName(id Identity, processEpoch uint64) string {
	correlation := id.Key() + "\x1f" + strconv.FormatUint(processEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:]) + sessionSuffix
}

// PutSessionSidecar durably publishes one launch's session identity,
// replacing any previous sidecar for the same identity and epoch.
func (r *Registry) PutSessionSidecar(s SessionSidecar) error {
	raw, err := s.encode()
	if err != nil {
		return err
	}
	return r.publish(sessionSidecarName(s.Identity(), s.ProcessEpoch), raw)
}

// SessionSidecar reads the session-identity sidecar for one identity and
// process epoch.
//
// ok=false with a nil error means no sidecar exists: the seat was launched
// by a daemon that predates sidecars, or its sidecar was already disposed.
// A sidecar that exists but fails the size, identity or schema bounds is an
// error, never silently empty facts.
func (r *Registry) SessionSidecar(id Identity, processEpoch uint64) (SessionSidecar, bool, error) {
	raw, err := r.readEntry(sessionSidecarName(id, processEpoch))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SessionSidecar{}, false, nil
		}
		return SessionSidecar{}, false, err
	}
	s, err := decodeSessionSidecar(raw)
	if err != nil {
		return SessionSidecar{}, false, err
	}
	if s.Identity() != id || s.ProcessEpoch != processEpoch {
		return SessionSidecar{}, false, errors.New("sessionshim: session sidecar does not match the requested launch")
	}
	return s, true, nil
}

// RemoveSessionSidecar deletes one launch's sidecar. Idempotent: a missing
// file is not an error, because the launch-failure path and the terminal
// disposal path may both reach it.
func (r *Registry) RemoveSessionSidecar(id Identity, processEpoch uint64) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(sessionSidecarName(id, processEpoch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.New("sessionshim: remove session sidecar")
	}
	return nil
}
