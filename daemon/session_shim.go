package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// SessionShimAdoptionEvidence is the exact local fact handed to a composing
// carrier after controller generation commits and before the daemon advertises
// ready. The correlation fields are deliberately per session: session-shim-v1
// defines no host-wide controller generation.
type SessionShimAdoptionEvidence struct {
	Identity               sessionshim.Identity
	HostID                 string
	ControllerID           string
	ShimID                 string
	ProcessEpoch           uint64
	ControllerGeneration   uint64
	LastForwardedSeq       uint64
	Extensions             shimwire.Extensions
	PreparedCorrelation    []byte
	ObservedAtUnixNano     int64
	ProtocolVersion        uint32
	CarrierCompatible      bool
	CarrierIncompatibility SessionShimCarrierIncompatibility
	// SnapshotProxy is the exact just-adopted controller capability. It is
	// callable during OnAdoption, before the controller enters the daemon's
	// published adopted map, so carrier takeover can perform its mandatory fresh
	// resync without a circular lookup. Nil for selected v1.
	SnapshotProxy *SessionShimSnapshotProxy
}

// SessionShimSnapshotProxy exposes only the two fresh authoritative snapshot
// operations. It owns no VT and has no cache/fabrication fallback.
type SessionShimSnapshotProxy struct {
	controller *sessionshim.Controller
	daemon     *Daemon
	identity   sessionshim.Identity
	active     atomic.Bool
}

// Inspect returns a fresh read-only shim-owned screen and atSeq.
func (p *SessionShimSnapshotProxy) Inspect(ctx context.Context) (shimwire.SnapshotResult, error) {
	if p == nil || p.controller == nil || !p.active.Load() {
		return shimwire.SnapshotResult{}, fmt.Errorf("session shim: %w: authoritative snapshot proxy unavailable", shimwire.ErrVersionMismatch)
	}
	return p.controller.InspectSnapshot(ctx)
}

// Emit asks the shim-owned PTY host to emit exactly one snapshot frame and
// waits until the daemon's ordered durable stream consumer has accepted it.
func (p *SessionShimSnapshotProxy) Emit(ctx context.Context) (shimwire.SnapshotResult, error) {
	if p == nil || p.controller == nil || !p.active.Load() {
		return shimwire.SnapshotResult{}, fmt.Errorf("session shim: %w: authoritative snapshot proxy unavailable", shimwire.ErrVersionMismatch)
	}
	staged := false
	if p.daemon != nil {
		cfg := p.daemon.sessionShimConfig()
		if cfg.RequireAuthoritativeSnapshot && cfg.OnAdoptionPublished != nil {
			if err := p.daemon.beginStagedSessionShimSnapshot(p.identity); err != nil {
				return shimwire.SnapshotResult{}, err
			}
			staged = true
		}
	}
	result, err := p.controller.EmitSnapshot(ctx)
	if err != nil || !result.InStream || p.daemon == nil {
		if staged {
			p.daemon.cancelStagedSessionShimSnapshot(p.identity)
		}
		return result, err
	}
	if staged {
		// D14's one staged exception: OnSessionEventDurable stores the paired raw
		// HostFrame and its strict receipt while the ordered event consumer remains
		// blocked on the not-yet-permitted host_ack. The selected-v3 result is
		// correlation-only and carries no duplicate bytes. Waiting for the forwarded
		// cursor here would make local publication depend on activation and deadlock.
		want := result.AtSeq + 1
		if err := p.daemon.waitStagedSessionShimSnapshot(ctx, p.identity, want); err != nil {
			p.daemon.cancelStagedSessionShimSnapshot(p.identity)
			return shimwire.SnapshotResult{}, err
		}
		return result, nil
	}
	// The result is completion/correlation evidence, not a second transmission:
	// the exact frame is delivered once through OnSessionEventDurable. The result
	// is ordered after the emitted frame on shimwire, but takeover is
	// not complete until the daemon's early consumer has durably crossed that
	// frame. This also drains arbitrarily large replay before OnAdoption returns.
	want := result.AtSeq + 1
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for p.daemon.SessionShimForwardedSeq(p.identity.OrgID, p.identity.SessionID) < want {
		select {
		case <-ctx.Done():
			return shimwire.SnapshotResult{}, fmt.Errorf("session shim: wait for emitted snapshot durability: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return result, nil
}

func (p *SessionShimSnapshotProxy) deactivate() {
	if p != nil {
		p.active.Store(false)
	}
}

// SessionShimCarrierIncompatibility is typed capability evidence for a shim
// that remains adopted/capacity-charged but cannot back the composing carrier.
type SessionShimCarrierIncompatibility string

// ErrSessionShimCarrierConfig reports an activation configuration that cannot
// durably consume the authoritative emitted frame.
var ErrSessionShimCarrierConfig = errors.New("session shim: authoritative snapshot carrier configuration incomplete")

const (
	// SessionShimCarrierCompatible means selected local wire satisfies the
	// composing carrier's declared requirements.
	SessionShimCarrierCompatible SessionShimCarrierIncompatibility = ""
	// SessionShimCarrierAuthoritativeSnapshotV2Required reports selected v1's
	// typed external-carrier refusal while retaining local controller ownership.
	SessionShimCarrierAuthoritativeSnapshotV2Required SessionShimCarrierIncompatibility = "authoritative_snapshot_unsupported"
	// SessionShimCarrierDurableHostFrameV3Required reports selected v2's typed
	// external-carrier refusal while retaining ownership and snapshot authority.
	SessionShimCarrierDurableHostFrameV3Required SessionShimCarrierIncompatibility = "durable_host_frame_unsupported"
)

// SessionShimAdoptionPreparation is the post-Hello, pre-Welcome fact supplied
// to a composing carrier reservation. HostID is already resolved for the
// identity's organization; shim/process/generation values come only from the
// authenticated live Hello.
type SessionShimAdoptionPreparation struct {
	Identity                    sessionshim.Identity
	HostID                      string
	ControllerID                string
	ShimID                      string
	ProcessEpoch                uint64
	CurrentControllerGeneration shimwire.Generation
	LocalResumeFrom             uint64
	LastHostSeq                 uint64
	// LastForwardedSeq is a deprecated alias for LocalResumeFrom-1. It is not
	// external carrier proof authority.
	LastForwardedSeq uint64
	SelectedVersion  uint32
}

// SessionShimAdoptionPreparationState is the closed result posture returned by
// the additive proof-v2 composing prepare seam.
type SessionShimAdoptionPreparationState string

const (
	// SessionShimPreparationFreshCandidate follows the full proof-v2 reservation,
	// receipt, mandatory-Snapshot, adoption, and activation pipeline.
	SessionShimPreparationFreshCandidate SessionShimAdoptionPreparationState = "fresh_candidate"
	// SessionShimPreparationAdoptedCandidateRecovery rehydrates the exact
	// already-consumed candidate with its original bearer and retained receipt.
	SessionShimPreparationAdoptedCandidateRecovery SessionShimAdoptionPreparationState = "adopted_candidate_recovery"
)

// SessionShimRecoveryCorrelation is opaque authority returned by a composing
// resolver. Its bytes are private, non-JSON, defensively copied, and redacted
// under every fmt verb.
type SessionShimRecoveryCorrelation struct {
	value []byte
}

// NewSessionShimRecoveryCorrelation freezes exact non-empty opaque bytes.
func NewSessionShimRecoveryCorrelation(value []byte) (SessionShimRecoveryCorrelation, error) {
	if len(value) == 0 {
		return SessionShimRecoveryCorrelation{}, errors.New("session shim: adopted-candidate recovery correlation is empty")
	}
	return SessionShimRecoveryCorrelation{value: append([]byte(nil), value...)}, nil
}

// Bytes returns a defensive copy for the composing callback that owns the
// correlation's protocol. Donmai never parses these bytes.
func (c SessionShimRecoveryCorrelation) Bytes() []byte { return append([]byte(nil), c.value...) }

// IsZero reports whether no recovery correlation is present.
func (c SessionShimRecoveryCorrelation) IsZero() bool { return len(c.value) == 0 }

// Format prevents opaque authority bytes from reaching logs or errors.
func (SessionShimRecoveryCorrelation) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<redacted-session-shim-recovery-correlation>")
}

// SessionShimAdoptedCandidateRecovery contains only the immutable result
// server-resolved after proof/receipt adoption consume. Credential and
// correlation are opaque/non-JSON; the remaining fields are non-secret exact
// comparison evidence.
type SessionShimAdoptedCandidateRecovery struct {
	Credential          attachclient.V2RetainedCredential `json:"-"`
	RecoveryCorrelation SessionShimRecoveryCorrelation    `json:"-"`
	CarrierEpoch        uint64                            `json:"carrierEpoch"`
	PreStageAckSeq      uint64                            `json:"preStageAckSeq"`
	StagedHighWater     uint64                            `json:"stagedHighWater"`
	ResumeFrom          uint64                            `json:"resumeFrom"`
	CredentialExpiresAt time.Time                         `json:"credentialExpiresAt"`
	ResumeDisposition   attachclient.V2ResumeDisposition  `json:"-"`
}

// Format keeps original credential, recovery correlation, and retained raw
// Snapshot bytes out of logs while preserving bounded non-secret comparison
// evidence useful for diagnostics.
func (r SessionShimAdoptedCandidateRecovery) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state,
		"{carrierEpoch:%d preStageAckSeq:%d stagedHighWater:%d resumeFrom:%d credentialExpiresAt:%s authority:%s <redacted-recovery-authority>}",
		r.CarrierEpoch, r.PreStageAckSeq, r.StagedHighWater, r.ResumeFrom,
		r.CredentialExpiresAt.UTC().Format(time.RFC3339), r.ResumeDisposition.Authority)
}

// SessionShimAdoptionPreparationResult is the additive typed prepare result.
// Existing PrepareAdoption callers keep returning sessionshim.PreparedAdoption;
// PrepareAdoptionV2 returns this shape and the two callbacks are mutually
// exclusive.
type SessionShimAdoptionPreparationResult struct {
	State                    SessionShimAdoptionPreparationState  `json:"state"`
	PreparedAdoption         sessionshim.PreparedAdoption         `json:"-"`
	AdoptedCandidateRecovery *SessionShimAdoptedCandidateRecovery `json:"-"`
}

// SessionShimAdoptionEvidenceV2 pairs the unchanged V1 adoption evidence with
// the exact additive proof-v2 preparation outcome for one synchronous callback.
// The recovery credential is not retained in the daemon's adopted-session map.
type SessionShimAdoptionEvidenceV2 struct {
	Evidence          SessionShimAdoptionEvidence
	PreparationResult SessionShimAdoptionPreparationResult
}

// Format prevents PreparedAdoption correlation and retained recovery authority
// from becoming loggable through a detailed struct format.
func (r SessionShimAdoptionPreparationResult) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "{state:%s <redacted-preparation-authority>}", r.State)
}

// SessionShimAdoptionReceipt is opaque durable correlation state returned by a
// composing carrier. Donmai never parses or rewrites it. The exact bytes are
// retained in memory and handed back with terminal evidence so a downstream
// implementation can carry its own fence and adoption revisions without
// importing that control-plane schema into OSS.
type SessionShimAdoptionReceipt struct {
	DurableCorrelation []byte
}

// SessionShimAdoptionOutcome pairs one committed local adoption with the exact
// durable per-session receipt returned by the composing callback.
type SessionShimAdoptionOutcome struct {
	Evidence SessionShimAdoptionEvidence
	Receipt  SessionShimAdoptionReceipt
}

// SessionShimClearedDisposition is the closed set of dispositions a cleared
// quarantine entry may carry. There is exactly one: a lineage can be reported
// abandoned, never dead — absence of evidence proves unobservability, not
// death, the same rule the absent attestation encodes.
type SessionShimClearedDisposition string

// SessionShimDispositionAbandoned is the only admissible cleared disposition.
// The control plane converts the lineage's recovery obligation from active to
// abandoned — it never resolves it, because no terminal evidence exists.
const SessionShimDispositionAbandoned SessionShimClearedDisposition = "abandoned"

// SessionShimClearedReason is a closed token naming why a lineage was cleared
// without terminal evidence. Free-form text is deliberately not admissible:
// the receiver refuses a reason it does not recognize.
type SessionShimClearedReason string

// SessionShimClearedReasonAcceptanceClearWithoutTerminalEvidence is the reason
// the token-gated acceptance quarantine-clear sends: the fixture proved its
// helper's process and record are gone, but no tombstone exists and none may
// be manufactured.
const SessionShimClearedReasonAcceptanceClearWithoutTerminalEvidence SessionShimClearedReason = "acceptance_clear_without_terminal_evidence"

// Known reports whether r is an assigned cleared-reason token.
func (r SessionShimClearedReason) Known() bool {
	return r == SessionShimClearedReasonAcceptanceClearWithoutTerminalEvidence
}

// SessionShimClearedQuarantine reports one currently-quarantined, unterminated
// lineage this daemon is explicitly abandoning rather than silently omitting.
//
// It exists because a complete adoption batch that simply OMITS a lineage the
// composer still holds is refused — genuine omission stays refused — while a
// quarantine cleared without terminal evidence has nothing to report through
// the tombstoned section either. This entry is the third disposition: it
// carries the exact lifecycle identity of the quarantined entry it clears
// (identity fields only — never live-only projection facts such as age or
// capacity charge) plus the explicit disposition and closed reason. The
// receiver converts the lineage's recovery obligation active → abandoned,
// removes it from the completeness set and the host quarantine projection, and
// advances the revision; the batch receipt must echo each entry exactly before
// this daemon forgets the lineage locally.
type SessionShimClearedQuarantine struct {
	OrgID     string `json:"orgId"`
	SessionID string `json:"sessionId"`

	ShimID       string `json:"shimId,omitempty"`
	ProcessEpoch uint64 `json:"processEpoch,omitempty"`
	// ControllerGeneration mirrors the quarantined entry's committed or last
	// authenticated generation. Zero is the explicit conservative "unknown",
	// exactly as on the quarantined projection entry it clears.
	ControllerGeneration uint64 `json:"controllerGeneration"`

	Disposition SessionShimClearedDisposition `json:"disposition"`
	Reason      SessionShimClearedReason      `json:"reason"`
}

// Identity returns the cleared lineage's lifecycle identity.
func (c SessionShimClearedQuarantine) Identity() sessionshim.Identity {
	return sessionshim.Identity{OrgID: c.OrgID, SessionID: c.SessionID}
}

// Validate refuses a cleared entry that does not name an exact shim
// incarnation or that carries anything but the closed disposition/reason
// vocabulary. The incarnation requirement is the same rule the absent
// attestation enforces: clearing "some shim for this session" could abandon a
// live lineage the daemon still owes.
func (c SessionShimClearedQuarantine) Validate() error {
	if err := c.Identity().Validate(); err != nil {
		return err
	}
	if c.ShimID == "" || c.ProcessEpoch == 0 {
		return errors.New("session shim: cleared quarantine requires the exact shim incarnation")
	}
	if c.Disposition != SessionShimDispositionAbandoned {
		return fmt.Errorf("session shim: cleared quarantine disposition %q is not admissible", c.Disposition)
	}
	if !c.Reason.Known() {
		return fmt.Errorf("session shim: cleared quarantine reason %q is not a known token", c.Reason)
	}
	return nil
}

// sortSessionShimClearedQuarantines orders cleared entries by the same full
// tuple SortQuarantined uses, so the section is deterministic across publishes
// and the receiver's byte-exact echo has one canonical order to echo.
func sortSessionShimClearedQuarantines(in []SessionShimClearedQuarantine) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].OrgID != in[j].OrgID {
			return in[i].OrgID < in[j].OrgID
		}
		if in[i].SessionID != in[j].SessionID {
			return in[i].SessionID < in[j].SessionID
		}
		if in[i].ShimID != in[j].ShimID {
			return in[i].ShimID < in[j].ShimID
		}
		if in[i].ProcessEpoch != in[j].ProcessEpoch {
			return in[i].ProcessEpoch < in[j].ProcessEpoch
		}
		return in[i].ControllerGeneration < in[j].ControllerGeneration
	})
}

// SessionShimAdoptionBatch is one complete per-organization startup
// publication. ExpectedRevision is opaque compare-and-swap state resolved just
// before publication; all outcome slices are complete and deterministic.
type SessionShimAdoptionBatch struct {
	OrgID            string
	HostID           string
	ExpectedRevision []byte
	Adopted          []SessionShimAdoptionOutcome
	Quarantined      []sessionshim.QuarantinedSession
	Tombstoned       []SessionShimTerminalEvidence
	// Cleared enumerates currently-quarantined unterminated lineages this batch
	// explicitly abandons. Each entry must name a lineage the receiver still
	// holds quarantined; the commit removes it from the completeness set and the
	// host quarantine projection. A batch that OMITS a held lineage without a
	// cleared entry remains refused.
	Cleared []SessionShimClearedQuarantine
}

// SessionShimAdoptionBatchReceipt is the durable host-level revision retained
// after a complete batch publication.
type SessionShimAdoptionBatchReceipt struct {
	DurableCorrelation []byte
	// AdoptionRevision is the non-secret current scope revision returned after
	// the complete batch commit. Hosted attested recovery requires it; legacy
	// composing callbacks may leave it empty for source compatibility.
	AdoptionRevision string
	// Cleared echoes the batch's cleared entries exactly — same entries, same
	// order, byte-identical fields. The daemon refuses a receipt whose echo
	// disagrees and keeps projecting the lineage quarantined; it forgets a
	// cleared lineage only after this confirmed commit.
	Cleared []SessionShimClearedQuarantine
}

// SessionShimScopeCredentialReceipt is the bounded non-secret authority fact
// Donmai retains for one served scope during auth-only recovery. Bearers remain
// owned by the composing credential hook and never enter this type.
type SessionShimScopeCredentialReceipt struct {
	Scope            string
	WorkerHostID     string
	AdoptionRevision string
}

// SessionShimCarrierActivation identifies one exact v2 candidate that must be
// active after local adoption publication. CarrierEpoch is comparison evidence,
// never lifecycle identity or authority by itself.
type SessionShimCarrierActivation struct {
	OrgID        string
	SessionID    string
	CarrierEpoch uint64
}

// SessionShimCarrierActivationReceipt is the exact carrier_active evidence
// returned by the post-publication hook. AckSeq must exactly resolve the staged
// mandatory Snapshot for this correlation before Donmai advances shim heartbeat.
type SessionShimCarrierActivationReceipt struct {
	Activation SessionShimCarrierActivation
	AckSeq     uint64
}

// SessionShimControlRef is the exact authenticated local authority captured
// from one adoption. It contains no credential or terminal data. Every field
// must still match the daemon's current adopted controller before a composed
// carrier callback may mutate the shim.
type SessionShimControlRef struct {
	Identity             sessionshim.Identity
	ShimID               string
	ProcessEpoch         uint64
	ControllerGeneration uint64
}

// SessionShimPublishedBatchReceipt is the non-secret activation view of one
// retained scope batch. The callback never receives opaque durable selectors.
type SessionShimPublishedBatchReceipt struct {
	Scope            string
	AdoptionRevision string
}

// SessionShimAdoptionPublication is the immutable non-secret input to the
// post-publication activation hook.
type SessionShimAdoptionPublication struct {
	ControllerID string
	Batches      []SessionShimPublishedBatchReceipt
	Carriers     []SessionShimCarrierActivation
}

// SessionShimAbsentAttestation reports a lineage this daemon PROVED it can no
// longer observe, without ever having watched it terminate.
//
// It exists because there are two facts a daemon can hold about a shim that is
// gone, and only one of them is a tombstone. A shim that exits cleanly writes
// its own terminal record: exit code, last sequence, and positive proof that
// it reaped its harness process group. A shim that is SIGKILLed, or whose
// registry record is removed underneath it, writes nothing — and the lineage
// it leaves behind is unreportable. It cannot be tombstoned, because no
// tombstone exists and manufacturing one would forge the reap proof a claim
// release depends on; and it cannot simply be dropped, because a complete
// adoption batch that omits a lineage the composer still holds is refused.
//
// So this says only what was actually checked, and the receiver must treat it
// as strictly weaker than a tombstone: enough to stop carrying a lineage that
// can no longer be observed, never enough to conclude the harness died. The
// shim vanishing says nothing about the process group it was supervising.
type SessionShimAbsentAttestation struct {
	// ProcessIdentityAbsent is true when the recorded (pid, start time) pair is
	// proven not to be running. The start time is what makes this a proof
	// rather than a guess: a bare pid can be reused by an unrelated process.
	ProcessIdentityAbsent bool
	// RegistryRecordAbsent is true when the shim's registry record is gone.
	//
	// Both facts are required together, and they are separate fields rather
	// than one boolean because either alone means something different: a dead
	// process with a live record is still adoptable, and a missing record with
	// a live process is a shim no daemon may forget about.
	RegistryRecordAbsent bool
	ObservedAtUnixNano   int64
}

