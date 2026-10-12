// Package muse implements the agent.Provider contract against the
// `muse serve` MSP JSON-RPC subprocess (JSON-RPC 2.0 over stdio JSONL).
//
// Architecture (one Provider, one Handle, one subprocess):
//
//	muse.Provider
//	  └── child process: `muse serve` (per-session, JSON-RPC over stdio)
//	        └── session_1 (Handle) — one agent.Spec, many turns
//
// One Provider instance owns exactly one `muse serve` subprocess, and one
// Handle owns the Provider for its whole session lifetime. This differs
// from codex (one shared app-server, N threads): the MSP host loads its
// session set per process, so multiplexing sessions across one host would
// couple their lifecycles. Sessions still multiplex TURNS over the one
// connection: each turn is a `turn/start` call carrying a client-minted
// UUIDv7 commandId whose echo is the turnId.
//
// Wire reference: the MSP bundle exported by `muse schema
// generate-json-schema` (schema version 1). The Go types in types.go are
// generated from that bundle — see generate.go for the generator and its
// provenance stamp — never hand-copied.
//
// # Spawn shape
//
//   - argv: `muse serve [--trust-workspace] [--disable-sandbox]`; the
//     read-only lane adds `--disable-shell --disable-write`.
//   - cwd: the realpath of Spec.Cwd (the host rejects non-canonical
//     roots such as macOS /tmp).
//   - env: the parent environment with MUSE_NO_AUTO_UPDATE=1 set and
//     META_API_KEY stripped unless the Spec sets it.
//   - handshake: `initialize{clientInfo:{name (^[a-z0-9_]+$), version},
//     capabilities:{requestedCapabilities:["sessionMcp"]}}`, then the
//     `initialized` notification. The host drops anything sent before
//     the handshake completes. Schema version 1 is required.
//   - session: `session/start{sessionId (client UUIDv7), commandId
//     (UUIDv7, required), workspaceRoot, modelId, providerId:"meta",
//     approvalMode, config}` with inline MCP servers; resume via
//     `session/resume{sessionId, excludeItems:true, config}`.
//   - turns: `turn/start{sessionId, commandId, input, displayText,
//     workspaceRoots, ifBusy:"queue", reasoningEffort}`,
//     `turn/interrupt{turnId}`, `turn/steer{expectedTurnId, input}`.
//
// # Event mapping
//
// Events route by params.sessionId. Item events are revision-aware
// (replace-iff-higher; deltas never bump revision); a `view/gap` is
// fatal: the Handle re-anchors via `view/subscribe` and emits an
// ErrorEvent when recovery fails. `turn/completed` maps to the
// terminal ResultEvent (completed, cancelled→interrupted, failed).
//
// # Failure modes
//
//   - JSON-RPC request: 3-attempt exponential backoff (1s/2s/4s) on
//     transient errors. Permanent errors return immediately.
//   - Host crash: detected via process exit; the live Handle receives
//     an ErrorEvent (Code: "serve_host_crashed") and its events
//     channel closes.
//   - ctx.Done() on a Handle: send `turn/interrupt`, close channel.
//
// # Package layout
//
//   - muse.go          — Provider lifecycle (New, Spawn, Resume, Shutdown)
//   - jsonrpc.go       — JSON-RPC 2.0 stdio client (request/notification dispatch)
//   - types.go         — MSP wire types (generated from `muse schema`; see generate.go)
//   - generate.go      — schema-codegen provenance + generator entry point
//   - handle.go        — Per-session Handle (session + events channel)
//   - approval.go      — Approval bridge (Spec.PermissionConfig → approval/decide)
//   - spec_translation.go — agent.Spec → JSON-RPC param mapping
//   - event_mapping.go — JSON-RPC notification → agent.Event mapping
package muse
