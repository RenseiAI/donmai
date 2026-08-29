package sessionshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// FenceState is the lifecycle of a restart fence (ADR-2026-08-17 §D9).
type FenceState string

// The closed v1 fence-state registry.
const (
	// FenceHeld: the fence is durable and in force. External reapers may OBSERVE
	// the covered sessions but may not release, requeue, or terminalize them.
	FenceHeld FenceState = "held"
	// FenceReconciliationRequired: HoldUntil has passed without proof of outcome.
	// This is emphatically NOT "released" — see ReleaseDecision.
	FenceReconciliationRequired FenceState = "reconciliation_required"
	// FenceConsumed: every covered session reached a proven outcome and the fence
	// is finished.
	FenceConsumed FenceState = "consumed"
)

// ErrFenceRequired reports that a planned restart could not obtain a durable,
// acknowledged fence. The correct response is to REFUSE the restart and keep
// serving (§D9) — an unfenced restart is the split-brain the fence exists to
// prevent.
var ErrFenceRequired = errors.New("sessionshim: durable restart fence not acknowledged")

// Fence is the durable record of a planned restart.
//
// It names the exact session identities being handed across the restart. "Exact"
// is load-bearing: a fence that covered "this host" rather than an enumerated
// set would silently cover sessions created after it was issued, which is
// precisely the coverage nobody verified.
type Fence struct {
	FenceID string `json:"fenceId"`
	HostID  string `json:"hostId"`
	// There is deliberately no fence-level controller generation here.
	// session-shim-v1 owns one monotonic generation PER SHIM, while one fence may
	// cover many adopted and quarantined shims. Taking a maximum, minimum, PID,
	// or daemon-local counter would fabricate an authority the protocol does not
	// define. A composing plane that requires one host-level generation must first
	// define and persist that separate monotonic authority.

	// Sessions is the enumerated set of adopted AND quarantined identities the
	// restart must preserve. Quarantined sessions are included because their
	// harnesses are still running — they are refused authority, not dead.
	Sessions []FencedSession `json:"sessions"`

	IssuedAtUnixNano  int64 `json:"issuedAt"`
	HoldUntilUnixNano int64 `json:"holdUntil"`

	State FenceState `json:"state"`

	// DurableRevision is the composing store's non-empty persistence receipt.
	// It is response-only: the exact request bytes omit it. An OSS-only daemon
	// with no store leaves it empty because local intent is not remote durability.
	DurableRevision string `json:"revision,omitempty"`
}

// FencedSession is one identity covered by a fence, with its shim and carrier
// correlation values. No correlation field is lifecycle identity (§D2).
type FencedSession struct {
	OrgID                string `json:"orgId"`
	SessionID            string `json:"sessionId"`
	ShimID               string `json:"shimId,omitempty"`
	ProcessEpoch         uint64 `json:"processEpoch"`
	ControllerGeneration uint64 `json:"controllerGeneration"`
	// LastForwardedSeq is the highest shim-owned output sequence the daemon had
	// durably handed to its carrier when it snapshotted this restart intent. Zero
	// means no sequence has been durably forwarded; it is a real correlation
	// value and therefore is never omitted from the wire.
	LastForwardedSeq uint64 `json:"lastForwardedSeq"`
}

// Identity returns the covered session's lifecycle identity.
func (f FencedSession) Identity() Identity {
	return Identity{OrgID: f.OrgID, SessionID: f.SessionID}
}

// IssuedAt returns when the fence was issued.
func (f Fence) IssuedAt() time.Time { return time.Unix(0, f.IssuedAtUnixNano) }

// HoldUntil returns the end of the fence's hold window.
func (f Fence) HoldUntil() time.Time { return time.Unix(0, f.HoldUntilUnixNano) }

// Covers reports whether the fence names this identity.
func (f Fence) Covers(id Identity) bool {
	for _, s := range f.Sessions {
		if s.Identity() == id {
			return true
		}
	}
	return false
}

// Expired reports whether the hold window has elapsed.
//
// Expired does NOT mean released. It means the fence has moved to
// reconciliation_required and the session needs PROOF of outcome. Callers must
// route through ReleaseDecision rather than acting on this bool.
func (f Fence) Expired(now time.Time) bool {
	return f.HoldUntilUnixNano > 0 && !now.Before(f.HoldUntil())
}

