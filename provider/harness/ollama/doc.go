// Package ollama implements the agent.Provider interface against a
// locally running Ollama HTTP endpoint (default http://localhost:11434).
//
// Ollama is a local-first LLM runtime that exposes a small native HTTP
// API; this provider talks to that API directly instead of shelling out
// to a CLI:
//
//   - GET  /api/tags  — used at provider construction as a probe (no
//     credentials required); failures surface as
//     agent.ErrProviderUnavailable so the runner can short-circuit.
//   - POST /api/chat  — NDJSON streaming chat endpoint. Each line of the
//     response body is one JSON object that the provider maps onto an
//     agent.Event variant.
//
// # Capability profile
//
// Ollama is the simplest realistic agent runtime: text-only, single-shot
// per HTTP request. The provider declares a deliberately conservative
// capability matrix that matches what the underlying API actually
// supports:
//
//   - SupportsMessageInjection: false. Each Spawn corresponds to one
//     POST /api/chat request; mid-session injection would require an
//     out-of-band mechanism the API does not provide. (Re-spawning a
//     fresh session with conversation history is the upstream
//     equivalent — left to the runner / template layer.)
//   - SupportsSessionResume:    false. Ollama has no server-side
//     conversation state; there is no "resume" concept to honor.
//   - SupportsToolPlugins:      false. Tool-use via Ollama is
//     model-dependent and routed through OpenAI-compatible endpoints
//     (`/v1/chat/completions` with `tools=`); the v0.1 provider does not
//     advertise tool plugins so the runner does not attempt to wire
//     af_linear_* / af_code_* MCP servers through it.
//   - NeedsBaseInstructions:    false. Spec.SystemPromptAppend is
//     concatenated into a system message inline.
//   - NeedsPermissionConfig:    false. There is no tool-permission
//     surface to gate.
//   - SupportsCodeIntelligenceEnforcement: false. No canUseTool
//     callback equivalent.
//   - EmitsSubagentEvents:      false. No sub-agent semantics.
//   - SupportsReasoningEffort:  false. Ollama does not expose a
//     reasoning-effort knob; the runner drops the value.
//   - ToolPermissionFormat:     "claude" (the default; harmless because
//     SupportsToolPlugins is false).
//   - HumanLabel:               "Ollama".
//
// # Cancellation
//
// Spawn returns a Handle whose Stop method cancels the in-flight HTTP
// request via context cancellation. Ctx cancellation on the spawn ctx
// is mirrored into the Handle's lifecycle (the request goroutine exits
// and the events channel closes).
//
// # No tools, no MCP
//
// Spec.AllowedTools, Spec.DisallowedTools, Spec.MCPServers are silently
// ignored — see the capability flags above. Per the rensei-architecture
// 002 base contract: providers that do not advertise a capability MUST
// NOT pretend to honor it; the runner is expected to gate before
// invoking Spawn.
//
// # Construction probe
//
// New issues a short-deadline GET /api/tags against the configured
// endpoint at construction. A non-2xx or transport error wraps
// agent.ErrProviderUnavailable with a remediation hint ("ollama serve")
// so operators see at-a-glance why the provider is missing from the
// daemon's registry.
package ollama
