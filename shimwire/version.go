package shimwire

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Protocol identity and the version range this build speaks.
const (
	// ProtocolName is the wire's stable name. It is carried in Hello/Welcome so
	// a mis-dialled socket fails closed rather than being decoded as garbage.
	ProtocolName = "session-shim-v1"

	// V1 is the frozen first selectable protocol version.
	V1 uint32 = 1
	// V2 adds request-correlated authoritative snapshot inspect/emit while
	// retaining every v1 message and the stable protocol-family token.
	V2 uint32 = 2
	// V3 adds one exact full-host-frame observation without changing selected
	// v1 or v2 behavior.
	V3 uint32 = 3

	// ProtocolMin / ProtocolMax is the range THIS build advertises. A protocol
	// bump widens Max and only ever raises Min after an overlap window at least
	// as long as the maximum supported session duration (ADR-2026-08-17 §D3) —
	// raising Min is a separate migration decision, not a release detail.
	ProtocolMin = V1
	ProtocolMax = V3
)

// Negotiate selects the highest version both peers speak.
//
// The daemon is the selector: the shim advertises its range in Hello and the
// daemon echoes the single selected version in Welcome. Selection is by RANGE
// OVERLAP, never by binary equality — a daemon built months after a shim must
// still adopt it.
//
// A disjoint range is not an error the caller may paper over: it is the
// quarantine trigger in §D7. The returned error wraps ErrVersionMismatch so the
// adoption path can classify it without string matching.
func Negotiate(peerMin, peerMax, ourMin, ourMax uint32) (uint32, error) {
	if peerMin > peerMax {
		return 0, fmt.Errorf("shimwire: %w: peer advertised inverted range [%d,%d]", ErrVersionMismatch, peerMin, peerMax)
	}
	if ourMin > ourMax {
		return 0, fmt.Errorf("shimwire: %w: local inverted range [%d,%d]", ErrVersionMismatch, ourMin, ourMax)
	}
	lo, hi := peerMin, peerMax
	if ourMin > lo {
		lo = ourMin
	}
	if ourMax < hi {
		hi = ourMax
	}
	if lo > hi {
		return 0, fmt.Errorf(
			"shimwire: %w: no overlap between peer [%d,%d] and local [%d,%d]",
			ErrVersionMismatch, peerMin, peerMax, ourMin, ourMax,
		)
	}
	return hi, nil
}

// Extension names understood by the OSS protocol.
const (
	// ExtCarrierEpoch is the ONE generic extension point the OSS protocol
	// defines: a composing carrier that fences its own connection generations
	// puts its epoch here. It deliberately names no relay, service, or endpoint
	// — an OSS-only daemon omits it entirely and nothing degrades (§D3).
	ExtCarrierEpoch = "carrier_epoch"
)

// Extensions is the optional, namespaced negotiation map carried on
// Hello/Welcome.
//
// Optional entries are ignored when unknown. An entry named in Required MUST be
// understood by the receiving peer or negotiation fails CLOSED — an unsupported
// requirement is never downgraded silently, because a silent downgrade is
// indistinguishable from a working session until the missing behaviour matters.
type Extensions struct {
	Values   map[string]string `json:"values,omitempty"`
	Required []string          `json:"required,omitempty"`
}

// supported is the set of extension names this build understands. Membership is
// what makes a Required entry satisfiable.
var supported = map[string]bool{ExtCarrierEpoch: true}

// CheckRequired fails closed when the peer requires an extension this build does
// not understand. An empty Required set always passes, so an OSS-only peer that
// negotiates no extensions is never penalised.
func (e Extensions) CheckRequired() error {
	for _, name := range e.Required {
		if !supported[name] {
			return fmt.Errorf("shimwire: %w: peer requires unsupported extension %q", ErrExtensionUnsupported, name)
		}
	}
	return nil
}

// Get returns an optional extension value. Unknown names read as absent, which
// is the whole point of "unknown OPTIONAL extensions are ignored".
func (e Extensions) Get(name string) (string, bool) {
	v, ok := e.Values[name]
	return v, ok
}

// ExactEqual compares the canonical JSON bytes of two extension maps. Object
// key order is normalized by encoding/json, while required-list order and
// absent-versus-present-empty fields remain exact. Adoption uses this to prove
// the shim committed the precise carrier generation proposed in Welcome.
func (e Extensions) ExactEqual(other Extensions) bool {
	a, err := json.Marshal(e)
	if err != nil {
		return false
	}
	b, err := json.Marshal(other)
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}
