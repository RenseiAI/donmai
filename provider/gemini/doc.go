// Package gemini is the agent.Provider implementation for Google's
// native Gemini API
// (https://ai.google.dev/api/generate-content#streamgeneratecontent).
//
// This is distinct from a "gemini" backend reached via A2A — that
// route lives in a separate provider package and goes through the A2A
// JSON-RPC bridge. Here we speak HTTP directly to
// generativelanguage.googleapis.com using the official server-sent
// events (SSE) streaming endpoint.
//
// Auth: the constructor probes for GEMINI_API_KEY (preferred) or
// GOOGLE_API_KEY in the environment. Missing key → wrapped
// agent.ErrProviderUnavailable; the daemon's `donmai agent run` registry
// build logs WARN and skips registration, identical to the existing
// claude / codex probes.
//
// Capabilities: agentic parity for the native tool surface. The provider
// drives a multi-turn generateContent conversation with native
// function-calling, reasoning-effort via thinkingConfig (thinkingLevel
// on the 3.x family, thinkingBudget on the 2.5 family), and post-
// completion steering by appending a turn and re-driving the loop.
// Per-Spawn credentials resolve from Spec.Env[GEMINI_API_KEY] then
// Spec.Env[GOOGLE_API_KEY] then the construction-time fallback,
// supporting per-session BYOK + rotation. TotalCostUsd is computed from a
// per-model USD pricing table.
//
// Conversation model: Gemini's REST endpoint is stateless AND does not
// execute tools — it returns functionCall parts and expects the caller
// to run the tool and POST a matching functionResponse. The Handle
// therefore owns the contents history across turns AND runs the model's
// functionCalls itself via a session-local executor (provider/gemini/
// executor.go): native Bash/Read/Edit/Write run in the session's working
// directory, the result surfaces as a ToolResult event, and the
// functionResponse is folded back into the loop as a USER-role turn (the
// public generateContent API rejects the legacy "function" role).
// Post-completion steering arrives via Handle.Inject (a user turn).
//
// MCP: there is NO native MCP and NO in-box MCP client. Spec.MCPServers
// entries are surfaced to the model as catch-all functionDeclarations
// for forward-compatibility, but no code routes an mcp__* functionCall
// to a live server — the executor returns a structured "not executable"
// error and Capabilities.AcceptsMcpServerSpec is false. A real MCP
// bridge is future work; until it lands, MCP tools are not honored
// end-to-end.
//
// File layout (parallels provider/codex):
//
//   - gemini.go            — Provider impl: New / Spawn / Resume / Shutdown
//   - probe.go             — env-var probe at construction
//   - spec_translation.go  — agent.Spec → Gemini request scaffold
//   - tools.go             — functionDeclarations, thinkingConfig, pricing
//   - event_mapping.go     — generateContent response → agent.Event
//   - handle.go            — Handle impl + multi-turn driver goroutine
//
// Tracked in REN-1500 (Gemini native runner) on the Rensei Linear team.
package gemini