// FenceStore is the OPTIONAL composing-plane callback that persists a fence.
//
// It is an interface, and optional, because §D9 says so explicitly: an OSS-only
// daemon has no remote reaper and therefore needs no fence. Making this a
// required dependency would import a control-plane assumption into a package
// that must run standalone. A nil FenceStore is a supported configuration —
// RequestFence then returns a locally-satisfied fence.
type FenceStore interface {
	// Acknowledge durably persists the fence and returns the acknowledged record.
	// This is the v0.67 public contract. Legacy stores retain semantic
	// exact-session verification below; exact-byte durability is opt-in through
	// ExactFenceStore so this interface remains source-compatible.
	Acknowledge(ctx context.Context, f Fence) (Fence, error)
}

// ExactFenceStore is the additive composing-plane contract for hosted restart
// fencing. The store receives the immutable request bytes produced by
// RequestFence and must echo those bytes byte-for-byte together with a
// non-empty, store-issued durable revision. A store that normalizes JSON,
// reorders sessions, drops correlations, or returns a partial acknowledgement
// is refused. It is intentionally separate from FenceStore: changing the
// latter would break implementations compiled against the v0.67 API.
type ExactFenceStore interface {
	AcknowledgeExact(ctx context.Context, request FenceRequest) (FenceAcknowledgement, error)
}

// FenceRequest is the semantic restart intent plus its exact serialized bytes.
//
// RequestFence constructs both from one immutable snapshot. Passing the bytes
// to the composing store avoids a second encoder silently changing array order,
// numeric representation, or correlation fields before durable persistence.
type FenceRequest struct {
	Fence        Fence
	RequestBytes []byte
}

// FenceAcknowledgement is the minimum proof a composing store returns.
// RequestBytes echo the exact bytes it durably accepted. DurableRevision is an
// opaque, non-empty store revision suitable for later reconciliation.
type FenceAcknowledgement struct {
	RequestBytes    []byte
	DurableRevision string
}

// CloneFenceRequest returns a deep copy safe to retain across composing-store
// calls that may mutate their input values.
func CloneFenceRequest(request FenceRequest) FenceRequest {
	request.Fence.Sessions = append([]FencedSession(nil), request.Fence.Sessions...)
	request.RequestBytes = append([]byte(nil), request.RequestBytes...)
	return request
}

// FencePolicy computes the hold window.
//
// HoldUntil covers the restart budget PLUS the entire orphan bound, because the
// window that must be protected is not "how long the restart takes" but "how
// long until the outcome is knowable". A fence sized to the restart alone
// expires exactly when a slow restart needs it most.
type FencePolicy struct {
	RestartBudget time.Duration
	Orphan        OrphanPolicy
}

// HoldWindow returns the total hold duration.
func (p FencePolicy) HoldWindow() time.Duration {
	budget := p.RestartBudget
	if budget <= 0 {
		budget = 2 * time.Minute
	}
	return budget + p.Orphan.TotalBound()
}

// RequestFence obtains a durable, acknowledged fence covering exactly ids
// using the v0.67 FenceStore contract.
//
// With a nil store the fence is locally satisfied: an OSS-only daemon records
// the intent without a remote acknowledgement, which is correct because there is
// no remote reaper to fence against.
//
// The acknowledgement is verified to cover the same semantic identity and
// correlation set as the requested fence. It preserves the old source and
// runtime contract for OSS embedders.
func RequestFence(ctx context.Context, store FenceStore, fenceID, hostID string, ids []FencedSession, policy FencePolicy, now time.Time) (Fence, error) {
	if fenceID == "" {
		return Fence{}, fmt.Errorf("%w: fence id is required", ErrFenceRequired)
	}
	requested := sortedFencedSessions(ids)
	f := Fence{
		FenceID:           fenceID,
		HostID:            hostID,
		Sessions:          requested,
		IssuedAtUnixNano:  now.UnixNano(),
		HoldUntilUnixNano: now.Add(policy.HoldWindow()).UnixNano(),
		State:             FenceHeld,
	}
	if store == nil {
		return f, nil
	}
	ack, err := store.Acknowledge(ctx, f)
	if err != nil {
		return Fence{}, fmt.Errorf("%w: %w", ErrFenceRequired, err)
	}
	if ack.FenceID != f.FenceID {
		return Fence{}, fmt.Errorf("%w: acknowledgement names fence %q, requested %q", ErrFenceRequired, ack.FenceID, f.FenceID)
	}
	ackSessions := sortedFencedSessions(ack.Sessions)
	if len(ackSessions) != len(requested) {
		return Fence{}, fmt.Errorf("%w: acknowledgement covers %d sessions, requested %d", ErrFenceRequired, len(ackSessions), len(requested))
	}
	for i, want := range requested {
		if ackSessions[i] != want {
			return Fence{}, fmt.Errorf(
				"%w: acknowledgement session %d correlation differs: got %+v, requested %+v",
				ErrFenceRequired, i, ackSessions[i], want)
		}
	}
	ack.Sessions = ackSessions
	return ack, nil
}

