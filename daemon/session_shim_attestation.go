package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SessionShimSupportState is the three-state session-shim support fact a host
// may present, encoded so that each state has exactly one wire form.
//
// A plain bool cannot carry this: with `omitempty` a false is indistinguishable
// from silence, and without it every legacy daemon starts emitting a key it
// never emitted before. Both matter, because a control plane distinguishes
// them — a host it has recorded as shim-enabled must repeat its complete
// attestation, and silence from such a host is refused rather than read as
// "no longer running one". A daemon that cannot compose the shim therefore
// needs a way to SAY so; that is SessionShimStandDown.
//
// The state is a distinct type rather than a *bool for two reasons. Every
// existing equality and copy site keeps working by construction (a *bool turns
// `a.Supported == b.Supported` from a value comparison into a silent pointer
// comparison), and the marshaller stays scoped to this field: encoding/json
// promotes a Marshaler implemented on SessionShimHostAttestation itself onto
// every request type that embeds it, which would replace the whole
// registration body with the attestation tuple.
type SessionShimSupportState uint8

const (
	// SessionShimSupportAbsent presents no support fact at all. It is the zero
	// value, and it emits NO sessionShimSupported key, so a daemon that never
	// touches this field sends the pre-session-shim request bytes exactly.
	SessionShimSupportAbsent SessionShimSupportState = iota

	// SessionShimStandDown explicitly declares that this host is NOT running a
	// session shim. It emits {"sessionShimSupported": false} and no other shim
	// key. This is the one thing silence cannot express: a host that attested
	// support before, and has since lost its composition, says so with this.
	SessionShimStandDown

	// SessionShimSupported is the positive fact, valid only alongside the
	// complete controller/protocol/capability tuple. It emits
	// {"sessionShimSupported": true}.
	SessionShimSupported
)

// MarshalJSON writes the boolean the wire contract defines for the two states
// that have one. Absent has no wire form and never reaches here: `omitempty`
// drops the zero value before the field is encoded, which is what preserves
// the legacy request bytes. Reaching here with Absent means a caller put this
// state somewhere `omitempty` does not cover, where the only two available
// encodings would both be a lie — so it fails loudly instead of picking one.
func (s SessionShimSupportState) MarshalJSON() ([]byte, error) {
	switch s {
	case SessionShimSupported:
		return []byte("true"), nil
	case SessionShimStandDown:
		return []byte("false"), nil
	case SessionShimSupportAbsent:
		return nil, errors.New("session shim attestation: absent support has no wire form; it must be omitted")
	default:
		return nil, fmt.Errorf("session shim attestation: unknown support state %d", uint8(s))
	}
}

// UnmarshalJSON reads the two present forms. An absent key never calls this,
// leaving the field at SessionShimSupportAbsent, so decode is the exact
// inverse of encode across all three states.
func (s *SessionShimSupportState) UnmarshalJSON(data []byte) error {
	var supported bool
	if err := json.Unmarshal(data, &supported); err != nil {
		return fmt.Errorf("session shim attestation: sessionShimSupported must be a boolean: %w", err)
	}
	if supported {
		*s = SessionShimSupported
	} else {
		*s = SessionShimStandDown
	}
	return nil
}

// SessionShimHostAttestation is the additive, non-secret recovery capability
// tuple presented on worker registration and every runtime-token refresh.
//
// The JSON names are deliberately flat: embedding this value in a request
// produces the public wire keys without adding a nested object. Its zero value
// is absent, preserving the pre-session-shim request bytes.
type SessionShimHostAttestation struct {
	Supported    SessionShimSupportState `json:"sessionShimSupported,omitempty"`
	ControllerID string                  `json:"sessionShimControllerId,omitempty"`
	ProtocolMin  uint32                  `json:"sessionShimProtocolMin,omitempty"`
	ProtocolMax  uint32                  `json:"sessionShimProtocolMax,omitempty"`
	Capabilities []string                `json:"sessionShimCapabilities,omitempty"`
}

