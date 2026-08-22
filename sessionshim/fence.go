package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

	// Sessions is the enumerated set of adopted AND quarantined identities the
	// restart must preserve. Quarantined sessions are included because their
	// harnesses are still running — they are refused authority, not dead.
	Sessions []FencedSession `json:"sessions"`

	IssuedAtUnixNano  int64 `json:"issuedAt"`
	HoldUntilUnixNano int64 `json:"holdUntil"`

	State FenceState `json:"state"`
}

// FencedSession is one identity covered by a fence, with its shim correlation
// values. Neither correlation value is lifecycle identity (§D2).
type FencedSession struct {
	OrgID        string `json:"orgId"`
	SessionID    string `json:"sessionId"`
	ShimID       string `json:"shimId,omitempty"`
	ProcessEpoch uint64 `json:"processEpoch,omitempty"`
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
	// Acknowledge durably persists the fence and returns the acknowledged
	// record. The returned Sessions set MUST equal the requested set; a partial
	// acknowledgement is a refusal.
	Acknowledge(ctx context.Context, f Fence) (Fence, error)
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

// RequestFence obtains a durable, acknowledged fence covering exactly ids.
//
// With a nil store the fence is locally satisfied: an OSS-only daemon records
// the intent without a remote acknowledgement, which is correct because there is
// no remote reaper to fence against.
//
// With a store, the acknowledgement is VERIFIED to cover the exact requested set
// before it is accepted. A store that acknowledges a subset has not agreed to
// protect the rest, and treating that as success is how a release path forgets
// the fence for one session.
func RequestFence(ctx context.Context, store FenceStore, fenceID, hostID string, ids []FencedSession, policy FencePolicy, now time.Time) (Fence, error) {
	if fenceID == "" {
		return Fence{}, fmt.Errorf("%w: fence id is required", ErrFenceRequired)
	}
	sorted := sortedFencedSessions(ids)
	requested := append([]FencedSession(nil), sorted...)
	f := Fence{
		FenceID:           fenceID,
		HostID:            hostID,
		Sessions:          sorted,
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
		return Fence{}, fmt.Errorf("%w: acknowledgement covers %d sessions, requested %d",
			ErrFenceRequired, len(ackSessions), len(requested))
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
		return sorted[i].ProcessEpoch < sorted[j].ProcessEpoch
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
}

// Proves reports whether this evidence closes the lifecycle loop.
func (p TerminalProof) Proves() bool {
	if p.AdoptedReceipt {
		return true
	}
	return p.Tombstone != nil && p.Tombstone.GroupReaped
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
	if proof.Proves() {
		return ReleaseAllowed
	}
	if fence != nil && fence.Covers(id) && !fence.Expired(now) && fence.State == FenceHeld {
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