// RequestFenceExact is the additive hosted restart-fence path. It preserves
// the caller's ordered snapshot, sends the exact bytes from that one snapshot,
// and accepts only a byte-for-byte echo with a non-empty durable revision.
// Normalizing JSON or reconstructing the request in a composing store is a
// refusal because it can change the durable intent.
func RequestFenceExact(ctx context.Context, store ExactFenceStore, fenceID, hostID string, ids []FencedSession, policy FencePolicy, now time.Time) (Fence, error) {
	request, err := NewExactFenceRequest(fenceID, hostID, ids, policy, now)
	if err != nil {
		return Fence{}, err
	}
	return AcknowledgeExactFence(ctx, store, request)
}

// NewExactFenceRequest constructs one immutable exact request without sending
// it. A caller may retain and replay this object after a partial multi-scope
// acknowledgement; re-sampling time or coverage under the same fence id would
// violate the store's byte-idempotency contract.
func NewExactFenceRequest(fenceID, hostID string, ids []FencedSession, policy FencePolicy, now time.Time) (FenceRequest, error) {
	if fenceID == "" {
		return FenceRequest{}, fmt.Errorf("%w: fence id is required", ErrFenceRequired)
	}
	f := Fence{
		FenceID:           fenceID,
		HostID:            hostID,
		Sessions:          append([]FencedSession(nil), ids...),
		IssuedAtUnixNano:  now.UnixNano(),
		HoldUntilUnixNano: now.Add(policy.HoldWindow()).UnixNano(),
		State:             FenceHeld,
	}
	requestBytes, err := json.Marshal(f)
	if err != nil {
		return FenceRequest{}, fmt.Errorf("%w: serialize exact request: %w", ErrFenceRequired, err)
	}
	return FenceRequest{
		Fence: Fence{
			FenceID:           f.FenceID,
			HostID:            f.HostID,
			Sessions:          append([]FencedSession(nil), f.Sessions...),
			IssuedAtUnixNano:  f.IssuedAtUnixNano,
			HoldUntilUnixNano: f.HoldUntilUnixNano,
			State:             f.State,
		},
		RequestBytes: append([]byte(nil), requestBytes...),
	}, nil
}

