package agent

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
)

// This file declares the additional-extension delivery seam for harnesses
// with a host-side extension API (currently pi; provider/harness/pi), per
// donmai-architecture's ADR-2026-08-12-pi-extension-delivery-seam-and-
// capability-pack-boundary.md D1/D2, mirrored into
// 002-provider-base-contract.md §E "Additional-extension delivery" and
// 013-orchestrator-and-governor.md's pre-spawn sequence.
//
// It generalizes the mechanism the pi harness's own embedded trust-boundary
// extension already used for its one consumer (materialize into a
// runner-owned per-session directory, load by explicit path, verify a digest
// AFTER materialization, disable every other extension-discovery source in
// the same argv) into an ordered list any caller can populate. A harness
// without a host-side extension API ignores this field, exactly like every
// other capability-gated Spec field (Spec.Interactive, Spec.ResponseSchema):
// per D5.5 there is no cross-harness "supports extensions" boolean, so the
// exact selected harness — pi today — is what decides whether this field is
// honored, not a generic capability flag.

// ExtensionDeliveryKind selects how ExtensionDelivery.Path or .Source
// supplies an additional extension's bytes.
type ExtensionDeliveryKind string

// Extension delivery kinds. D1: "Each delivery is one of two forms, and both
// are supported because each covers a case the other cannot."
const (
	// ExtensionDeliveryPath names an artifact the caller has already
	// materialized at an absolute path the runner can read — the shape a
	// composing binary uses when it carries the pack as embedded bytes and
	// materializes them itself.
	ExtensionDeliveryPath ExtensionDeliveryKind = "path"

	// ExtensionDeliveryInline supplies the source bytes and a basename; the
	// runner materializes them into the per-session state directory. The
	// shape a pack takes when it is composed at spawn time, where a tool
	// list that varies with the admitted capability grants has no fixed
	// file to point at.
	ExtensionDeliveryInline ExtensionDeliveryKind = "inline"
)

// ExtensionDelivery is one additional extension delivered on the spawn spec,
// per ADR-2026-08-12 D1. Spec.AdditionalExtensions carries an ORDERED list;
// the harness's own trust-boundary extension (pi's embedded policy
// extension) always loads first and no entry here may displace, reorder, or
// disable it (D1: "the policy extension is always first and cannot be
// displaced, reordered, or disabled by a delivery").
type ExtensionDelivery struct {
	// ID names this delivery for denial errors and its cleanup entry (D1.3).
	// Required; unique within one Spec's AdditionalExtensions.
	ID string `json:"id"`

	// Kind selects Path or Inline delivery. Required.
	Kind ExtensionDeliveryKind `json:"kind"`

	// Path is an absolute path to an artifact the caller has already
	// materialized. Required and must be absolute when Kind ==
	// ExtensionDeliveryPath; ignored otherwise.
	Path string `json:"path,omitempty"`

	// Source is the extension's exact source bytes. Required and non-empty
	// when Kind == ExtensionDeliveryInline; ignored otherwise.
	Source []byte `json:"source,omitempty"`

	// Basename names the file the runner materializes Source under, inside
	// the per-session state directory. Required, non-empty, and must be a
	// bare filename (no path separators) when Kind == ExtensionDeliveryInline.
	Basename string `json:"basename,omitempty"`

	// Digest is the REQUIRED lowercase-hex sha256 of the extension's exact
	// on-disk bytes. Per D2(b), the runner verifies this AFTER
	// materialization — reading back whatever actually landed at the load
	// path, never Source or a caller's in-memory copy — so a mismatch always
	// reflects what is actually loadable rather than what was merely
	// intended. This is the TOCTOU-closing half of the trust-bypass
	// preconditions: a file rewritten between materialization and load is
	// caught here. Never optional: an empty Digest is malformed input and
	// denies before any child process starts (D1.2 fail-closed).
	Digest string `json:"digest"`

	// Required marks the delivery load-bearing: per D1.2, a required
	// delivery that cannot be materialized, verified, or loaded denies spawn
	// closed, before credential delivery, with no warn-and-strip path. Every
	// delivery this Wave-1 seam accepts is treated as required regardless of
	// this field's value — there is no admission system yet that grants an
	// OPTIONAL delivery a graceful-downgrade path — so Required is carried
	// for forward compatibility and self-documentation rather than branched
	// on today.
	Required bool `json:"required,omitempty"`
}

// sha256HexPattern matches a lowercase-hex sha256 digest.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateExtensionDelivery reports the first structural defect in d, or nil
// when d is well-formed. It inspects shape only — never file contents or
// Source bytes against Digest — so callers can validate before any
// filesystem or subprocess work; the exact harness adapter still verifies
// Digest against what actually lands on disk (VerifyExtensionDigest).
func ValidateExtensionDelivery(d ExtensionDelivery) error {
	if d.ID == "" {
		return fmt.Errorf("extension delivery: id is required")
	}
	if !sha256HexPattern.MatchString(d.Digest) {
		return fmt.Errorf("extension delivery %q: digest must be a lowercase sha256 hex string", d.ID)
	}
	switch d.Kind {
	case ExtensionDeliveryPath:
		if d.Path == "" || !filepath.IsAbs(d.Path) {
			return fmt.Errorf("extension delivery %q: path delivery requires an absolute path", d.ID)
		}
	case ExtensionDeliveryInline:
		if len(d.Source) == 0 {
			return fmt.Errorf("extension delivery %q: inline delivery requires non-empty source", d.ID)
		}
		if d.Basename == "" || d.Basename != filepath.Base(d.Basename) || d.Basename == "." || d.Basename == ".." {
			return fmt.Errorf("extension delivery %q: inline delivery requires a bare basename (no path separators)", d.ID)
		}
	default:
		return fmt.Errorf("extension delivery %q: unknown kind %q", d.ID, d.Kind)
	}
	return nil
}

// ValidateExtensionDeliveries validates an ordered list, additionally
// requiring unique IDs (D1.3: each delivery names its own cleanup entry — a
// duplicate ID makes cleanup and denial-reporting ambiguous).
func ValidateExtensionDeliveries(deliveries []ExtensionDelivery) error {
	seen := make(map[string]bool, len(deliveries))
	for _, d := range deliveries {
		if err := ValidateExtensionDelivery(d); err != nil {
			return err
		}
		if seen[d.ID] {
			return fmt.Errorf("extension delivery %q: duplicate id", d.ID)
		}
		seen[d.ID] = true
	}
	return nil
}

// VerifyExtensionDigest reports whether the sha256 of loaded — bytes read
// back from wherever the delivery actually landed, never the caller's
// original Source — matches want, in constant time. Fail-closed: an empty
// want never matches. This is the TOCTOU-closing verification D2(b)
// requires: always against what is actually loadable, after materialization.
func VerifyExtensionDigest(loaded []byte, want string) bool {
	if want == "" {
		return false
	}
	sum := sha256.Sum256(loaded)
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
