// Package attachtest is a deliberately-dumb, single-room, mechanism-only stub
// relay for exercising the attachclient host leg end to end. It is TEST
// INFRASTRUCTURE, not a relay: it carries the wire MECHANISM the client must
// interoperate with — WSS accept + subprotocol echo, the (sessionId, epoch)
// host-leg CAS, the seq-keyed ring with § 13 resume (ring hit / snapshot+tail),
// relay→host snapshot_request round-trips, the degraded host lane
// (/host/sse + /host/output with the batch/ack/409/batchId-dedup contract), and
// minimal room_state/pen_state/presence — but NONE of the platform's relay
// POLICY.
//
// # Scope note for W5 (the real, closed relay)
//
// Everything policy-shaped is intentionally trivial or absent here, because
// relay policy lives in the platform (owning-ADR decision D7, protocol § 5/§ 7/
// § 11):
//
//   - Arbitration/pen: the stub uses the single-driver minimum — the first
//     driver-role connection holds the pen forever. No grab/release
//     arbitration, cooldowns, auto-release, or approval flow.
//   - Auth: tokens are parsed UNVERIFIED; the stub checks only aud == "relay"
//     and epoch-presence on host legs (cheap conformance). No Ed25519
//     verification, jti single-use accounting, orgId scoping, exp/leeway, or
//     the browser bearer-subprotocol slot.
//   - Multi-tenancy: one room, one org. No (orgId, sessionId) room key.
//   - Backpressure: none — the ring is bounded but per-viewer send queues,
//     token buckets, and the backpressure disconnect are the real relay's job.
//   - Sanitization (§ 9): NOT applied — the stub forwards host Output verbatim.
//     Viewer-bound sanitization is a relay + viewer responsibility W5/W7/W11 own.
//   - Snapshot+tail contiguity: implemented to the § 13 atSeq+1 rule for the
//     happy path, but without the relay's evict-and-retry loop under load.
//
// W5 reimplements all of the above as the production relay; this stub exists so
// the OSS client has something to talk to in tests and so W5 has an executable
// reference for the wire mechanism it must match.
package attachtest
