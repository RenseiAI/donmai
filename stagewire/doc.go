// Package stagewire holds the optional fields the 1.0 dispatch wire adds to
// a session work item: the stage the work item is in and its role, the
// dispatched card and its completion contract, the resolved outcome
// transitions, the mechanical retry policy and the stage context block.
//
// Every field is optional and additive. A control plane keeps sending the
// legacy workType and workTypeRole until every host runs a runner that reads
// these fields, so a runner prefers them when present and falls back to the
// legacy fields when not; an older runner ignores them.
//
// Decoding is defensive. Values outside the closed vocabularies (roles,
// dispatch reasons) are dropped, every string is bounded, and the retry
// policy is clamped, so a malformed control-plane emit can never widen what
// the runner does. Structurally wrong JSON (a number where an object belongs)
// is still a decode error, as for every other work-item field.
//
// The package is a leaf: it imports only the standard library, so the
// daemon, the prompt renderer, the runner and the public client types can
// all share one definition. Canonical design: donmai-architecture
// ADR-2026-11-02-stages-roles-and-stage-cards.md, "The 1.0 dispatch wire".
package stagewire