// Complete reports whether this attestation proves both of its facts. A
// partial attestation is not weaker evidence, it is no evidence.
func (a SessionShimAbsentAttestation) Complete() bool {
	return a.ProcessIdentityAbsent && a.RegistryRecordAbsent && a.ObservedAtUnixNano > 0
}

// SessionShimTerminalEvidence is emitted after a tombstone positively proves
// process-group reap, or — carrying Absent instead — after this daemon proved
// a lineage is no longer observable. It carries every local correlation plus
// the exact opaque adoption receipt returned above. When a daemon discovers a
// tombstone after an unplanned gap, Adoption is nil: the callback receives the
// positive tombstone as such and must not manufacture a live-adoption fact the
// daemon never observed.
type SessionShimTerminalEvidence struct {
	Identity     sessionshim.Identity
	HostID       string
	ShimID       string
	ProcessEpoch uint64
	// Absent, when set, replaces Tombstone: this evidence proves the lineage is
	// unobservable, not that it ended. The two are mutually exclusive, and a
	// receiver that treats an attestation as a reap proof reintroduces exactly
	// the double execution the restart fence exists to prevent.
	Absent *SessionShimAbsentAttestation
	// Adoption is present when this daemon observed the live controller
	// generation that preceded the terminal fact. It is nil for an orphan
	// tombstone discovered after restart; D9 permits that authenticated positive
	// proof without fabricating a live-adoption receipt.
	Adoption                   *SessionShimAdoptionEvidence
	DurableAdoptionCorrelation []byte
	Tombstone                  sessionshim.Tombstone
}

// SessionShimConfig configures per-session shim ownership and daemon adoption
// (ADR-2026-08-17). EnableAdoption defaults to false, preserving the accepted
// migration law and standalone behavior.
type SessionShimConfig struct {
	// EnableAdoption turns on the startup adoption pass (§D11 step 3).
	EnableAdoption bool

	// EnableOwnership makes this daemon LAUNCH new interactive sessions under
	// per-session shim ownership (§D11 step 2). It is separate from
	// EnableAdoption on purpose, and the separation is the migration order
	// itself: a fleet turns adoption on first so a daemon can take over shims it
	// finds, and only then starts creating them. Reversing that would produce
	// shims no daemon in the fleet knows how to adopt.
	EnableOwnership bool

	// ControllerID optionally overrides the opaque process-scoped controller
	// correlation. Empty generates one high-entropy id once in daemon.New.
	ControllerID string

	// RequireCredentialAttestation enables the D12 hosted-recovery contract.
	// The daemon constructs one immutable attestation from its resolved
	// controller id, shimwire protocol range, and AttestationCapabilities. The
	// zero value keeps registration, refresh, cache, heartbeat, and startup order
	// byte-compatible with pre-D12 embedders.
	RequireCredentialAttestation bool

	// AttestationCapabilities is the caller-declared, non-secret capability set.
	// It is sorted and frozen once in New; duplicates and empty names are refused.
	// Hosted activation requires the exact closed set returned by
	// RequiredSessionShimHostCapabilities.
	AttestationCapabilities []string

	// GetCarrierProofV2Readiness returns current, independently persisted proof-
	// v2 readiness. Donmai calls it before initial registration, every refresh
	// installation, and every hosted heartbeat projection. The carrier-owned
	// durable acknowledgement and all four composing support facts must each be
	// explicitly true; one is never inferred from the others.
	GetCarrierProofV2Readiness func() (SessionShimCarrierProofV2Readiness, error)

	// AcquireRecoveryScopes performs auth-only acquisition for served scopes in
	// addition to the primary registration scope. It retains all credentials and
	// returns only deterministic non-secret scope/host/revision receipts. Its
	// result must exactly cover AdoptionBatchOrgIDs excluding the primary scope.
	//
	// primary is the primary scope's receipt exactly as the founding round trip
	// resolved it — the registration on the inline path, the declaring refresh
	// on a deferred composition install: Scope is OrgID, WorkerHostID the stable
	// host authority, AdoptionRevision the revision the control plane answered
	// with. It is non-secret, and the embedder must RETAIN it for its own
	// host-authority lookups rather than re-derive it: the only other way to
	// learn the host id is to present the attestation again, and a presentation
	// outside the credential refresher's lock races the lanes the refresher
	// keeps on one identity — the control plane sees the posture flip-flop,
	// answers an attestation conflict, and revokes the losing credential. The
	// hook is called AFTER the primary receipt is retained here and BEFORE any
	// readiness resolver that needs the host id runs, so the embedder can
	// record it first.
	AcquireRecoveryScopes func(ctx context.Context, attestation SessionShimHostAttestation, primary SessionShimScopeCredentialReceipt) ([]SessionShimScopeCredentialReceipt, error)

	// RequireAuthoritativeSnapshot declares that the composing external attach
	// carrier needs fresh inspect/emit proxying and the selected-v3 raw HostFrame
	// rail. It also requires the exact hosted credential attestation. Selected v1
	// and v2 are still adopted and capacity-charged, but their carrier callbacks
	// are not activated.
	RequireAuthoritativeSnapshot bool

	// EventBacklogBudget overrides the per-session controller event backlog
	// budget, in payload bytes. Zero uses sessionshim.EventBacklogBudget, which
	// equals the shim's own output ring budget. A host running many sessions at
	// once may want a smaller per-session budget; setting it BELOW what the
	// shim's ring holds makes the daemon the first component to give up on a
	// burst again.
	EventBacklogBudget int

	// OrgID is the organization half of the lifecycle identity (§D2). A
	// standalone OSS daemon has no organization boundary, so it defaults to
	// "local" — a real value rather than an empty one, because the identity is
	// hashed into a filename and an empty half would make every session on the
	// host key off its session id alone with no room to ever add a second tenant.
	OrgID string

	// HostID is the durable host authority named by restart fences. It is NOT
	// the controller id or worker-registration id. Empty means no stable host
	// identity is exposed; it never falls back to another correlation.
	HostID string

	// HostIDForOrg resolves the durable host authority inside one organization.
	// A multi-organization hosted composition uses this because worker-host row
	// ids may be tenant-scoped. It supersedes HostID for non-empty org ids. Error
	// fails adoption/fencing closed; no worker/controller id is substituted.
	HostIDForOrg func(context.Context, string) (string, error)

	// LaunchTimeout bounds how long a launch waits for the new shim to publish
	// its discovery record and complete a handshake. Zero uses
	// defaultShimLaunchTimeout.
	LaunchTimeout time.Duration

	// RegistryDir is where discovery records, sockets, and tombstones live.
	// Empty resolves through the injected state-directory seam, so no
	// install-specific path is compiled in.
	RegistryDir string

	// Orphan bounds the shim-owned controller-loss rule (§D8). A zero policy
	// uses sessionshim.DefaultOrphanPolicy.
	//
	// ExternalReleaseThreshold is how a composing deployment declares the
	// smallest interval after which something OUTSIDE this host would consider a
	// session abandoned. Setting it makes the §D8 inequality checkable; leaving
	// it zero means "nothing external can release a claim", which is true of a
	// standalone daemon.
	Orphan sessionshim.OrphanPolicy

	// RestartBudget is how long a planned restart is expected to take. It sizes
	// the restart fence's hold window together with the orphan bound.
	RestartBudget time.Duration

	// FenceStore is the OPTIONAL composing-plane restart-fence persister (§D9).
	// Nil is fully supported: a standalone daemon has no remote reaper to fence
	// against and still gets the local bounded-orphan rule. This field retains the
	// v0.67 source contract; hosted activation uses ExactFenceStore below.
	FenceStore sessionshim.FenceStore

	// ExactFenceStore is the additive hosted restart-fence persister. When set,
	// RequestSessionShimRestartFence uses the exact request-byte and durable
	// revision contract. It is separate so the v0.67 FenceStore field remains
	// source-compatible for OSS embedders.
	ExactFenceStore sessionshim.ExactFenceStore

	// PrepareAdoption runs after the live shim's authenticated Hello exposes its
	// current per-shim generation and before Welcome proposes new authority. It
	// atomically resolves the exact next generation and generic carrier_epoch
	// extension, allowing a durable carrier reservation to bind the two. An error
	// aborts startup rather than producing a ready-but-unreachable session.
	PrepareAdoption func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error)

	// PrepareAdoptionV2 is the additive typed proof-v2 prepare seam. It is
	// mutually exclusive with PrepareAdoption. The adopted_candidate_recovery
	// result explicitly supplies retained original-credential, remaining-validity,
	// candidate, Snapshot, cursor, and recovery-correlation evidence.
	PrepareAdoptionV2 func(context.Context, SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error)

	// OnAdoption runs after the shim commits the new controller generation and
	// before readiness/claim advertisement. It returns only after the composing
	// layer has durably rehydrated its external carrier and, when applicable,
	// posted adoption evidence. Its opaque receipt is retained for terminal
	// correlation. An error aborts the launch/startup pass fail-closed.
	OnAdoption func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error)

	// OnAdoptionV2 consumes the additive typed proof-v2 preparation outcome. It
	// is mutually exclusive with OnAdoption and is required with
	// PrepareAdoptionV2, keeping the original callback and evidence shape frozen.
	OnAdoptionV2 func(context.Context, SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error)

	// OnTerminalEvidence runs after exact process-group reap proof exists and
	// before Donmai disposes the tombstone. It must durably post or retain the
	// terminal fact before returning nil. Error retains the tombstone for exact,
	// idempotent replay on a later reconciliation/startup pass.
	OnTerminalEvidence func(context.Context, SessionShimTerminalEvidence) error

	// AdoptionBatchOrgIDs enumerates organizations that require a complete host
	// adoption publication even when no shim record exists for that scope.
	AdoptionBatchOrgIDs []string

	// PrepareAdoptionBatch resolves the opaque expected host revision used by
	// the composing store's final per-org compare-and-swap.
	PrepareAdoptionBatch func(context.Context, string, string) ([]byte, error)

	// OnAdoptionBatch atomically publishes the complete adopted/quarantined/
	// tombstoned/cleared outcome after every per-session durable callback and
	// before adoptionComplete/Ready. Error fails startup closed. When the batch
	// carries cleared entries, the returned receipt must echo them exactly
	// (same entries, same order); the daemon refuses a receipt that does not.
	OnAdoptionBatch func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)

	// OnAdoptionPublished is the D13 activation edge. It runs only after every
	// per-session and per-scope commit and after the complete local set is
	// published. It must return the exact complete carrier set it activated plus
	// each exact carrier_active durable cursor.
	OnAdoptionPublished func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error)

	// OnCarrierActivationAcknowledged is the infallible local-authority release
	// edge for one exact scope/revision. It runs only after the server has echoed
	// that current revision on heartbeat, before global admission may reopen.
	// Stale, foreign, readiness-only, and no-pending acknowledgements never call
	// it. A composer uses this to release its retained remote-ACKed carrier set.
	// The hook is synchronous, bounded, infallible, and non-reentrant: Donmai
	// holds the publication serialization barrier while invoking it, so it must
	// not call back into adoption, publication, or heartbeat acknowledgement.
	OnCarrierActivationAcknowledged func(SessionShimPublishedBatchReceipt)

	// OnAdoptionActivated announces that an adoption publication has completed
	// carrier activation for one scope and is now waiting only for the control
	// plane to echo that exact revision back on heartbeat. It runs once per
	// scope still awaiting acknowledgement, in scope order.
	//
	// It exists for an embedder that owns a scope's own heartbeat lane. Donmai
	// rings the lane it owns immediately (HeartbeatService.SendNow) so a
	// dynamically published adoption does not sit behind a full heartbeat
	// interval; this hook is that same edge offered to lanes donmai does not
	// own, so a satellite scope need not wait one out either.
	//
	// Optional, best-effort, bounded by CallbackTimeout, and never fatal — an
	// adoption that already committed is not undone by a hook. It MAY call
	// heartbeat send paths; it MUST NOT call back into adoption or publication.
	//
	// Ordering: donmai fires this AFTER its own immediate beat, and on the
	// dynamic launch path only once the publication serialization barrier has
	// been released. Firing it under that barrier would forbid the very thing
	// the hook is for — a beat whose acknowledgement needs the same barrier
	// would deadlock against the launch that raised it.
	OnAdoptionActivated func(ctx context.Context, scope, adoptionRevision string)

	// CallbackTimeout bounds PrepareAdoption, OnAdoption, and
	// OnTerminalEvidence. Zero uses the launch timeout/default.
	CallbackTimeout time.Duration

	// ExpectedWorkarea returns the workarea this daemon believes a session
	// belongs to, for the adoption-time workarea identity check. Nil skips only
	// the daemon-side half; the record-versus-live-shim half always runs.
	ExpectedWorkarea func(orgID, sessionID string) string
	// ExpectedWorkareaRoot cross-checks the optional session-owned root in new
	// records. Old records without it remain adoptable.
	ExpectedWorkareaRoot func(orgID, sessionID string) string

	// OnSessionEvent, when set, receives EVERY event from every adopted session:
	// output frames carrying the shim-allocated sequence, declared gaps,
	// snapshots, error frames, and the terminal Exit.
	//
	// It is the attachment point for a composing carrier. The daemon deliberately
	// does not interpret output — §D5 makes the shim the sole allocator of host
	// sequence, and a daemon that re-framed or renumbered what it forwards would
	// be fabricating the continuity the ADR forbids. What arrives here is what
	// the shim produced, in order, gaps included.
	//
	// It is called from the per-session consumer goroutine and MUST NOT block
	// indefinitely: a stalled consumer stops acknowledgements, which costs the
	// next adoption an avoidable replay gap.
	OnSessionEvent func(sessionshim.Identity, sessionshim.ControllerEvent)

	// OnSessionEventDurable is the optional durable carrier handoff. Unlike
	// OnSessionEvent, this callback is not an observer: a nil callback means no
	// carrier durability is available, and a non-nil callback must return nil
	// only after it has durably accepted the event. Output and snapshot sequence
	// state advances, and the shim heartbeat acknowledgement is sent, only after
	// that successful return. It MUST be bounded for the same reason as
	// OnSessionEvent.
	OnSessionEventDurable func(sessionshim.Identity, sessionshim.ControllerEvent) error

	// ResumeFrom returns the first output sequence the composing durable store
	// still needs (its last_forwarded_seq + 1). Nil delegates to the shim's
	// fsync-backed ACK sidecar, or the start of the stream when none exists. An
	// explicit callback may advance but cannot regress that sidecar silently.
	ResumeFrom func(orgID, sessionID string) uint64
}

func (c SessionShimConfig) requiresStableHostIdentity() bool {
	return c.HostIDForOrg != nil || c.PrepareAdoption != nil || c.PrepareAdoptionV2 != nil || c.OnAdoption != nil || c.OnAdoptionV2 != nil ||
		c.OnTerminalEvidence != nil || c.PrepareAdoptionBatch != nil || c.OnAdoptionBatch != nil ||
		c.OnAdoptionPublished != nil || c.FenceStore != nil || c.ExactFenceStore != nil
}

func (c SessionShimConfig) validateSnapshotCarrier() error {
	if c.PrepareAdoption != nil && c.PrepareAdoptionV2 != nil {
		return fmt.Errorf("%w: PrepareAdoption and PrepareAdoptionV2 cannot both be configured", ErrSessionShimCarrierConfig)
	}
	if c.OnAdoption != nil && c.OnAdoptionV2 != nil {
		return fmt.Errorf("%w: OnAdoption and OnAdoptionV2 cannot both be configured", ErrSessionShimCarrierConfig)
	}
	if (c.PrepareAdoptionV2 == nil) != (c.OnAdoptionV2 == nil) {
		return fmt.Errorf("%w: PrepareAdoptionV2 and OnAdoptionV2 must be configured together", ErrSessionShimCarrierConfig)
	}
	if !c.RequireAuthoritativeSnapshot {
		return nil
	}
	if !c.RequireCredentialAttestation {
		return fmt.Errorf("%w: RequireAuthoritativeSnapshot needs the exact hosted credential attestation", ErrSessionShimCarrierConfig)
	}
	if c.GetCarrierProofV2Readiness == nil {
		return fmt.Errorf("%w: RequireAuthoritativeSnapshot needs proof-v2 readiness evidence", ErrSessionShimCarrierConfig)
	}
	if (c.PrepareAdoption == nil && c.PrepareAdoptionV2 == nil) || (c.OnAdoption == nil && c.OnAdoptionV2 == nil) ||
		c.OnSessionEventDurable == nil || c.OnAdoptionBatch == nil || c.OnAdoptionPublished == nil ||
		c.OnCarrierActivationAcknowledged == nil {
		return fmt.Errorf("%w: RequireAuthoritativeSnapshot needs PrepareAdoption, OnAdoption, OnSessionEventDurable, OnAdoptionBatch, OnAdoptionPublished, and OnCarrierActivationAcknowledged", ErrSessionShimCarrierConfig)
	}
	if c.ResumeFrom != nil {
		return fmt.Errorf("%w: proof-resolving PrepareAdoption and free-standing ResumeFrom cannot both be configured", ErrSessionShimCarrierConfig)
	}
	return nil
}

func (d *Daemon) resolveSessionShimCarrierProofV2Readiness() (SessionShimCarrierProofV2Readiness, error) {
	if !d.sessionShimEnabled() {
		return SessionShimCarrierProofV2Readiness{}, nil
	}
	resolve := d.sessionShimConfig().GetCarrierProofV2Readiness
	if resolve == nil {
		return SessionShimCarrierProofV2Readiness{}, errors.New("session shim: proof-v2 readiness resolver is required")
	}
	readiness, err := resolve()
	if err != nil {
		return SessionShimCarrierProofV2Readiness{}, fmt.Errorf("session shim: resolve proof-v2 readiness: %w", err)
	}
	if err := readiness.validate(); err != nil {
		return SessionShimCarrierProofV2Readiness{}, err
	}
	return readiness, nil
}

func (d *Daemon) validateSessionShimCarrierProofV2Readiness() error {
	_, err := d.resolveSessionShimCarrierProofV2Readiness()
	return err
}

// beginSessionShimRecoveryHeartbeatBarrier closes every new-work rail before a
// dynamic adoption publication can change a scope's authoritative revision.
// Carrier activation is necessary but does not clear this fence: only the
// exact current server-echoed heartbeat does.
func (d *Daemon) beginSessionShimRecoveryHeartbeatBarrier() {
	if !d.sessionShimEnabled() {
		return
	}
	d.lifecycleMu.Lock()
	if d.spawner != nil {
		d.spawner.Pause()
	}
	if d.stopGen == nil {
		switch d.State() {
		case StateRunning, StateStopped:
			d.setState(StateRecovering)
		}
	}
	d.lifecycleMu.Unlock()
}

// withdrawSessionShimProofV2Readiness closes every new-work rail on the first
// dynamic readiness failure. The atomic fence flips first so heartbeat capacity,
// poll/claim, and Daemon.AcceptWork fail closed even while the spawner/lifecycle
// projections are converging on recovering.
func (d *Daemon) withdrawSessionShimProofV2Readiness() {
	if !d.sessionShimEnabled() ||
		!d.sessionShimReadinessWithdrawn.CompareAndSwap(false, true) {
		return
	}
	d.lifecycleMu.Lock()
	if d.spawner != nil {
		d.spawner.Pause()
	}
	if d.stopGen == nil && d.State() == StateRunning {
		d.setState(StateRecovering)
	}
	d.lifecycleMu.Unlock()
	slog.Warn("session shim: proof-v2 readiness withdrawn; admission paused")
}

// sessionShimPublicationCheckpoint is one scope's exact publication posture,
// captured under publicationMu immediately before a dynamic adoption attempt
// mutates it. It exists because the attempt closes the heartbeat lane
// (carrierActivationComplete drops, the recovery barrier pauses admission) on
// the promise that the attempt will either commit or restore — and before this
// checkpoint existed the failure path restored nothing: the projection kept
// erroring, HeartbeatService skipped every beat, and a NACKed launch left the
// daemon permanently silent with no repair channel.
type sessionShimPublicationCheckpoint struct {
	scope                       string
	adoptionComplete            bool
	carrierActivationComplete   bool
	adoptionCompletedAtUnixNano int64
	pendingRevision             string
	pendingAckPresent           bool
	readinessWithdrawn          bool
	lifecycleState              State
	spawnerAccepting            bool
}

