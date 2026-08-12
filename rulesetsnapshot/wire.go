package rulesetsnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/executioncell"
)

// rawSections holds each section as the ORIGINAL bytes received on the
// wire (json.RawMessage), not a round-trip through this package's own
// (necessarily best-effort) typed structs. This is load-bearing for
// correctness: if a publisher's section carries a field this package's
// Sections struct does not know about, decoding into Sections and
// re-marshaling to hash would silently DROP that field before hashing —
// producing a hash that can never match the publisher's signed one. Hashing
// the raw bytes instead means verification is correct regardless of how
// complete this package's typed mirror is.
type rawSections struct {
	PolicyBundle        json.RawMessage `json:"policyBundle"`
	CapacityProfiles    json.RawMessage `json:"capacityProfiles"`
	PoolHostInventory   json.RawMessage `json:"poolHostInventory"`
	ExecutionCellMatrix json.RawMessage `json:"executionCellMatrix"`
	PosteriorSummary    json.RawMessage `json:"posteriorSummary"`
}

// wireSnapshot is the GET response envelope: the signed snapshot plus its
// signature metadata. Field names match the contract's JSON wire shape.
type wireSnapshot struct {
	OrgID          string            `json:"orgId"`
	Revision       int               `json:"revision"`
	RulesetRev     string            `json:"rulesetRev"`
	ContentHash    string            `json:"contentHash"`
	SectionDigests map[string]string `json:"sectionDigests"`
	Signature      string            `json:"signature"`
	SigningKeyID   string            `json:"signingKeyId"`
	Algorithm      string            `json:"algorithm"`
	Validators     []string          `json:"validators"`
	CompiledAt     string            `json:"compiledAt"`
	Sections       rawSections       `json:"sections"`
}

// contentHash returns the lower-case hex SHA-256 of the RFC 8785 canonical
// JSON of {policyBundle, capacityProfiles, poolHostInventory,
// executionCellMatrix, posteriorSummary} in that fixed key order — the
// exact payload the Ed25519 signature is computed over. Reuses
// executioncell.CanonicalJSON (an RFC 8785 / JCS implementation already
// vendored and tested in this repo) rather than a second, hand-rolled
// canonicalizer.
func contentHash(sections rawSections) (string, error) {
	// A struct (not a map) fixes the section keys at compile time; encoding/json
	// marshals json.RawMessage fields verbatim, so this reproduces exactly the
	// object a publisher canonicalizes and signs, with no lossy round trip
	// through this package's typed Sections mirror.
	ordered := struct {
		PolicyBundle        json.RawMessage `json:"policyBundle"`
		CapacityProfiles    json.RawMessage `json:"capacityProfiles"`
		PoolHostInventory   json.RawMessage `json:"poolHostInventory"`
		ExecutionCellMatrix json.RawMessage `json:"executionCellMatrix"`
		PosteriorSummary    json.RawMessage `json:"posteriorSummary"`
	}{
		PolicyBundle:        normalizeRaw(sections.PolicyBundle),
		CapacityProfiles:    normalizeRaw(sections.CapacityProfiles),
		PoolHostInventory:   normalizeRaw(sections.PoolHostInventory),
		ExecutionCellMatrix: normalizeRaw(sections.ExecutionCellMatrix),
		PosteriorSummary:    normalizeRaw(sections.PosteriorSummary),
	}
	canonical, err := executioncell.CanonicalJSON(ordered)
	if err != nil {
		return "", fmt.Errorf("rulesetsnapshot: canonicalize sections: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// normalizeRaw maps an absent/empty section to JSON null (mirroring the
// publisher's own "undefined -> null" normalization) so a missing section
// hashes identically to an explicit null rather than an empty byte slice
// producing invalid JSON.
func normalizeRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// decodeTypedSections best-effort decodes each raw section into this
// package's typed Sections mirror for domain logic (claim_eval.go). Unknown
// fields are ignored deliberately — this is an external wire contract, not
// this package's own versioned schema.
func decodeTypedSections(raw rawSections) (Sections, error) {
	var out Sections
	fields := []struct {
		name string
		raw  json.RawMessage
		dst  any
	}{
		{"policyBundle", raw.PolicyBundle, &out.PolicyBundle},
		{"capacityProfiles", raw.CapacityProfiles, &out.CapacityProfiles},
		{"poolHostInventory", raw.PoolHostInventory, &out.PoolHostInventory},
		{"executionCellMatrix", raw.ExecutionCellMatrix, &out.ExecutionCellMatrix},
		{"posteriorSummary", raw.PosteriorSummary, &out.PosteriorSummary},
	}
	for _, f := range fields {
		if len(f.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			return Sections{}, fmt.Errorf("rulesetsnapshot: decode %s section: %w", f.name, err)
		}
	}
	return out, nil
}
