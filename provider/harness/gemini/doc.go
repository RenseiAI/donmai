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
// MCP: Gemini has no native MCP loader, so the provider bridges in-box
// (mcp.go over runtime/mcp): Spawn dials every Spec.MCPServers entry
// (stdio subprocess or Streamable HTTP — the platform's per-session MCP
// endpoint), lists the server's tools, declares them to the model as
// mcp__<server>__<tool> functionDeclarations (schemas sanitized to the
// Gemini OpenAPI subset), and the session-local executor routes the
// resulting mcp__* functionCalls to the live server. Connection failures
// degrade per-server to structured tool errors — never a failed Spawn —
// and Capabilities.AcceptsMcpServerSpec is true.
//
// File layout (parallels provider/codex):
//
//   - gemini.go            — Provider impl: New / Spawn / Resume / Shutdown
//   - probe.go             — env-var probe at construction
//   - spec_translation.go  — agent.Spec → Gemini request scaffold
//   - tools.go             — functionDeclarations, thinkingConfig, pricing
//   - mcp.go               — in-box MCP bridge (dial, discover, route)
//   - event_mapping.go     — generateContent response → agent.Event
//   - handle.go            — Handle impl + multi-turn driver goroutine
//
// A native Gemini runner is tracked as follow-up work.
package gemini
