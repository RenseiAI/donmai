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
// fails fast before doing any worktree work.
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
// HarnessCaps.SupportsInteractivePTY declares a SECOND, independent spawn
// mode (interactive.go: SpawnInteractive): Spec.Interactive != nil routes
// Spawn to the bare `codex` TUI under a PTY (provider/harness/ptycli)
// instead of `codex app-server`/`codex exec`. It never touches the
// JSON-RPC client or thread/turn machinery below — see interactive.go for
// the full contract.
//
// # Approval bridge
//
// The codex app-server fires JSON-RPC server-requests (id + method) for
// every tool execution when `approvalPolicy: "on-request"` is set on the
// thread. The bridge in approval.go consumes Spec.PermissionConfig and
// replies with an accept / decline / acceptForSession decision. v0.5.0
// ships the bridge so autonomous fleets do not have to default-allow
// every command (per F.1.1 open-question #5: ship the bridge).
//
// # MCP servers
//
// Each Provider owns an isolated CODEX_HOME and never targets the operator's
// persistent config or auth files. Before thread/start or thread/resume, the
// exact Spec.MCPServers set is written to the owned config.toml via
// config/batchWrite and proved active via config/read at the session cwd.
// Equal concurrent sets share a lease; incompatible live sets are denied; the
// final release clears and re-verifies the set. Application or cleanup failure
// is a hard typed denial, never a session-without-tools soft fallback.
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