// checkpointSessionShimPublication snapshots the last-committed posture for
// one scope. Callers must hold publicationMu: the checkpoint is only coherent
// while no other dynamic publication can move the flags it captures.
func (d *Daemon) checkpointSessionShimPublication(scope string) sessionShimPublicationCheckpoint {
	cp := sessionShimPublicationCheckpoint{
		scope:              scope,
		lifecycleState:     d.State(),
		readinessWithdrawn: d.sessionShimReadinessWithdrawn.Load(),
	}
	if d.spawner != nil {
		cp.spawnerAccepting = d.spawner.IsAccepting()
	}
	d.shims.mu.RLock()
	cp.adoptionComplete = d.shims.adoptionComplete
	cp.carrierActivationComplete = d.shims.carrierActivationComplete
	cp.adoptionCompletedAtUnixNano = d.shims.adoptionCompletedAtUnixNano
	cp.pendingRevision, cp.pendingAckPresent = d.shims.pendingHeartbeatAcks[scope]
	d.shims.mu.RUnlock()
	return cp
}

// rollbackSessionShimPublication restores the pre-attempt posture after a
// dynamic publication failed BEFORE its durable batch committed.
//
// The beat is the repair channel and must never fall silent: restoring the
// last-committed projection means the next beat re-attests exactly the state
// the platform last acknowledged, so a control plane that cleared or demoted
// the host's row while the attempt was in flight receives a correcting beat
// instead of eternal silence. The barrier's invariant — never announce an
// adoption that did not commit — holds because everything restored here was
// already announced and acknowledged; the rolled-back beat says nothing new.
//
// Admission reopens with the projection: nothing durable advanced, so the base
// a queued launch would publish from is exactly the base this attempt started
// from, and there is nothing left for a heartbeat acknowledgement to clear on
// this scope's behalf (on the measured failure path the readiness fence was
// never even withdrawn, so no acknowledgement edge could ever have reopened
// admission — the restore has to do it directly).
//
// Callers must hold publicationMu.
func (d *Daemon) rollbackSessionShimPublication(cp sessionShimPublicationCheckpoint) {
	d.shims.mu.Lock()
	d.shims.adoptionComplete = cp.adoptionComplete
	d.shims.carrierActivationComplete = cp.carrierActivationComplete
	d.shims.adoptionCompletedAtUnixNano = cp.adoptionCompletedAtUnixNano
	if cp.pendingAckPresent {
		d.shims.pendingHeartbeatAcks[cp.scope] = cp.pendingRevision
	} else {
		delete(d.shims.pendingHeartbeatAcks, cp.scope)
	}
	// Same critical section as the pending-ack bookkeeping, mirroring
	// updateSessionShimAdoptionRevision: a heartbeat must never observe the
	// fence without the bookkeeping it fences, in either direction.
	d.sessionShimReadinessWithdrawn.Store(cp.readinessWithdrawn)
	d.shims.mu.Unlock()
	d.lifecycleMu.Lock()
	if d.stopGen == nil && d.State() == StateRecovering && cp.lifecycleState != StateRecovering {
		d.setState(cp.lifecycleState)
	}
	if d.spawner != nil && cp.spawnerAccepting {
		d.spawner.Resume()
	}
	d.lifecycleMu.Unlock()
	slog.Warn("session shim: dynamic publication failed; rolled back to the last-committed projection so the heartbeat lane stays live",
		"scope", cp.scope)
}

// restoreSessionShimHeartbeatLane re-arms only the heartbeat projection after
// a dynamic publication whose durable batch DID commit could not complete
// locally (a stale published entry, a refused carrier activation).
//
// The committed revision is real and retained, so admission deliberately stays
// latched closed exactly as before — but the projection flags are restored so
// the beat keeps flowing and attests the committed truth, rather than skipping
// forever with "carrier activation is not complete" while the platform's row
// decays with no channel left to repair it.
//
// Callers must hold publicationMu.
func (d *Daemon) restoreSessionShimHeartbeatLane(cp sessionShimPublicationCheckpoint) {
	d.shims.mu.Lock()
	d.shims.adoptionComplete = cp.adoptionComplete
	d.shims.carrierActivationComplete = cp.carrierActivationComplete
	d.shims.mu.Unlock()
	slog.Warn("session shim: committed dynamic publication could not complete; admission stays closed but the heartbeat lane stays live",
		"scope", cp.scope)
}

// AcknowledgeSessionShimRecoveryHeartbeat is the only dynamic reopening edge.
// Primary and satellite heartbeat services invoke it with their explicit scope
// after an exact server acknowledgement. A second live readiness resolution is
// required so a stale positive projection cannot reopen a daemon after either
// the scope revision or durable store changed in flight.
func (d *Daemon) AcknowledgeSessionShimRecoveryHeartbeat(
	orgID string,
	acknowledged SessionShimHeartbeatProjection,
) {
	if !d.sessionShimEnabled() || !d.sessionShimReadinessWithdrawn.Load() {
		return
	}
	d.shims.publicationMu.Lock()
	defer d.shims.publicationMu.Unlock()
	if !d.sessionShimReadinessWithdrawn.Load() || d.shims.dynamicPublicationFailed {
		return
	}
	current, err := d.SessionShimHeartbeatProjection(orgID)
	if err != nil || !acknowledged.exactEqual(current) {
		return
	}
	var activationAcknowledgement *SessionShimPublishedBatchReceipt
	d.shims.mu.Lock()
	if len(d.shims.pendingHeartbeatAcks) > 0 {
		pendingRevision, pending := d.shims.pendingHeartbeatAcks[orgID]
		if !pending || pendingRevision != acknowledged.AdoptionRevision {
			d.shims.mu.Unlock()
			return
		}
		delete(d.shims.pendingHeartbeatAcks, orgID)
		activationAcknowledgement = &SessionShimPublishedBatchReceipt{
			Scope: orgID, AdoptionRevision: pendingRevision,
		}
	}
	pendingScopes := len(d.shims.pendingHeartbeatAcks)
	d.shims.mu.Unlock()
	if activationAcknowledgement != nil {
		if hook := d.sessionShimConfig().OnCarrierActivationAcknowledged; hook != nil {
			hook(*activationAcknowledgement)
		}
	}
	if pendingScopes != 0 {
		return
	}
	if !d.SessionShimAdoptionComplete() || !d.SessionShimCarrierActivationComplete() {
		return
	}
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.stopGen != nil || !d.sessionShimReadinessWithdrawn.Load() {
		return
	}
	switch d.State() {
	case StateRecovering:
		if d.spawner != nil {
			d.spawner.Resume()
		}
		d.setState(StateRunning)
		d.sessionShimReadinessWithdrawn.Store(false)
	case StateRunning:
		// The fence was raised while this daemon was already SERVING. Every
		// other path that raises it also moves the lifecycle to recovering
		// first, so this case did not exist — but a composition installed after
		// startup publishes its adoption revision without ever leaving
		// StateRunning, which is the entire point of installing it late.
		//
		// The acknowledgement is still the one reopening edge; the state the
		// daemon happened to be in when the fence went up is not what the fence
		// is about. Without this case the fence stays raised for the life of the
		// process and the poll lane never claims again — a host that came up
		// faster and then silently stopped taking work.
		//
		// Nothing paused the spawner on this path (the paths that do also leave
		// recovering), so nothing is resumed here.
		d.sessionShimReadinessWithdrawn.Store(false)
	case StatePaused, StateDraining:
		// Preserve an operator pause. The acknowledged heartbeat clears only the
		// proof-v2 fence; ResumeContext remains the explicit admission edge for a
		// manual pause or non-terminal drain.
		d.sessionShimReadinessWithdrawn.Store(false)
	}
}

// sessionShimActivatedScope is one scope whose adoption publication has
// completed carrier activation and is still waiting for the control plane to
// echo that revision back on heartbeat.
type sessionShimActivatedScope struct {
	scope    string
	revision string
}

// sessionShimActivatedScopes snapshots those scopes, in scope order.
//
// It reports nothing unless carrier activation is actually complete. The
// projection a beat would carry is refused until then (see
// SessionShimHeartbeatProjection), so announcing activation earlier would be
// announcing a state this daemon cannot yet prove.
func (d *Daemon) sessionShimActivatedScopes() []sessionShimActivatedScope {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	if !d.shims.carrierActivationComplete || len(d.shims.pendingHeartbeatAcks) == 0 {
		return nil
	}
	out := make([]sessionShimActivatedScope, 0, len(d.shims.pendingHeartbeatAcks))
	for scope, revision := range d.shims.pendingHeartbeatAcks {
		out = append(out, sessionShimActivatedScope{scope: scope, revision: revision})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].scope < out[j].scope })
	return out
}

// notifySessionShimAdoptionActivated offers OnAdoptionActivated one already
// snapshotted scope at a time.
//
// The snapshot is taken by the caller BEFORE any beat this daemon rings itself,
// so the set announced is the one that existed when activation completed rather
// than whatever survived donmai's own acknowledgement. Best-effort and
// infallible by construction: the hook returns nothing, each call is bounded by
// CallbackTimeout, and every scope is offered even if an earlier one was slow —
// the acknowledgement each hook chases is per-scope.
func (d *Daemon) notifySessionShimAdoptionActivated(ctx context.Context, scopes []sessionShimActivatedScope) {
	hook := d.sessionShimConfig().OnAdoptionActivated
	if hook == nil {
		return
	}
	for _, activated := range scopes {
		func() {
			hookCtx, cancel := d.sessionShimCallbackContext(ctx)
			defer cancel()
			hook(hookCtx, activated.scope, activated.revision)
		}()
	}
}

// defaultShimRegistryDir resolves the registry location through the injected
// state-directory seam.
func defaultShimRegistryDir() string {
	return statepath.Resolve("session-shims", "")
}

// adoptedShim is one session under this daemon's control.
//
// ShimID is recorded here at adoption time rather than read back off the
// controller on demand. That keeps the reporting paths — capacity, the restart
// fence, host diagnostics — independent of whether a live connection is still
// held, which is exactly when those paths matter most.
type adoptedShim struct {
	controller *sessionshim.Controller
	shimID     string
	// carrierActivationResolved records only that this exact adopted entry's
	// remote carrier receipt resolved the pending Snapshot or consumed recovery
	// and its cursor persisted. Local viewer-mutation authority stays held until
	// the later exact scope heartbeat invokes OnCarrierActivationAcknowledged.
	// Absence of pending state alone never proves either fact.
	carrierActivationResolved bool
	// handle is the published SessionHandle for a shim this daemon LAUNCHED. A
	// shim adopted at startup has none: the spec that created it belonged to a
	// daemon generation that is gone, and inventing project/repository fields
	// from nothing would put guesses on an operator-facing surface.
	handle SessionHandle
	// spec is retained only for a shim this daemon launched. Its exact value is
	// the lifecycle payload delivered to ordinary WorkerSpawner listeners; an
	// adopted shim deliberately has no fabricated spec.
	spec SessionSpec
	// launched distinguishes "this daemon created it" from "this daemon adopted
	// it after a restart" — a diagnostic distinction only (§D11: ownership mode
	// is a diagnostic field, never a second lifecycle authority).
	launched bool
	// terminal serializes immutable Exit handling. The entry stays present while
	// synchronous Ended listeners run, preserving capacity ownership until their
	// generation-scoped cleanup is complete.
	terminal bool
	// adoption and its opaque receipt remain attached to this exact controller
	// generation until terminal proof is handed downstream.
	adoption        SessionShimAdoptionEvidence
	adoptionReceipt SessionShimAdoptionReceipt
	// consumedRecovery is the non-secret local activation resolution for a
	// proof/receipt adoption that committed before the prior controller died. It
	// binds carrier_active to the original staged high-water and exact replayed
	// adoption receipt without retaining the bearer, recovery correlation, raw
	// Snapshot, or any authority that could mint a second candidate.
	consumedRecovery *sessionShimConsumedRecovery
}

type sessionShimConsumedRecovery struct {
	preStageAckSeq  uint64
	stagedHighWater uint64
	adoptionReceipt SessionShimAdoptionReceipt
}

func newSessionShimConsumedRecovery(
	preparation SessionShimAdoptionPreparationResult,
	receipt SessionShimAdoptionReceipt,
) *sessionShimConsumedRecovery {
	if preparation.State != SessionShimPreparationAdoptedCandidateRecovery ||
		preparation.AdoptedCandidateRecovery == nil {
		return nil
	}
	return &sessionShimConsumedRecovery{
		preStageAckSeq:  preparation.AdoptedCandidateRecovery.PreStageAckSeq,
		stagedHighWater: preparation.AdoptedCandidateRecovery.StagedHighWater,
		adoptionReceipt: cloneSessionShimAdoptionReceipt(receipt),
	}
}

type shimIncarnation struct {
	identity     sessionshim.Identity
	shimID       string
	processEpoch uint64
}

// sessionShimTerminalReport is the per-incarnation durable-handoff state the
// reconcile keeps so repeated passes cannot re-commit or re-POST one tombstone.
type sessionShimTerminalReport struct {
	// inFlight is non-nil while one pass owns the durable handoff and is closed
	// when it finishes. A second pass WAITS on it rather than skipping: every
	// reconcile call site reads the quarantine projection straight afterwards,
	// and a pass that returned early would let its caller observe a lineage the
	// winner is in the middle of withdrawing.
	inFlight  chan struct{}
	committed bool
	retryAt   time.Time
}

type sessionShimAdoptionCorrelation struct {
	evidence SessionShimAdoptionEvidence
	receipt  SessionShimAdoptionReceipt
}

// sessionShimState is the daemon's live view of per-session shim ownership.
type sessionShimState struct {
	mu sync.RWMutex
	// publicationMu serializes the full dynamic adoption -> batch -> local
	// publication -> carrier activation transaction. Composing batch revisions
	// are global across served scopes, so overlapping dynamic publications would
	// otherwise each validate a transient incomplete set.
	publicationMu sync.Mutex
	// dynamicPublicationFailed is protected by publicationMu. A launch already
	// queued across the old admission edge must not publish after an earlier
	// serialized activation failed and closed readiness.
	dynamicPublicationFailed bool
	registry                 *sessionshim.Registry
	adopted                  map[sessionshim.Identity]adoptedShim
	quarantined              []sessionshim.QuarantinedSession
	tombstoned               []sessionshim.Tombstone
	// reportingTerminal marks incarnations whose durable terminal handoff is in
	// flight, already committed, or cooling off after a refusal, so the
	// reconcile's many call sites cannot double-report one tombstone and a
	// polling caller cannot amplify one refusal into a burst of commits.
	reportingTerminal map[shimIncarnation]sessionShimTerminalReport
	fence             *sessionshim.Fence
	fences            map[string]sessionshim.Fence
	fenceRequests     map[string]sessionshim.FenceRequest
	// forwarded is the highest output sequence this daemon durably forwarded per
	// session — the resume point a LATER adoption asks the shim to replay from
	// (§D5). The daemon records only this; it never allocates sequence.
	forwarded map[sessionshim.Identity]uint64
	// correlations survive a controller disconnect so the later exact tombstone
	// callback still receives the same opaque durable adoption receipt.
	correlations       map[shimIncarnation]sessionShimAdoptionCorrelation
	batchReceipts      map[string]SessionShimAdoptionBatchReceipt
	credentialReceipts map[string]SessionShimScopeCredentialReceipt
	// declaringComposition is set only while a deferred composition install is
	// performing the ONE refresh that first presents its attestation. That
	// refresh's receipt is the scope's founding authority, so it has nothing
	// retained to be checked against; every other refresh does, and is.
	declaringComposition bool
	// pendingHeartbeatAcks binds every dynamically published scope to the exact
	// adoption revision whose first server-echoed heartbeat must be observed
	// before local capacity, poll, claim, and spawn admission reopen. A later
	// same-scope revision supersedes the older pending acknowledgement.
	pendingHeartbeatAcks map[string]string
	// stagingSnapshots marks the one mandatory emitting snapshot call before it
	// reaches the ordered event consumer. pendingSnapshots retains the exact event
	// until carrier_active returns its durable ack; activationGates backpressure
	// every later event without advancing forwarded or shim Heartbeat.
	stagingSnapshots map[sessionshim.Identity]bool
	pendingSnapshots map[sessionshim.Identity]sessionshim.ControllerEvent
	activationGates  map[sessionshim.Identity]*shimAdoptionGate
	// restart is the one controller-local planned-restart authorization. It is
	// deliberately memory-only: a replacement controller resolves adoption from
	// authenticated live correlations and never inherits this old controller's
	// stop permission. The separate audit file records state but is never read as
	// recovery authority.
	restart *restartPreparation
	// acceptanceRefusals and acceptanceQuarantines are populated only through
	// the dormant, bearer-authenticated installed-artifact acceptance control.
	// They never derive authority from request data: a quarantine is admitted
	// only after a live registry record is matched to an already-adopted
	// lifecycle, and a refusal can affect only that exact lifecycle.
	acceptanceRefusals   map[sessionshim.Identity]acceptanceRefusalState
	acceptanceQuarantine map[shimIncarnation]sessionshim.ProcessIdentity
	// reconciling marks scopes with one commit-outcome reconciliation pass in
	// flight, so concurrent triggers (an ambiguous commit and the
	// revision-stale beat it causes) arm exactly one bounded loop.
	// reconcileStop is closed when the daemon releases its shims; a
	// reconciliation loop parked in backoff exits on it instead of outliving
	// the daemon. Protected by mu; the channel itself is assigned once.
	reconciling      map[string]bool
	reconcileStop    chan struct{}
	reconcileStopped bool
	// restartStateWriter and restartID are package-private test seams. Production
	// uses the atomic secret-free state writer and crypto/rand identifier.
	restartStateWriter func(restartPreparationAudit) error
	restartID          func() (string, error)
	restartNow         func() time.Time
	// wg joins the per-session event consumers so shutdown cannot race one that
	// is still writing bookkeeping.
	wg sync.WaitGroup
	// adoptionComplete records that the §D4 pass ran to completion. Capacity and
	// readiness read it: a daemon that has NOT finished adopting must not
	// advertise, because it does not yet know what is occupied.
	adoptionComplete bool
	// adoptionCompletedAtUnixNano is the completion observation for the current
	// controller's §D4 pass. Zero means adoption is disabled or did not complete.
	adoptionCompletedAtUnixNano int64
	// carrierActivationComplete is distinct from adoptionComplete and means the
	// exact remote carrier_active receipt/cursor set resolved. Local control
	// authority remains held until each changed scope's later exact heartbeat
	// invokes OnCarrierActivationAcknowledged.
	carrierActivationComplete bool
}

func newSessionShimState() *sessionShimState {
	return &sessionShimState{
		adopted:              make(map[sessionshim.Identity]adoptedShim),
		forwarded:            make(map[sessionshim.Identity]uint64),
		correlations:         make(map[shimIncarnation]sessionShimAdoptionCorrelation),
		reportingTerminal:    make(map[shimIncarnation]sessionShimTerminalReport),
		fences:               make(map[string]sessionshim.Fence),
		fenceRequests:        make(map[string]sessionshim.FenceRequest),
		batchReceipts:        make(map[string]SessionShimAdoptionBatchReceipt),
		credentialReceipts:   make(map[string]SessionShimScopeCredentialReceipt),
		pendingHeartbeatAcks: make(map[string]string),
		stagingSnapshots:     make(map[sessionshim.Identity]bool),
		pendingSnapshots:     make(map[sessionshim.Identity]sessionshim.ControllerEvent),
		activationGates:      make(map[sessionshim.Identity]*shimAdoptionGate),
		acceptanceRefusals:   make(map[sessionshim.Identity]acceptanceRefusalState),
		acceptanceQuarantine: make(map[shimIncarnation]sessionshim.ProcessIdentity),
		reconciling:          make(map[string]bool),
		reconcileStop:        make(chan struct{}),
	}
}

// setDeclaringComposition arms (or disarms) the founding-declaration window
// for a deferred composition install. See declaringComposition.
func (s *sessionShimState) setDeclaringComposition(declaring bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declaringComposition = declaring
}