// SessionShimStandDownAttestation is the attestation a host presents when it
// has no session-shim composition to attest: an explicit "not me", carrying no
// controller, protocol, or capability claim.
//
// It exists so a degraded daemon can complete registration instead of going
// silent. Silence from a host the control plane has recorded as shim-enabled
// is a conflict, and a daemon that cannot get past registration cannot serve
// at all — which turns one unavailable feature into a dead host.
func SessionShimStandDownAttestation() SessionShimHostAttestation {
	return SessionShimHostAttestation{Supported: SessionShimStandDown}
}

// StandsDown reports whether this attestation is the explicit "not running a
// shim" declaration, as opposed to saying nothing at all.
func (a SessionShimHostAttestation) StandsDown() bool {
	return a.Supported == SessionShimStandDown
}

// Supports reports whether this attestation claims session-shim support. It is
// the exported reading of the same fact the package gates recovery on.
func (a SessionShimHostAttestation) Supports() bool { return a.enabled() }

// The closed hosted session-shim capability vocabulary. The canonical tuple is
// the lexical complete set returned by RequiredSessionShimHostCapabilities.
const (
	SessionShimCapabilityAuthoritativeSnapshotV2   = "authoritative_snapshot_v2"
	SessionShimCapabilityCarrierEpochPrepareCommit = "carrier_epoch_prepare_commit"
	// SessionShimCapabilityDurableCarrierProofV1 is retained as a public name
	// for frozen exact same-handoff replay/drain. It is never part of a new
	// admission attestation after proof-v2 cutover.
	SessionShimCapabilityDurableCarrierProofV1 = "durable_carrier_proof_v1"
	SessionShimCapabilityDurableCarrierProofV2 = "durable_carrier_proof_v2"
	SessionShimCapabilityFullHostFrameV3       = "full_host_frame_v3"
	SessionShimCapabilityInteractiveAttachV2   = "interactive_attach_v2"
)

var requiredSessionShimHostCapabilities = []string{
	SessionShimCapabilityAuthoritativeSnapshotV2,
	SessionShimCapabilityCarrierEpochPrepareCommit,
	SessionShimCapabilityDurableCarrierProofV2,
	SessionShimCapabilityFullHostFrameV3,
	SessionShimCapabilityInteractiveAttachV2,
}

// SessionShimCarrierProofV2Readiness is the exact durable dependency required
// before the daemon may advertise or continue using the proof-v2 capability.
// DurableCarrierProofV2Ready is separate carrier-owned durable ACK evidence; it
// is never derived from the four composing support facts.
//
// The five facts are omitted from the wire whenever they are not all true,
// which is exactly the tri-state's non-ready shape: a projection that is not
// ready carries no readiness facts rather than publishing them as false. A
// ready projection has all five true, so its bytes are unchanged.
type SessionShimCarrierProofV2Readiness struct {
	DurableCarrierProofV2Ready          bool `json:"durable_carrier_proof_v2_ready,omitempty"`
	ComposingProofV1WritesClosed        bool `json:"composingProofV1WritesClosed,omitempty"`
	EncryptedOriginalCredentialRetained bool `json:"encryptedOriginalCredentialRetained,omitempty"`
	RemainingValidityConsumeGate        bool `json:"remainingValidityConsumeGate,omitempty"`
	AdoptedCandidateRecovery            bool `json:"adoptedCandidateRecovery,omitempty"`
}

// The readiness tri-state carried by a heartbeat projection.
//
// SessionShimReadinessReady is the established state; the projection omits the
// readiness state field entirely for it, so a healthy beat is byte-identical to
// one produced before the tri-state existed. SessionShimReadinessNotReady is a
// definite refusal — the resolver answered, and the answer withdraws readiness.
// SessionShimReadinessUnknown means the resolver could not be consulted; it
// never withdraws an established readiness, and it is bounded by the
// configurable staleness bound, after which it becomes not-ready.
const (
	SessionShimReadinessReady    = "ready"
	SessionShimReadinessNotReady = "not-ready"
	SessionShimReadinessUnknown  = "unknown"
)

