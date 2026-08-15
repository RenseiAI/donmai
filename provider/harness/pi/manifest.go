package pi

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the pi provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the pi loop-driver.
// Additive alongside Capabilities(); the agent-loop bools project
// Capabilities() (parity-tested in matrix/parity_test.go). pi runs
// `pi --mode rpc` as a subprocess and drives it over LF-delimited JSONL, so
// Transport is subprocess-jsonrpc and StreamingTransport is "ndjson".
//
// pi's pi-ai layer natively speaks anthropic-messages, openai-chat,
// openai-responses, and gemini-generate (research/pi.md §3), making pi the
// broadest Drive surface in the fleet. The DrivesHosts set is intentionally
// broader than the authored matrix cells (DEC-2: declared capability is not
// claimed capability) — only pi × anthropic × direct and pi × openai ×
// direct are authored as cells today (both experimental/untested); every
// other host is a later one-row add gated on smoke coverage.
//
// DRIFT vs design §3 (code reality wins): the design listed HostGateway in
// DrivesHosts, but agent.HostGateway does not exist yet (the gateway is W3 /
// ADR-A, not merged). It is omitted here and re-added when the gateway ships.
// A `pi × local × local` cell is also NOT authored: the CompanyLocal endpoint
// only speaks ProtoOllama (provider/endpoint/local), which pi does not drive
// — a local openai-compat pi cell needs either a local-openai host on the
// local endpoint or routing via google × local, deferred as a follow-up.
func (*Provider) Manifest() agent.HarnessManifest {
	events := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventToolProgress, agent.EventResult, agent.EventError}
	// The interactive PTY spawn mode observes only the coarse Init/terminal
	// boundary the shared ptycli driver emits (byte-accurate terminal stream is
	// the product; program decision D4) — never the structured per-tool events
	// the headless RPC lane carries.
	ptyEvents := []agent.EventKind{agent.EventInit, agent.EventResult}
	return agent.HarnessManifest{
		Name:        agent.HarnessPi,
		HumanLabel:  "pi",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true, // steer / follow_up
			SupportsSessionResume:    true, // session file + cursor replay (get_entries since=<id>)
			// SupportsToolPlugins is true: Spec.AdditionalExtensions
			// (ADR-2026-08-12 D1) now routes through the generic
			// tool-lifecycle plan (agent/tool_adaptation.go
			// legacyToolRequirements, ToolChannelToolPlugin), and the
			// headless ToolLifecycle profile below declares a real
			// ToolPluginDelivery for it — a populated delivery list is
			// admitted and receipted, never silently dropped, and the real
			// pi.registerTool call is proven against the pinned binary
			// (extension_delivery_real_binary_test.go). This flag used to
			// disagree with a truthful ToolPluginDelivery: Unsupported below;
			// both now agree. The interactive PTY profile still declares
			// ToolPluginDelivery: Unsupported (no fixture proves tool
			// registration through that lane yet — ADR-2026-08-06 D6:
			// interactive evidence never inherits headless), so a caller
			// that hands pi's PTY spawn mode a required AdditionalExtensions
			// delivery denies closed, and an all-advisory batch is dropped
			// with a receipt at the generic plan layer — never silently
			// loaded unevidenced either way.
			SupportsToolPlugins:     true,
			AcceptsMcpServerSpec:    false, // pi has no MCP by design; Spec.MCPServers is capability-gated-ignored
			AcceptsAllowedToolsList: true,  // enforced by OUR policy extension, not by pi
			EmitsSubagentEvents:     false,
			SupportsReasoningEffort: true,  // set_thinking_level off…max
			SupportsOneShot:         true,  // pi --mode json single-shot lane
			NativeJSONMode:          false, // no server-constrained structured output ⇒ spawn-collect
			ToolPermissionFormat:    "claude",
			StreamingTransport:      "ndjson",
			// SupportsInteractivePTY declares an ADDITIONAL spawn mode (the bare
			// `pi` TUI under the shared ptycli driver — see interactive.go), NOT
			// a change to the declared Transport: Transport (subprocess-jsonrpc,
			// below) still names how the DEFAULT headless loop runs. The two are
			// orthogonal, exactly as claude/manifest.go and codex/manifest.go
			// document; PTY is selected per Spawn call by Spec.Interactive != nil,
			// never by a Transport value.
			SupportsInteractivePTY: true,
			// `pi --mode rpc` carries an explicit steering verb on the same
			// JSONL channel that drives the session: Handle.Inject maps to
			// steer while a turn is in flight and follow_up while idle
			// (handle.go). Shipped and driven.
			NoticeDelivery: agent.NoticeDeliveryRPCSteer,
			Drives: []agent.WireProtocol{
				agent.ProtoAnthropicMessages,
				agent.ProtoOpenAIChat,
				agent.ProtoOpenAIResponses,
				agent.ProtoGeminiGenerate,
			},
			DrivesHosts: []agent.ServingHost{
				agent.HostDirect,
				agent.HostLocal,
				agent.HostBedrock,
				agent.HostVertex,
			},
			Transport: agent.TransportSubprocessRPC,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{
			{
				ID: "pi/headless/rpc-v1", Mode: agent.PromptModeAutonomous,
				SystemDelivery: agent.PromptDeliveryPiSystemAppend, BaseAppendDelivery: agent.PromptDeliveryPiSystemAppend,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryPiSystemAppend,
				UserDelivery: agent.PromptDeliveryPiRPCPrompt, AmendmentDelivery: agent.PromptDeliveryPiRPCPrompt,
			},
			{
				// Interactive PTY: system instructions still ride pi's
				// `--append-system-prompt` CLI flag (a global flag the bare TUI
				// honors exactly as the headless lane does), but the user prompt
				// and initial context are seeded as pi's positional prompt
				// argument — the coarse PTY-seed channel, not an RPC prompt frame
				// (there is no RPC channel in this spawn mode). Mirrors the
				// codex/interactive PromptDelivery shape.
				ID: "pi/interactive/pty-v1", Mode: agent.PromptModeHumanControlled,
				SystemDelivery: agent.PromptDeliveryPiSystemAppend, BaseAppendDelivery: agent.PromptDeliveryPiSystemAppend,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryPiPTYSeed,
				UserDelivery: agent.PromptDeliveryPiPTYSeed, AmendmentDelivery: agent.PromptDeliveryPiPTYSeed,
			},
		},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{
				// Bumped v1 → v2 (ADR-2026-08-12 D6 / D1.3a): loading a pack
				// through Spec.AdditionalExtensions moves the declared
				// surface this exact profile grants, which is precisely what
				// the adapter version names — the family ABI (ContractABI
				// above) and the binary pin do not move. A receipt pinned to
				// "pi/headless/tool-lifecycle-v1" now denies at spawn rather
				// than silently reusing a stale profile identity.
				ID: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous,
				// ToolPluginDelivery is real, not Unsupported: a populated
				// Spec.AdditionalExtensions is materialized, digest-verified,
				// and loaded via pi's `-e` extension API
				// (materializeAdditionalExtensions in extension.go),
				// registering real tools against the pinned binary
				// (extension_delivery_real_binary_test.go
				// TestRealBinary_AdditionalExtension_ToolRegistersAndHeadlessUIRefusesPromptly).
				// Distinct delivery value from NativeToolPolicyDelivery/
				// PermissionConfigDelivery below: those name the ONE
				// handshake-verified policy extension every session loads;
				// this names the caller-supplied extensions riding alongside
				// it.
				ToolPluginDelivery: agent.ToolDeliveryPiAdditionalExtension, MCPDelivery: agent.ToolDeliveryUnsupported,
				NativeToolPolicyDelivery: agent.ToolDeliveryPiInjectedBoundary, PermissionConfigDelivery: agent.ToolDeliveryPiInjectedBoundary,
				MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: events,
				ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: events,
				CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
			},
			{
				// Interactive PTY tells the D6 truth (ADR-2026-08-06): the coarse
				// terminal boundary the shared ptycli driver emits, replayed as a
				// terminal cast — NOT the headless lane's structured per-tool
				// events. It deliberately does NOT claim
				// pi_handshake_policy_extension: in PTY mode the Go adjudication
				// round-trip has no RPC channel, so the HUMAN at the attached
				// terminal plus pi's own native approval UI is the tool authority.
				// Declaring the injected-boundary GAP (Unsupported) rather than
				// inheriting the headless profile's evidence is D6's exact
				// requirement — interactive evidence never inherits headless.
				// ToolPluginDelivery stays Unsupported for the same reason:
				// materializeAdditionalExtensions runs in this lane too
				// (interactive.go), but no real-binary fixture proves tool
				// registration through it, so the generic plan layer answers
				// per the batch's declared posture instead of loading
				// unevidenced: a batch carrying any required delivery denies
				// closed, and an all-advisory batch is dropped with a
				// receipt and stripped from the adapted Spec
				// (dropDeniedAdvisoryExtensions) before this lane's
				// materializer ever sees it — the headless profile's flip
				// does not carry over.
				//
				// NativeToolPolicyDelivery is REAL, though, and for a different
				// reason than the injected-boundary claim above stays a gap. The
				// SAME embedded extension this lane loads (never a second file)
				// also matches a stamped AllowedTools/DisallowedTools list LOCALLY
				// against every guarded tool_call, entirely in-process, with no RPC
				// and no handshake (extensions/donmai-policy.ts's !rpcMode branch;
				// interactive.go's interactiveChildEnv carries the stamped list onto
				// DONMAI_PI_ALLOWED_TOOLS/DONMAI_PI_DISALLOWED_TOOLS). That is a
				// genuinely different mechanism from ToolDeliveryPiInjectedBoundary
				// (which still names only the RPC-backed handshake+adjudication
				// round trip headless uses), so it gets its own delivery value —
				// ToolDeliveryPiInteractiveLocalToolPolicy — instead of reusing or
				// inheriting the headless claim. Scoped deliberately narrow:
				// PermissionConfigDelivery stays Unsupported, because the richer
				// regex/containment/default-decision engine (policy.go) still needs
				// the Go round trip this lane does not run.
				//
				// Bumped v1 -> v2 per the seam ADR's adapter-version rule
				// (ADR-2026-08-12 D1.3a / D6: a profile whose declared surface moves
				// bumps the ADAPTER version; the family ABI and binary pin do not).
				// A receipt pinned to "pi/interactive/tool-lifecycle-v1" now denies
				// at spawn rather than silently reusing a stale profile identity.
				// Conformance: agent/tool_adaptation_test.go
				// TestToolLifecyclePiInteractiveAllowedDisallowedToolsAdmitLocally
				// (generic-plan admission) and this package's scripted
				// interactive-local-tool-policy fixtures (the extension's actual
				// enforcement, against the real production source, no pi binary
				// needed).
				ID: "pi/interactive/tool-lifecycle-v2", Mode: agent.PromptModeHumanControlled,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryUnsupported,
				NativeToolPolicyDelivery: agent.ToolDeliveryPiInteractiveLocalToolPolicy, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
				MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryCoarsePTYEvents, LifecycleFidelity: agent.EvidenceCoarse, LifecycleEvents: ptyEvents,
				ReplayDelivery: agent.ToolDeliveryTerminalCastReplay, ReplayFidelity: agent.EvidenceCoarse, ReplayEvents: ptyEvents,
				CleanupDelivery:    agent.ToolDeliveryHandleCleanup,
				FallbackDeliveries: []agent.ToolDeliveryKind{agent.ToolDeliveryCoarsePTYEvents, agent.ToolDeliveryTerminalCastReplay},
				EvidenceTier:       "unit_verified",
			},
		},
	}
}

// Capabilities returns the flat agent-loop capability projection. The matrix
// parity gate asserts these agree field-for-field with Manifest().Caps.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection: true,
		SupportsSessionResume:    true,
		// SupportsToolPlugins is true — see the matching comment on
		// Manifest().Caps; TestParity_ManifestAgreesWithCapabilities pins
		// the two together.
		SupportsToolPlugins: true,
		// pi has NO permission system of its own — the donmai policy
		// extension IS the permission config consumer, so the provider
		// requires a structured policy to adjudicate against.
		NeedsPermissionConfig:   true,
		EmitsSubagentEvents:     false,
		SupportsReasoningEffort: true,
		ToolPermissionFormat:    "claude",
		AcceptsAllowedToolsList: true,
		AcceptsMcpServerSpec:    false,
		HumanLabel:              "pi",
	}
}
