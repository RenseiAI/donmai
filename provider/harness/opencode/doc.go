// Package opencode implements the agent.Provider for the OpenCode local
// agent (https://opencode.ai/, github.com/sst/opencode).
//
// This package is NOT a registration-only stub. It ships a working
// one-shot CLI runner and is structured around two execution lanes
// behind a single Provider:
//
//   - Lane A — one-shot CLI (shipped): New() probes for the `opencode`
//     binary on $PATH; when found, Spawn execs `opencode run --format
//     json` with the prompt delivered on stdin. Endpoint, tool-policy, and
//     MCP inputs are delivered through the same provider-owned temporary
//     opencode.json boundary as Lane B. The NDJSON event stream
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
//     written 0600 into a unique provider-owned 0700 temp boundary outside the
//     worktree, then injected via $OPENCODE_CONFIG (config_boundary.go). The
//     boundary is removed after the child stops on spawn failure, Stop,
//     terminal, crash, or Provider.Shutdown; cleanup failure is returned and
//     replaces a successful terminal with a bounded ErrorEvent. The config
//     contains a single custom OpenAI-compatible provider whose baseURL is the
//     resolved endpoint, and an enabled_providers + experimental.policies
//     lockout so the worker can
//     never fall back to an unresolved provider, the Spec's tool policy
//     projected onto opencode's allow/ask/deny maps, and the Spec's MCP
//     whitelist. The v2 /api/ REST surface and the SSE /api/event feed are
//     spoken behind the serverClient interface (client.go) so an opencode API
//     reshape is absorbed by one implementation swap; SSE frames map to
//     agent.Events in events_sse.go; runtime "ask" verdicts are adjudicated by
//     the provider's own permission pump (permission.go), never a human. Lane
//     B backs SupportsMessageInjection (Inject → prompt POST), SupportsSessionResume
//     (Resume → reattach), AcceptsAllowedToolsList, and independently
//     AcceptsMcpServerSpec through the §5.3 project config path.
//
// Lane selection follows lifecycle needs (useServerLane, opencode.go):
// binary-backed Spawn uses Lane A by default, including endpoint, permission,
// and MCP inputs delivered by its owned config. Lane B is selected instead —
// even with a binary present — by attach mode (no binary), an explicit
// Options.PreferServer, or a positive need signal Lane A's exited-with-the-
// turn process structurally cannot satisfy: an MCP-bearing spec
// (Spec.MCPServers), a non-allow permission default
// (Spec.PermissionConfig.DefaultDecision "deny"/"prompt" — Lane A has no
// channel to answer a live "ask"), or a steer-capable requirement
// (Spec.RequiresLiveNotice, which admission already accepted against this
// manifest's declared NoticeDeliveryHTTPSession — only Lane B backs it).
// Resume always uses Lane B because it needs the server API.
//
// The opencode.preferServer producer contract: Options.PreferServer is
// wired at construction time by whatever composes this package (donmai's own
// `agent run` entry point threads it from a fetched session's
// ResolvedProfile.ProviderConfig["opencode.preferServer"], a bool-typed,
// provider-namespaced key on the generic map[string]any wire shape shared by
// every provider's ctor hints — see afcli/agent_run.go's opencodeCtorHintKey/
// opencodeCtorHints/opencodeCtorOptions for the concrete donmai-side
// consumer). This package places NO requirement on who sets that key or how
// a session's resolved profile is produced; a missing key, a non-bool value,
// or a nil profile all resolve to the zero value (Lane-A default, unchanged
// behavior) rather than an error — an optional routing hint must never fail
// preflight. Callers that need Lane B unconditionally (tests, an operator
// override) should still prefer the positive need signals above over forcing
// PreferServer, so the routing decision stays legible from the Spec alone.
//
// DRIFT (code wins over 07 §4/§4.2/§5): the pinned binary (opencode 1.17.18)
// already ships the v2-style API — every route under /api/, session-scoped,
// abort via /interrupt, an admission-model prompt lane, and a
// session.next.* SSE event vocabulary. clientV1 + events_sse.go target THAT
// surface (verified live against the binary's own OpenAPI at GET /doc), not
// the older flat 1.x shapes the design named. Also: the v2 SessionRunner in
// 1.17.18 does not resolve custom OpenAI-compatible providers into its model
// catalog (v2 provider policy is "Proposed/unimplemented" upstream), so a
// full real-model turn through the injected baseURL is not exercisable through
// the pinned server API. Binary-backed one-shot work therefore uses Lane A,
// where the same owned config is live and the pinned CLI resolves custom
// OpenAI-compatible providers. Lane B remains available for explicit server
// probes and attach mode; its config-injection, serve-lifecycle, event-stream,
// and permission wiring are verified in-repo against real wire shapes.
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