// SessionShimReadinessStaleReason is the readiness reason published when an
// unknown readiness state has persisted past the configured staleness bound.
const SessionShimReadinessStaleReason = "stale"

// The three classes a readiness resolution can fail in. They are wrapped with
// %w on the way out of the resolver and matched with errors.Is, so a reworded
// message can never silently reclassify a failure.
//
// ErrSessionShimReadinessUnavailable classifies a resolver error or timeout:
// the answer is unknown, and unknown never withdraws an established readiness.
// ErrSessionShimReadinessRejected classifies a resolver that answered with an
// incomplete fact set: a definite not-ready that withdraws as before.
// ErrSessionShimReadinessMisconfigured classifies a daemon with no readiness
// resolver configured at all. It is a permanent programming fault, never a
// transient one: it is never degraded to unknown, never cached, and fails
// closed at every seam.
var (
	ErrSessionShimReadinessUnavailable   = errors.New("session shim: readiness is unavailable")
	ErrSessionShimReadinessRejected      = errors.New("session shim: readiness is refused")
	ErrSessionShimReadinessMisconfigured = errors.New("session shim: readiness resolver is misconfigured")
)

func (r SessionShimCarrierProofV2Readiness) validate() error {
	if !r.DurableCarrierProofV2Ready {
		return errors.New("session shim: durable carrier proof v2 readiness acknowledgement is required")
	}
	if !r.ComposingProofV1WritesClosed || !r.EncryptedOriginalCredentialRetained ||
		!r.RemainingValidityConsumeGate || !r.AdoptedCandidateRecovery {
		return errors.New("session shim: durable carrier proof v2 composition support is incomplete")
	}
	return nil
}

// RequiredSessionShimHostCapabilities returns the canonical closed hosted set.
func RequiredSessionShimHostCapabilities() []string {
	return append([]string(nil), requiredSessionShimHostCapabilities...)
}

// SessionShimCredentialReceipt is the exact non-secret authority receipt a
// registration or refresh response must return before its credential can be
// installed or cached for hosted session-shim recovery.
type SessionShimCredentialReceipt struct {
	Enabled          bool     `json:"enabled"`
	State            string   `json:"state"`
	WorkerHostID     string   `json:"workerHostId"`
	AdoptionRevision string   `json:"adoptionRevision"`
	ControllerID     string   `json:"controllerId"`
	ProtocolMin      uint32   `json:"protocolMin"`
	ProtocolMax      uint32   `json:"protocolMax"`
	Capabilities     []string `json:"capabilities"`
}

// SessionShimHeartbeatProjection is one coherent adoption publication sampled
// for a heartbeat. Callers supply the whole value through one callback so host,
// controller, revision, and quarantine state can never come from torn reads.
type SessionShimHeartbeatProjection struct {
	Enabled          bool   `json:"enabled"`
	AdoptionComplete bool   `json:"adoptionComplete"`
	WorkerHostID     string `json:"workerHostId"`
	ControllerID     string `json:"controllerId"`
	AdoptionRevision string `json:"adoptionRevision"`

	// ReadinessState is the tri-state: ready, not-ready, or unknown. It is
	// empty on a ready projection, which is what keeps a healthy beat's bytes
	// identical to a pre-tri-state beat; an empty state reads as ready. Reason
	// and ObservedAt accompany every non-ready state and only a non-ready
	// state. ReadinessObservedAt is the time the CURRENT non-ready state was
	// first observed — for a continuing unknown it is the onset of the
	// degradation, not the time of the latest retry, which is what the
	// staleness bound measures against.
	ReadinessState      string `json:"readinessState,omitempty"`
	ReadinessReason     string `json:"readinessReason,omitempty"`
	ReadinessObservedAt string `json:"readinessObservedAt,omitempty"`

	// SessionShimCarrierProofV2Readiness stays flat on the existing sessionShim
	// object so the five live facts are covered by the same organization-scoped
	// host, controller, and adoption-revision echo. The values come from the
	// configured readiness resolver; capability advertisement is not a fallback.
	SessionShimCarrierProofV2Readiness

	QuarantinedSessions []SessionShimQuarantinedSession `json:"quarantinedSessions"`
}

