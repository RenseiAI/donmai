// Package codex implements the agent.Provider contract against the
// `codex app-server` JSON-RPC subprocess.
//
// Architecture (one Provider, N Handles, one shared subprocess):
//
//	codex.Provider
//	  └── child process: `codex app-server` (long-lived, JSON-RPC over stdio)
//	        ├── thread_1 (Handle A) — independent agent.Spec
//	        ├── thread_2 (Handle B)
//	        └── thread_N (Handle …)
//
// One Provider instance owns exactly one app-server subprocess. Sessions
// are multiplexed as JSON-RPC `thread/start` calls; each Handle subscribes
// to notifications matching its threadId. This mirrors the legacy TS
// `CodexAppServerProvider`/`AppServerProcessManager` pair from
// ../donmai-libraries/packages/core/src/providers/codex-app-server-provider.ts.
//
// # Why no exec fallback
//
// Per F.0.1 §6 (item 2) and F.1.1 §3.2 the v0.5.0 Go port ships
// app-server only. The legacy `codex exec` band-aid covered stale codex
// binaries; Wave 6 requires a known-good codex on PATH. If the binary is
// missing, codex.New returns agent.ErrProviderUnavailable so the runner
// fails fast before doing any worktree work. Process start and the
// initialize handshake are deferred to the first headless Spawn/Resume so
// the child environment can be composed from that session's Spec.Env; a
// handshake failure there wraps agent.ErrProviderUnavailable in
// agent.ErrSpawnFailed. That session layer is applied exactly once, so the
// Provider pins it and refuses a later Spawn/Resume whose Spec.Env
// materially differs instead of serving it the first session's routing
// values; an identical layer (a session resuming its own thread) is always
// accepted, and the interactive PTY spawn mode is outside the invariant.
//
// # Capability matrix (locked in F.1.1 §3.2)
//
//   - SupportsMessageInjection : false (Codex CLI lacks mid-session
//     user-message injection per legacy TS comment)
//   - SupportsSessionResume    : true (thread/resume)
//   - SupportsToolPlugins      : true (isolated config/batchWrite mcp_servers)
//   - NeedsBaseInstructions    : true (thread/start.baseInstructions)
//   - NeedsPermissionConfig    : true (approval bridge consumes it)
//   - SupportsCodeIntelligenceEnforcement : false (no canUseTool callback)
//   - EmitsSubagentEvents      : false (Codex has no Anthropic Task tool)
//   - SupportsReasoningEffort  : true (turn/start.reasoningEffort)
//   - ToolPermissionFormat     : "codex"
//   - SupportsTurnInputContext : true. Codex re-includes session-level
//     developer/base instructions in the model prompt on every turn,
//     while turn/start.input is sent once and then cached in
//     conversation history. The runner therefore routes large, volatile
//     context (recalled agent memory) through Spec.InitialContext so it
//     rides the first turn's input instead of inflating the re-sent
//     instruction prefix — avoiding O(turns × prefix) input-token
//     amplification on long sessions.
//
// # Interactive spawn mode (W4)
//
// HarnessCaps.SupportsInteractivePTY declares a SECOND spawn mode
// (interactive.go: SpawnInteractive): Spec.Interactive != nil routes Spawn to
// the codex TUI under a PTY (provider/harness/ptycli). Named sessions use a
// per-session app-server to apply thread/name/set before attaching the TUI by
// name; they never reuse the Provider's shared headless client/process. See
// interactive.go for the full contract.
//
// # Approval bridge
//
// The codex app-server fires JSON-RPC server-requests (id + method) for command
// and file-change approvals when `approvalPolicy: "on-request"` is set on the
// thread. The bridge in approval.go consumes Spec.PermissionConfig and replies
// with an accept / decline / acceptForSession decision. MCP tool approval stays
// internal to Codex, so each requested server instead carries the scoped
// default_tools_approval_mode seed in the isolated config. v0.5.0 ships the
// bridge so autonomous fleets do not have to default-allow every command (per
// F.1.1 open-question #5: ship the bridge).
//
// # MCP servers
//
// Each Provider owns an isolated CODEX_HOME and never targets the operator's
// persistent config. An explicitly selected host-session route may hard-link
// only the host auth.json into that home at headless Spawn/Resume, after
// harness preparation and without admitting ambient MCP/project
// configuration. Before thread/start or thread/resume, the exact
// Spec.MCPServers set is written to the owned config.toml via
// config/batchWrite, checked via config/read at the session cwd, then held
// until mcpServerStatus/list reports every requested server initialized and
// every retired Provider-managed server absent. Codex-owned ambient inventory
// entries are outside that comparison. Equal concurrent sets share a lease;
// incompatible live sets are denied; the final release clears and re-verifies
// both config and the absence of Provider-managed servers. Application,
// initialization, or cleanup failure is a hard typed denial, never a
// session-without-tools soft fallback.
//
// # Failure modes (F.1.1 §5)
//
//   - JSON-RPC request: 3-attempt exponential backoff (1s/2s/4s) on
//     transient errors. Permanent errors return immediately.
//   - App-server crash: detected via process exit; every live Handle
//     receives an ErrorEvent (Code: "app_server_crashed") and its events
//     channel closes.
//   - ctx.Done() on a Handle: send `thread/unsubscribe` + `turn/interrupt`,
//     drain remaining notifications, close channel.
//
// # Package layout
//
//   - codex.go            — Provider lifecycle (New, Spawn, Resume, Shutdown)
//   - jsonrpc.go          — JSON-RPC 2.0 stdio client (request/notification dispatch)
//   - handle.go           — Per-session Handle (thread + events channel)
//   - approval.go         — Approval bridge (Spec.PermissionConfig → decisions)
//   - spec_translation.go — agent.Spec → JSON-RPC param mapping
//   - event_mapping.go    — JSON-RPC notification → agent.Event mapping
//
// See README.md for the operator-facing overview.
package codex
