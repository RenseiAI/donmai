// Package gateway is the translating-gateway ModelEndpoint host: a loopback
// component, owned by the donmai daemon, that lets any harness consume any
// upstream through the wire protocol it natively drives.
//
// It fills the extension point ADR-2026-06-06 §D2 reserves ("a translating-
// gateway host that owns the shim + its own serving host + cost attribution")
// and implements ADR-2026-07-24-translating-gateway-model-endpoint-host (the
// accepted contract). The design is
// runs/2026-07-21-open-harness-strategy/08-design-gateway-host.md.
//
// # Shape
//
//   - speaks INWARD (to harnesses on loopback): existing wire surfaces
//     (M1: OpenAI Chat Completions; M2 adds Anthropic Messages, later Gemini);
//   - speaks OUTWARD (to upstreams): direct provider / OpenAI-compatible
//     aggregator + self-hosted endpoints (M1); enterprise-federated and
//     sanctioned subscription backends land in later milestones;
//   - translates through one canonical intermediate representation (ir/),
//     including reasoning/thinking normalization — never pairwise ad-hoc;
//   - selects a credential with rotation/cooldown/failover (pool/), single
//     credential in OSS;
//   - meters every exchange into a cost record (costfeed/) keyed by endpoint
//     company + host (primary) and harness (sibling).
//
// # M1 cut (this package as shipped)
//
// gateway.go (lifecycle + loopback listener + per-session bearer binding),
// surface/openai_chat.go, upstream/openai_compat.go, translate/openai_chat.go
// (a near-identity codec through the IR seam), pool/ (single-key source + the
// full rotation state machine), token/, costfeed/ (local JSONL ledger + a
// poster-hook interface). Cross-protocol translation, the Anthropic surface,
// and the auth-policy engine are M2; Class E and sanctioned Class S are later.
//
// # Boundary (OSS-public, brand-neutral)
//
// OSS ships a WORKING gateway, never only the type (001 §contract). What is
// platform-only rides the same interfaces and never appears here: multi-
// credential pools (a CredentialSource over an org vault), org policy
// administration, hosted cost ingest, and any routing brain that CHOOSES a
// cell — the gateway EXECUTES a chosen cell, it never chooses
// (ADR-2026-06-07). Structural exclusions are absences of code, not policy
// defaults: no vendor-OAuth-client reuse, no identity cloaking, no fabricated
// client fingerprints, no anti-detection options, no pooling of consumer
// subscription accounts.
package gateway