// SessionShimQuarantinedSession is the bounded heartbeat form of one
// quarantined shim. It intentionally excludes display detail and local paths.
type SessionShimQuarantinedSession struct {
	OrgID        string `json:"orgId"`
	SessionID    string `json:"sessionId"`
	ShimID       string `json:"shimId,omitempty"`
	ProcessEpoch uint64 `json:"processEpoch"`
	// ControllerGeneration is a canonical decimal string so the uint64 fence is
	// not rounded by JSON number consumers. "0" is the explicit unknown value.
	ControllerGeneration string `json:"controllerGeneration"`
	ProtocolMin          uint32 `json:"protocolMin"`
	ProtocolMax          uint32 `json:"protocolMax"`
	Reason               string `json:"reason"`
	AgeSeconds           int64  `json:"ageSeconds"`
	ConsumesCapacity     bool   `json:"consumesCapacity"`
}

func (p SessionShimHeartbeatProjection) validateReady() error {
	if !p.Enabled {
		return errors.New("session shim heartbeat projection is not enabled")
	}
	if !p.AdoptionComplete {
		return errors.New("session shim heartbeat projection is not adoption-complete")
	}
	if p.WorkerHostID == "" || p.ControllerID == "" || p.AdoptionRevision == "" {
		return errors.New("session shim heartbeat projection is missing host, controller, or adoption revision")
	}
	switch p.ReadinessState {
	case "", SessionShimReadinessReady:
		if p.ReadinessReason != "" || p.ReadinessObservedAt != "" {
			return errors.New("session shim heartbeat projection carries a readiness reason for a ready state")
		}
		if err := p.SessionShimCarrierProofV2Readiness.validate(); err != nil {
			return fmt.Errorf("session shim heartbeat projection: %w", err)
		}
	case SessionShimReadinessUnknown, SessionShimReadinessNotReady:
		if p.ReadinessReason == "" || p.ReadinessObservedAt == "" {
			return fmt.Errorf(
				"session shim heartbeat projection %s readiness is missing reason or observed-at", p.ReadinessState,
			)
		}
		if p.SessionShimCarrierProofV2Readiness != (SessionShimCarrierProofV2Readiness{}) {
			return fmt.Errorf(
				"session shim heartbeat projection %s readiness carries proof-v2 facts", p.ReadinessState,
			)
		}
	default:
		return fmt.Errorf("session shim heartbeat projection has invalid readiness state %q", p.ReadinessState)
	}
	for i, q := range p.QuarantinedSessions {
		if q.OrgID == "" || q.SessionID == "" || q.Reason == "" || !q.ConsumesCapacity {
			return fmt.Errorf("session shim heartbeat quarantine %d is incomplete", i)
		}
		generation, err := strconv.ParseUint(q.ControllerGeneration, 10, 64)
		if err != nil || strconv.FormatUint(generation, 10) != q.ControllerGeneration {
			return fmt.Errorf("session shim heartbeat quarantine %d has non-canonical controller generation", i)
		}
		if i > 0 && !sessionShimQuarantineLess(p.QuarantinedSessions[i-1], q) {
			return errors.New("session shim heartbeat quarantine set is not in strict deterministic order")
		}
	}
	return nil
}

// normalizedSessionShimReadinessState maps the two spellings of an established
// readiness onto one. validateReady accepts "" and "ready" interchangeably, and
// the exported SessionShimReadinessReady constant invites a consumer to echo
// "ready" for a healthy beat that sent no field at all; without this the echo
// would pass validation and then fail the exact comparison, breaking every
// healthy beat's response processing.
func normalizedSessionShimReadinessState(state string) string {
	if state == "" {
		return SessionShimReadinessReady
	}
	return state
}

