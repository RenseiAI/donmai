// Package muse implements the agent.Provider contract against the
// `muse serve` MSP session host (JSON-RPC 2.0 over stdio JSONL).
//
// Architecture (one server child per Handle — unlike the codex provider,
// which multiplexes N threads on one shared app-server):
//
//	muse.Provider (no shared child; a binary path + spawn options)
//	  └── Handle A → child: `muse serve` (one process, one session)
//	  └── Handle B → child: `muse serve` (one process, one session)
//
// The wire shapes in types.go are generated from the host's own
// `muse schema` export (schema fingerprint recorded in schema_fingerprint.go),
// not hand-copied. MSP details (sessionMcp capability, UUIDv7 command ids,
// revision-aware items, view/gap recovery) follow the verified integration
// notes in the package README.
//
// # Package layout
//
//   - doc.go              — this file: architecture + lifecycle overview
//   - types.go            — wire types generated from `muse schema`
//   - schema_fingerprint.go — the schema export identity the wire types track
//   - jsonrpc.go          — JSON-RPC 2.0 stdio client (request/notification dispatch)
//   - muse.go             — Provider lifecycle (New, Spawn, Resume, Shutdown)
//   - handle.go           — Per-session Handle (session + turns + events channel)
//   - approval.go         — Approval bridge (Spec.PermissionConfig → decide/decline)
//   - spec_translation.go — agent.Spec → JSON-RPC param mapping
//   - event_mapping.go    — JSON-RPC notification → agent.Event mapping
//   - manifest.go         — HarnessProvider manifest declaration
//   - process_unix.go / process_windows.go / process_other.go — process-group setup
package muse
