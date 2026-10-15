package workarea

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TerminalArchiveReference is the separately versioned additive projection
// describing one durably committed immutable interactive-workarea archive.
//
// It is emitted only after the archive bytes and manifest are atomically
// committed and independently re-read: ArchiveID names the committed
// archive directory and ArchiveDigest carries the exact committed tree
// digest, so recovery clients can name the bytes they will read back.
//
// The closed terminal-lease v1 projection is preserved untouched: this
// reference travels beside it (never inside it) and carries the lease's
// four identities plus the session that owns the terminal result.
type TerminalArchiveReference struct {
	SchemaVersion    string    `json:"schemaVersion"`
	ArchiveID        string    `json:"archiveId"`
	ArchiveDigest    string    `json:"archiveDigest"`
	SessionID        string    `json:"sessionId"`
	WorkareaID       string    `json:"workareaId"`
	TerminalResultID string    `json:"terminalResultId"`
	CommittedAt      time.Time `json:"committedAt"`
}

// Validate implements the documented terminal-workarea contract.
func (r TerminalArchiveReference) Validate() error {
	if r.SchemaVersion != TerminalArchiveReferenceSchemaV1 {
		return fmt.Errorf("runtime/workarea: unsupported terminal archive reference schema %q", r.SchemaVersion)
	}
	if strings.TrimSpace(r.ArchiveID) == "" || strings.ContainsAny(r.ArchiveID, `/\`) || r.ArchiveID == "." || r.ArchiveID == ".." {
		return errors.New("runtime/workarea: archiveId must be a safe single-component archive id")
	}
	if !validArchiveDigest(r.ArchiveDigest) {
		return errors.New("runtime/workarea: archiveDigest must use sha256:<64 lowercase hex> form")
	}
	if err := validateCanonicalUUID(r.SessionID); err != nil {
		return fmt.Errorf("runtime/workarea: sessionId: %w", err)
	}
	if err := validateGeneratedID(r.WorkareaID, "wa_"); err != nil {
		return fmt.Errorf("runtime/workarea: workareaId: %w", err)
	}
	if err := validateGeneratedID(r.TerminalResultID, "tr_"); err != nil {
		return fmt.Errorf("runtime/workarea: terminalResultId: %w", err)
	}
	if r.CommittedAt.IsZero() {
		return errors.New("runtime/workarea: committedAt must name the committed archive instant")
	}
	if _, err := formatCanonicalMillis(r.CommittedAt); err != nil {
		return fmt.Errorf("runtime/workarea: committedAt: %w", err)
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (r TerminalArchiveReference) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	committedAt, _ := formatCanonicalMillis(r.CommittedAt)
	fields := [][2]string{
		{"schemaVersion", r.SchemaVersion},
		{"archiveId", r.ArchiveID},
		{"archiveDigest", r.ArchiveDigest},
		{"sessionId", r.SessionID},
		{"workareaId", r.WorkareaID},
		{"terminalResultId", r.TerminalResultID},
		{"committedAt", committedAt},
	}
	return marshalCanonicalStringObject(fields)
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (r *TerminalArchiveReference) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	fields := []string{"schemaVersion", "archiveId", "archiveDigest", "sessionId", "workareaId", "terminalResultId", "committedAt"}
	if err := requireClosedFields(values, fields...); err != nil {
		return err
	}
	outputs := []*string{&r.SchemaVersion, &r.ArchiveID, &r.ArchiveDigest, &r.SessionID, &r.WorkareaID, &r.TerminalResultID}
	for i, field := range fields[:6] {
		if *outputs[i], err = requireString(values, field); err != nil {
			return err
		}
	}
	committedAt, err := requireString(values, "committedAt")
	if err != nil {
		return err
	}
	if r.CommittedAt, err = parseCanonicalMillis(committedAt); err != nil {
		return err
	}
	return r.Validate()
}

// IdentityEqual reports whether two references name the same committed
// archive bytes for the same terminal result.
func (r TerminalArchiveReference) IdentityEqual(other TerminalArchiveReference) bool {
	return r.SchemaVersion == other.SchemaVersion &&
		r.ArchiveID == other.ArchiveID &&
		r.ArchiveDigest == other.ArchiveDigest &&
		r.SessionID == other.SessionID &&
		r.WorkareaID == other.WorkareaID &&
		r.TerminalResultID == other.TerminalResultID
}

// validArchiveDigest accepts the tree-digest form the archive registry
// commits alongside every archive manifest.
func validArchiveDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return validLowerHex(strings.TrimPrefix(value, prefix), 64)
}
