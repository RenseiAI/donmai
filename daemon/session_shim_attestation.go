package daemon

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SessionShimHostAttestation is the additive, non-secret recovery capability
// tuple presented on worker registration and every runtime-token refresh.
//
// The JSON names are deliberately flat: embedding this value in a request
// produces the public wire keys without adding a nested object. Its zero value
// is absent, preserving the pre-session-shim request bytes.
type SessionShimHostAttestation struct {
	Supported    bool     `json:"sessionShimSupported,omitempty"`
	ControllerID string   `json:"sessionShimControllerId,omitempty"`
	ProtocolMin  uint32   `json:"sessionShimProtocolMin,omitempty"`
	ProtocolMax  uint32   `json:"sessionShimProtocolMax,omitempty"`
	Capabilities []string `json:"sessionShimCapabilities,omitempty"`
}

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
type SessionShimCarrierProofV2Readiness struct {
	DurableCarrierProofV2Ready          bool `json:"durable_carrier_proof_v2_ready"`
	ComposingProofV1WritesClosed        bool `json:"composingProofV1WritesClosed"`
	EncryptedOriginalCredentialRetained bool `json:"encryptedOriginalCredentialRetained"`
	RemainingValidityConsumeGate        bool `json:"remainingValidityConsumeGate"`
	AdoptedCandidateRecovery            bool `json:"adoptedCandidateRecovery"`
}

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
	if err := p.SessionShimCarrierProofV2Readiness.validate(); err != nil {
		return fmt.Errorf("session shim heartbeat projection: %w", err)
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

func (p SessionShimHeartbeatProjection) exactEqual(other SessionShimHeartbeatProjection) bool {
	if p.Enabled != other.Enabled || p.AdoptionComplete != other.AdoptionComplete ||
		p.WorkerHostID != other.WorkerHostID || p.ControllerID != other.ControllerID ||
		p.AdoptionRevision != other.AdoptionRevision ||
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
	in.QuarantinedSessions = append([]SessionShimQuarantinedSession(nil), in.QuarantinedSessions...)
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

func (a SessionShimHostAttestation) enabled() bool { return a.Supported }

func (a SessionShimHostAttestation) validate() error {
	if !a.Supported {
		if a.ControllerID != "" || a.ProtocolMin != 0 || a.ProtocolMax != 0 || len(a.Capabilities) != 0 {
			return errors.New("session shim attestation: fields require sessionShimSupported")
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
	got := SessionShimHostAttestation{
		Supported:    receipt.Enabled,
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