// exactEqual compares the identity of two authority claims.
//
// ReadinessObservedAt is deliberately NOT part of it. It is a timestamp, not an
// authority fact, and an acknowledgement re-samples the projection: including a
// clock reading here makes two samples of one unchanged authority compare
// unequal, and the acknowledgement edge that reopens admission then never
// fires. State and reason are compared, because those do identify the claim.
func (p SessionShimHeartbeatProjection) exactEqual(other SessionShimHeartbeatProjection) bool {
	if p.Enabled != other.Enabled || p.AdoptionComplete != other.AdoptionComplete ||
		p.WorkerHostID != other.WorkerHostID || p.ControllerID != other.ControllerID ||
		p.AdoptionRevision != other.AdoptionRevision ||
		normalizedSessionShimReadinessState(p.ReadinessState) !=
			normalizedSessionShimReadinessState(other.ReadinessState) ||
		p.ReadinessReason != other.ReadinessReason ||
		p.SessionShimCarrierProofV2Readiness != other.SessionShimCarrierProofV2Readiness ||
		len(p.QuarantinedSessions) != len(other.QuarantinedSessions) {
		return false
	}
	for i := range p.QuarantinedSessions {
		if p.QuarantinedSessions[i] != other.QuarantinedSessions[i] {
			return false
		}
	}
	return true
}

func cloneSessionShimHeartbeatProjection(in SessionShimHeartbeatProjection) SessionShimHeartbeatProjection {
	in.QuarantinedSessions = append([]SessionShimQuarantinedSession{}, in.QuarantinedSessions...)
	return in
}

func sessionShimQuarantineLess(a, b SessionShimQuarantinedSession) bool {
	if a.OrgID != b.OrgID {
		return a.OrgID < b.OrgID
	}
	if a.SessionID != b.SessionID {
		return a.SessionID < b.SessionID
	}
	if a.ShimID != b.ShimID {
		return a.ShimID < b.ShimID
	}
	if a.ProcessEpoch != b.ProcessEpoch {
		return a.ProcessEpoch < b.ProcessEpoch
	}
	aGeneration, aErr := strconv.ParseUint(a.ControllerGeneration, 10, 64)
	bGeneration, bErr := strconv.ParseUint(b.ControllerGeneration, 10, 64)
	if aErr == nil && bErr == nil {
		return aGeneration < bGeneration
	}
	return a.ControllerGeneration < b.ControllerGeneration
}

const (
	// SessionShimCredentialStateRecovering is auth-only pre-adoption authority.
	SessionShimCredentialStateRecovering = "recovering"
	// SessionShimCredentialStateReady is post-publication serving authority.
	SessionShimCredentialStateReady = "ready"
)

func (a SessionShimHostAttestation) enabled() bool { return a.Supported == SessionShimSupported }

func (a SessionShimHostAttestation) validate() error {
	if !a.enabled() {
		// Absent and stand-down are both "no shim here", and neither may carry
		// a controller, protocol range, or capability claim: a stand-down that
		// smuggled one would be read as a malformed hosted attestation rather
		// than as the declaration it is.
		if a.ControllerID != "" || a.ProtocolMin != 0 || a.ProtocolMax != 0 || len(a.Capabilities) != 0 {
			return errors.New("session shim attestation: fields require sessionShimSupported")
		}
		if a.Supported != SessionShimSupportAbsent && a.Supported != SessionShimStandDown {
			return fmt.Errorf("session shim attestation: unknown support state %d", uint8(a.Supported))
		}
		return nil
	}
	if a.ControllerID == "" || strings.TrimSpace(a.ControllerID) != a.ControllerID {
		return errors.New("session shim attestation: controller id is required without surrounding whitespace")
	}
	if a.ProtocolMin != 1 || a.ProtocolMax < 3 || a.ProtocolMax < a.ProtocolMin {
		return fmt.Errorf("session shim attestation: invalid protocol range [%d,%d]", a.ProtocolMin, a.ProtocolMax)
	}
	if !canonicalStringSet(a.Capabilities) {
		return errors.New("session shim attestation: capabilities must be sorted, duplicate-free, and non-empty values")
	}
	if !equalStrings(a.Capabilities, requiredSessionShimHostCapabilities) {
		return errors.New("session shim attestation: capabilities must be the exact closed hosted set")
	}
	return nil
}

