// Package geminicli implements an agent.Provider that shells out to the
// Google Gemini CLI (`gemini`).
//
// This is the CLI-wrap provider for running the official Google Gemini CLI
// in-box as a donmai execution backend. It is DISTINCT from the existing
// provider/gemini package which is an API-direct Go implementation. Both
// coexist under different ProviderName identifiers:
//
//   - "gemini"     — API-direct Go provider (net/http to generativelanguage.googleapis.com)
//   - "gemini-cli" — this package: subprocess wrap of the `gemini` CLI
//
// # Implementation strategy
//
// Per the FEASIBILITY.md plan (runs/2026-06-03-gemini-cli-sandbox-runner/FEASIBILITY.md):
//
//   - Shells out to the `gemini` binary in
//     `--output-format stream-json --yolo --skip-trust` mode.
//     Each JSONL line on stdout is parsed and mapped to an agent.Event.
//   - GEMINI_API_KEY is the only auth channel: the user (or daemon
//     credential socket) must supply it. No OAuth token reuse; no
//     auto-install. The provider returns agent.ErrProviderUnavailable
//     when the binary is not on PATH — CLI installation is the user's
//     responsibility.
//   - AUTH MODE = local/host-session ONLY. Do NOT select this provider
//     for cloud (e2b) sandboxes — the `gemini` binary (and its Node
//     runtime) must be present on the host. Future: user-supplied e2b
//     image with gemini baked in.
//   - Writes a per-session `.gemini/settings.json` into the worktree cwd
//     with mcpServers configured (type:"http", trust:true) when
//     Spec.MCPServers is non-empty. Cleaned up by Stop().
//   - Maps the CLI's JSONL stream-json event family (init, message,
//     tool_use, tool_result, error, result) to donmai's agent.Event
//     model. See jsonl.go for the full mapping table.
//   - Process-group management (SIGTERM → SIGKILL) mirrors process_unix.go
//     from provider/claude.
//
// # Capability matrix
//
//   - SupportsMessageInjection=false: the `gemini` CLI's --resume flag
//     takes session indexes, not UUIDs, making between-turn injection
//     impractical. Each task is a single headless run.
//   - SupportsSessionResume=false: same constraint.
//   - AcceptsMcpServerSpec=true: MCP HTTP servers are wired via
//     .gemini/settings.json written at Spawn time.
//   - SupportsToolPlugins=true: the CLI has first-class MCP client support.
//   - SupportsReasoningEffort=false: the CLI does not expose a
//     --thinking-budget or effort flag in headless mode (v0.44.x).
//
// # Stream-json event contract (gemini CLI v0.44.x)
//
// Captured from source at
// node_modules/@google/gemini-cli/bundle/chunk-GPVT36PL.js (JsonStreamEventType):
//
//	{"type":"init",        "timestamp":..., "session_id":..., "model":...}
//	{"type":"message",     "timestamp":..., "role":"user"|"assistant", "content":..., "delta"?:true}
//	{"type":"tool_use",    "timestamp":..., "tool_name":..., "tool_id":..., "parameters":{...}}
//	{"type":"tool_result", "timestamp":..., "tool_id":..., "status":"success"|"error", "output":..., "error"?:{...}}
//	{"type":"error",       "timestamp":..., "severity":"warning"|"error", "message":...}
//	{"type":"result",      "timestamp":..., "status":"success", "stats":{...}}
//
// # Auth
//
// GEMINI_API_KEY in spec.Env (or inherited from os.Environ() via standalone/
// host-export mode) is the only headless auth path. The provider does NOT
// inject or discover OAuth credentials. ToS-safe: official documented path.
package geminicli
