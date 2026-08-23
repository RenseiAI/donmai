package sessionshim

import (
	"sort"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// QuarantineReason is the closed set of reasons a survivor was refused adoption
// (ADR-2026-08-17 §D7).
//
// Every local-adoption reason here means the same two things: no controller
// authority is granted, and the shim is not killed. The external-carrier-only
// reason is the deliberate exception: local controller ownership is conserved
// while a capability-dependent carrier is refused. The distinction is diagnostic — an operator
// needs to know whether a shim is unreachable because of a protocol gap or
// because two records claim the same session, and those call for different
// responses.
type QuarantineReason string

// The closed v1 quarantine-reason registry.
const (
	// QuarantineProtocolMismatch: no protocol version overlaps. The shim drains
	// naturally or a compatible daemon adopts it later.
	QuarantineProtocolMismatch QuarantineReason = "protocol_mismatch"
	// QuarantineRecordMalformed: the record failed schema, size, ownership, or
	// mode validation.
	QuarantineRecordMalformed QuarantineReason = "record_malformed"
	// QuarantineDuplicateIdentity: two live records claim one lifecycle identity.
	// BOTH are quarantined — picking one would be a guess about which is real.
	QuarantineDuplicateIdentity QuarantineReason = "duplicate_identity"
	// QuarantineIdentityMismatch: the live peer's self-reported identity, process
	// identity, or workarea disagrees with the record.
	QuarantineIdentityMismatch QuarantineReason = "identity_mismatch"
	// QuarantineUnauthenticated: peer credentials could not be verified, or the
	// socket is not the one the record binds to.
	QuarantineUnauthenticated QuarantineReason = "unauthenticated"
	// QuarantinePhaseUnknown: the shim reports a phase this daemon cannot
	// interpret.
	QuarantinePhaseUnknown QuarantineReason = "phase_unknown"
	// QuarantineGenerationNotAdvanced: adoption could not prove a strictly newer
	// controller generation, so single-controller cannot be guaranteed.
	QuarantineGenerationNotAdvanced QuarantineReason = "generation_not_advanced"
	// QuarantineSocketUnreachable: the record's PID/start identity is live but
	// the socket cannot be reached. Explicitly NOT a kill and NOT a claim release
	// (§D10) — the shim's own orphan deadline is the escape hatch.
	QuarantineSocketUnreachable QuarantineReason = "socket_unreachable"
	// QuarantineAdoptionFailed: the handshake failed for a transport or protocol
	// reason not covered above.
	QuarantineAdoptionFailed QuarantineReason = "adoption_failed"
	// QuarantineGroupReapUnproven: a terminal tombstone exists but could not
	// prove the harness process group disappeared. The session remains visible
	// capacity until exact positive evidence reconciles it; a tombstone-shaped
	// file alone is not proof of death (§D10).
	QuarantineGroupReapUnproven QuarantineReason = "group_reap_unproven"
	// QuarantineAuthoritativeSnapshotUnsupported is a composing-carrier outcome,
	// not a local ownership refusal. Selected v1 remains locally adopted.
	QuarantineAuthoritativeSnapshotUnsupported QuarantineReason = "authoritative_snapshot_unsupported"
)

// Known reports whether r is an assigned v1 quarantine reason.
func (r QuarantineReason) Known() bool {
	switch r {
	case QuarantineProtocolMismatch, QuarantineRecordMalformed, QuarantineDuplicateIdentity,
		QuarantineIdentityMismatch, QuarantineUnauthenticated, QuarantinePhaseUnknown,
		QuarantineGenerationNotAdvanced, QuarantineSocketUnreachable, QuarantineAdoptionFailed,
		QuarantineGroupReapUnproven, QuarantineAuthoritativeSnapshotUnsupported:
		return true
	default:
		return false
	}
}

// QuarantinedSession is the bounded projection of one quarantined shim.
//
// ConsumesCapacity is a field rather than an implied constant so it survives
// serialization into a host-status or heartbeat payload: §D7's whole point is
// that a quarantined shim is VISIBLE and capacity-honest, and a consumer that
// has to infer occupancy from the reason code will eventually infer it wrong.
// It is always true — see NewQuarantinedSession.
type QuarantinedSession struct {
	OrgID     string `json:"orgId"`
	SessionID string `json:"sessionId"`

	ShimID       string `json:"shimId,omitempty"`
	ProcessEpoch uint64 `json:"processEpoch,omitempty"`
	// ControllerGeneration is the exact committed generation for an adopted
	// controller, or the last authenticated Hello generation when adoption was
	// refused before commit. Zero is explicit conservative "unknown": record-only,
	// malformed, or identity-mismatched discovery has no trustworthy Hello, and
	// frozen v1 registry JSON deliberately carries no generation to infer from.
	ControllerGeneration uint64 `json:"controllerGeneration"`
	ProtocolMin          uint32 `json:"protocolMin,omitempty"`
	ProtocolMax          uint32 `json:"protocolMax,omitempty"`

	Reason QuarantineReason `json:"reason"`
	// Detail is display-only. It is never parsed and never carries a secret.
	Detail string `json:"detail,omitempty"`

	// AgeSeconds is how long the quarantined shim has existed, from its record's
	// creation time.
	AgeSeconds int64 `json:"ageSeconds"`

	// ConsumesCapacity is always true (§D7).
	ConsumesCapacity bool `json:"consumesCapacity"`

	Phase shimwire.Phase `json:"phase,omitempty"`
}

// Identity returns the quarantined session's lifecycle identity.
func (q QuarantinedSession) Identity() Identity {
	return Identity{OrgID: q.OrgID, SessionID: q.SessionID}
}

// NewQuarantinedSession builds a projection from a record and a reason.
//
// It is the ONLY constructor, and it hard-codes ConsumesCapacity: true. That is
// deliberate — the field exists to be transported, not to be decided at each
// call site, and a call site that could pass false is a call site that could
// hide occupied capacity.
func NewQuarantinedSession(rec Record, reason QuarantineReason, detail string, now time.Time) QuarantinedSession {
	q := QuarantinedSession{
		OrgID:                rec.OrgID,
		SessionID:            rec.SessionID,
		ShimID:               rec.ShimID,
		ProcessEpoch:         rec.ProcessEpoch,
		ControllerGeneration: 0, // frozen record v1 has no authenticated generation
		ProtocolMin:          rec.ProtocolMin,
		ProtocolMax:          rec.ProtocolMax,
		Reason:               reason,
		Detail:               detail,
		ConsumesCapacity:     true,
		Phase:                rec.Phase,
	}
	if rec.CreatedAtUnixNano > 0 {
		if age := now.Sub(rec.CreatedAt()); age > 0 {
			q.AgeSeconds = int64(age / time.Second)
		}
	}
	return q
}

// SortQuarantined orders a projection deterministically (by identity, shim id,
// then process epoch) so host-status and heartbeat payloads are stable across
// beats and an operator diffing two snapshots sees real changes rather than map
// ordering.
func SortQuarantined(in []QuarantinedSession) {
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
