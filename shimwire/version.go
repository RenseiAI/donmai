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
	// V4 adds AttributedInput — Input carrying the relay-stamped userId a
	// SYSTEM-authority write is identified by (see ptyhost/systeminput.go) —
	// without changing selected v1/v2/v3 Input (still the exact
	// EncodeInput/DecodeInput byte-identical pair those versions have always
	// carried) or any other selected v1/v2/v3 behavior. Mirrors the V3
	// precedent exactly: a new capability is a new message type legal only at
	// the new selected version, never a silent change to bytes an older
	// selected version already carries (donmai-architecture
	// ADR-2026-08-17-session-shim-adoption.md, rejected alternatives "Add a
	// snapshot-request message to v1" and "Add HostFrame to selected v2").
	// V4 is additive over V3's vocabulary too (HostFrame, SnapshotRequest/
	// Result remain legal) so every existing selected>=V3 code path — which
	// requires ControllerOptions.RequireFullHostFrames — keeps working
	// unchanged at V4.
	V4 uint32 = 4
	// V5 adds correlated out-of-band complete continuation inspection.
	// Canonical host frames and all selected v1-v4 messages remain unchanged.
	V5 uint32 = 5
	// V6 adds the headless workload profile: the runner-owning shape that
	// carries ownership, the generation fence, liveness, stop, credential
	// refresh and the terminal observation, and nothing terminal-shaped. Its
	// vocabulary is NOT a superset of v5: a headless connection refuses every
	// PTY-shaped type (see MessageType.RefusedIn). Selection stays "highest
	// common version", but a headless shim advertises HeadlessRange ([6,6]),
	// which never overlaps an interactive-only peer.
	V6 uint32 = 6

	// HeadlessMin is the headless shim's advertised minimum: v6 exclusively.
	// A daemon that predates v6 has no overlap, so it quarantines the shim as
	// a protocol mismatch — it never treats it as a terminal session and
	// never ends it.
	HeadlessMin uint32 = V6
	// HeadlessMax is the headless shim's advertised maximum: v6 exclusively.
	HeadlessMax uint32 = V6

	// ProtocolMin / ProtocolMax is the range THIS build advertises. A protocol
	// bump widens Max and only ever raises Min after an overlap window at least
	// as long as the maximum supported session duration (ADR-2026-08-17 §D3) —
	// raising Min is a separate migration decision, not a release detail.
	//
	// ProtocolMax stays at V5 until the headless launch gate lands: the v6
	// codec, vocabulary and corpus ship first so every peer can decode the
	// headless range before any shim advertises it or any controller selects
	// it. Widening Max to V6 is a separate release decision, not part of this
	// codec change.
	ProtocolMin = V1
	ProtocolMax = V5
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
	// ExtContinuationCheckpoint advertises optional v5 checkpoint support in
	// Hello. Older controllers ignore its value before selecting their version.
	ExtContinuationCheckpoint = "continuation_checkpoint"
	// ExtWorkload names the closed workload-profile field inside the optional
	// Hello extension map. A headless shim carries ExtWorkload="headless";
	// an interactive shim omits the key entirely. The key must never appear in
	// the required-extension list: an older controller that cannot interpret
	// the profile must still complete version selection (and refuse by range
	// overlap) rather than fail on a requirement it predates.
	ExtWorkload = "workload"
	// WorkloadHeadless is the one workload value a headless shim advertises.
	WorkloadHeadless = "headless"
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
// what makes a Required entry satisfiable. ExtContinuationCheckpoint stays
// optional-only (a required advertisement is refused) and ExtWorkload is
// informational (an older controller must still complete version selection),
// so neither joins this set.
var supported = map[string]bool{ExtCarrierEpoch: true}

// Workload reports the optional workload-profile advertisement carried in the
// Hello extension map. Absence means the interactive profile: every released
// shim predates the field, and only a headless shim sets it. A present but
// unassigned value is a protocol defect, surfaced rather than guessed.
func (e Extensions) Workload() (Profile, error) {
	value, ok := e.Values[ExtWorkload]
	if !ok {
		return ProfileInteractive, nil
	}
	if Profile(value) != ProfileHeadless {
		return "", fmt.Errorf("shimwire: %w: unknown workload %q", ErrMalformed, value)
	}
	return ProfileHeadless, nil
}

// checkWorkloadRange enforces the contract's binding between the workload
// advertisement and the advertised range: the headless key on a shim whose
// range includes a version below 6 is malformed. The value registry alone
// (Extensions.Workload) is not enough, because a headless Hello on [1,6] would
// otherwise decode cleanly as headless while a released controller selects an
// interactive version from the same range. Only the minimum matters: a range
// that starts at 6 or above includes no version below 6.
func checkWorkloadRange(profile Profile, advertisedMin uint32) error {
	if profile == ProfileHeadless && advertisedMin < V6 {
		return fmt.Errorf("shimwire: %w: headless workload on a range that includes v%d", ErrMalformed, advertisedMin)
	}
	return nil
}

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
