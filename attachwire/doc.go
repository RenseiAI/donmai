// Package attachwire implements the framing and codec layer of the
// interactive-attach-v1 wire protocol — the binary frame format, the two
// sequence namespaces, the typed event payloads, the JSON control-plane
// message set, the snapFormat 0x01 screen serialization, the degraded-lane
// batch envelopes, and a token-bucket backpressure helper.
//
// # Normative source
//
// The authoritative specification is
// donmai-architecture/protocol/interactive-attach-v1.md (revision
// v1.0-draft3). Every byte-level fact in this package is governed by that
// document; where code and spec disagree, the spec wins. This package is
// pure-Go, standard-library only, and carries no transport (WebSocket/HTTP/SSE)
// or relay logic — it is the shared framing library that hosts, viewers, and
// the relay all encode and decode against.
//
// # Frozen vs draft
//
// The specification tags every normative section as either v1-frozen
// (immutable for the life of protocol version interactive-attach-v1) or
// v1-draft (amendable within v1 by the owning wave). This package implements
// both, but the distinction is load-bearing for callers:
//
//   - Frame format (§2), varint encoding (§2.1), event-type byte values (§3),
//     payload layouts (§3.1), the two sequence namespaces (§4/§4.1), Input
//     framing (§5), the control-message SET (§7), Exit ordering (§12.2), and
//     the auth invariants are FROZEN. Implementations MUST reject a frame or
//     behavior that violates a frozen rule rather than tolerate it.
//   - The snapFormat 0x01 byte layout (§12.1), the control-message optional
//     FIELDS (§7), the degraded-lane batch sizing (§14), and the backpressure
//     parameters (§11.2) are DRAFT. Unknown JSON control fields are ignored for
//     forward-compatibility; unknown control-message TYPE values are rejected.
//
// # Out of scope by design
//
// Pen/arbitration POLICY — who may take the pen, when a grab succeeds,
// cooldowns, auto-release, presence derivation — is deliberately NOT
// implemented here. This package carries the grab/release/pen_* messages as an
// opaque, parse-only registry (§7, §11.1); arbitration policy is defined
// elsewhere and can evolve without touching this wire. This library implements
// only the wire encoding and the wire-visible invariants (framing errors, role
// enum, error-code registry).
package attachwire