// sessionShimFoundingDeclaration reports whether the refresh receipt about to
// be validated for scope is the FOUNDING one: a deferred composition is in its
// declaring window and nothing has been retained for the scope yet. Both halves
// matter — the window alone would also match an unrelated lane refresh that
// happens to land after retention, and an empty retention alone would match a
// refresh on a daemon that never declared at all.
func (d *Daemon) sessionShimFoundingDeclaration(scope string) bool {
	if d.shims == nil {
		return false
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	_, retained := d.shims.credentialReceipts[scope]
	return d.shims.declaringComposition && !retained
}

func shimIncarnationFor(evidence SessionShimAdoptionEvidence) shimIncarnation {
	return shimIncarnation{
		identity:     evidence.Identity,
		shimID:       evidence.ShimID,
		processEpoch: evidence.ProcessEpoch,
	}
}

// sessionShimConfig returns the effective configuration.
//
// It reads the CURRENT identity generation rather than a value snapshotted at
// New, which is what lets a composition installed after startup take effect for
// every caller at once (see session_shim_composition.go).
func (d *Daemon) sessionShimConfig() SessionShimConfig {
	cfg := *d.shimIdentity().config
	if cfg.RegistryDir == "" {
		cfg.RegistryDir = defaultShimRegistryDir()
	}
	if cfg.Orphan.Deadline == 0 {
		policy := sessionshim.DefaultOrphanPolicy()
		policy.ExternalReleaseThreshold = cfg.Orphan.ExternalReleaseThreshold
		cfg.Orphan = policy
	}
	return cfg
}

// defaultShimOrgID is the organization identity a standalone OSS daemon uses.
const defaultShimOrgID = "local"

// orgID returns the effective organization half of every lifecycle identity.
func (c SessionShimConfig) orgID() string {
	if c.OrgID == "" {
		return defaultShimOrgID
	}
	return c.OrgID
}

// orgIDForSession resolves the organization half of one lifecycle identity.
// The per-session value is authoritative when present; OrgID remains the
// source-compatible standalone/legacy fallback.
func (c SessionShimConfig) orgIDForSession(spec SessionSpec) string {
	if spec.OrganizationID != "" {
		return spec.OrganizationID
	}
	return c.orgID()
}

// launchTimeout returns the effective bound on one shim launch.
func (c SessionShimConfig) launchTimeout() time.Duration {
	if c.LaunchTimeout <= 0 {
		return defaultShimLaunchTimeout
	}
	return c.LaunchTimeout
}

func (c SessionShimConfig) callbackTimeout() time.Duration {
	if c.CallbackTimeout > 0 {
		return c.CallbackTimeout
	}
	return c.launchTimeout()
}

// sessionShimAdoptionPublicationStages is the depth of the dynamic adoption
// publication pipeline: durable adoption (OnAdoption/OnAdoptionV2), batch
// preparation (PrepareAdoptionBatch), batch commit (OnAdoptionBatch), and
// carrier activation (OnAdoptionPublished). Sequential, fixed, and each stage
// individually bounded by sessionShimCallbackContext.
const sessionShimAdoptionPublicationStages = 4

// adoptionPublicationTimeout bounds one whole dynamic adoption publication.
//
// It is derived, not chosen: every stage of the pipeline already runs under
// callbackTimeout, and the pipeline has a fixed depth, so the flow's bound is
// exactly the per-stage bound times that depth. Deriving it keeps this one
// stream bounded in one unit — a second hand-picked number is how the launch
// clock came to be the binding constraint on the batch prepare, handing it the
// one or two seconds discovery left over while the callback's own retry policy
// still held a full budget.
func (c SessionShimConfig) adoptionPublicationTimeout() time.Duration {
	return sessionShimAdoptionPublicationStages * c.callbackTimeout()
}

// adoptSessionShims runs the §D4 startup pass: discover, classify, adopt every
// compatible live shim, and account for every quarantined one.
//
// It MUST complete before the daemon registers, advertises ready capacity, or
// claims work. Start calls it in exactly that position. Returning an error is a
// startup failure rather than a warning: a daemon that could not determine what
// is already running on this host cannot honestly advertise how much it can take.
func (d *Daemon) adoptSessionShims(ctx context.Context) error {
	cfg := d.sessionShimConfig()
	if err := cfg.validateSnapshotCarrier(); err != nil {
		return err
	}
	if d.sessionShimAttestationError() != nil {
		return d.sessionShimAttestationError()
	}

	// The §D8 inequality is validated at STARTUP, before any session is admitted.
	// A configuration whose orphan bound can outlast an external release
	// threshold is capable of double execution, and discovering that at deadline
	// time means discovering it from the damage.
	if err := cfg.Orphan.Validate(); err != nil {
		return fmt.Errorf("session shim: %w", err)
	}
	if d.sessionShimEnabled() && cfg.OnAdoptionBatch == nil {
		return errors.New("session shim: attested recovery requires OnAdoptionBatch")
	}

	if !cfg.EnableAdoption {
		// §D11 step 1: inspection-only. The registry is still opened and scanned
		// so `host status` can SHOW what is out there, but nothing is adopted and
		// nothing is quarantined — this daemon claims no authority it has not
		// negotiated.
		return nil
	}

	registry, err := d.sessionShimRegistry()
	if err != nil {
		return err
	}

	opts := sessionshim.AdoptOptions{
		Registry:              registry,
		ControllerID:          d.controllerID(),
		EventBacklogBudget:    cfg.EventBacklogBudget,
		RequireFullHostFrames: cfg.RequireAuthoritativeSnapshot && d.sessionShimEnabled(),
		Logger:                slog.Default(),
	}
	preparedByID := make(map[sessionshim.Identity]SessionShimAdoptionPreparationResult)
	hostByID := make(map[sessionshim.Identity]string)
	if cfg.PrepareAdoption != nil || cfg.PrepareAdoptionV2 != nil || cfg.HostIDForOrg != nil {
		opts.Prepare = func(prepareCtx context.Context, evidence sessionshim.AdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			hostID, hostErr := d.sessionShimHostID(prepareCtx, evidence.Identity.OrgID)
			if hostErr != nil {
				return sessionshim.PreparedAdoption{}, hostErr
			}
			prepared, err := d.prepareSessionShimAdoption(prepareCtx, hostID, evidence)
			if err != nil {
				return sessionshim.PreparedAdoption{}, err
			}
			hostByID[evidence.Identity] = hostID
			preparedByID[evidence.Identity] = prepared
			return prepared.PreparedAdoption, nil
		}
	}
	if cfg.ResumeFrom != nil {
		resume := cfg.ResumeFrom
		opts.ResumeFrom = func(id sessionshim.Identity) uint64 {
			return resume(id.OrgID, id.SessionID)
		}
	}
	if cfg.ExpectedWorkarea != nil {
		expected := cfg.ExpectedWorkarea
		opts.ExpectedWorkarea = func(id sessionshim.Identity) string {
			return expected(id.OrgID, id.SessionID)
		}
	}
	if cfg.ExpectedWorkareaRoot != nil {
		expected := cfg.ExpectedWorkareaRoot
		opts.ExpectedWorkareaRoot = func(id sessionshim.Identity) string {
			return expected(id.OrgID, id.SessionID)
		}
	}
	if cfg.ExpectedWorkarea == nil && cfg.ExpectedWorkareaRoot == nil {
		worktreeParent := d.opts.SpawnerOptions.WorktreeParentDir
		if worktreeParent == "" {
			worktreeParent = statepath.Resolve("worktrees", "/tmp/.donmai/worktrees")
		}
		acquisitions, found, storeErr := workarea.OpenExistingAcquisitionStore(worktreeParent, nil)
		if storeErr != nil {
			return fmt.Errorf("session shim: open workarea adoption journal: %w", storeErr)
		}
		if found {
			readyWorkareas, readyErr := acquisitions.ReadyRecords()
			if readyErr != nil {
				return fmt.Errorf("session shim: read workarea adoption journal: %w", readyErr)
			}
			if len(readyWorkareas) > 0 {
				opts.ExpectedWorkareaLayout = func(id sessionshim.Identity) (string, string, error) {
					return resolveExpectedAdoptionWorkarea(acquisitions, worktreeParent, id.SessionID)
				}
			}
		}
	}

	result, err := sessionshim.Adopt(ctx, opts)
	if err != nil {
		if errors.Is(err, sessionshim.ErrShimUnsupported) {
			// §D3: a platform without a trustworthy peer-credential primitive
			// keeps adoption disabled rather than running unauthenticated. Nothing
			// was adopted and nothing is claimed to be occupied.
			slog.Warn("session shim: adoption unsupported on this platform; continuing without it")
			return nil
		}
		return fmt.Errorf("session shim: adopt: %w", err)
	}
	carrierActivationSettled := false
	defer func() {
		if !carrierActivationSettled {
			d.failPendingSessionShimActivations()
		}
	}()

	// The local generation is committed, but startup is still NOT ready. Give
	// the composing carrier each exact fact and require its durable handoff
	// before publishing adoptionComplete or starting registration.
	entries := make(map[sessionshim.Identity]adoptedShim, len(result.Adopted))
	gates := make(map[*sessionshim.Controller]*shimAdoptionGate, len(result.Adopted))
	gatesCommitted := false
	defer func() {
		if !gatesCommitted {
			for _, gate := range gates {
				gate.finish(false)
			}
		}
	}()
	for _, c := range result.Adopted {
		id := c.Identity()
		preparation := preparedByID[id]
		evidence, evidenceErr := d.sessionShimAdoptionEvidence(ctx, c, preparation, hostByID[id])
		if evidenceErr != nil {
			result.Close()
			return fmt.Errorf("session shim: resolve adoption host for %s: %w", id, evidenceErr)
		}
		gate := newShimAdoptionGate()
		gates[c] = gate
		d.consumeShimEventsGated(c, gate)
		receipt, callbackErr := d.completeSessionShimAdoption(ctx, evidence, preparation)
		delete(preparedByID, id)
		evidence.SnapshotProxy.deactivate()
		if callbackErr != nil {
			result.Close()
			return fmt.Errorf("session shim: durable adoption for %s: %w", id, callbackErr)
		}
		// The proxy is a synchronous takeover capability, not retained state.
		evidence.SnapshotProxy = nil
		entries[id] = adoptedShim{
			controller:       c,
			shimID:           c.Hello().ShimID,
			adoption:         evidence,
			adoptionReceipt:  receipt,
			consumedRecovery: newSessionShimConsumedRecovery(preparation, receipt),
		}
	}

	// A startup tombstone is the orphan path's retained positive evidence. Post
	// it before readiness too: otherwise a controller could become ready while a
	// fenced claim remained needlessly unreconciled. Unproven tombstones were
	// classified as capacity-consuming quarantine by sessionshim.Adopt and never
	// enter this loop.
	terminalOutcomes := make([]SessionShimTerminalEvidence, 0, len(result.Tombstoned))
	for _, tombstone := range result.Tombstoned {
		hostID, hostErr := d.sessionShimHostID(ctx, tombstone.OrgID)
		if hostErr != nil {
			result.Close()
			return fmt.Errorf("session shim: resolve terminal host for %s: %w", tombstone.Identity(), hostErr)
		}
		evidence := SessionShimTerminalEvidence{
			Identity:     tombstone.Identity(),
			HostID:       hostID,
			ShimID:       tombstone.ShimID,
			ProcessEpoch: tombstone.ProcessEpoch,
			Tombstone:    tombstone,
		}
		if callbackErr := d.reportSessionShimTerminalEvidence(ctx, evidence); callbackErr != nil {
			result.Close()
			return fmt.Errorf("session shim: durable terminal evidence for %s: %w", tombstone.Identity(), callbackErr)
		}
		terminalOutcomes = append(terminalOutcomes, evidence)
	}

	batchReceipts := make(map[string]SessionShimAdoptionBatchReceipt)
	if cfg.OnAdoptionBatch != nil {
		scopeSet := make(map[string]struct{})
		for _, orgID := range cfg.AdoptionBatchOrgIDs {
			scopeSet[orgID] = struct{}{}
		}
		for _, controller := range result.Adopted {
			scopeSet[controller.Identity().OrgID] = struct{}{}
		}
		for _, quarantined := range result.Quarantined {
			scopeSet[quarantined.OrgID] = struct{}{}
		}
		for _, terminal := range terminalOutcomes {
			scopeSet[terminal.Identity.OrgID] = struct{}{}
		}
		if len(scopeSet) == 0 {
			scopeSet[cfg.orgID()] = struct{}{}
		}
		scopeOrgIDs := make([]string, 0, len(scopeSet))
		for orgID := range scopeSet {
			scopeOrgIDs = append(scopeOrgIDs, orgID)
		}
		sort.Strings(scopeOrgIDs)
		for _, orgID := range scopeOrgIDs {
			hostID, hostErr := d.sessionShimHostID(ctx, orgID)
			if hostErr != nil {
				result.Close()
				return fmt.Errorf("session shim: resolve adoption batch host for organization %q: %w", orgID, hostErr)
			}
			batch := SessionShimAdoptionBatch{OrgID: orgID, HostID: hostID}
			for _, controller := range result.Adopted {
				entry := entries[controller.Identity()]
				if entry.adoption.Identity.OrgID == orgID {
					if entry.adoption.CarrierCompatible {
						batch.Adopted = append(batch.Adopted, SessionShimAdoptionOutcome{
							Evidence: entry.adoption,
							Receipt:  entry.adoptionReceipt,
						})
					} else {
						hello := entry.controller.Hello()
						batch.Quarantined = append(batch.Quarantined, sessionshim.QuarantinedSession{
							OrgID: entry.adoption.Identity.OrgID, SessionID: entry.adoption.Identity.SessionID,
							ShimID: hello.ShimID, ProcessEpoch: hello.ProcessEpoch,
							ControllerGeneration: entry.adoption.ControllerGeneration,
							ProtocolMin:          hello.Min, ProtocolMax: hello.Max, Phase: hello.Phase,
							Reason:           sessionShimCarrierQuarantineReason(entry.adoption.CarrierIncompatibility),
							ConsumesCapacity: true,
						})
					}
				}
			}
			for _, quarantined := range result.Quarantined {
				if quarantined.OrgID == orgID {
					batch.Quarantined = append(batch.Quarantined, quarantined)
				}
			}
			for _, terminal := range terminalOutcomes {
				if terminal.Identity.OrgID == orgID {
					batch.Tombstoned = append(batch.Tombstoned, terminal)
				}
			}
			receipt, batchErr := d.completeSessionShimAdoptionBatch(ctx, batch)
			if batchErr != nil {
				result.Close()
				return fmt.Errorf("session shim: durable adoption batch for organization %q: %w", orgID, batchErr)
			}
			heartbeatAckPending := d.sessionShimEnabled() && cfg.OnAdoptionPublished != nil &&
				cfg.OnCarrierActivationAcknowledged != nil
			if revisionErr := d.updateSessionShimAdoptionRevision(
				orgID, receipt.AdoptionRevision, heartbeatAckPending,
			); revisionErr != nil {
				result.Close()
				return fmt.Errorf("session shim: retain adoption revision for organization %q: %w", orgID, revisionErr)
			}
			batchReceipts[orgID] = receipt
		}
	}

	d.shims.mu.Lock()
	d.shims.carrierActivationComplete = false
	d.shims.registry = registry
	for id, entry := range entries {
		d.shims.adopted[id] = entry
		d.shims.correlations[shimIncarnationFor(entry.adoption)] = sessionShimAdoptionCorrelation{
			evidence: entry.adoption,
			receipt:  cloneSessionShimAdoptionReceipt(entry.adoptionReceipt),
		}
		c := entry.controller
		if resumeFrom := c.ResumeFrom(); resumeFrom > 0 {
			// ResumeFrom is exactly last_forwarded_seq + 1. Seed the replacement
			// daemon's snapshot before its event consumer starts so an immediate
			// second planned restart cannot regress the durable correlation to zero.
			if durableBeforeAdoption := resumeFrom - 1; durableBeforeAdoption > d.shims.forwarded[id] {
				d.shims.forwarded[id] = durableBeforeAdoption
			}
		}
	}
	d.shims.quarantined = result.QuarantinedProjection()
	d.shims.tombstoned = append(d.shims.tombstoned, result.Tombstoned...)
	for orgID, receipt := range batchReceipts {
		d.shims.batchReceipts[orgID] = receipt
	}
	d.shims.mu.Unlock()
	for _, gate := range gates {
		gate.finish(true)
	}
	gatesCommitted = true
	d.shims.mu.Lock()
	d.shims.adoptionComplete = true
	d.shims.adoptionCompletedAtUnixNano = time.Now().UTC().UnixNano()
	d.shims.mu.Unlock()
	if activationErr := d.activatePublishedSessionShimCarriers(ctx, entries); activationErr != nil {
		result.Close()
		return fmt.Errorf("session shim: activate published carriers: %w", activationErr)
	}
	carrierActivationSettled = true
	// Startup's own beat is StartSynchronized, which Daemon.Start rings for the
	// scope it owns. Announce the activated set here so an embedder holding a
	// lane donmai does not own can ring that one too.
	d.notifySessionShimAdoptionActivated(ctx, d.sessionShimActivatedScopes())
	for _, tombstone := range result.Tombstoned {
		// Withdraw the liveness claim BEFORE disposing the proof. A shim
		// publishes its tombstone and then removes its record, so a crash
		// between the two leaves both on disk — and disposing first collapses
		// "terminal, proven" into "a record whose process is gone", which §D10
		// classifies as stale and leaves unresolved with no proof left to
		// reach the other conclusion. Remove is idempotent, so the ordinary
		// case where the shim withdrew its own record costs nothing.
		if removeErr := registry.RemoveIncarnation(
			tombstone.Identity(), tombstone.ShimID, tombstone.ProcessEpoch); removeErr != nil {
			slog.Warn("session shim: withdraw startup discovery record after durable terminal handoff",
				"session", tombstone.Identity().String(), "error", removeErr)
			continue
		}
		if removeErr := registry.RemoveTombstoneIncarnation(tombstone); removeErr != nil {
			slog.Warn("session shim: dispose startup tombstone after durable terminal handoff",
				"session", tombstone.Identity().String(), "error", removeErr)
		}
	}

	slog.Info("session shim: startup adoption complete",
		"adopted", len(result.Adopted),
		"quarantined", len(result.Quarantined),
		"tombstoned", len(result.Tombstoned),
		"stale", len(result.Stale),
		"occupiedSlots", result.OccupiedSlots())
	return nil
}

func resolveExpectedAdoptionWorkarea(acquisitions *workarea.AcquisitionStore, parent, sessionID string) (string, string, error) {
	record, err := acquisitions.RecordForSessionID(sessionID)
	if err == nil {
		if record.State != workarea.AcquisitionReady {
			return "", "", fmt.Errorf("workarea acquisition is not ready")
		}
		declaration, readErr := workarea.ReadDeclaration(workarea.RootPath(record.FinalRoot))
		if readErr != nil {
			return "", "", readErr
		}
		selectedRepository := declaration.SelectedRepository
		if sessionID != record.SessionID {
			selectedRepository = ""
			for _, participant := range record.Participants {
				if participant.SessionID == sessionID {
					selectedRepository = participant.SelectedRepository
					break
				}
			}
		}
		for _, repository := range declaration.Repositories {
			if repository.Name == selectedRepository {
				return filepath.Join(record.FinalRoot, repository.Leaf), record.FinalRoot, nil
			}
		}
		return "", "", fmt.Errorf("selected repository is absent from the declaration")
	}
	if !errors.Is(err, workarea.ErrAcquisitionNotFound) {
		return "", "", err
	}
	layout, found, err := workarea.DiscoverLayout(parent, sessionID, "")
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", fmt.Errorf("no retained workarea for session %q", sessionID)
	}
	return layout.Repository.String(), layout.Root.String(), nil
}

func (d *Daemon) acquireSessionShimRecoveryReceipts(
	ctx context.Context,
	primary *SessionShimCredentialReceipt,
) error {
	if !d.sessionShimEnabled() {
		return nil
	}
	if primary == nil || primary.State != SessionShimCredentialStateRecovering {
		return errors.New("session shim: auth-only registration requires a recovering credential receipt")
	}
	cfg := d.sessionShimConfig()
	primaryScope := cfg.orgID()
	primaryReceipt := SessionShimScopeCredentialReceipt{
		Scope: primaryScope, WorkerHostID: primary.WorkerHostID, AdoptionRevision: primary.AdoptionRevision,
	}
	receipts := []SessionShimScopeCredentialReceipt{primaryReceipt}
	expected := make([]string, 0, len(cfg.AdoptionBatchOrgIDs))
	seen := make(map[string]bool, len(cfg.AdoptionBatchOrgIDs))
	for _, scope := range cfg.AdoptionBatchOrgIDs {
		if scope == "" {
			return errors.New("session shim: adoption batch scope must not be empty")
		}
		if seen[scope] {
			return fmt.Errorf("session shim: duplicate adoption batch scope %q", scope)
		}
		seen[scope] = true
		if scope != primaryScope {
			expected = append(expected, scope)
		}
	}
	sort.Strings(expected)
	if len(expected) > 0 && cfg.AcquireRecoveryScopes == nil {
		return errors.New("session shim: additional recovery scopes require AcquireRecoveryScopes")
	}
	if cfg.AcquireRecoveryScopes != nil {
		callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
		additional, err := cfg.AcquireRecoveryScopes(
			callbackCtx,
			d.SessionShimHostAttestation(),
			cloneSessionShimScopeCredentialReceipt(primaryReceipt),
		)
		cancel()
		if err != nil {
			return fmt.Errorf("session shim: acquire additional recovery scopes: %w", err)
		}
		additional = append([]SessionShimScopeCredentialReceipt(nil), additional...)
		if err := validateSessionShimScopeReceipts(additional); err != nil {
			return err
		}
		if len(additional) != len(expected) {
			return errors.New("session shim: additional recovery scopes are partial or contain unexpected scopes")
		}
		for i, receipt := range additional {
			if receipt.Scope != expected[i] {
				return errors.New("session shim: additional recovery scopes do not exactly match the served scope set")
			}
		}
		receipts = append(receipts, additional...)
	}
	return d.retainSessionShimCredentialReceipts(receipts)
}

