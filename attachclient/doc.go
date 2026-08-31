// Package attachclient is the OSS generic outbound attach client for the
// interactive-attach-v1 wire protocol (donmai-architecture/protocol/
// interactive-attach-v1.md, owning ADR ADR-2026-07-12-interactive-pty-session-
// host.md § 3). It dials OUT to a relay, presents a per-session bearer JWT, and
// runs the HOST LEG of one interactive PTY session behind a single interface:
// RunHost forwards a live Session's frames outbound and applies the relay's
// inbound control (input, resize, snapshot_request, kill) to that Session.
//
// The client is brand-neutral and points at no default endpoint: the composing
// binary supplies AttachURL and a TokenSource. It NEVER opens an inbound
// listener — it only dials out (WSS, or HTTPS for the degraded lane), honoring
// the ADR's outbound-only mandate. Viewers are W5/W7/W11; this package is the
// host side only.
//
// TokenSource is resolved before every top-level carrier attempt (§ 15), and
// the degraded carrier may additionally call it concurrently to recover from a
// 401 on SSE-down or POST-up and from its background WSS upgrade probe. Sources
// MUST therefore be safe for concurrent use. This re-resolution is the token-
// refresh seam: a composing binary whose relay provisioner maintains a fresh
// short-exp token (e.g. rewritten to a file the source re-reads, see the
// runner's ATTACH_TOKEN_FILE contract) keeps the host leg reconnectable past
// the initial token's exp; a static closure bounds reconnectability at that exp.
// The same seam repairs an epoch-stale race without allowing zombie takeover:
// every token is checked against Session.Snapshot's local PTY epoch plus the
// immutable InitialAuthorityToken supplied at spawn. The initial token is
// mandatory for the shipped valid-zero legacy epoch surface, so a successor in
// the shared live token file can never redefine an old process's authority. An
// exact same-epoch stale response retries under a finite capped budget while a
// higher current grant stops this host leg as superseded.
//
// # Two carriers, one interface
//
// RunHost speaks the WSS lane (§ 1) and, when WSS is unavailable, transparently
// falls back to the degraded SSE-down + POST-up host lane (§ 14). Carrier
// fallback and upgrade-back are invisible to the Session: the host output
// sequence space is never reset by a carrier switch, and the Session sees one
// continuous consumer.
//
// # Session equivalence
//
// The Session and Subscription interfaces here are STRUCTURALLY IDENTICAL to
// agent.InteractiveSession / agent.InteractiveSubscription
// (donmai/agent/interactive.go). attachclient deliberately does not import
// agent, so the dependency arrow points inward from the composing binary (which
// owns both) rather than between the two libraries. See session.go for the
// exact equivalence and the one-line adapter the composer supplies.
//
// # Two spec readings worth pinning
//
// WSS reconnect has NO host-ack (§ 4.1): the host does NOT retransmit on a WSS
// reconnect. Resuming from the last seq handed to the dropped connection is
// WRONG — it would re-send the gap the relay has already truncated from its
// ring. On reconnect the client instead subscribes fresh and continues from the
// CURRENT stream head (Session.Snapshot().atSeq), so only genuinely new frames
// go out; the relay then requests a resync Snapshot, which the host answers as a
// normal seq-bearing frame that rides the live stream. See host.subscribeFromSeq.
//
// Degraded-lane Input de-duplication (§ 14) names the key (userId, jti,
// inputSeq), where jti identifies the SENDING viewer/driver connection. But the
// Input wire payload (§ 5) carries only [inputSeq][penGeneration][userId][data]
// — there is NO jti on the wire, so the host cannot key on it. The host
// substitutes penGeneration for jti: penGeneration is wire-carried, and the
// relay increments it on every pen change (§ 11.1), so each connection's
// pen-holding period(s) carry distinct penGeneration values. (userId,
// penGeneration, inputSeq) is therefore collision-free across pen handoffs —
// exactly where (userId, inputSeq) alone would collide (the same user's
// laptop→phone handoff both restarting inputSeq at 1) — using only fields the
// wire carries. This satisfies the at-least-once idempotency requirement (drop
// SSE replays) as a bounded recency set. See dedupSet.
package attachclient
