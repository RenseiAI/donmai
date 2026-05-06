// Package amp is a registration-only stub for Sourcegraph's Amp coding
// agent (https://ampcode.com/).
//
// Amp ships as a Node-based CLI and a hosted product but does not yet
// expose a stable, publicly-documented HTTP API the daemon can drive
// programmatically the way it drives Anthropic / Google / OpenAI APIs.
// Until that API is published (or until we ship a CLI shell-out runner
// equivalent to provider/claude), the daemon registers the provider so
// operators selecting "amp" via a resolved profile see a clear,
// agent.ErrSpawnFailed-coded error instead of agent.ErrNoProvider.
//
// Capabilities advertised here are intentionally conservative — every
// optional flag is false. Spawn always fails with a deterministic
// "runner not yet implemented" error wrapping agent.ErrSpawnFailed; the
// runner records this as a normal session failure (failure mode =
// "spawn"), and the operator-visible Linear comment names the gap.
//
// When real Amp support lands, drop the placeholder Spawn and follow
// the provider/claude or provider/gemini layout: probe.go,
// spec_translation.go, event_mapping.go, handle.go.
//
// Tracked in REN-1499 (Amp runner) on the Rensei Linear team.
package amp
