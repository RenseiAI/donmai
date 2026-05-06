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
// agent.ErrProviderUnavailable; the daemon's `af agent run` registry
// build logs WARN and skips registration, identical to the existing
// claude / codex probes.
//
// Capabilities: this v0.1 ships text-only spawn — no tool use, no
// session resume, no reasoning-effort knob (Gemini does expose a
// `thinkingBudget` parameter on 2.5 / 2.6 thinking models, but we
// don't honour it yet). When tool use lands, flip SupportsToolPlugins
// after wiring the function-calling round-trip.
//
// File layout (parallels provider/codex):
//
//   - gemini.go            — Provider impl: New / Spawn / Resume / Shutdown
//   - probe.go             — env-var probe at construction
//   - spec_translation.go  — agent.Spec → Gemini request body
//   - event_mapping.go     — Gemini SSE chunk → agent.Event
//   - handle.go            — Handle impl + body-reader goroutine + Stop
//
// Tracked in REN-1500 (Gemini native runner) on the Rensei Linear team.
package gemini