// controllerID identifies this daemon process in shim diagnostics.
func (d *Daemon) controllerID() string {
	return d.shimIdentity().controllerID
}

// ControllerID returns the immutable process-scoped session-shim controller id.
func (d *Daemon) ControllerID() string { return d.controllerID() }

// SessionShimHostAttestation returns a defensive copy of the D12 tuple this
// process presents. Its zero value means attested recovery is off; the explicit
// stand-down is StandsDown(). The tuple is immutable within one generation —
// installing a composed configuration publishes a new one (see
// session_shim_composition.go), it never edits this one.
func (d *Daemon) SessionShimHostAttestation() SessionShimHostAttestation {
	return cloneSessionShimHostAttestation(d.shimIdentity().attestation)
}

func resolveSessionShimHostAttestation(cfg SessionShimConfig, controllerID string) (SessionShimHostAttestation, error) {
	if !cfg.RequireCredentialAttestation {
		return SessionShimHostAttestation{}, nil
	}
	if !cfg.EnableAdoption {
		return SessionShimHostAttestation{}, errors.New("session shim: credential attestation requires startup adoption")
	}
	capabilities, err := canonicalizeStringSet(cfg.AttestationCapabilities)
	if err != nil {
		return SessionShimHostAttestation{}, err
	}
	attestation := SessionShimHostAttestation{
		Supported:    SessionShimSupported,
		ControllerID: controllerID,
		ProtocolMin:  shimwire.ProtocolMin,
		ProtocolMax:  shimwire.ProtocolMax,
		Capabilities: capabilities,
	}
	if err := attestation.validate(); err != nil {
		return SessionShimHostAttestation{}, err
	}
	return attestation, nil
}

func resolveControllerID(cfg SessionShimConfig) (string, error) {
	if cfg.ControllerID != "" {
		if strings.TrimSpace(cfg.ControllerID) != cfg.ControllerID {
			return "", errors.New("session shim: controller id must not contain surrounding whitespace")
		}
		if cfg.ControllerID == "daemon" {
			return "", errors.New("session shim: controller id refuses reserved alias \"daemon\"")
		}
		if cfg.HostID != "" && cfg.ControllerID == cfg.HostID {
			return "", errors.New("session shim: controller id must differ from stable host id")
		}
		return cfg.ControllerID, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("session shim: generate controller id: %w", err)
	}
	generated := "ctl_" + hex.EncodeToString(raw)
	if cfg.HostID != "" && generated == cfg.HostID {
		return "", errors.New("session shim: generated controller id aliases stable host id")
	}
	return generated, nil
}

func (d *Daemon) validateControllerAlias(value, kind string) error {
	if value != "" && value == d.controllerID() {
		return fmt.Errorf("session shim: controller id must differ from %s", kind)
	}
	return nil
}

// sessionShimHostID returns the host authority named by restart/adoption
// evidence for one organization. The resolver wins, then the explicit static
// config. No other correlation is ever substituted.
func (d *Daemon) sessionShimHostID(ctx context.Context, orgID string) (string, error) {
	cfg := d.sessionShimConfig()
	if d.sessionShimEnabled() {
		d.shims.mu.RLock()
		receipt, ok := d.shims.credentialReceipts[orgID]
		d.shims.mu.RUnlock()
		if !ok {
			return "", fmt.Errorf("session shim: no retained credential receipt for organization %q", orgID)
		}
		if err := d.validateControllerAlias(receipt.WorkerHostID, "stable host id"); err != nil {
			return "", err
		}
		return receipt.WorkerHostID, nil
	}
	if cfg.HostIDForOrg != nil {
		if err := (sessionshim.Identity{OrgID: orgID, SessionID: "scope"}).Validate(); err != nil {
			return "", fmt.Errorf("session shim: invalid organization fence scope: %w", err)
		}
		callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
		defer cancel()
		hostID, err := cfg.HostIDForOrg(callbackCtx, orgID)
		if err != nil {
			return "", err
		}
		if hostID == "" {
			return "", fmt.Errorf("session shim: host identity resolver returned empty for organization %q", orgID)
		}
		if err := d.validateControllerAlias(hostID, "stable host id"); err != nil {
			return "", err
		}
		return hostID, nil
	}
	if cfg.HostID != "" {
		if err := d.validateControllerAlias(cfg.HostID, "stable host id"); err != nil {
			return "", err
		}
		return cfg.HostID, nil
	}
	if cfg.requiresStableHostIdentity() {
		return "", errors.New("session shim: stable host identity is required by composing callbacks")
	}
	return "", nil
}

func cloneShimExtensions(in shimwire.Extensions) shimwire.Extensions {
	out := shimwire.Extensions{Required: append([]string(nil), in.Required...)}
	if in.Values != nil {
		out.Values = make(map[string]string, len(in.Values))
		for key, value := range in.Values {
			out.Values[key] = value
		}
	}
	return out
}

func cloneSessionShimAdoptionReceipt(in SessionShimAdoptionReceipt) SessionShimAdoptionReceipt {
	return SessionShimAdoptionReceipt{DurableCorrelation: append([]byte(nil), in.DurableCorrelation...)}
}

func cloneSessionShimAdoptionEvidence(in SessionShimAdoptionEvidence) SessionShimAdoptionEvidence {
	in.Extensions = cloneShimExtensions(in.Extensions)
	in.PreparedCorrelation = append([]byte(nil), in.PreparedCorrelation...)
	return in
}

func (d *Daemon) sessionShimCallbackContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, d.sessionShimConfig().callbackTimeout())
}

func (d *Daemon) prepareSessionShimAdoption(
	ctx context.Context,
	hostID string,
	evidence sessionshim.AdoptionPreparation,
) (SessionShimAdoptionPreparationResult, error) {
	if d.sessionShimReadinessWithdrawn.Load() {
		return SessionShimAdoptionPreparationResult{}, errors.New("session shim: proof-v2 recovery heartbeat is not acknowledged")
	}
	if err := d.validateSessionShimCarrierProofV2Readiness(); err != nil {
		d.withdrawSessionShimProofV2Readiness()
		return SessionShimAdoptionPreparationResult{}, err
	}
	if d.sessionShimConfig().RequireAuthoritativeSnapshot && evidence.SelectedVersion < shimwire.V3 {
		return SessionShimAdoptionPreparationResult{}, nil
	}
	cfg := d.sessionShimConfig()
	if cfg.PrepareAdoption != nil && cfg.PrepareAdoptionV2 != nil {
		return SessionShimAdoptionPreparationResult{}, fmt.Errorf("%w: PrepareAdoption and PrepareAdoptionV2 cannot both be configured", ErrSessionShimCarrierConfig)
	}
	if cfg.PrepareAdoption == nil && cfg.PrepareAdoptionV2 == nil {
		return SessionShimAdoptionPreparationResult{}, nil
	}
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	input := SessionShimAdoptionPreparation{
		Identity:                    evidence.Identity,
		HostID:                      hostID,
		ControllerID:                evidence.ControllerID,
		ShimID:                      evidence.ShimID,
		ProcessEpoch:                evidence.ProcessEpoch,
		CurrentControllerGeneration: evidence.CurrentControllerGeneration,
		LocalResumeFrom:             evidence.LocalResumeFrom,
		LastHostSeq:                 evidence.LastHostSeq,
		LastForwardedSeq:            evidence.LastForwardedSeq,
		SelectedVersion:             evidence.SelectedVersion,
	}
	var result SessionShimAdoptionPreparationResult
	var err error
	if cfg.PrepareAdoptionV2 != nil {
		result, err = cfg.PrepareAdoptionV2(callbackCtx, input)
	} else {
		var prepared sessionshim.PreparedAdoption
		prepared, err = cfg.PrepareAdoption(callbackCtx, input)
		result = SessionShimAdoptionPreparationResult{
			State: SessionShimPreparationFreshCandidate, PreparedAdoption: prepared,
		}
	}
	if err != nil {
		return SessionShimAdoptionPreparationResult{}, err
	}
	if err := validateSessionShimAdoptionPreparationResult(result, evidence.ProcessEpoch, d.shimNow()); err != nil {
		return SessionShimAdoptionPreparationResult{}, err
	}
	prepared := result.PreparedAdoption
	if d.sessionShimConfig().RequireAuthoritativeSnapshot && prepared.ResumeFrom == nil {
		return SessionShimAdoptionPreparationResult{}, fmt.Errorf("%w: proof-bound carrier preparation omitted ResumeFrom", ErrSessionShimCarrierConfig)
	}
	prepared.Extensions = cloneShimExtensions(prepared.Extensions)
	prepared.Correlation = append([]byte(nil), prepared.Correlation...)
	if prepared.ResumeFrom != nil {
		resume := *prepared.ResumeFrom
		prepared.ResumeFrom = &resume
	}
	result.PreparedAdoption = prepared
	return cloneSessionShimAdoptionPreparationResult(result), nil
}

func validateSessionShimAdoptionPreparationResult(
	result SessionShimAdoptionPreparationResult,
	liveProcessEpoch uint64,
	now time.Time,
) error {
	switch result.State {
	case SessionShimPreparationFreshCandidate:
		if result.AdoptedCandidateRecovery != nil {
			return errors.New("session shim: fresh candidate preparation contains adopted recovery authority")
		}
		return nil
	case SessionShimPreparationAdoptedCandidateRecovery:
		recovery := result.AdoptedCandidateRecovery
		if recovery == nil || recovery.Credential.IsZero() || recovery.RecoveryCorrelation.IsZero() {
			return errors.New("session shim: adopted-candidate recovery is missing original credential or correlation")
		}
		if recovery.CarrierEpoch == 0 || recovery.StagedHighWater == ^uint64(0) ||
			recovery.ResumeFrom != recovery.StagedHighWater+1 || result.PreparedAdoption.ResumeFrom == nil ||
			*result.PreparedAdoption.ResumeFrom != recovery.ResumeFrom {
			return errors.New("session shim: adopted-candidate recovery cursor is not exact")
		}
		if len(result.PreparedAdoption.Correlation) != 0 {
			return errors.New("session shim: adopted-candidate recovery must not carry a new proof or receipt correlation")
		}
		carrierEpoch, ok := result.PreparedAdoption.Extensions.Get(shimwire.ExtCarrierEpoch)
		if !ok || carrierEpoch != strconv.FormatUint(recovery.CarrierEpoch, 10) {
			return errors.New("session shim: adopted-candidate recovery extension changed the original carrier")
		}
		resume := recovery.ResumeDisposition
		if err := resume.Validate(); err != nil {
			return fmt.Errorf("session shim: adopted-candidate recovery disposition: %w", err)
		}
		// ResumeDisposition is independently bound to the original bearer claims by
		// attachclient. Binding the same field here to authenticated shim Hello
		// evidence closes the other half of the authority chain before Welcome:
		// live shim PTY == retained disposition PTY == original bearer PTY.
		if liveProcessEpoch == 0 || resume.PTYEpoch != liveProcessEpoch {
			return errors.New("session shim: adopted-candidate recovery PTY epoch does not match the authenticated live shim")
		}
		if resume.ProofSchemaVersion != attachclient.V2ProofSchemaV2 ||
			resume.Authority != attachclient.V2ResumeAdoptedCandidateRecovery ||
			resume.CarrierEpoch != recovery.CarrierEpoch {
			return errors.New("session shim: adopted-candidate recovery disposition is not exact")
		}
		switch resume.State {
		case attachclient.V2ResumeReceiptStored, attachclient.V2ResumeServerRetained:
			if resume.AckSeq != recovery.PreStageAckSeq || resume.CandidateSnapshotSeq != recovery.StagedHighWater {
				return errors.New("session shim: adopted-candidate recovery disposition is not exact")
			}
		default:
			return errors.New("session shim: adopted-candidate recovery disposition is not exact")
		}
		if recovery.CredentialExpiresAt.IsZero() || !recovery.CredentialExpiresAt.After(now) {
			return errors.New("session shim: adopted-candidate recovery original credential is expired")
		}
		return nil
	default:
		return errors.New("session shim: unknown proof-v2 adoption preparation state")
	}
}

func cloneSessionShimAdoptionPreparationResult(in SessionShimAdoptionPreparationResult) SessionShimAdoptionPreparationResult {
	in.PreparedAdoption.Extensions = cloneShimExtensions(in.PreparedAdoption.Extensions)
	in.PreparedAdoption.Correlation = append([]byte(nil), in.PreparedAdoption.Correlation...)
	if in.PreparedAdoption.ResumeFrom != nil {
		resume := *in.PreparedAdoption.ResumeFrom
		in.PreparedAdoption.ResumeFrom = &resume
	}
	if in.AdoptedCandidateRecovery != nil {
		recovery := *in.AdoptedCandidateRecovery
		recovery.Credential = recovery.Credential.Clone()
		clonedCorrelation, _ := NewSessionShimRecoveryCorrelation(recovery.RecoveryCorrelation.Bytes())
		recovery.RecoveryCorrelation = clonedCorrelation
		recovery.ResumeDisposition.CandidateSnapshot = append([]byte(nil), recovery.ResumeDisposition.CandidateSnapshot...)
		in.AdoptedCandidateRecovery = &recovery
	}
	return in
}

func (d *Daemon) sessionShimAdoptionEvidence(
	ctx context.Context,
	ctrl *sessionshim.Controller,
	preparation SessionShimAdoptionPreparationResult,
	preparedHostID string,
) (SessionShimAdoptionEvidence, error) {
	prepared := preparation.PreparedAdoption
	lastForwarded := uint64(0)
	if resumeFrom := ctrl.ResumeFrom(); resumeFrom > 0 {
		lastForwarded = resumeFrom - 1
	}
	hostID := preparedHostID
	if hostID == "" {
		var err error
		hostID, err = d.sessionShimHostID(ctx, ctrl.Identity().OrgID)
		if err != nil {
			return SessionShimAdoptionEvidence{}, err
		}
	}
	carrierCompatible, carrierIncompatibility := sessionShimCarrierCompatibility(d.sessionShimConfig(), ctrl)
	evidence := SessionShimAdoptionEvidence{
		Identity:               ctrl.Identity(),
		HostID:                 hostID,
		ControllerID:           ctrl.ControllerID(),
		ShimID:                 ctrl.Hello().ShimID,
		ProcessEpoch:           ctrl.Hello().ProcessEpoch,
		ControllerGeneration:   uint64(ctrl.Generation()),
		LastForwardedSeq:       lastForwarded,
		Extensions:             cloneShimExtensions(ctrl.Adoption().Extensions),
		PreparedCorrelation:    append([]byte(nil), prepared.Correlation...),
		ObservedAtUnixNano:     d.shimNow().UnixNano(),
		ProtocolVersion:        ctrl.SelectedVersion(),
		CarrierCompatible:      carrierCompatible,
		CarrierIncompatibility: carrierIncompatibility,
		SnapshotProxy: func() *SessionShimSnapshotProxy {
			if ctrl.SupportsAuthoritativeSnapshot() {
				proxy := &SessionShimSnapshotProxy{controller: ctrl, daemon: d, identity: ctrl.Identity()}
				proxy.active.Store(true)
				return proxy
			}
			return nil
		}(),
	}
	if preparation.State == SessionShimPreparationAdoptedCandidateRecovery {
		evidence.SnapshotProxy = nil
	}
	return evidence, nil
}

func sessionShimCarrierCompatibility(cfg SessionShimConfig, ctrl *sessionshim.Controller) (bool, SessionShimCarrierIncompatibility) {
	if !cfg.RequireAuthoritativeSnapshot {
		return true, SessionShimCarrierCompatible
	}
	if !ctrl.SupportsAuthoritativeSnapshot() {
		return false, SessionShimCarrierAuthoritativeSnapshotV2Required
	}
	if !ctrl.SupportsFullHostFrames() {
		return false, SessionShimCarrierDurableHostFrameV3Required
	}
	return true, SessionShimCarrierCompatible
}

func sessionShimCarrierQuarantineReason(incompatibility SessionShimCarrierIncompatibility) sessionshim.QuarantineReason {
	if incompatibility == SessionShimCarrierDurableHostFrameV3Required {
		return sessionshim.QuarantineDurableHostFrameUnsupported
	}
	return sessionshim.QuarantineAuthoritativeSnapshotUnsupported
}

func (d *Daemon) completeSessionShimAdoption(
	ctx context.Context,
	evidence SessionShimAdoptionEvidence,
	preparation SessionShimAdoptionPreparationResult,
) (SessionShimAdoptionReceipt, error) {
	if !evidence.CarrierCompatible {
		return SessionShimAdoptionReceipt{}, nil
	}
	if preparation.State == SessionShimPreparationAdoptedCandidateRecovery && evidence.SnapshotProxy != nil {
		return SessionShimAdoptionReceipt{}, errors.New("session shim: adopted-candidate recovery must not receive new Snapshot authority")
	}
	cfg := d.sessionShimConfig()
	if cfg.OnAdoption == nil && cfg.OnAdoptionV2 == nil {
		return SessionShimAdoptionReceipt{}, nil
	}
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	var receipt SessionShimAdoptionReceipt
	var err error
	if cfg.OnAdoptionV2 != nil {
		receipt, err = cfg.OnAdoptionV2(callbackCtx, SessionShimAdoptionEvidenceV2{
			Evidence:          cloneSessionShimAdoptionEvidence(evidence),
			PreparationResult: cloneSessionShimAdoptionPreparationResult(preparation),
		})
	} else {
		receipt, err = cfg.OnAdoption(callbackCtx, cloneSessionShimAdoptionEvidence(evidence))
	}
	if err != nil {
		return SessionShimAdoptionReceipt{}, err
	}
	return cloneSessionShimAdoptionReceipt(receipt), nil
}

func (d *Daemon) reportSessionShimTerminalEvidence(ctx context.Context, evidence SessionShimTerminalEvidence) error {
	// Exactly one of the two proofs, and it must be a whole one. A report
	// carrying both would let a receiver pick the stronger reading of a
	// lineage the daemon only proved unobservable, which is how an
	// attestation silently becomes a reap proof.
	switch {
	case evidence.Absent != nil:
		if evidence.Tombstone != (sessionshim.Tombstone{}) {
			return errors.New("session shim: terminal evidence carries both an absent attestation and a tombstone")
		}
		if !evidence.Absent.Complete() {
			return errors.New("session shim: absent attestation does not prove both process and record absence")
		}
		if evidence.ShimID == "" || evidence.ProcessEpoch == 0 {
			return errors.New("session shim: absent attestation requires the exact shim incarnation")
		}
	case !evidence.Tombstone.GroupReaped:
		return errors.New("session shim: terminal tombstone does not prove process-group reap")
	}
	hook := d.sessionShimConfig().OnTerminalEvidence
	if hook == nil {
		return nil
	}
	if evidence.Adoption != nil {
		adoption := cloneSessionShimAdoptionEvidence(*evidence.Adoption)
		evidence.Adoption = &adoption
	}
	evidence.DurableAdoptionCorrelation = append([]byte(nil), evidence.DurableAdoptionCorrelation...)
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	return hook(callbackCtx, evidence)
}