func (a SessionShimHostAttestation) exactEqual(other SessionShimHostAttestation) bool {
	if a.Supported != other.Supported || a.ControllerID != other.ControllerID ||
		a.ProtocolMin != other.ProtocolMin || a.ProtocolMax != other.ProtocolMax ||
		len(a.Capabilities) != len(other.Capabilities) {
		return false
	}
	for i := range a.Capabilities {
		if a.Capabilities[i] != other.Capabilities[i] {
			return false
		}
	}
	return true
}

func cloneSessionShimHostAttestation(in SessionShimHostAttestation) SessionShimHostAttestation {
	in.Capabilities = append([]string(nil), in.Capabilities...)
	return in
}

func cloneSessionShimCredentialReceipt(in *SessionShimCredentialReceipt) *SessionShimCredentialReceipt {
	if in == nil {
		return nil
	}
	out := *in
	out.Capabilities = append([]string(nil), in.Capabilities...)
	return &out
}

func validateSessionShimCredentialReceipt(
	attestation SessionShimHostAttestation,
	receipt *SessionShimCredentialReceipt,
	workerID string,
) error {
	if !attestation.enabled() {
		return nil
	}
	if err := attestation.validate(); err != nil {
		return err
	}
	if receipt == nil {
		return errors.New("session shim credential receipt is required")
	}
	if !receipt.Enabled {
		return errors.New("session shim credential receipt is not enabled")
	}
	if receipt.State != SessionShimCredentialStateRecovering && receipt.State != SessionShimCredentialStateReady {
		return fmt.Errorf("session shim credential receipt has invalid state %q", receipt.State)
	}
	if receipt.WorkerHostID == "" {
		return errors.New("session shim credential receipt missing stable host authority")
	}
	if receipt.AdoptionRevision == "" {
		return errors.New("session shim credential receipt missing adoption revision")
	}
	if receipt.WorkerHostID == attestation.ControllerID {
		return errors.New("session shim credential receipt aliases controller and stable host authority")
	}
	if workerID != "" && (receipt.WorkerHostID == workerID || attestation.ControllerID == workerID) {
		return errors.New("session shim credential receipt aliases worker registration identity")
	}
	// receipt.Enabled is known true here (checked above), and a receipt echoes a
	// supported attestation or it echoes nothing — there is no stand-down
	// receipt to mirror.
	got := SessionShimHostAttestation{
		Supported:    SessionShimSupported,
		ControllerID: receipt.ControllerID,
		ProtocolMin:  receipt.ProtocolMin,
		ProtocolMax:  receipt.ProtocolMax,
		Capabilities: receipt.Capabilities,
	}
	if !attestation.exactEqual(got) {
		return errors.New("session shim credential receipt does not exactly echo the host attestation")
	}
	return nil
}

func canonicalStringSet(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for i, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return false
		}
		if i > 0 && values[i-1] >= value {
			return false
		}
	}
	return true
}

func canonicalizeStringSet(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("session shim capability set must not be empty")
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	for i, value := range out {
		if value == "" || strings.TrimSpace(value) != value {
			return nil, errors.New("session shim capability names must be non-empty without surrounding whitespace")
		}
		if i > 0 && out[i-1] == value {
			return nil, fmt.Errorf("session shim capability %q is duplicated", value)
		}
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
