// Package opencode is a registration-only stub for the OpenCode local
// agent (https://opencode.ai/, github.com/sst/opencode).
//
// OpenCode is local-first: operators run `opencode serve` and the
// daemon points at the resulting localhost HTTP server (default port
// 7700). The server exposes REST + WebSocket endpoints today, but the
// shape is still pre-1.0 and the maintainers reserve the right to break
// it between minor releases. Wiring the daemon to it without a stable
// contract risks silent regressions whenever opencode ships, so v0.1
// of this provider stops at probe-and-register: we confirm the server
// is reachable, advertise the capability matrix conservatively, and
// fail Spawn with a clear "runner not yet implemented" error.
//
// Operators selecting opencode via a resolved profile see a deterministic
// agent.ErrSpawnFailed instead of agent.ErrNoProvider — same UX as the
// amp provider — and the daemon stays usable for the providers that
// ARE wired (claude / codex / stub / gemini).
//
// When the OpenCode REST contract stabilizes (or when we ship a CLI
// shell-out runner equivalent to provider/claude), drop the placeholder
// Spawn and follow the provider/claude or provider/gemini layout
// (probe.go, spec_translation.go, event_mapping.go, handle.go).
//
// Auth model: OpenCode's local server is unauthenticated by default;
// the optional OPENCODE_API_KEY env var is forwarded as a Bearer token
// for future hosted variants.
//
// Tracked in REN-1501 (OpenCode runner) on the Rensei Linear team.
package opencode
