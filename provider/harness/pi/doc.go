// Package pi implements the agent.Provider contract against a
// `pi --mode rpc` JSONL-over-stdio subprocess.
//
// Architecture (one child per session — NOT the codex "one app-server, N
// threads" model):
//
//	pi.Provider
//	  └── Spawn(spec) ─▶ child process: `pi --mode rpc` (cmd.Dir = spec.Cwd)
//	        ├── JSONL commands  in  (prompt / steer / follow_up / abort / …)
//	        └── JSONL events    out (agent_start / message_* / tool_* / agent_end / …)
//
// pi's own per-process session model makes multiplexing a non-feature (the
// experimental `pi-orchestrator` is deliberately not used); one child per
// session buys worktree/credential isolation for free. This mirrors the
// codex/ subprocess-RPC shape (rpc.go ≈ jsonrpc.go, handle.go ≈ handle.go,
// policy.go generalizes approval.go) but over pi's simpler LF-delimited
// JSONL framing rather than JSON-RPC 2.0.
//
// # Wire-shape provenance (READ THIS)
//
// The command/event shapes in this package are VERIFIED against the real
// pinned binary (@earendil-works/pi-coding-agent@0.80.10) and its bundled,
// authoritative protocol docs (docs/rpc.md, docs/extensions.md), plus live
// probes of a running `pi --mode rpc` process. This corrects the earlier cut,
// which was transcribed from the design doc's description of the protocol and
// did not match reality (wrong command envelope key, wrong event field shapes,
// and an extension API — pi.ui.request / pi.defineTool — that does not exist).
// The donmai-smokes step20 lane (harness/pi_install.go, pinned binary in CI)
// remains the authority that validates the end-to-end model-turn path; until
// it accrues green history the pi matrix cells stay Stability:"experimental",
// Smoked:false and the runner registry does NOT enable pi for non-stub
// dispatch (DEC-2/DEC-3). The pinned version lives in probe.go (PinnedVersion)
// and the generated matrix binaryPins section.
//
// Wire summary (real): commands are JSON objects keyed by "type"
// (prompt/message, steer/message, follow_up/message, abort, set_model with
// provider+modelId, set_thinking_level/level, get_state, get_entries/since,
// extension_ui_response with a top-level value). Events are keyed by "type"
// (agent_start [no id], message_update with assistantMessageEvent deltas,
// message_end, tool_execution_start/update/end with toolCallId, turn_end with
// message.usage, agent_end [not terminal — retry may follow], agent_settled
// [the terminal], extension_error). The session id comes from the get_state
// response, not agent_start.
//
// # The trust boundary (§5 of the design — load-bearing)
//
// pi runs tools with the full permissions of the spawning user and ships NO
// permission system, no sandbox, no MCP. Any trust boundary here must be
// built and owned entirely by donmai. This package builds it as an
// in-process policy boundary with three layers, all fail-closed, against pi's
// REAL extension API (pi.on("tool_call") blocking + ctx.ui round-trips):
//
//  1. Tool constraint via the tool_call hook (extensions/donmai-policy.ts):
//     the embedded, pinned donmai policy extension (compiled INTO the donmai
//     binary via an embed directive, never fetched; loaded with
//     `pi --mode rpc -e <path> --no-extensions` so it loads regardless of
//     project trust and nothing else can shadow it) registers a `tool_call`
//     handler that fires before EVERY built-in tool executes. For each guarded
//     tool it serializes the intended call and raises a ctx.ui.input round-trip
//     over stdio (which pi surfaces as extension_ui_request /
//     extension_ui_response), blocks until the Go side answers allow/deny, and
//     on deny returns { block: true, reason } so pi blocks the tool and the
//     model sees WHY (mirroring codex ApprovalDecision.Reason).
//
//  2. Go-side adjudication + handshake (policy.go, extension.go, handle.go):
//     policy.go is the codex approval.go engine generalized — built-in
//     safety-deny regexes first, then path containment (writes/edits outside
//     Spec.Cwd denied unless an AllowPattern covers them; reads outside cwd
//     denied by default for autonomous sessions), then Spec allow/deny patterns
//     in the Claude grammar, then DefaultDecision. At session_start the
//     extension sends a handshake carrying a per-session secret TOKEN (read
//     from the DONMAI_PI_HANDSHAKE env var the harness set on the child) AND
//     the sha256 of its own on-disk source (import.meta.url). The provider
//     verifies the token (liveness/identity: proves this is the exact child it
//     spawned) AND the SHA (integrity: proves the exact policy bytes loaded),
//     both constant-time, and replies. NO handshake within the timeout ⇒ no
//     prompt is ever sent, spawn fails closed ("policy extension failed to
//     load"). A token or SHA mismatch ⇒ session killed. This closes both the
//     "pi loaded a stale/different extension" hole and the "session ran with no
//     policy at all" hole.
//
//  3. Integrity monitors (handle.go, fail-closed at runtime): an
//     extension_error referencing the donmai extension aborts the session
//     (ErrorEvent{Code:"policy_extension_failed"}) rather than continuing
//     unguarded; a built-in tool_execution_END WITHOUT a completed adjudication
//     round-trip for its call id is a policy bypass ⇒ session aborted (the real
//     pi lifecycle emits tool_execution_start before the tool_call hook, so the
//     bypass check point is the END, not the start); the child env is
//     allowlist-composed so a fleet box's personal ~/.pi credentials and
//     blocklisted host secrets are never visible to fleet sessions.
//
// What this deliberately does NOT claim: OS-level sandboxing. The policy
// extension is an in-process boundary — a hostile MODEL OUTPUT is contained
// (it can only call overridden tools), but a hostile TOOL EXECUTION still
// runs as the user. OS/sandbox-family enforcement stays the sandbox provider
// family's job (E2B/container cells), unchanged. Do not mistake this
// extension for a sandbox.
//
// # D8 fixture family
//
// ADR-2026-08-06 D8 requires a named positive/negative fixture family per
// harness. pi's row names: "RPC policy-handshake boundary, config-home
// isolation, tool registration, no-MCP truth, replay/resume" (plus D8's
// cross-cutting item 7, cleanup idempotence, which every family owes). Tool
// registration is out of scope until a registerTool follow-up wires real
// ToolPluginDelivery (see the Caps.SupportsToolPlugins comment in
// manifest.go); the rest are proved here:
//
//   - RPC policy-handshake boundary (positive + tampered/forged negatives):
//     handle_test.go — TestSpawn_HandshakeVerified,
//     TestSpawn_TamperedExtensionFailsClosed, TestSpawn_ForgedTokenFailsClosed,
//     TestSpawn_NoHandshakeFailsClosed.
//   - config-home isolation: env_security_test.go —
//     TestConfigHomeIsolation_AllHomeVarsRedirected (all four PI_*/XDG_*
//     candidate home vars, not PI_HOME alone).
//   - no-MCP truth (a required mcp_server channel is denied by name, and the
//     mcp-tool-names name filter is fatal — not merely recorded — because pi
//     has no mount boundary to narrow): agent/tool_adaptation_test.go —
//     TestToolLifecycleMCPServerRequiredDeniesPiByName,
//     TestToolLifecycleMCPToolNamesFatalWherePiHasNoMountBoundary.
//   - replay/resume: pi_test.go —
//     TestResume_DrivesGetEntriesCursorNotAFreshPrompt.
//   - cleanup idempotence: handle_test.go —
//     TestStop_IdempotentAfterChannelClose.
//
// Each is generated from the exact manifest profile (the delivery kinds and
// channels above are read off ToolLifecycleProfile / HarnessCaps, not
// re-typed), and every negative in this list was watched RED before the
// behavior it pins (or, where the behavior predates this labeling, RED
// against a deliberately reintroduced regression — see each PR's proof).
//
// # Package layout
//
//   - doc.go            — this charter + the §5 trust-boundary statement
//   - manifest.go       — HarnessManifest (§3) + Capabilities projection
//   - pi.go             — Provider: New (probe + version pin), Spawn, Resume, Shutdown
//   - probe.go          — version-pin bounds + construction-time enforcement
//   - rpc.go            — JSONL client: LF-framed command writer / event reader
//   - handle.go         — per-session Handle: event pump, extension round-trips, Inject, Stop
//   - event_mapping.go  — pi event union → agent.Event (§4)
//   - policy.go         — trust-boundary adjudicator (generalized codex approval.go)
//   - extension.go      — materialize + token/SHA-verify the embedded policy extension (§5.2)
//   - extensions/donmai-policy.ts — the embedded extension (tool_call hook + handshake)
//   - spec_translation.go — agent.Spec → argv/env/config
//   - interactive.go    — bare-`pi` PTY spawn mode via the shared ptycli driver (see below)
//
// # Interactive PTY spawn mode (the endpoint-driven cell)
//
// Spawn routes Spec.Interactive != nil to spawnInteractive (interactive.go),
// which execs the bare `pi` TUI under the shared ptycli driver — the same PTY
// spawn-mode shape claude/codex/shell use. It is a per-Spawn MODE, not a
// Transport change: the manifest keeps Transport subprocess-jsonrpc for the
// headless loop and additionally declares SupportsInteractivePTY.
//
// The endpoint binding is consumed FROM BIRTH: Spawn runs prepare()
// (PrepareHarness + applyEndpoint) BEFORE the interactive/headless split, so the
// provider pin argv (`--provider donmai --model <id>`) and the DONMAI_PI_* pin
// env are minted from the same projected spec both modes see — a gateway-backed
// pi interactive session reaches the resolved endpoint exactly as headless does.
// This is the preventive form of the retrofit claude's interactive spawn needed
// (its spawnInteractive forked off before applyEndpoint ran).
//
// The SAME embedded extension loads (never a second file). Its provider
// registration from env is unconditional; the handshake + Go adjudication
// round-trip below are RPC-mode-only — the extension skips them when
// DONMAI_PI_HANDSHAKE is absent, which the interactive spawn deliberately never
// sets. In PTY mode the human at the attached terminal plus pi's own native
// approval UI is the tool authority, and the pi/interactive tool-lifecycle
// profile declares that injected-boundary GAP rather than inheriting the
// headless profile's evidence (ADR-2026-08-06 D6). The A5 seam contract is
// unchanged: one embedded file, one handshake, one adjudication channel.
package pi
