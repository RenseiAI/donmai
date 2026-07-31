package codeintelhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// RevisionKind distinguishes the two supported revision-resolution modes.
// Both require the configured repository source to contain the exact
// immutable Revision object; neither permits a branch name or HEAD fallback.
type RevisionKind string

const (
	// RevisionResolvedRef names a revision the platform already resolved to
	// an immutable object (typically a commit SHA) against the configured
	// remote.
	RevisionResolvedRef RevisionKind = "resolved-ref"

	// RevisionSessionCheckout names a revision produced by an interactive
	// session (e.g. a workarea commit) that must be present in the
	// configured remote or an operator-provided local mirror; the host
	// never guesses or falls back when it is absent.
	RevisionSessionCheckout RevisionKind = "session-checkout"
)

// Binding is the complete immutable request binding — the fixed v0.1.0 wire
// shape shared by the request body and the JWT claims. It is also the sole
// identity of a resident workarea (Pool key): no field is optional and no
// field is omitted from that identity, so a ref rotation always creates a
// distinct workarea rather than retargeting an existing one in place.
type Binding struct {
	OrgID            string       `json:"orgId"`
	ProjectID        string       `json:"projectId"`
	RepositoryPathID string       `json:"repositoryPathId"`
	RevisionKind     RevisionKind `json:"revisionKind"`
	Revision         string       `json:"revision"`
}

// Validate reports whether b is a complete, well-formed binding. It does not
// verify that the binding resolves to a real repository or revision — that
// is Factory's job during warm-up.
func (b Binding) Validate() error {
	switch {
	case b.OrgID == "":
		return errors.New("binding: orgId is required")
	case b.ProjectID == "":
		return errors.New("binding: projectId is required")
	case b.RepositoryPathID == "":
		return errors.New("binding: repositoryPathId is required")
	case b.Revision == "":
		return errors.New("binding: revision is required")
	case !isFullObjectID(b.Revision):
		return errors.New("binding: revision must be a full 40- or 64-character hexadecimal object id")
	}
	switch b.RevisionKind {
	case RevisionResolvedRef, RevisionSessionCheckout:
		return nil
	default:
		return fmt.Errorf("binding: revisionKind must be %q or %q, got %q",
			RevisionResolvedRef, RevisionSessionCheckout, b.RevisionKind)
	}
}

func isFullObjectID(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, r := range revision {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// Equal reports whether b and o carry the identical binding identity.
// Binding's fields are all comparable, so this is a plain value comparison —
// the method exists for call-site clarity at the held-binding recheck.
func (b Binding) Equal(o Binding) bool {
	return b == o
}

// Key returns a stable, injective string identity for b suitable for use as
// a map key. Fields are NUL-joined so no combination of field values can
// collide with a different field partition (NUL cannot appear in any of the
// identifier or revision fields we accept).
func (b Binding) Key() string {
	return b.OrgID + "\x00" + b.ProjectID + "\x00" + b.RepositoryPathID + "\x00" + string(b.RevisionKind) + "\x00" + b.Revision
}

// Fingerprint returns a filesystem-safe, fixed-length hex digest of Key(),
// for use as a workarea directory name — identifier and revision fields are
// operator/platform controlled but not guaranteed path-safe verbatim.
func (b Binding) Fingerprint() string {
	sum := sha256.Sum256([]byte(b.Key()))
	return hex.EncodeToString(sum[:])
}