func cloneSessionShimAdoptionBatch(in SessionShimAdoptionBatch) SessionShimAdoptionBatch {
	in.ExpectedRevision = append([]byte(nil), in.ExpectedRevision...)
	in.Adopted = append([]SessionShimAdoptionOutcome(nil), in.Adopted...)
	for i := range in.Adopted {
		in.Adopted[i].Evidence = cloneSessionShimAdoptionEvidence(in.Adopted[i].Evidence)
		in.Adopted[i].Receipt = cloneSessionShimAdoptionReceipt(in.Adopted[i].Receipt)
	}
	in.Quarantined = append([]sessionshim.QuarantinedSession(nil), in.Quarantined...)
	in.Tombstoned = append([]SessionShimTerminalEvidence(nil), in.Tombstoned...)
	for i := range in.Tombstoned {
		if in.Tombstoned[i].Adoption != nil {
			adoption := cloneSessionShimAdoptionEvidence(*in.Tombstoned[i].Adoption)
			in.Tombstoned[i].Adoption = &adoption
		}
		in.Tombstoned[i].DurableAdoptionCorrelation = append(
			[]byte(nil), in.Tombstoned[i].DurableAdoptionCorrelation...)
	}
	in.Cleared = append([]SessionShimClearedQuarantine(nil), in.Cleared...)
	return in
}

func (d *Daemon) completeSessionShimAdoptionBatch(ctx context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
	cfg := d.sessionShimConfig()
	if cfg.OnAdoptionBatch == nil {
		return SessionShimAdoptionBatchReceipt{}, nil
	}
	// Order every section at the one place every batch passes through. The
	// receiver re-checks the order of ALL FOUR and refuses a batch that
	// disagrees, so leaving it to each assembling caller means whichever one
	// forgets ships an unpublishable batch — which is what happened to the
	// tombstoned section, ordered by nothing but append order until now.
	sortSessionShimAdoptionBatch(&batch)
	if cfg.PrepareAdoptionBatch != nil {
		callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
		expected, err := cfg.PrepareAdoptionBatch(callbackCtx, batch.OrgID, batch.HostID)
		cancel()
		if err != nil {
			return SessionShimAdoptionBatchReceipt{}, err
		}
		if len(expected) == 0 {
			return SessionShimAdoptionBatchReceipt{}, errors.New("session shim: adoption batch preparation omitted expected durable revision")
		}
		batch.ExpectedRevision = append([]byte(nil), expected...)
	}
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	receipt, err := cfg.OnAdoptionBatch(callbackCtx, cloneSessionShimAdoptionBatch(batch))
	if err != nil {
		if sessionShimCommitOutcomeUnknown(err) {
			// The commit request went out and no decoded refusal came back:
			// the control plane may have stamped this batch and advanced the
			// scope's adoption revision while our copy of the answer was lost.
			// Callers classify on this wrap and schedule reconciliation
			// instead of latching the superseded revision forever.
			return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("%w: %w", errSessionShimAmbiguousBatchCommit, err)
		}
		return SessionShimAdoptionBatchReceipt{}, err
	}
	if len(receipt.DurableCorrelation) == 0 {
		return SessionShimAdoptionBatchReceipt{}, errors.New("session shim: adoption batch callback omitted durable revision receipt")
	}
	if (d.sessionShimEnabled() || cfg.OnAdoptionPublished != nil) && receipt.AdoptionRevision == "" {
		return SessionShimAdoptionBatchReceipt{}, errors.New("session shim: attested adoption batch omitted adoption revision")
	}
	if err := validateSessionShimClearedReceiptEcho(batch.Cleared, receipt.Cleared); err != nil {
		return SessionShimAdoptionBatchReceipt{}, err
	}
	receipt.DurableCorrelation = append([]byte(nil), receipt.DurableCorrelation...)
	receipt.Cleared = append([]SessionShimClearedQuarantine(nil), receipt.Cleared...)
	return receipt, nil
}

// validateSessionShimClearedReceiptEcho requires a batch receipt to echo the
// cleared section exactly: same entries, same order, byte-identical fields.
//
// This daemon has no producer for the cleared/abandoned disposition any more —
// an acceptance clear leaves through a real terminal tombstone — so in practice
// this asserts that a receipt claims NOTHING the batch did not send. Keeping
// the check rather than deleting it is deliberate: receipt.Cleared is still
// copied and retained, and a retained field nothing validates is a field a
// receiver can fabricate for free. If a producer ever returns, the check is
// already the right one.
func validateSessionShimClearedReceiptEcho(sent, echoed []SessionShimClearedQuarantine) error {
	if len(echoed) != len(sent) {
		return fmt.Errorf("session shim: adoption batch receipt echoed %d cleared entries, want %d", len(echoed), len(sent))
	}
	for i := range sent {
		if echoed[i] != sent[i] {
			return fmt.Errorf("session shim: adoption batch receipt did not exactly echo cleared entry %d", i)
		}
	}
	return nil
}

// The daemon has no producer for the cleared/abandoned disposition any more.
// An acceptance clear leaves through a real terminal tombstone, and that was
// the only path that ever staged one. The WIRE types below the daemon
// (SessionShimClearedQuarantine, the batch's Cleared section, the receipt's
// echo of it) are retained because the composing plane accepts and records
// them, and the echo is still validated above; the local staging and the
// drop-at-commit bookkeeping are deleted rather than deprecated.

// sessionShimProjectionBatch assembles this daemon's complete durable
// projection for one organization from current state: everything adopted,
// everything quarantined, everything tombstoned. It takes the read lock itself.
//
// The platform's heartbeat preflight compares the beat's quarantine set against
// the snapshot the last adoption-batch commit stored, byte for byte, and demotes
// the host to `draining` when they disagree. So this projection is not merely
// informational: whatever changes the quarantine set must publish the result, or
// the host argues with the platform about its own state until something else
// happens to change it back.
func (d *Daemon) sessionShimProjectionBatch(orgID, hostID string) SessionShimAdoptionBatch {
	batch := SessionShimAdoptionBatch{OrgID: orgID, HostID: hostID}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	for _, entry := range d.shims.adopted {
		if entry.adoption.Identity.OrgID != batch.OrgID {
			continue
		}
		if entry.adoption.CarrierCompatible {
			batch.Adopted = append(batch.Adopted, SessionShimAdoptionOutcome{
				Evidence: cloneSessionShimAdoptionEvidence(entry.adoption),
				Receipt:  cloneSessionShimAdoptionReceipt(entry.adoptionReceipt),
			})
			continue
		}
		hello := entry.controller.Hello()
		batch.Quarantined = append(batch.Quarantined, sessionshim.QuarantinedSession{
			OrgID: entry.adoption.Identity.OrgID, SessionID: entry.adoption.Identity.SessionID,
			ShimID: entry.shimID, ProcessEpoch: hello.ProcessEpoch,
			ControllerGeneration: entry.adoption.ControllerGeneration,
			ProtocolMin:          hello.Min, ProtocolMax: hello.Max, Phase: hello.Phase,
			Reason: sessionShimCarrierQuarantineReason(entry.adoption.CarrierIncompatibility), ConsumesCapacity: true,
		})
	}
	for _, quarantined := range d.shims.quarantined {
		if quarantined.OrgID != batch.OrgID {
			continue
		}
		batch.Quarantined = append(batch.Quarantined, quarantined)
	}
	for _, tombstone := range d.shims.tombstoned {
		if tombstone.OrgID == batch.OrgID {
			batch.Tombstoned = append(batch.Tombstoned, SessionShimTerminalEvidence{
				Identity: tombstone.Identity(), HostID: batch.HostID,
				ShimID: tombstone.ShimID, ProcessEpoch: tombstone.ProcessEpoch, Tombstone: tombstone,
			})
		}
	}
	return batch
}

// publishSessionShimProjection republishes an organization's durable projection
// after the quarantine set changed outside an adoption.
//
// Adoption and tombstone reconciliation already publish; a quarantine that
// arrives between them — a controller stream that ended without a terminal
// observation, or the acceptance seam — did not, and the host then failed every
// heartbeat until the shim happened to be tombstoned.
//
// A failure here is logged rather than returned: the callers are release paths
// that must not be blocked by the platform being unreachable, and the next
// adoption or tombstone republishes the same projection anyway. A caller that
// must observe the outcome — the acceptance clear waiting for its cleared
// entry's confirmed commit — uses republishSessionShimProjection directly.
func (d *Daemon) publishSessionShimProjection(ctx context.Context, orgID string) {
	_ = d.republishSessionShimProjection(ctx, orgID)
}

// republishSessionShimProjection publishes the organization's complete current
// projection and returns the first failure (host resolution, batch commit,
// receipt retention). Every failure is also logged, so the fire-and-forget
// wrapper above loses nothing.
func (d *Daemon) republishSessionShimProjection(ctx context.Context, orgID string) error {
	if d.shims == nil || d.sessionShimConfig().OnAdoptionBatch == nil || orgID == "" {
		return nil
	}
	hostID, err := d.sessionShimHostID(ctx, orgID)
	if err != nil {
		slog.Warn("session shim: quarantine projection not published after host identity resolution failed",
			"org", orgID, "error", err)
		return fmt.Errorf("session shim: resolve host authority for republish: %w", err)
	}
	batch := d.sessionShimProjectionBatch(orgID, hostID)
	receipt, err := d.completeSessionShimAdoptionBatch(ctx, batch)
	if err != nil {
		if errors.Is(err, errSessionShimAmbiguousBatchCommit) {
			// OUTCOME-UNKNOWN: the control plane may hold this batch and a
			// revision this daemon never learned. The beat keeps presenting
			// the last-committed projection meanwhile; reconciliation learns
			// the committed revision through the refresher and republishes
			// the complete batch. A trigger landing from inside the
			// reconciliation loop itself is deduplicated by the scope mark.
			d.scheduleSessionShimReconciliation(orgID, "ambiguous-batch-commit")
		}
		slog.Warn("session shim: quarantine projection not published",
			"org", orgID, "adopted", len(batch.Adopted), "quarantined", len(batch.Quarantined),
			"tombstoned", len(batch.Tombstoned), "cleared", len(batch.Cleared), "error", err)
		return err
	}
	// Committing a batch advances the host's adoption revision. The heartbeat
	// attests the revision this daemon believes it is at, and the platform
	// refuses — and demotes the host — when the two disagree. So a republish
	// that does not retain its own receipt trades one divergence for another.
	//
	// No heartbeat acknowledgement is pending: this batch activates no carrier,
	// so readiness is not withdrawn and the revision applies immediately.
	if revisionErr := d.updateSessionShimAdoptionRevision(orgID, receipt.AdoptionRevision, false); revisionErr != nil {
		slog.Warn("session shim: adoption revision not retained after republishing the projection",
			"org", orgID, "error", revisionErr)
		return revisionErr
	}
	slog.Info("session shim: republished the durable projection after a quarantine change",
		"org", orgID, "adopted", len(batch.Adopted), "quarantined", len(batch.Quarantined),
		"revision", receipt.AdoptionRevision)
	return nil
}

// sortSessionShimAdoptionOutcomes puts a batch's adopted set in the exact order
// the platform's own comparator defines, rather than leaking Go map order.
//
// The receiving side re-checks the order and refuses a batch that disagrees, so
// the two comparators must be the same comparator. This one omitted
// ControllerGeneration, which the platform's includes: two adopted entries for
// one shim incarnation differing only in generation could be emitted in an
// order the platform rejects. SortQuarantined already keys on the full tuple.
func sortSessionShimAdoptionOutcomes(in []SessionShimAdoptionOutcome) {
	sort.Slice(in, func(i, j int) bool {
		a, b := in[i].Evidence, in[j].Evidence
		if a.Identity.OrgID != b.Identity.OrgID {
			return a.Identity.OrgID < b.Identity.OrgID
		}
		if a.Identity.SessionID != b.Identity.SessionID {
			return a.Identity.SessionID < b.Identity.SessionID
		}
		if a.ShimID != b.ShimID {
			return a.ShimID < b.ShimID
		}
		if a.ProcessEpoch != b.ProcessEpoch {
			return a.ProcessEpoch < b.ProcessEpoch
		}
		return a.ControllerGeneration < b.ControllerGeneration
	})
}

// sortSessionShimAdoptionBatch puts every section of one batch in the exact
// order the receiver's comparator defines.
func sortSessionShimAdoptionBatch(batch *SessionShimAdoptionBatch) {
	sortSessionShimAdoptionOutcomes(batch.Adopted)
	sessionshim.SortQuarantined(batch.Quarantined)
	sortSessionShimTerminalEvidence(batch.Tombstoned)
	sortSessionShimClearedQuarantines(batch.Cleared)
}

// sortSessionShimTerminalEvidence orders a batch's tombstoned set by the same
// tuple as every other section. A terminal entry carries no controller
// generation, so the lineage correlation is the whole key.
func sortSessionShimTerminalEvidence(in []SessionShimTerminalEvidence) {
	// Stable: the key ends at the process epoch, so two entries for one exact
	// incarnation compare equal and an unstable sort could reorder them between
	// two publications of the same set — which the receiver reads as a changed
	// batch.
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Identity.OrgID != b.Identity.OrgID {
			return a.Identity.OrgID < b.Identity.OrgID
		}
		if a.Identity.SessionID != b.Identity.SessionID {
			return a.Identity.SessionID < b.Identity.SessionID
		}
		if a.ShimID != b.ShimID {
			return a.ShimID < b.ShimID
		}
		return a.ProcessEpoch < b.ProcessEpoch
	})
}

func (d *Daemon) completeLaunchedSessionShimAdoptionBatch(
	ctx context.Context,
	evidence SessionShimAdoptionEvidence,
	receipt SessionShimAdoptionReceipt,
) (SessionShimAdoptionBatchReceipt, error) {
	if d.sessionShimConfig().OnAdoptionBatch == nil {
		return SessionShimAdoptionBatchReceipt{}, nil
	}
	batch := d.sessionShimProjectionBatch(evidence.Identity.OrgID, evidence.HostID)
	if evidence.CarrierCompatible {
		batch.Adopted = append(batch.Adopted, SessionShimAdoptionOutcome{
			Evidence: cloneSessionShimAdoptionEvidence(evidence), Receipt: cloneSessionShimAdoptionReceipt(receipt),
		})
	} else {
		batch.Quarantined = append(batch.Quarantined, sessionshim.QuarantinedSession{
			OrgID: evidence.Identity.OrgID, SessionID: evidence.Identity.SessionID,
			ShimID: evidence.ShimID, ProcessEpoch: evidence.ProcessEpoch,
			ControllerGeneration: evidence.ControllerGeneration,
			Reason:               sessionShimCarrierQuarantineReason(evidence.CarrierIncompatibility), ConsumesCapacity: true,
		})
	}
	return d.completeSessionShimAdoptionBatch(ctx, batch)
}

func cloneSessionShimScopeCredentialReceipt(in SessionShimScopeCredentialReceipt) SessionShimScopeCredentialReceipt {
	return in
}

func sortSessionShimScopeReceipts(in []SessionShimScopeCredentialReceipt) {
	sort.Slice(in, func(i, j int) bool { return in[i].Scope < in[j].Scope })
}

func validateSessionShimScopeReceipts(in []SessionShimScopeCredentialReceipt) error {
	for i, receipt := range in {
		if receipt.Scope == "" || receipt.WorkerHostID == "" || receipt.AdoptionRevision == "" {
			return fmt.Errorf("session shim: recovery scope receipt %d is incomplete", i)
		}
		if receipt.WorkerHostID == "daemon" {
			return fmt.Errorf("session shim: recovery scope %q uses a reserved stable host authority", receipt.Scope)
		}
		if i > 0 && in[i-1].Scope >= receipt.Scope {
			return errors.New("session shim: recovery scope receipts must be strictly ordered without duplicates")
		}
	}
	return nil
}

func (d *Daemon) retainSessionShimCredentialReceipts(receipts []SessionShimScopeCredentialReceipt) error {
	if !d.sessionShimEnabled() {
		return nil
	}
	cloned := append([]SessionShimScopeCredentialReceipt(nil), receipts...)
	sortSessionShimScopeReceipts(cloned)
	if err := validateSessionShimScopeReceipts(cloned); err != nil {
		return err
	}
	for _, receipt := range cloned {
		if err := d.validateControllerAlias(receipt.WorkerHostID, "stable host id"); err != nil {
			return err
		}
	}
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	for _, receipt := range cloned {
		if prior, ok := d.shims.credentialReceipts[receipt.Scope]; ok && prior != receipt {
			return fmt.Errorf("session shim: recovery scope %q changed authority or adoption revision", receipt.Scope)
		}
		d.shims.credentialReceipts[receipt.Scope] = cloneSessionShimScopeCredentialReceipt(receipt)
	}
	return nil
}

func (d *Daemon) sessionShimCredentialReceipts() []SessionShimScopeCredentialReceipt {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	out := make([]SessionShimScopeCredentialReceipt, 0, len(d.shims.credentialReceipts))
	for _, receipt := range d.shims.credentialReceipts {
		out = append(out, cloneSessionShimScopeCredentialReceipt(receipt))
	}
	d.shims.mu.RUnlock()
	sortSessionShimScopeReceipts(out)
	return out
}

func (d *Daemon) updateSessionShimAdoptionRevision(scope, revision string, heartbeatAckPending bool) error {
	if !d.sessionShimEnabled() {
		return nil
	}
	if revision == "" {
		return errors.New("session shim: adoption revision is required")
	}
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	receipt, ok := d.shims.credentialReceipts[scope]
	if !ok {
		return fmt.Errorf("session shim: no credential receipt retained for scope %q", scope)
	}
	receipt.AdoptionRevision = revision
	d.shims.credentialReceipts[scope] = receipt
	if heartbeatAckPending {
		d.shims.pendingHeartbeatAcks[scope] = revision
		// The lifecycle state and spawner are already closed. Publish the atomic
		// admission fence in the same critical section as the exact revision so a
		// heartbeat can never observe one without the other.
		d.sessionShimReadinessWithdrawn.Store(true)
	}
	return nil
}

func (d *Daemon) validateAndRetainSessionShimRefreshReceipt(result *RefreshTokenResult) error {
	if !d.sessionShimEnabled() {
		return nil
	}
	scope := d.sessionShimConfig().orgID()
	// Readiness is checked before a refreshed receipt is adopted — except for
	// the ONE receipt that founds the scope's host authority. An embedder's
	// readiness resolver answers for the primary host, and the primary host id
	// is what this very receipt carries: asking before it is retained asks for a
	// fact this round trip is producing. That is what drove an embedder to
	// present the attestation early, outside the refresher's lock, to learn the
	// id some other way — and the control plane answered the resulting posture
	// flip-flop with an attestation conflict. The check is deferred, not
	// dropped: declareSessionShimComposition runs it once the receipt is
	// retained and the embedder has been handed it. Every other refresh keeps
	// the check exactly here.
	if !d.sessionShimFoundingDeclaration(scope) {
		if err := d.validateSessionShimCarrierProofV2Readiness(); err != nil {
			d.withdrawSessionShimProofV2Readiness()
			return err
		}
	}
	if result == nil || result.SessionShim == nil {
		return errors.New("session shim: refresh omitted credential receipt")
	}
	receipt := result.SessionShim
	wantState := SessionShimCredentialStateRecovering
	if !d.sessionShimReadinessWithdrawn.Load() && d.State() == StateRunning &&
		d.SessionShimAdoptionComplete() && d.SessionShimCarrierActivationComplete() {
		wantState = SessionShimCredentialStateReady
	}
	if receipt.State != wantState {
		return fmt.Errorf("session shim: refresh receipt state %q, want %q", receipt.State, wantState)
	}
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	prior, ok := d.shims.credentialReceipts[scope]
	if !ok {
		if d.shims.declaringComposition {
			// The refresh that declares a deferred composition IS the round
			// trip that establishes this scope's host authority. There is
			// nothing retained yet to compare against, and retention is the
			// installer's next step — see declareSessionShimComposition.
			return nil
		}
		return fmt.Errorf("session shim: no retained credential receipt for refresh scope %q", scope)
	}
	if prior.WorkerHostID != receipt.WorkerHostID {
		return errors.New("session shim: refresh changed stable host authority")
	}
	prior.AdoptionRevision = receipt.AdoptionRevision
	d.shims.credentialReceipts[scope] = prior
	return nil
}

func sessionShimCarrierActivationFor(evidence SessionShimAdoptionEvidence) (SessionShimCarrierActivation, bool, error) {
	raw, ok := evidence.Extensions.Get(shimwire.ExtCarrierEpoch)
	if !ok {
		return SessionShimCarrierActivation{}, false, nil
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || epoch == 0 || strconv.FormatUint(epoch, 10) != raw {
		return SessionShimCarrierActivation{}, false, errors.New("session shim: carrier_epoch must be a canonical positive uint64")
	}
	return SessionShimCarrierActivation{
		OrgID: evidence.Identity.OrgID, SessionID: evidence.Identity.SessionID, CarrierEpoch: epoch,
	}, true, nil
}

func sortSessionShimCarrierActivations(in []SessionShimCarrierActivation) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].OrgID != in[j].OrgID {
			return in[i].OrgID < in[j].OrgID
		}
		if in[i].SessionID != in[j].SessionID {
			return in[i].SessionID < in[j].SessionID
		}
		return in[i].CarrierEpoch < in[j].CarrierEpoch
	})
}

