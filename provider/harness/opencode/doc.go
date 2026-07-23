// Package opencode implements the agent.Provider for the OpenCode local
// agent (https://opencode.ai/, github.com/sst/opencode).
//
// This package is NOT a registration-only stub. It ships a working
// one-shot CLI runner and is structured around two execution lanes
// behind a single Provider:
//
//   - Lane A — one-shot CLI (shipped): New() probes for the `opencode`
//     binary on $PATH; when found, Spawn execs `opencode run --format
//     json` with the prompt delivered on stdin. The NDJSON event stream
//     is translated to agent.Event values by mapOpenCodeLine
//     (step_start→InitEvent, text→AssistantTextEvent, tool_use→ToolUse/
//     ToolResult, step_finish→LlmCallEvent+ResultEvent). Teardown is
//     process-group SIGTERM → 5s grace → SIGKILL. This lane serves
//     SupportsOneShot dispatches and simple fleet legs; it has no
//     session resume, message injection, or live permission round-trip.
//
//   - Lane B — serve/HTTP (not yet wired): when the binary is absent,
//     New() falls back to probing an `opencode serve` HTTP server
//     (default 127.0.0.1:4096, override with $OPENCODE_ENDPOINT). Spawn
//     in this mode currently returns agent.ErrSpawnFailed with an
//     actionable message. The REST + SSE client, per-session config
//     injection, and permission mediation are follow-up work (design:
//     runs/2026-07-21-open-harness-strategy/07-design-opencode-spawn.md).
//
// Provider selection is by resolved profile; operators pointed at an
// unwired lane see a deterministic agent.ErrSpawnFailed (not
// agent.ErrNoProvider), keeping the daemon usable for the providers that
// ARE wired (claude / codex / stub / gemini) — same UX as the amp
// provider.
//
// Auth model: opencode's local server is unauthenticated by default; the
// optional OPENCODE_API_KEY env var is forwarded as a Bearer token on the
// HTTP-server probe for future hosted variants.
//
// Layout follows the provider/harness/claude convention (probe.go /
// spec translation / event mapping / handle) as the Lane-B client lands.
package opencode
