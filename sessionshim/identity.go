package sessionshim

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Identity is the SOLE lifecycle identity of a session (ADR-2026-08-17 §D2).
//
// It is a value type on purpose: everything downstream — the registry key, the
// adoption request, credential rehydration, carrier reconnection, terminal
// reporting — takes this and nothing else. A shim id or PID cannot be
// substituted for it, which is what stops an execution detail from becoming a
// second session namespace.
type Identity struct {
	OrgID     string
	SessionID string
}

// ErrInvalidIdentity reports a malformed or incomplete lifecycle identity.
var ErrInvalidIdentity = errors.New("sessionshim: invalid session identity")

// maxIdentityField bounds each half of the identity. Registry records are
// bounded by contract (§D6); an unbounded identity would defeat that through the
// record body.
const maxIdentityField = 256

// Validate rejects an identity that cannot key a registry record.
//
// The NUL and path-separator rejections matter beyond hygiene: the identity is
// hashed into a filename and compared against a live peer's self-report, so a
// value containing a separator would make two different identities capable of
// colliding in operator-facing output.
func (id Identity) Validate() error {
	if err := validateIdentityField("orgId", id.OrgID); err != nil {
		return err
	}
	return validateIdentityField("sessionId", id.SessionID)
}

func validateIdentityField(name, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("%w: %s is empty", ErrInvalidIdentity, name)
	case len(v) > maxIdentityField:
		return fmt.Errorf("%w: %s is %d bytes, max %d", ErrInvalidIdentity, name, len(v), maxIdentityField)
	case strings.ContainsAny(v, "\x00/\\"):
		return fmt.Errorf("%w: %s contains a path separator or NUL", ErrInvalidIdentity, name)
	}
	return nil
}

// Key is the canonical, unambiguous string form.
//
// The separator is "\x1f" (ASCII unit separator) rather than a printable
// character because Validate permits ordinary punctuation in both halves: with a
// printable separator, ("a:b", "c") and ("a", "b:c") would produce the same key
// and silently alias two sessions.
func (id Identity) Key() string { return id.OrgID + "\x1f" + id.SessionID }

// String is the operator-facing form. Unlike Key it is for display only.
func (id Identity) String() string { return id.OrgID + "/" + id.SessionID }

// RecordName is the on-disk registry filename for this identity.
//
// It is a fixed-length digest, not the identity itself, for a concrete reason
// stated in §D6: the socket lives beside the record under the state directory
// and Unix socket paths have a short platform limit (as low as 104 bytes on
// some systems). A variable-length identity in the path would make the limit
// depend on tenant naming. The unhashed identity lives INSIDE the bounded
// record and is verified against the live shim's handshake, so nothing is lost.
func (id Identity) RecordName() string {
	sum := sha256.Sum256([]byte(id.Key()))
	return hex.EncodeToString(sum[:]) + recordSuffix
}

// SocketName is the on-disk socket filename for this identity. Same
// fixed-length reasoning as RecordName.
func (id Identity) SocketName() string {
	sum := sha256.Sum256([]byte(id.Key()))
	return hex.EncodeToString(sum[:16]) + socketSuffix
}

// File suffixes under the registry directory.
const (
	recordSuffix    = ".json"
	socketSuffix    = ".sock"
	tombstoneSuffix = ".tombstone.json"
)

// TombstoneName is the on-disk terminal-tombstone filename for this identity.
func (id Identity) TombstoneName() string {
	sum := sha256.Sum256([]byte(id.Key()))
	return hex.EncodeToString(sum[:]) + tombstoneSuffix
}