func exactSessionShimCarrierActivationSet(a, b []SessionShimCarrierActivation) bool {
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

func (d *Daemon) beginStagedSessionShimSnapshot(id sessionshim.Identity) error {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if _, pending := d.shims.pendingSnapshots[id]; d.shims.stagingSnapshots[id] || pending {
		return errors.New("session shim: a mandatory carrier Snapshot is already staged")
	}
	d.shims.stagingSnapshots[id] = true
	d.shims.activationGates[id] = newShimAdoptionGate()
	return nil
}

func (d *Daemon) cancelStagedSessionShimSnapshot(id sessionshim.Identity) {
	d.shims.mu.Lock()
	delete(d.shims.stagingSnapshots, id)
	delete(d.shims.pendingSnapshots, id)
	gate := d.shims.activationGates[id]
	delete(d.shims.activationGates, id)
	d.shims.mu.Unlock()
	gate.finish(false)
}

func (d *Daemon) failPendingSessionShimActivations() {
	d.shims.mu.Lock()
	gates := make([]*shimAdoptionGate, 0, len(d.shims.activationGates))
	for _, gate := range d.shims.activationGates {
		gates = append(gates, gate)
	}
	d.shims.stagingSnapshots = make(map[sessionshim.Identity]bool)
	d.shims.pendingSnapshots = make(map[sessionshim.Identity]sessionshim.ControllerEvent)
	d.shims.activationGates = make(map[sessionshim.Identity]*shimAdoptionGate)
	d.shims.mu.Unlock()
	for _, gate := range gates {
		gate.finish(false)
	}
}

func (d *Daemon) retainStagedSessionShimSnapshot(id sessionshim.Identity, event sessionshim.ControllerEvent) (*shimAdoptionGate, bool) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if !d.shims.stagingSnapshots[id] || event.Kind != sessionshim.EventHostFrame ||
		event.FrameType != attachwire.TypeSnapshot || event.RequestID == 0 {
		return nil, false
	}
	delete(d.shims.stagingSnapshots, id)
	if _, pending := d.shims.pendingSnapshots[id]; pending {
		return d.shims.activationGates[id], false
	}
	d.shims.pendingSnapshots[id] = event
	return d.shims.activationGates[id], true
}

func (d *Daemon) isStagedSessionShimSnapshot(id sessionshim.Identity, event sessionshim.ControllerEvent) bool {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.stagingSnapshots[id] && event.Kind == sessionshim.EventHostFrame &&
		event.FrameType == attachwire.TypeSnapshot && event.RequestID != 0
}