// AcknowledgeExactFence sends a previously frozen request and accepts only its
// byte-identical durable acknowledgement.
func AcknowledgeExactFence(ctx context.Context, store ExactFenceStore, request FenceRequest) (Fence, error) {
	request = CloneFenceRequest(request)
	expectedBytes, err := json.Marshal(request.Fence)
	if err != nil {
		return Fence{}, fmt.Errorf("%w: serialize retained exact request: %w", ErrFenceRequired, err)
	}
	if !bytes.Equal(request.RequestBytes, expectedBytes) {
		return Fence{}, fmt.Errorf("%w: retained request bytes differ from semantic fence", ErrFenceRequired)
	}
	if store == nil {
		return request.Fence, nil
	}
	ack, err := store.AcknowledgeExact(ctx, FenceRequest{
		Fence: Fence{
			FenceID:           request.Fence.FenceID,
			HostID:            request.Fence.HostID,
			Sessions:          append([]FencedSession(nil), request.Fence.Sessions...),
			IssuedAtUnixNano:  request.Fence.IssuedAtUnixNano,
			HoldUntilUnixNano: request.Fence.HoldUntilUnixNano,
			State:             request.Fence.State,
		},
		RequestBytes: append([]byte(nil), request.RequestBytes...),
	})
	if err != nil {
		return Fence{}, fmt.Errorf("%w: %w", ErrFenceRequired, err)
	}
	if strings.TrimSpace(ack.DurableRevision) == "" {
		return Fence{}, fmt.Errorf("%w: acknowledgement omitted durable revision", ErrFenceRequired)
	}
	if !bytes.Equal(ack.RequestBytes, request.RequestBytes) {
		return Fence{}, fmt.Errorf("%w: acknowledgement bytes differ from exact ordered request", ErrFenceRequired)
	}
	f := request.Fence
	f.DurableRevision = ack.DurableRevision
	return f, nil
}

func sortedFencedSessions(in []FencedSession) []FencedSession {
	sorted := append([]FencedSession(nil), in...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].OrgID != sorted[j].OrgID {
			return sorted[i].OrgID < sorted[j].OrgID
		}
		if sorted[i].SessionID != sorted[j].SessionID {
			return sorted[i].SessionID < sorted[j].SessionID
		}
		if sorted[i].ShimID != sorted[j].ShimID {
			return sorted[i].ShimID < sorted[j].ShimID
		}
		if sorted[i].ProcessEpoch != sorted[j].ProcessEpoch {
			return sorted[i].ProcessEpoch < sorted[j].ProcessEpoch
		}
		if sorted[i].ControllerGeneration != sorted[j].ControllerGeneration {
			return sorted[i].ControllerGeneration < sorted[j].ControllerGeneration
		}
		return sorted[i].LastForwardedSeq < sorted[j].LastForwardedSeq
	})
	return sorted
}

// TerminalProof is evidence that a session's workload actually ended.
//
// There are exactly two admissible forms (§D10), and both are POSITIVE
// observations. Neither elapsed time nor an unreachable socket nor a dead PID is
// on this list, because none of them observes the harness stopping.
type TerminalProof struct {
	// AdoptedReceipt is true when a live adopted owner reported an ordinary
	// terminal receipt for this session.
	AdoptedReceipt bool
	// Tombstone is a durable shim tombstone proving the harness process group
	// was reaped. Only a tombstone with GroupReaped set is proof.
	Tombstone *Tombstone
	// Correlations are exact per-shim proofs used when one lifecycle identity has
	// multiple fenced incarnations. Legacy scalar fields above remain valid for
	// a fence with zero or one matching correlation.
	Correlations []TerminalCorrelationProof
}

// TerminalCorrelationProof is positive evidence for one exact shim/process
// incarnation. AdoptedReceipt and Tombstone have the same meaning as their
// legacy scalar counterparts but cannot authorize a sibling correlation.
type TerminalCorrelationProof struct {
	ShimID         string
	ProcessEpoch   uint64
	AdoptedReceipt bool
	Tombstone      *Tombstone
}

// Proves reports whether this evidence closes the lifecycle loop.
func (p TerminalProof) Proves() bool {
	if p.AdoptedReceipt {
		return true
	}
	if p.Tombstone != nil && p.Tombstone.GroupReaped {
		return true
	}
	for _, proof := range p.Correlations {
		if proof.AdoptedReceipt || (proof.Tombstone != nil && proof.Tombstone.GroupReaped) {
			return true
		}
	}
	return false
}

func (p TerminalProof) provesCorrelation(id Identity, fenced FencedSession) bool {
	for _, proof := range p.Correlations {
		if proof.ShimID != fenced.ShimID || proof.ProcessEpoch != fenced.ProcessEpoch {
			continue
		}
		if proof.AdoptedReceipt {
			return true
		}
		if proof.Tombstone != nil && proof.Tombstone.GroupReaped &&
			proof.Tombstone.Identity() == id && proof.Tombstone.ShimID == fenced.ShimID &&
			proof.Tombstone.ProcessEpoch == fenced.ProcessEpoch {
			return true
		}
	}
	return false
}

