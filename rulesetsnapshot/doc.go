// Package rulesetsnapshot is the daemon-side client for a compiled,
// versioned, Ed25519-signed ruleset snapshot — the durability unit an
// embedding control plane MAY publish so the daemon's claim path keeps
// working (fail-static, within a bounded TTL) when that control plane is
// unreachable.
//
// This mirrors a CONTRACT, not an implementation: the wire shape (five
// sections — policy bundle, capacity profiles + grants, pool/host inventory,
// execution-cell matrix, posterior summary — canonicalized and Ed25519-signed
// with the content hash as the signed payload) is described in
// ADR-2026-08-12-placement-composition-law-and-single-fallback-rule.md §D6
// ("the evaluable snapshot is the durability unit ... versioned together")
// and in the layered-execution-model's boundary ruling: the OSS layer
// defines and ships a working *evaluator* of the snapshot; compiling one
// requires a multi-tenant policy/capacity control plane and stays downstream.
// Nothing in this package imports, calls, or hardcodes a path belonging to
// that control plane — every network fact this package touches (the fetch
// endpoint, the trusted signing key(s) or a JWKS URL, any bearer credential)
// arrives via Config, supplied by whatever composes the daemon as a library.
//
// Fail-static posture (05-sota-research.md §A5 — Envoy xDS TTL persistence,
// Consul `Age`/`stale-if-error`): a Client always answers Current() from the
// last verified snapshot, in memory and (across a daemon restart) on disk.
// A fetch, a bad signature, or a content-hash mismatch never touches the
// cached value — see Refresh's doc comment. Only two things ever change
// what Current() returns: a strictly newer snapshot that verifies, or the
// clock crossing the configured DegradedAfter/RefuseAfter bounds on the
// snapshot already held. Nothing in this package ever fails OPEN: an
// unconfigured Client (Config.Endpoint == "") reports Configured() == false
// and every claim-gate caller must treat that identically to "no opinion",
// never "permitted" — see daemon.FailStaticClaimGateProvider.
package rulesetsnapshot
