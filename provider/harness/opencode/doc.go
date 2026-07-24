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
//   - Lane B — serve/HTTP (shipped): `opencode serve` + REST/SSE. One serve
//     child per donmai session on an ephemeral loopback port the provider
//     owns (server.go), or an attached external server via $OPENCODE_ENDPOINT
//     when no binary is present. A per-session opencode.json is rendered and
//     injected via $OPENCODE_CONFIG (config.go): a single custom
//     OpenAI-compatible provider whose baseURL is the resolved endpoint, an
//     enabled_providers + experimental.policies lockout so the worker can
//     never fall back to an unresolved provider, the Spec's tool policy
//     projected onto opencode's allow/ask/deny maps, and the Spec's MCP
//     whitelist. The v2 /api/ REST surface and the SSE /api/event feed are
//     spoken behind the serverClient interface (client.go) so an opencode API
//     reshape is absorbed by one implementation swap; SSE frames map to
//     agent.Events in events_sse.go; runtime "ask" verdicts are adjudicated by
//     the provider's own permission pump (permission.go), never a human. Lane
//     B backs SupportsMessageInjection (Inject → prompt POST), SupportsSessionResume
//     (Resume → reattach), and AcceptsAllowedToolsList. The §5.3 MCP config
//     injection is implemented, but AcceptsMcpServerSpec stays advertised false
//     pending the cross-provider AcceptsMcpServerSpec→SupportsToolPlugins
//     invariant (see manifest.go).
//
// Lane selection is Spec-driven (07 §2): a fire-and-forget one-shot takes Lane
// A; a session needing injection/resume/permission mediation/MCP wiring — or an
// explicit Options.PreferServer, or attach-mode — takes Lane B.
//
// DRIFT (code wins over 07 §4/§4.2/§5): the pinned binary (opencode 1.17.18)
// already ships the v2-style API — every route under /api/, session-scoped,
// abort via /interrupt, an admission-model prompt lane, and a
// session.next.* SSE event vocabulary. clientV1 + events_sse.go target THAT
// surface (verified live against the binary's own OpenAPI at GET /doc), not
// the older flat 1.x shapes the design named. Also: the v2 SessionRunner in
// 1.17.18 does not resolve custom OpenAI-compatible providers into its model
// catalog (v2 provider policy is "Proposed/unimplemented" upstream), so a
// full real-model turn through the injected baseURL is not exercisable against
// the pinned binary yet — the config-injection, serve-lifecycle, event-stream,
// and permission wiring are verified in-repo against httptest stubs replaying
// the real wire shapes.
//
// Auth model: opencode's local server is unauthenticated by default; the
// optional OPENCODE_API_KEY env var is forwarded as a Bearer token on the
// HTTP-server probe for future hosted variants.
//
// Version pin (07 §8): opencode ships ~2 releases/day, independently of
// donmai. New() in CLI mode probes "<binary> --version" and enforces
// MinVersion/PinnedVersion/VerifiedAgainst (probe.go) — a version below
// MinVersion fails construction with agent.ErrProviderUnavailable; a
// version above VerifiedAgainst (or one the probe couldn't determine)
// proceeds but emits a SystemEvent{Subtype:"unverified_harness_version"}
// once per session (DEC-2: label, don't block). These same three
// constants back the generated matrix's binaryPins section
// (matrix/cells.go) so the pin can never drift between enforcement and
// documentation.
//
// Layout follows the provider/harness/claude convention (probe.go /
// spec translation / event mapping / handle) as the Lane-B client lands.
package opencode