func (p TerminalProof) legacyProvesSingleCorrelation(id Identity, fenced FencedSession) bool {
	if p.AdoptedReceipt {
		return true
	}
	return p.Tombstone != nil && p.Tombstone.GroupReaped && p.Tombstone.Identity() == id &&
		p.Tombstone.ShimID == fenced.ShimID && p.Tombstone.ProcessEpoch == fenced.ProcessEpoch
}

// TerminalProofCovers reports whether proof positively closes ONE exact
// incarnation of id, by EITHER admissible §D10 form.
//
// It is the incarnation-scoped question, exported because a caller that holds
// several live correlations must be able to ask about each one rather than
// about "the session". It still honours the scalar AdoptedReceipt and scalar
// Tombstone: those are identity-scoped by design — they predate duplicate
// identities — and ReleaseDecision itself accepts them, so a pre-check that
// consulted only Correlations would be STRICTER than the predicate it guards
// and would answer `reconcile` for a session whose adopted owner had already
// reported an ordinary terminal receipt.
func TerminalProofCovers(proof TerminalProof, id Identity, shimID string, processEpoch uint64) bool {
	fenced := FencedSession{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: processEpoch,
	}
	return proof.provesCorrelation(id, fenced) || proof.legacyProvesSingleCorrelation(id, fenced)
}

// ReleaseVerdict is the outcome of the single claim-release predicate.
type ReleaseVerdict string

// The closed v1 release-verdict registry.
const (
	// ReleaseAllowed: a terminal proof exists; the claim may be released.
	ReleaseAllowed ReleaseVerdict = "allowed"
	// ReleaseHeld: a fence is in force; observe but do not act.
	ReleaseHeld ReleaseVerdict = "held"
	// ReleaseReconcile: no proof and no protection — the session and its claim
	// stay VISIBLE in reconciliation quarantine rather than being released.
	ReleaseReconcile ReleaseVerdict = "reconciliation_required"
)

// ReleaseDecision is the ONE predicate every claim-release and terminalization
// path must consult (§D9).
//
// It is a single function on purpose. The ADR names "a release path forgets the
// restart fence" as a top risk, and observes that a per-reaper check recreates
// split-brain through whichever path was omitted. Centralising the rule here
// means a new reaper cannot silently opt out — it either calls this or it is
// visibly not using the contract at all.
//
// The rule, stated once:
//
//   - Terminal proof present  -> ReleaseAllowed, whatever the fence says. A
//     proven-ended workload cannot be double-executed.
//   - No proof, fence in force -> ReleaseHeld.
//   - No proof, fence expired  -> ReleaseReconcile. NOT released. Expiry proves
//     that time passed, not that a harness stopped.
//   - No proof, no fence       -> ReleaseReconcile, for the same reason.
func ReleaseDecision(fence *Fence, id Identity, proof TerminalProof, now time.Time) ReleaseVerdict {
	if fence == nil || !fence.Covers(id) {
		if proof.Proves() {
			return ReleaseAllowed
		}
		return ReleaseReconcile
	}
	covered := make([]FencedSession, 0, 1)
	for _, session := range fence.Sessions {
		if session.Identity() == id {
			covered = append(covered, session)
		}
	}
	if len(covered) <= 1 {
		if len(covered) == 1 && (proof.legacyProvesSingleCorrelation(id, covered[0]) || proof.provesCorrelation(id, covered[0])) {
			return ReleaseAllowed
		}
	} else {
		allProven := true
		for _, session := range covered {
			if !proof.provesCorrelation(id, session) {
				allProven = false
				break
			}
		}
		if allProven {
			return ReleaseAllowed
		}
	}
	if !fence.Expired(now) && fence.State == FenceHeld {
		return ReleaseHeld
	}
	return ReleaseReconcile
}

// Reconcile advances an expired fence to reconciliation_required. It never
// advances to a released state — no state transition in this package can release
// a claim, which is what makes "expiry is not proof of death" structural rather
// than a rule someone has to remember.
func (f Fence) Reconcile(now time.Time) Fence {
	if f.State == FenceHeld && f.Expired(now) {
		f.State = FenceReconciliationRequired
	}
	return f
}