func (d *Daemon) waitStagedSessionShimSnapshot(ctx context.Context, id sessionshim.Identity, sequence uint64) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		d.shims.mu.RLock()
		event, ok := d.shims.pendingSnapshots[id]
		d.shims.mu.RUnlock()
		if ok {
			if event.Kind != sessionshim.EventHostFrame || event.FrameType != attachwire.TypeSnapshot ||
				event.RequestID == 0 || event.Seq != sequence {
				return errors.New("session shim: staged mandatory Snapshot does not match the emitting result")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("session shim: wait for staged mandatory Snapshot: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (d *Daemon) sessionShimPublishedBatchReceipts() []SessionShimPublishedBatchReceipt {
	d.shims.mu.RLock()
	out := make([]SessionShimPublishedBatchReceipt, 0, len(d.shims.batchReceipts))
	for scope, receipt := range d.shims.batchReceipts {
		out = append(out, SessionShimPublishedBatchReceipt{
			Scope: scope, AdoptionRevision: receipt.AdoptionRevision,
		})
	}
	d.shims.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

func (d *Daemon) activatePublishedSessionShimCarriers(
	ctx context.Context,
	entries map[sessionshim.Identity]adoptedShim,
) error {
	if d.sessionShimReadinessWithdrawn.Load() {
		d.shims.mu.RLock()
		awaitingPublicationHeartbeat := len(d.shims.pendingHeartbeatAcks) > 0
		d.shims.mu.RUnlock()
		if !awaitingPublicationHeartbeat {
			return errors.New("session shim: proof-v2 readiness is withdrawn")
		}
	}
	if err := d.validateSessionShimCarrierProofV2Readiness(); err != nil {
		d.withdrawSessionShimProofV2Readiness()
		return fmt.Errorf("session shim: proof-v2 activation readiness: %w", err)
	}
	carriers := make([]SessionShimCarrierActivation, 0, len(entries))
	d.shims.mu.RLock()
	for id, entry := range entries {
		current, adopted := d.shims.adopted[id]
		if !adopted || current.controller != entry.controller {
			d.shims.mu.RUnlock()
			return fmt.Errorf("session shim: carrier activation publication is not the current adopted entry for %s", id)
		}
		_, pendingSnapshot := d.shims.pendingSnapshots[id]
		consumedRecovery := current.consumedRecovery != nil
		carrier, ok, err := sessionShimCarrierActivationFor(entry.adoption)
		if err != nil {
			d.shims.mu.RUnlock()
			return err
		}
		if !pendingSnapshot && !consumedRecovery {
			if current.carrierActivationResolved {
				continue
			}
			if ok && entry.adoption.CarrierCompatible {
				d.shims.mu.RUnlock()
				return fmt.Errorf("session shim: published carrier candidate has no pending Snapshot or consumed recovery for %s", id)
			}
			continue
		}
		if ok && entry.adoption.CarrierCompatible {
			carriers = append(carriers, carrier)
		} else if entry.adoption.CarrierCompatible && d.sessionShimConfig().RequireAuthoritativeSnapshot {
			d.shims.mu.RUnlock()
			return errors.New("session shim: authoritative Snapshot carrier omitted carrier_epoch")
		}
	}
	d.shims.mu.RUnlock()
	sortSessionShimCarrierActivations(carriers)
	for i := 1; i < len(carriers); i++ {
		if carriers[i-1] == carriers[i] {
			return errors.New("session shim: duplicate carrier activation correlation")
		}
	}
	hook := d.sessionShimConfig().OnAdoptionPublished
	if len(carriers) == 0 {
		d.shims.mu.RLock()
		pending := len(d.shims.pendingSnapshots)
		d.shims.mu.RUnlock()
		if pending != 0 {
			return errors.New("session shim: staged Snapshot has no carrier activation correlation")
		}
		d.shims.mu.Lock()
		d.shims.carrierActivationComplete = true
		d.shims.mu.Unlock()
		return nil
	}
	if hook == nil {
		return errors.New("session shim: carrier candidates require OnAdoptionPublished")
	}
	batches := d.sessionShimPublishedBatchReceipts()
	batchScopes := make(map[string]bool, len(batches))
	for _, batch := range batches {
		if batch.Scope == "" || batch.AdoptionRevision == "" {
			return errors.New("session shim: carrier activation is missing a complete published batch receipt")
		}
		batchScopes[batch.Scope] = true
	}
	for _, carrier := range carriers {
		if !batchScopes[carrier.OrgID] {
			return fmt.Errorf("session shim: carrier activation has no published batch for organization %q", carrier.OrgID)
		}
	}
	publication := SessionShimAdoptionPublication{
		ControllerID: d.controllerID(),
		Batches:      batches,
		Carriers:     append([]SessionShimCarrierActivation(nil), carriers...),
	}
	callbackCtx, cancel := d.sessionShimCallbackContext(ctx)
	defer cancel()
	activated, err := hook(callbackCtx, publication)
	if err != nil {
		return err
	}
	activated = append([]SessionShimCarrierActivationReceipt(nil), activated...)
	sort.Slice(activated, func(i, j int) bool {
		a, b := activated[i].Activation, activated[j].Activation
		if a.OrgID != b.OrgID {
			return a.OrgID < b.OrgID
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return a.CarrierEpoch < b.CarrierEpoch
	})
	activatedSet := make([]SessionShimCarrierActivation, len(activated))
	for i := range activated {
		activatedSet[i] = activated[i].Activation
	}
	if !exactSessionShimCarrierActivationSet(carriers, activatedSet) {
		return errors.New("session shim: carrier activation callback did not return the exact complete set")
	}
	type resolvedActivation struct {
		id                  sessionshim.Identity
		sequence            uint64
		requestID           uint64
		gate                *shimAdoptionGate
		ctrl                *sessionshim.Controller
		consumedRecovery    bool
		recoveryPreStageAck uint64
		recoveryReceipt     SessionShimAdoptionReceipt
	}
	resolved := make([]resolvedActivation, 0, len(activated))
	resolvedPendingSnapshots := 0
	d.shims.mu.Lock()
	for _, activation := range activated {
		id := sessionshim.Identity{
			OrgID: activation.Activation.OrgID, SessionID: activation.Activation.SessionID,
		}
		event, ok := d.shims.pendingSnapshots[id]
		entry, adopted := d.shims.adopted[id]
		published, wasPublished := entries[id]
		if !adopted || !wasPublished || entry.controller != published.controller {
			d.shims.mu.Unlock()
			return fmt.Errorf("session shim: carrier_active ack did not resolve the current adopted entry for %s", id)
		}
		recovery := entry.consumedRecovery
		if recovery == nil && ok && event.Kind == sessionshim.EventHostFrame &&
			event.FrameType == attachwire.TypeSnapshot && event.RequestID != 0 && activation.AckSeq == event.Seq {
			resolved = append(resolved, resolvedActivation{
				id: id, sequence: event.Seq, requestID: event.RequestID,
				gate: d.shims.activationGates[id], ctrl: entry.controller,
			})
			resolvedPendingSnapshots++
			continue
		}
		if ok || recovery == nil || recovery.stagedHighWater == 0 ||
			activation.AckSeq != recovery.stagedHighWater || recovery.preStageAckSeq >= recovery.stagedHighWater ||
			len(recovery.adoptionReceipt.DurableCorrelation) == 0 ||
			!bytes.Equal(recovery.adoptionReceipt.DurableCorrelation, entry.adoptionReceipt.DurableCorrelation) ||
			published.consumedRecovery == nil ||
			published.consumedRecovery.preStageAckSeq != recovery.preStageAckSeq ||
			published.consumedRecovery.stagedHighWater != recovery.stagedHighWater ||
			!bytes.Equal(published.consumedRecovery.adoptionReceipt.DurableCorrelation, recovery.adoptionReceipt.DurableCorrelation) {
			d.shims.mu.Unlock()
			return fmt.Errorf("session shim: carrier_active ack did not exactly resolve the staged Snapshot or consumed recovery for %s", id)
		}
		resolved = append(resolved, resolvedActivation{
			id: id, sequence: recovery.stagedHighWater, ctrl: entry.controller,
			consumedRecovery: true, recoveryPreStageAck: recovery.preStageAckSeq,
			recoveryReceipt: cloneSessionShimAdoptionReceipt(recovery.adoptionReceipt),
		})
	}
	if len(d.shims.pendingSnapshots) != resolvedPendingSnapshots {
		d.shims.mu.Unlock()
		return errors.New("session shim: carrier activation left a staged Snapshot unresolved")
	}
	d.shims.mu.Unlock()
	for _, activation := range resolved {
		var acknowledger sessionShimCursorAcknowledger
		if activation.ctrl != nil {
			acknowledger = activation.ctrl
		}
		if err := d.recordShimForwardedSeqForController(activation.id, acknowledger, activation.sequence); err != nil {
			return fmt.Errorf("session shim: persist staged Snapshot acknowledgement for %s: %w", activation.id, err)
		}
	}
	d.shims.mu.Lock()
	for _, activation := range resolved {
		if activation.consumedRecovery {
			current, adopted := d.shims.adopted[activation.id]
			recovery := current.consumedRecovery
			if !adopted || current.controller != activation.ctrl || recovery == nil ||
				recovery.preStageAckSeq != activation.recoveryPreStageAck ||
				recovery.stagedHighWater != activation.sequence ||
				!bytes.Equal(recovery.adoptionReceipt.DurableCorrelation, activation.recoveryReceipt.DurableCorrelation) ||
				!bytes.Equal(current.adoptionReceipt.DurableCorrelation, activation.recoveryReceipt.DurableCorrelation) {
				d.shims.mu.Unlock()
				return fmt.Errorf("session shim: consumed recovery changed while persisting acknowledgement for %s", activation.id)
			}
			current.consumedRecovery = nil
			current.carrierActivationResolved = true
			d.shims.adopted[activation.id] = current
			continue
		}
		current, pending := d.shims.pendingSnapshots[activation.id]
		if !pending || current.Seq != activation.sequence || current.RequestID != activation.requestID ||
			d.shims.activationGates[activation.id] != activation.gate {
			d.shims.mu.Unlock()
			return fmt.Errorf("session shim: staged Snapshot changed while persisting acknowledgement for %s", activation.id)
		}
		delete(d.shims.pendingSnapshots, activation.id)
		delete(d.shims.activationGates, activation.id)
		entry, adopted := d.shims.adopted[activation.id]
		if !adopted || entry.controller != activation.ctrl || entry.carrierActivationResolved {
			d.shims.mu.Unlock()
			return fmt.Errorf("session shim: adopted carrier changed while completing activation for %s", activation.id)
		}
		entry.carrierActivationResolved = true
		d.shims.adopted[activation.id] = entry
	}
	d.shims.mu.Unlock()
	for _, activation := range resolved {
		activation.gate.finish(true)
	}
	d.shims.mu.Lock()
	d.shims.carrierActivationComplete = true
	d.shims.mu.Unlock()
	return nil
}

// SessionShimOccupancy returns how many capacity slots per-session shims hold.
//
// Adopted AND quarantined shims count. §D7 is explicit that a quarantined shim
// is still occupied capacity: its harness is running, this daemon simply has no
// authority over it. Excluding it would advertise slots that are physically in
// use and let the host claim work it cannot actually run.
func (d *Daemon) SessionShimOccupancy() int {
	if d.shims == nil {
		return 0
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return len(d.shims.adopted) + len(d.shims.quarantined)
}

// AdoptedSessionShims returns the identities currently under this daemon's
// control, for host diagnostics.
func (d *Daemon) AdoptedSessionShims() []sessionshim.Identity {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	out := make([]sessionshim.Identity, 0, len(d.shims.adopted))
	for id := range d.shims.adopted {
		out = append(out, id)
	}
	return out
}

// QuarantinedSessions returns the bounded quarantine projection for host status
// and heartbeat payloads (§D7). The same projection reaches both surfaces, so an
// operator reading `host status` and a consumer reading a beat cannot disagree
// about what is quarantined.
func (d *Daemon) QuarantinedSessions() []sessionshim.QuarantinedSession {
	if d.shims == nil {
		return nil
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	if len(d.shims.quarantined) == 0 {
		return nil
	}
	out := make([]sessionshim.QuarantinedSession, len(d.shims.quarantined))
	copy(out, d.shims.quarantined)
	return out
}

// SessionShimAdoptionComplete reports whether the §D4 pass finished.
//
// Readiness reads this. Adoption disabled reads as complete, because a daemon
// that never adopts has nothing left to discover.
func (d *Daemon) SessionShimAdoptionComplete() bool {
	if d.shims == nil {
		return true
	}
	if !d.sessionShimConfig().EnableAdoption {
		return true
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.adoptionComplete
}

// SessionShimCarrierActivationComplete reports remote D13 exact-set completion.
// It does not mean local viewer-mutation authority has been released: hosted
// composers do that per scope from OnCarrierActivationAcknowledged after the
// exact adoption revision heartbeat. Admission therefore also requires the
// heartbeat fence to be clear.
func (d *Daemon) SessionShimCarrierActivationComplete() bool {
	if d.shims == nil || !d.sessionShimConfig().EnableAdoption {
		return true
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.carrierActivationComplete
}

// SessionShimHeartbeatProjection returns one coherent scope-local snapshot for
// the first and subsequent strict heartbeats. It never includes credentials,
// opaque composing receipts, paths, or display detail.
func (d *Daemon) SessionShimHeartbeatProjection(orgID string) (SessionShimHeartbeatProjection, error) {
	if !d.sessionShimEnabled() {
		return SessionShimHeartbeatProjection{}, nil
	}
	readiness, err := d.resolveSessionShimCarrierProofV2Readiness()
	if err != nil {
		d.withdrawSessionShimProofV2Readiness()
		return SessionShimHeartbeatProjection{}, err
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	receipt, ok := d.shims.credentialReceipts[orgID]
	if !ok {
		d.shims.mu.RUnlock()
		return SessionShimHeartbeatProjection{}, fmt.Errorf("session shim: no heartbeat authority receipt for organization %q", orgID)
	}
	projection := SessionShimHeartbeatProjection{
		Enabled:                            true,
		AdoptionComplete:                   d.shims.adoptionComplete,
		WorkerHostID:                       receipt.WorkerHostID,
		ControllerID:                       d.controllerID(),
		AdoptionRevision:                   receipt.AdoptionRevision,
		SessionShimCarrierProofV2Readiness: readiness,
		QuarantinedSessions:                []SessionShimQuarantinedSession{},
	}
	if !d.shims.carrierActivationComplete {
		d.shims.mu.RUnlock()
		return SessionShimHeartbeatProjection{}, errors.New("session shim: carrier activation is not complete")
	}
	for _, q := range d.shims.quarantined {
		if q.OrgID != orgID {
			continue
		}
		projection.QuarantinedSessions = append(projection.QuarantinedSessions, SessionShimQuarantinedSession{
			OrgID: q.OrgID, SessionID: q.SessionID, ShimID: q.ShimID,
			ProcessEpoch: q.ProcessEpoch, ControllerGeneration: strconv.FormatUint(q.ControllerGeneration, 10),
			ProtocolMin: q.ProtocolMin, ProtocolMax: q.ProtocolMax,
			Reason: string(q.Reason), AgeSeconds: q.AgeSeconds, ConsumesCapacity: q.ConsumesCapacity,
		})
	}
	for id, entry := range d.shims.adopted {
		if id.OrgID != orgID || entry.adoption.CarrierCompatible {
			continue
		}
		hello := entry.controller.Hello()
		projection.QuarantinedSessions = append(projection.QuarantinedSessions, SessionShimQuarantinedSession{
			OrgID: id.OrgID, SessionID: id.SessionID, ShimID: entry.shimID,
			ProcessEpoch: hello.ProcessEpoch, ControllerGeneration: strconv.FormatUint(entry.adoption.ControllerGeneration, 10),
			ProtocolMin: hello.Min, ProtocolMax: hello.Max,
			Reason: string(sessionShimCarrierQuarantineReason(entry.adoption.CarrierIncompatibility)), ConsumesCapacity: true,
		})
	}
	d.shims.mu.RUnlock()
	sort.Slice(projection.QuarantinedSessions, func(i, j int) bool {
		return sessionShimQuarantineLess(projection.QuarantinedSessions[i], projection.QuarantinedSessions[j])
	})
	if err := projection.validateReady(); err != nil {
		return SessionShimHeartbeatProjection{}, err
	}
	return projection, nil
}

// SessionShimDiagnostics returns the bounded secret-free ownership projection
// shared by localhost status and doctor. Adopted correlations come from the
// authenticated live Controller.Hello plus durable forwarded sequence; every
// quarantined correlation is retained separately and capacity-charged.
func (d *Daemon) SessionShimDiagnostics() afclient.DaemonSessionShimStatus {
	cfg := d.sessionShimConfig()
	status := afclient.DaemonSessionShimStatus{OwnershipMode: sessionShimOwnershipMode(cfg), ControllerID: d.controllerID()}
	if d.shims == nil {
		status.AdoptionComplete = !cfg.EnableAdoption
		return status
	}
	d.shims.mu.RLock()
	status.AdoptionComplete = !cfg.EnableAdoption || d.shims.adoptionComplete
	status.CarrierActivationComplete = cfg.EnableAdoption && d.shims.carrierActivationComplete
	if d.shims.adoptionCompletedAtUnixNano > 0 {
		status.AdoptionCompletedAt = time.Unix(0, d.shims.adoptionCompletedAtUnixNano).UTC().Format(time.RFC3339Nano)
	}
	status.Adopted = make([]afclient.DaemonSessionShimAdoptedCorrelation, 0, len(d.shims.adopted))
	for id, entry := range d.shims.adopted {
		correlation := afclient.DaemonSessionShimAdoptedCorrelation{
			OrgID: id.OrgID, SessionID: id.SessionID, ShimID: entry.shimID,
			LastForwardedSeq: d.shims.forwarded[id], ConsumesCapacity: true,
			Source:       "adopted",
			ControllerID: d.controllerID(),
		}
		if entry.launched {
			correlation.Source = "launched"
		}
		if entry.controller != nil {
			hello := entry.controller.Hello()
			correlation.ShimID = hello.ShimID
			correlation.ProcessEpoch = hello.ProcessEpoch
			correlation.ControllerGeneration = uint64(entry.controller.Generation())
			correlation.HarnessPID = hello.HarnessPID
			correlation.HarnessStartedAt = hello.HarnessStartedAt
			correlation.ProtocolMin = hello.Min
			correlation.ProtocolMax = hello.Max
			correlation.ProtocolVersion = entry.controller.SelectedVersion()
			correlation.AuthoritativeSnapshot = entry.controller.SupportsAuthoritativeSnapshot()
			if cfg.RequireAuthoritativeSnapshot && !entry.adoption.CarrierCompatible {
				correlation.CarrierIncompatibility = string(entry.adoption.CarrierIncompatibility)
			}
			correlation.Phase = string(hello.Phase)
		}
		status.Adopted = append(status.Adopted, correlation)
	}
	status.Quarantined = append([]sessionshim.QuarantinedSession(nil), d.shims.quarantined...)
	d.shims.mu.RUnlock()
	for i := range status.Quarantined {
		// Detail is display-only and may contain a local socket/workarea path from
		// a comparison failure. Status/doctor need the closed reason and exact
		// correlation, not an unbounded path-bearing error string.
		status.Quarantined[i].Detail = ""
	}
	sort.Slice(status.Adopted, func(i, j int) bool {
		a, b := status.Adopted[i], status.Adopted[j]
		if a.OrgID != b.OrgID {
			return a.OrgID < b.OrgID
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.ShimID != b.ShimID {
			return a.ShimID < b.ShimID
		}
		return a.ProcessEpoch < b.ProcessEpoch
	})
	sessionshim.SortQuarantined(status.Quarantined)
	status.OccupiedSlots = len(status.Adopted) + len(status.Quarantined)
	return status
}

// InspectAdoptedSessionShimSnapshot proxies a fresh read-only snapshot to the
// shim-owned PTY host. The daemon owns no VT and has no cache fallback.
func (d *Daemon) InspectAdoptedSessionShimSnapshot(ctx context.Context, orgID, sessionID string) (shimwire.SnapshotResult, error) {
	ctrl, err := d.adoptedShimController(orgID, sessionID)
	if err != nil {
		return shimwire.SnapshotResult{}, err
	}
	return ctrl.InspectSnapshot(ctx)
}

// EmitAdoptedSessionShimSnapshot proxies one emitting snapshot call to the
// shim-owned PTY host and returns its exact encoded interactive-attach frame.
func (d *Daemon) EmitAdoptedSessionShimSnapshot(ctx context.Context, orgID, sessionID string) (shimwire.SnapshotResult, error) {
	ctrl, err := d.adoptedShimController(orgID, sessionID)
	if err != nil {
		return shimwire.SnapshotResult{}, err
	}
	return ctrl.EmitSnapshot(ctx)
}

// EmitAdoptedSessionShimSnapshotFor emits one Snapshot only when the complete
// captured adoption authority is still current. The exact frame continues
// through the controller event stream; this result is correlation evidence,
// not a second transmission.
func (d *Daemon) EmitAdoptedSessionShimSnapshotFor(
	ctx context.Context,
	ref SessionShimControlRef,
) (shimwire.SnapshotResult, error) {
	ctrl, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return shimwire.SnapshotResult{}, err
	}
	return ctrl.EmitSnapshot(ctx)
}

func (d *Daemon) adoptedShimController(orgID, sessionID string) (*sessionshim.Controller, error) {
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	d.shims.mu.RUnlock()
	if !ok || entry.controller == nil {
		return nil, fmt.Errorf("session shim: %s is not adopted by this daemon", id)
	}
	return entry.controller, nil
}

func sessionShimOwnershipMode(cfg SessionShimConfig) afclient.DaemonSessionShimOwnershipMode {
	switch {
	case cfg.EnableAdoption && cfg.EnableOwnership:
		return afclient.DaemonSessionShimAdoptionAndOwnership
	case cfg.EnableAdoption:
		return afclient.DaemonSessionShimAdoptionOnly
	case cfg.EnableOwnership:
		return afclient.DaemonSessionShimOwnershipOnly
	default:
		return afclient.DaemonSessionShimDisabled
	}
}

// SessionShimAdoptionBatchReceipt returns the durable host-level publication
// receipt retained for heartbeat/status composition in one organization.
func (d *Daemon) SessionShimAdoptionBatchReceipt(orgID string) (SessionShimAdoptionBatchReceipt, bool) {
	if d.shims == nil {
		return SessionShimAdoptionBatchReceipt{}, false
	}
	d.shims.mu.RLock()
	receipt, ok := d.shims.batchReceipts[orgID]
	d.shims.mu.RUnlock()
	receipt.DurableCorrelation = append([]byte(nil), receipt.DurableCorrelation...)
	return receipt, ok
}

// RequestSessionShimRestartFence obtains the durable, acknowledged restart fence
// a PLANNED restart requires (§D9).
//
// The fence enumerates every adopted AND quarantined session, because both kinds
// of harness are still running across the restart. If no acknowledgement
// arrives, this returns an error and the caller MUST refuse the restart and keep
// serving — an unfenced restart is exactly the split-brain the fence prevents.
func (d *Daemon) RequestSessionShimRestartFence(ctx context.Context, fenceID string) (sessionshim.Fence, error) {
	covered := d.sessionShimFenceSnapshot()
	scopeOrg := ""
	for _, session := range covered {
		if scopeOrg == "" {
			scopeOrg = session.OrgID
			continue
		}
		if session.OrgID != scopeOrg && d.sessionShimConfig().HostIDForOrg != nil {
			return sessionshim.Fence{}, fmt.Errorf(
				"%w: multi-organization host identity requires RequestSessionShimRestartFences",
				sessionshim.ErrFenceRequired,
			)
		}
	}
	fence, err := d.requestSessionShimRestartFence(ctx, fenceID, scopeOrg, covered)
	if err != nil {
		return sessionshim.Fence{}, err
	}
	if d.shims != nil {
		d.shims.mu.Lock()
		d.shims.fence = &fence
		d.shims.mu.Unlock()
	}
	return fence, nil
}

// RequestSessionShimRestartFences obtains one exact fence per organization.
//
// Hosted runtime credentials and lifecycle release predicates are tenant
// scoped, while one physical host may serve several organizations. A single
// cross-organization request cannot be authenticated without widening that
// boundary. This additive plural method snapshots once, partitions without
// collapsing identities, and requires every per-org acknowledgement before the
// caller may restart. The same fence id is safe across organizations because
// lifecycle scope is part of every covered identity and store key.
func (d *Daemon) RequestSessionShimRestartFences(ctx context.Context, fenceID string) ([]sessionshim.Fence, error) {
	if fenceID == "" {
		return nil, fmt.Errorf("%w: fence id is required", sessionshim.ErrFenceRequired)
	}
	covered := d.sessionShimFenceSnapshot()
	if len(covered) == 0 {
		return nil, nil
	}
	if d.sessionShimConfig().HostIDForOrg != nil {
		for _, session := range covered {
			if err := (sessionshim.Identity{OrgID: session.OrgID, SessionID: "scope"}).Validate(); err != nil {
				return nil, fmt.Errorf("%w: invalid organization fence scope: %w", sessionshim.ErrFenceRequired, err)
			}
		}
	}
	byOrg := make(map[string][]sessionshim.FencedSession)
	for _, session := range covered {
		byOrg[session.OrgID] = append(byOrg[session.OrgID], session)
	}
	orgIDs := make([]string, 0, len(byOrg))
	for orgID := range byOrg {
		orgIDs = append(orgIDs, orgID)
	}
	sort.Strings(orgIDs)

	fences := make([]sessionshim.Fence, 0, len(orgIDs))
	for _, orgID := range orgIDs {
		fence, err := d.requestSessionShimRestartFence(ctx, fenceID, orgID, byOrg[orgID])
		if err != nil {
			return fences, fmt.Errorf("session shim: restart fence for organization %q: %w", orgID, err)
		}
		fences = append(fences, fence)
		if d.shims != nil {
			d.shims.mu.Lock()
			d.shims.fences[orgID] = fence
			d.shims.mu.Unlock()
		}
	}
	return fences, nil
}

func (d *Daemon) sessionShimFenceSnapshot() []sessionshim.FencedSession {
	var covered []sessionshim.FencedSession
	if d.shims == nil {
		return covered
	}
	d.reconcileQuarantinedTombstones()
	d.shims.mu.RLock()
	for id, entry := range d.shims.adopted {
		coveredSession := sessionshim.FencedSession{
			OrgID: id.OrgID, SessionID: id.SessionID, ShimID: entry.shimID,
			LastForwardedSeq: d.shims.forwarded[id],
		}
		if entry.controller != nil {
			coveredSession.ProcessEpoch = entry.controller.Hello().ProcessEpoch
			coveredSession.ControllerGeneration = uint64(entry.controller.Generation())
		}
		covered = append(covered, coveredSession)
	}
	for _, q := range d.shims.quarantined {
		id := q.Identity()
		covered = append(covered, sessionshim.FencedSession{
			OrgID: q.OrgID, SessionID: q.SessionID, ShimID: q.ShimID, ProcessEpoch: q.ProcessEpoch,
			ControllerGeneration: q.ControllerGeneration,
			LastForwardedSeq:     d.shims.forwarded[id],
		})
	}
	d.shims.mu.RUnlock()
	// RequestFence preserves order byte-for-byte because the composing store's
	// durable acknowledgement must echo the exact request. The daemon owns the
	// snapshot order, so make it deterministic instead of leaking Go map order.
	sort.Slice(covered, func(i, j int) bool {
		if covered[i].OrgID != covered[j].OrgID {
			return covered[i].OrgID < covered[j].OrgID
		}
		if covered[i].SessionID != covered[j].SessionID {
			return covered[i].SessionID < covered[j].SessionID
		}
		if covered[i].ShimID != covered[j].ShimID {
			return covered[i].ShimID < covered[j].ShimID
		}
		if covered[i].ProcessEpoch != covered[j].ProcessEpoch {
			return covered[i].ProcessEpoch < covered[j].ProcessEpoch
		}
		if covered[i].ControllerGeneration != covered[j].ControllerGeneration {
			return covered[i].ControllerGeneration < covered[j].ControllerGeneration
		}
		return covered[i].LastForwardedSeq < covered[j].LastForwardedSeq
	})
	return covered
}

func (d *Daemon) requestSessionShimRestartFence(ctx context.Context, fenceID, orgID string, covered []sessionshim.FencedSession) (sessionshim.Fence, error) {
	cfg := d.sessionShimConfig()
	hostID, err := d.sessionShimHostID(ctx, orgID)
	if err != nil {
		return sessionshim.Fence{}, fmt.Errorf("%w: resolve host identity: %w", sessionshim.ErrFenceRequired, err)
	}
	if hostID == "" && (cfg.ExactFenceStore != nil || cfg.FenceStore != nil) {
		return sessionshim.Fence{}, fmt.Errorf("%w: stable host identity is required for a composing fence store", sessionshim.ErrFenceRequired)
	}
	policy := sessionshim.FencePolicy{RestartBudget: cfg.RestartBudget, Orphan: cfg.Orphan}
	var (
		fence    sessionshim.Fence
		fenceErr error
	)
	if cfg.ExactFenceStore != nil {
		requestKey := orgID + "\x1f" + fenceID
		d.shims.mu.Lock()
		request, ok := d.shims.fenceRequests[requestKey]
		d.shims.mu.Unlock()
		if ok && (request.Fence.HostID != hostID || !sameFencedSessions(request.Fence.Sessions, covered)) {
			return sessionshim.Fence{}, fmt.Errorf(
				"%w: retained fence id no longer matches host or covered correlations",
				sessionshim.ErrFenceRequired,
			)
		}
		if !ok {
			request, fenceErr = sessionshim.NewExactFenceRequest(fenceID, hostID, covered, policy, time.Now())
			if fenceErr != nil {
				return sessionshim.Fence{}, fenceErr
			}
			d.shims.mu.Lock()
			if retained, exists := d.shims.fenceRequests[requestKey]; exists {
				request = retained
			} else {
				d.shims.fenceRequests[requestKey] = sessionshim.CloneFenceRequest(request)
			}
			d.shims.mu.Unlock()
		}
		fence, fenceErr = sessionshim.AcknowledgeExactFence(ctx, cfg.ExactFenceStore, request)
	} else {
		fence, fenceErr = sessionshim.RequestFence(ctx, cfg.FenceStore, fenceID, hostID, covered, policy, time.Now())
	}
	if fenceErr != nil {
		return sessionshim.Fence{}, fenceErr
	}
	return fence, nil
}

func sameFencedSessions(a, b []sessionshim.FencedSession) bool {
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

// SessionShimReleaseDecision is the SINGLE predicate every claim-release and
// terminalization path must consult (§D9/§D10).
//
// It exists as one method rather than a check sprinkled through each reaper for
// the reason the ADR names directly: a per-reaper check recreates split-brain
// through whichever path forgets it. Routing every caller here means a new
// release path either uses the contract or is visibly not using it.
//
// The rule it enforces: fence EXPIRY never releases a claim. Release requires
// either a terminal receipt from an adopted live owner, or a durable shim
// tombstone proving the harness process group was reaped. Without either, the
// session stays visible in reconciliation.
func (d *Daemon) SessionShimReleaseDecision(orgID, sessionID string, proof sessionshim.TerminalProof) sessionshim.ReleaseVerdict {
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	var fence *sessionshim.Fence
	if d.shims != nil {
		d.shims.mu.RLock()
		if scoped, ok := d.shims.fences[id.OrgID]; ok && scoped.Covers(id) {
			fenceCopy := scoped
			fence = &fenceCopy
		} else {
			fence = d.shims.fence
		}
		d.shims.mu.RUnlock()
	}
	// Every lineage this daemon still holds for the identity must be proven,
	// not just SOME lineage of it.
	//
	// §D10 requires proof that "that exact harness process group" was reaped.
	// One lifecycle identity can hold several incarnations at once (§D7's
	// duplicate-identity case), and the scalar proof is identity-scoped: a
	// sibling's group-reaped tombstone would otherwise answer ReleaseAllowed
	// for a session whose real harness is still running, for the rest of this
	// daemon's life, because the retained tombstone never expires. When no
	// fence enumerates the correlations, this daemon's own live set is the
	// enumeration.
	if live := d.liveSessionShimCorrelations(id); len(live) > 0 {
		// The scalar, identity-scoped AdoptedReceipt closes an exact
		// incarnation only when the identity HAS exactly one — the same
		// precondition ReleaseDecision applies to a fence's covered set. With a
		// sibling still live it names no incarnation at all, and honouring it
		// per correlation would release a running harness.
		sole := len(live) == 1
		for _, correlation := range live {
			if !sessionshim.TerminalProofCovers(proof, id, correlation.shimID, correlation.processEpoch, sole) {
				if fence != nil && fence.Covers(id) && !fence.Expired(time.Now()) && fence.State == sessionshim.FenceHeld {
					return sessionshim.ReleaseHeld
				}
				return sessionshim.ReleaseReconcile
			}
		}
	}
	return sessionshim.ReleaseDecision(fence, id, proof, time.Now())
}

// liveSessionShimCorrelations enumerates the incarnations of one identity this
// daemon still holds: every adopted lineage and every quarantined one. A
// quarantined lineage counts — §D7 refuses it authority, not existence, and its
// harness is still running.
func (d *Daemon) liveSessionShimCorrelations(id sessionshim.Identity) []shimIncarnation {
	if d.shims == nil {
		return nil
	}
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	var out []shimIncarnation
	if entry, ok := d.shims.adopted[id]; ok {
		correlation := shimIncarnation{identity: id, shimID: entry.shimID}
		if entry.controller != nil {
			hello := entry.controller.Hello()
			if hello.ShimID != "" {
				correlation.shimID = hello.ShimID
			}
			correlation.processEpoch = hello.ProcessEpoch
		}
		out = append(out, correlation)
	}
	for _, q := range d.shims.quarantined {
		if q.Identity() != id {
			continue
		}
		out = append(out, shimIncarnation{identity: id, shimID: q.ShimID, processEpoch: q.ProcessEpoch})
	}
	return out
}

// SessionShimTerminalProof gathers whatever durable evidence exists that a
// session's workload actually ended.
//
// It looks ONLY for positive observations: an adopted live owner reporting a
// terminal receipt, or a tombstone whose GroupReaped flag records a verified
// reap. Absence of a record, an unreachable socket, and a dead PID are all
// deliberately absent from this function — none of them observes a harness
// stopping, and treating them as proof is the exact inference §D10 forbids.
func (d *Daemon) SessionShimTerminalProof(orgID, sessionID string) sessionshim.TerminalProof {
	if d.shims == nil {
		return sessionshim.TerminalProof{}
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	d.shims.mu.RLock()
	registry := d.shims.registry
	tombstones := append([]sessionshim.Tombstone(nil), d.shims.tombstoned...)
	d.shims.mu.RUnlock()

	if registry != nil {
		if durable, err := registry.ScanTombstones(); err == nil {
			tombstones = append(tombstones, durable...)
		}
	}
	type correlation struct {
		shimID       string
		processEpoch uint64
	}
	unique := make(map[correlation]sessionshim.Tombstone)
	for _, tombstone := range tombstones {
		if tombstone.Identity() != id || !tombstone.GroupReaped {
			continue
		}
		key := correlation{shimID: tombstone.ShimID, processEpoch: tombstone.ProcessEpoch}
		unique[key] = tombstone
	}
	proofTombstones := make([]sessionshim.Tombstone, 0, len(unique))
	for _, tombstone := range unique {
		proofTombstones = append(proofTombstones, tombstone)
	}
	proof := sessionshim.TerminalProof{}
	for i := range proofTombstones {
		tombstone := &proofTombstones[i]
		proof.Correlations = append(proof.Correlations, sessionshim.TerminalCorrelationProof{
			ShimID:       tombstone.ShimID,
			ProcessEpoch: tombstone.ProcessEpoch,
			Tombstone:    tombstone,
		})
	}
	if len(proofTombstones) == 1 {
		proof.Tombstone = &proofTombstones[0]
	}
	return proof
}

// ReleaseAdoptedSessionShims drops every adopted controller WITHOUT stopping any
// session.
//
// This is what an ordinary daemon shutdown does, and the asymmetry is the whole
// design: the daemon lets go of the socket, the shim keeps the harness and
// starts its bounded orphan clock, and the next daemon adopts. Stopping the
// sessions here would make a restart destructive again.
func (d *Daemon) ReleaseAdoptedSessionShims() {
	if d.shims == nil {
		return
	}
	d.shims.mu.Lock()
	adopted := d.shims.adopted
	d.shims.adopted = make(map[sessionshim.Identity]adoptedShim)
	d.shims.adoptionComplete = false
	d.shims.carrierActivationComplete = false
	d.shims.adoptionCompletedAtUnixNano = 0
	// Stop commit-outcome reconciliation with the shims it reconciles: a loop
	// parked in backoff exits on the closed channel, and the consumer join
	// below then observes a fully settled daemon.
	if !d.shims.reconcileStopped {
		d.shims.reconcileStopped = true
		close(d.shims.reconcileStop)
	}
	d.shims.mu.Unlock()
	for _, entry := range adopted {
		if entry.controller != nil {
			_ = entry.controller.Close()
		}
	}
	// Join the event consumers: closing a controller ends its stream, and a
	// consumer still recording bookkeeping while Stop returns would make
	// "shut down" mean less than it says.
	d.waitShimConsumers()
}

// StopAdoptedSessionShim sends a generation-fenced Stop to one adopted session.
// A session this daemon has NOT adopted (including a quarantined one) is refused
// rather than reached for by another means — quarantine means no stop authority
// (§D7), and honouring it here is what keeps "quarantine, not kill" true.
func (d *Daemon) StopAdoptedSessionShim(orgID, sessionID string, reason shimwire.StopReason) error {
	if d.shims == nil {
		return errors.New("session shim: adoption is not configured")
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	d.shims.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session shim: %s is not adopted by this daemon", id)
	}
	if entry.controller == nil {
		// An entry without a live connection cannot carry a generation-fenced
		// Stop, and reaching the shim by any other means would bypass the fence.
		return fmt.Errorf("session shim: %s has no live controller connection", id)
	}
	return entry.controller.Stop(reason)
}
