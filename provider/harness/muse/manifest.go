package muse

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the muse provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// museExecutionSecurity is this adapter's execution-security rendering
// declaration: index 0 only. The approval bridge carries the deny
// entries, but it only sees requests that leave the host's own sandbox
// (escalations); under a disabled sandbox nothing escalates, so the
// entries have nothing to act on (DenyChannelNeedsSandbox). The
// workspace-write posture is unproven, so fileWrite "workarea" is not
// declared. Nothing confines fileRead, network, credentials or
// isolation.
var museExecutionSecurity = agent.ExecutionSecurityRendering{DenyChannelNeedsSandbox: true}

// Manifest returns the harness-family declaration for the Muse loop
// driver. The adapter runs one `muse serve` MSP host per session as a
// subprocess and drives it over JSON-RPC, with inline session MCP
// servers and a Go-side approval bridge. No wire-level SSE framing —
// the serve host is the transport surface, so StreamingTransport is
// "none".
func (*Provider) Manifest() agent.HarnessManifest {
	headlessEvents := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError}
	// The interactive PTY spawn mode observes only the coarse Init/terminal
	// boundary the shared ptycli driver emits (byte-accurate terminal stream is
	// the product; program decision D4) — never the structured per-tool events
	// the headless serve lane carries.
	ptyEvents := []agent.EventKind{agent.EventInit, agent.EventResult}
	return agent.HarnessManifest{
		Name:              agent.HarnessMuse,
		HumanLabel:        "Muse",
		Family:            agent.FamilyHarness,
		ContractABI:       "harness/v2",
		ExecutionSecurity: museExecutionSecurity,
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true, // turn/steer into the running turn
			SupportsSessionResume:    true,
			SupportsToolPlugins:      true,
			AcceptsMcpServerSpec:     true,
			AcceptsAllowedToolsList:  false,
			EmitsSubagentEvents:      true, // subagent items map to SubagentEvent
			SetsParentToolUseID:      false,
			SupportsReasoningEffort:  true,
			SupportsOneShot:          true,
			NativeJSONMode:           false, // turn/start has no structured-output primitive
			ToolPermissionFormat:     "muse",
			StreamingTransport:       "none", // serve-host JSON-RPC, not SSE/ndjson
			// SupportsInteractivePTY declares an ADDITIONAL spawn mode (the bare
			// `muse` TUI under the shared ptycli driver — see interactive.go), NOT
			// a change to the declared Transport: Transport (subprocess-jsonrpc,
			// below) still names how the DEFAULT headless loop runs. The two are
			// orthogonal, exactly as claude/manifest.go and codex/manifest.go
			// document; PTY is selected per Spawn call by Spec.Interactive != nil,
			// never by a Transport value.
			SupportsInteractivePTY:           true,
			MultiRepositoryWorkareaProtocols: []string{"session-root-v1"},
			RepositoryAuthorityEnforcement:   "isolated-read-only-v1",
			// `muse serve` is a JSON-RPC control surface over a live
			// session, so a message is a method call (turn/steer),
			// not a keystroke.
			NoticeDelivery: agent.NoticeDeliveryMCPRPC,
			// session/tokenUsage (cumulative counters + cost) and
			// session/contextUsage (window pressure) are the
			// harness-native usage signals.
			ContextUsageSource: agent.ContextUsageSourceMuseTokenUsage,
			Drives:             []agent.WireProtocol{agent.ProtoMuseMSP},
			DrivesHosts:        []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect},
			Transport:          agent.TransportSubprocessRPC,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{
			{
				ID: "muse/headless/serve-leading-text-v1", Mode: agent.PromptModeAutonomous,
				SystemDelivery: agent.PromptDeliveryMuseLeadingText, BaseAppendDelivery: agent.PromptDeliveryMuseLeadingText,
				BaseReplaceDelivery: agent.PromptDeliveryMuseLeadingText, ContextDelivery: agent.PromptDeliveryMuseTurnInput,
				UserDelivery: agent.PromptDeliveryMuseTurnInput, AmendmentDelivery: agent.PromptDeliveryMuseTurnSteer,
			},
			{
				// Interactive PTY: the bare TUI exposes no system-prompt
				// flag and no turn/start frame, so its ONLY prompt channel
				// is the positional seed argument — system append,
				// initial context, user prompt and amendments all ride it
				// (interactiveArgs composes them in that order), and the
				// admission receipt records every channel under this same
				// delivery. Base replace has no channel and denies.
				ID: "muse/interactive/pty-v1", Mode: agent.PromptModeHumanControlled,
				SystemDelivery: agent.PromptDeliveryMusePTYSeed, BaseAppendDelivery: agent.PromptDeliveryMusePTYSeed,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryMusePTYSeed,
				UserDelivery: agent.PromptDeliveryMusePTYSeed, AmendmentDelivery: agent.PromptDeliveryMusePTYSeed,
			},
		},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{
				ID: "muse/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryMuseSessionMCP,
				NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryMuseApprovalBridge,
				MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: headlessEvents,
				ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: headlessEvents,
				CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
			},
			{
				// Interactive PTY tells the D6 truth: the coarse terminal
				// boundary the shared ptycli driver emits, replayed as a
				// terminal cast — NOT the headless lane's structured
				// per-tool events. Every enforceable channel is
				// Unsupported: the TUI has no inline-MCP surface outside
				// session/start, no approval-bridge RPC without the serve
				// host, and no flat allow-list grammar — so a spec
				// stamping any of them denies closed at admission rather
				// than spawning unenforced. Interactive evidence never
				// inherits headless.
				ID: "muse/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryUnsupported,
				NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
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
