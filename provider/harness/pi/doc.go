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
// registration THROUGH THE CLOSED TOOL-LIFECYCLE PLAN (ToolPluginDelivery /
// Caps.SupportsToolPlugins — see manifest.go) now lands: Spec.
// AdditionalExtensions projects onto agent/tool_adaptation.go's generic
// tool_plugin channel (legacyToolRequirements), the headless profile's
// ToolPluginDelivery answers ToolDeliveryPiAdditionalExtension truthfully
// (manifest.go's ID bumped tool-lifecycle-v1 → v2 per ADR-2026-08-12 D6/
// D1.3a — the adapter version moves, the family ABI and binary pin do not),
// and Caps.SupportsToolPlugins now agrees. The ADDITIONAL-EXTENSION DELIVERY
// SEAM itself (ADR-2026-08-12) shipped separately and gets its own row below
// — that row proves real registerTool delivery against the pinned binary;
// this row proves the SAME AdditionalExtensions list is admitted and
// receipted through the generic cross-harness-compiled plan/receipt path
// before it ever reaches pi's own materializer, and — on a harness/mode whose
// profile does not declare the channel — denies closed when the batch carries
// any required delivery, or drops an all-advisory batch with a receipt and
// strips it from the adapted Spec (ExtensionDelivery.Required decides;
// interactive PTY stays Unsupported — no fixture proves tool registration
// through that lane yet, so it declares the gap per D6 rather than inheriting
// headless evidence). Generic-path fixtures:
// agent/tool_adaptation_test.go —
// TestToolLifecyclePiAdditionalExtensionsRouteThroughGenericToolPluginChannel
// (positive: headless admits + receipts; negative: interactive denies by
// name; negative: a malformed batch denies before the requirement loop runs;
// negative: another harness with AdditionalExtensions populated denies —
// there is still no cross-harness "supports extensions" boolean, D5.5).
// pi-package integration fixture: extension_delivery_test.go —
// TestSpawn_AdditionalExtensions_ProducesToolPluginReceiptEntry (a real
// Spawn call's ToolLifecycleReceipt names the tool_plugin channel, admitted,
// with the ToolDeliveryPiAdditionalExtension delivery) and
// TestSpawn_Interactive_AdditionalExtensionsDeniesBeforePTYWork (the
// interactive-mode denial fires inside prepare(), before spawnInteractive
// ever runs). The rest of the original family is proved here:
//
//   - RPC policy-handshake boundary (positive + tampered/forged negatives):
//     handle_test.go — TestSpawn_HandshakeVerified,
//     TestSpawn_TamperedExtensionFailsClosed, TestSpawn_ForgedTokenFailsClosed,
//     TestSpawn_NoHandshakeFailsClosed.
//   - config-home isolation: env_security_test.go —
//     TestConfigHomeIsolation_DocumentedVarsRedirected (the two DOCUMENTED
//     PI_CODING_AGENT_DIR/PI_CODING_AGENT_SESSION_DIR vars — ADR-2026-08-12
//     D4.1 retired the four undocumented PI_HOME/PI_CONFIG_DIR/PI_STATE_DIR/
//     XDG_CONFIG_HOME candidates the prior cut guessed at).
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
// # Additional-extension delivery seam (ADR-2026-08-12 D1-D4)
//
// A second family, scoped to the seam that ADR adds. Fixtures split scripted
// (extension_delivery_test.go — Go-side mechanics, no real binary needed)
// from real-binary (extension_delivery_real_binary_test.go — proves the
// mechanics against the real pinned pi process; see that file's own doc
// comment for CI-scope caveats identical to real_binary_test.go's):
//
//   - materialization + TOCTOU-closing digest verification (D1/D2(b)), both
//     delivery kinds, fail-closed on mismatch/missing-path/malformed input:
//     extension_delivery_test.go —
//     TestMaterializeAdditionalExtensions_PathDelivery,
//     TestMaterializeAdditionalExtensions_InlineDelivery,
//     TestMaterializeAdditionalExtensions_DigestMismatchFailsClosed,
//     TestMaterializeAdditionalExtensions_MissingPathFailsClosed,
//     TestMaterializeAdditionalExtensions_MalformedDeliveryFailsClosedBeforeIO.
//   - boundary-first, undisplaceable ordering (D1), both spawn modes:
//     extension_delivery_test.go —
//     TestRPCArgs_BoundaryFirstThenAdditionalExtensionsInOrder,
//     TestInteractiveArgs_BoundaryFirstThenAdditionalExtensionsInOrder.
//   - required-delivery denial reaches Spawn itself, before any process
//     starts (D1.2): extension_delivery_test.go —
//     TestSpawn_RequiredExtensionDeliveryDenialFailsBeforeProcessSpawn,
//     TestSpawn_ValidAdditionalExtension_HandshakeStillVerifiesNormally.
//   - real registerTool delivery + the headless-UI guarantee (D3: an
//     unrecognized extension's own UI round-trip resolves promptly as a
//     refusal, never hangs) against the real binary, both delivery kinds:
//     extension_delivery_real_binary_test.go —
//     TestRealBinary_AdditionalExtension_ToolRegistersAndHeadlessUIRefusesPromptly.
//   - the trust rule (D2), both halves, against the real binary:
//     extension_delivery_real_binary_test.go —
//     TestRealBinary_WorkspaceDiscovery_StaysDisabled (workspace-discovered
//     extensions never load, autonomous session),
//     TestRealBinary_TrustBypass_OperatorInjectedExtensionLoadsWithoutApprove
//     (operator-injected `-e` loads even when trust is explicitly declined).
//   - state isolation (D4.3 offline defaults, both lanes) and the
//     PI_CODING_AGENT_DIR/PI_CODING_AGENT_SESSION_DIR collision this cut
//     found and fixed (see sessionLayout.agentHome): env_security_test.go —
//     TestOfflinePostureEnv_DefaultsOnUnlessExplicit; interactive_test.go —
//     TestInteractiveChildEnv_OmitsHandshakeTokenVsHeadless; pinned
//     end-to-end by TestRealBinary_Resume_StructuralReplay in
//     real_binary_test.go, which reproduces the collision if agentHome and
//     the session-storage root are ever collapsed back into one path.
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
//   - extension.go      — materialize + token/SHA-verify the embedded policy extension (§5.2);
//     also materializeAdditionalExtensions for the ADR-2026-08-12 seam
//   - extensions/donmai-policy.ts — the embedded extension (tool_call hook + handshake)
//   - spec_translation.go — agent.Spec → argv/env/config
//   - interactive.go    — bare-`pi` PTY spawn mode via the shared ptycli driver (see below)
//   - testdata/conformance-fixture.ts, testdata/workspace-discovery-canary.ts —
//     real-binary conformance fixtures for the additional-extension delivery
//     seam (extension_delivery_real_binary_test.go); never embedded into the
//     binary, test-only
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
//
// # Interactive allowed/disallowed-tools channel (local, no RPC)
//
// One exception to "the interactive profile only ever declares gaps" above:
// NativeToolPolicyDelivery is REAL on the interactive profile
// (agent.ToolDeliveryPiInteractiveLocalToolPolicy, manifest.go's
// pi/interactive/tool-lifecycle-v2 — bumped from v1 per the seam ADR's
// adapter-version rule, ADR-2026-08-12 D1.3a/D6). The SAME embedded extension
// this lane loads also matches a stamped Spec.AllowedTools/DisallowedTools
// list LOCALLY against every guarded tool_call, entirely in-process, with no
// RPC and no handshake (extensions/donmai-policy.ts's !rpcMode branch).
// interactive.go's interactiveChildEnv carries the stamped list onto
// DONMAI_PI_ALLOWED_TOOLS/DONMAI_PI_DISALLOWED_TOOLS — a mechanism deliberately
// distinct from ToolDeliveryPiInjectedBoundary, which still names only the
// RPC-backed handshake+adjudication boundary headless uses and which the
// interactive profile still does NOT claim. Scoped narrow on purpose:
// PermissionConfigDelivery stays Unsupported, since the richer regex/
// containment/default-decision engine (policy.go) still needs the Go round
// trip this lane does not run.
//
// Also consumed here: Spec.ToolSurfaceRequired (agent/tool_adaptation.go), the
// platform's optional-delivery wire flag. nil/true is unchanged default
// behavior (an undeliverable allowed/disallowed-tools entry denies the whole
// spawn); explicit false marks the entry optional, so an undeliverable
// profile (any harness, any mode — not pi-specific) drops the entry with a
// recorded, non-fatal receipt instead of denying. This does not change what
// pi's interactive profile now delivers; it changes what happens on a
// DIFFERENT profile that still declares Unsupported when the caller marked
// the surface as a nice-to-have rather than load-bearing.
//
// Fixtures: agent/tool_adaptation_test.go
// TestToolLifecyclePiInteractiveAllowedDisallowedToolsAdmitLocally (generic-
// plan admission, positive) and
// TestToolLifecycleOptionalToolSurfaceDropsWithReceiptInsteadOfDenying (the
// wire flag, both values, on a harness whose profile still declares
// Unsupported). interactive_tool_policy_test.go carries the pi-package half:
// interactiveToolPolicyEnv unit tests, a fake-pi Spawn fixture proving the
// stamped list reaches the PTY child's env, and a SCRIPTED conformance
// fixture (testdata/interactive-local-tool-policy-harness.mjs) that reads
// the REAL production extensions/donmai-policy.ts, strips its (deliberately
// narrow) TypeScript syntax down to plain JavaScript itself
// (stripErasableTypeScript, verified against node's own --check before use —
// a shape it cannot handle fails loudly, never a silent skip) so the fixture
// never assumes a Node version with native `.ts` loading, then evaluates it
// under node — no `pi` binary needed — and proves the stamped list reaches
// the extension's actual tool_call gate (block on a disallowed tool, block
// on an allow-gated unlisted tool, pass an allowed tool, register no handler
// at all when nothing is stamped).
//
// # Scale hardening
//
// Four properties beyond correctness matter once a fleet spawns many pi
// sessions concurrently, per ADR-2026-08-12 (the pi extension-delivery seam
// ADR — D4's state-isolation defaults, and 013-orchestrator-and-governor.md's
// pre-spawn sequence, which permits a content-addressed shared cache for
// materialized deliveries as long as every spawn still digest-verifies its
// own session-local copy).
//
//  1. Cold-start budget. materializeExtension and the INLINE branch of
//     materializeAdditionalExtensions go through writeViaCache
//     (extension.go): a content-addressed cache OUTSIDE any single session's
//     workarea, keyed by sha256 digest, that hardlinks a session's
//     materialized copy from a shared blob instead of re-encoding+writing
//     byte-identical bytes on every spawn. This is safe by construction — the
//     mandatory post-write digest verification (D2(b)'s TOCTOU rule) is
//     completely unchanged and reads back the session-local file regardless
//     of whether the cache was hit; a corrupted/poisoned/missing blob simply
//     falls back to a fresh write. Measured against testdata/fakepi (a
//     zero-compile stub — see scale_load_test.go, tag pi_scale_load): N=100
//     concurrent real-subprocess spawns, p50 in the 47-70ms range, p95 in the
//     63-92ms range, max under 100ms across repeated local runs (machine-load
//     dependent, always well inside spawnLatencyBudget). Measured against the
//     REAL pinned pi binary when available on PATH
//     (TestScaleLoad_RealBinary_OptionalSample, skips cleanly otherwise): a
//     3-sample cold-start including jiti's TS compile of the boundary
//     extension and node process startup, p50 in the 550ms-1.1s range across
//     repeated local runs. The documented budget
//     is therefore two-part and NOT conflated: donmai's own per-spawn
//     overhead (materialization + argv/env composition + process fork/exec +
//     the handshake round trip) is bounded well under 100ms at N=100
//     concurrent — spawnLatencyBudget in scale_load_test.go pins this as a
//     regression guard; the real binary's jiti-compile + node-startup cost
//     (~500ms, single-digit-sample measurement, machine-dependent) is a
//     THIRD-PARTY cost this package does not control and cannot cache away
//     (jiti compiles the extension's TypeScript inside the child process,
//     after donmai's own materialization has already finished) — it is
//     documented here as an external cost, not folded into the budget this
//     package tests against.
//
//  2. Per-session state isolation at N-concurrent scale. sessionLayout gives
//     every session a unique root/agentHome/injected triple under its own
//     workarea (D4.1); composeChildEnv redirects the two DOCUMENTED pi
//     variables (PI_CODING_AGENT_DIR/PI_CODING_AGENT_SESSION_DIR) to it.
//     isolation_scale_test.go proves this is observably true, not merely
//     env-asserted (D4.2's standard): N=100 concurrently-prepared sessions
//     resolve pairwise-distinct paths AND pairwise-distinct env bindings
//     (TestStateIsolation_NConcurrentSessions_DistinctRootsAndAgentHomes),
//     concurrent real filesystem writes into each session's agentHome never
//     cross-contaminate, and the pi CredentialStore's auth.json lockfile path
//     — the documented shared-state bottleneck (D4.4) — is proven to have no
//     shared value across any two of the N sessions
//     (TestStateIsolation_AuthLockfileBottleneckIsBypassed): per-session
//     agentHome means no two donmai-spawned pi sessions can ever contend the
//     same lock, because there is no path left for them to share.
//
//  3. N-instance load validation. scale_load_test.go (tag pi_scale_load, not
//     part of the default `make test` gate — see that file's own doc comment
//     for why and how to run it) spins N concurrent REAL subprocess sessions
//     against testdata/fakepi and measures spawn-latency and inject/steer
//     round-trip-latency distributions; N=100 locally by default,
//     DONMAI_PI_LOAD_N overrides explicitly, CI=1/-short bounds to N=10. The
//     real `pi` binary is exercised opportunistically
//     (TestScaleLoad_RealBinary_OptionalSample) and skips cleanly when
//     unavailable — donmai's own hosted CI does not install node/pi, mirroring
//     real_binary_test.go's existing scope note.
//
//  4. Wake-poll fail-quiet invariant. pi has no polling loop of its own — it
//     is subprocess-push (event-driven over stdout), never poll-driven — so
//     the "wake-poll" pattern lives one layer up: runner/loop.go's
//     drainMemoryInjects (a background-poll wakeup delivering buffered
//     memory injects) and runner/steering.go's attemptSteering (a
//     post-terminal check) both call Handle.Inject at the post-terminal seam,
//     and runner.injectDirective's documented contract is soft-fail
//     (ErrUnsupported / provider-specific "not ready"/"in flight" sentinels
//     are swallowed; any other error is returned once and NOT retried in a
//     loop — the caller logs and defers the remainder to the next scheduled
//     wake, the same bounded-retry-then-signal shape
//     013-orchestrator-and-governor.md states for an unreachable session).
//     This package's obligation is mechanical: Inject must never hang.
//     TestInject_AfterStop_ReturnsPromptlyAndDoesNotHang (handle_test.go)
//     proves Inject called on an already-Stopped, non-fatally-terminated
//     session — the race a wake-poll caller can actually hit — returns a
//     plain, non-panicking error within a bounded window, so the generic
//     runner-level fail-quiet wrapper always has something well-behaved to
//     log-and-defer on.
package pi
