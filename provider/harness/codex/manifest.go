package codex

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the codex provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the Codex loop-driver.
// Additive alongside Capabilities(); the agent-loop bools project
// Capabilities() (parity-tested). codex runs the `codex` app-server as a
// subprocess and drives it over JSON-RPC, speaking openai-responses (and
// openai-chat per its Drives). No wire-level SSE/ndjson framing — the
// app-server is the transport surface, so StreamingTransport is "none".
//
// SupportsInteractivePTY (W4) declares an ADDITIONAL spawn mode: bare
// `codex` under a PTY (interactive.go: SpawnInteractive), entirely
// independent of the app-server subprocess above — NOT a change to the
// declared Transport. Transport names how the DEFAULT headless loop runs
// (subprocess-jsonrpc, unchanged); TransportPTY is used only where PTY is a
// harness's ONLY transport (see provider/harness/shell/manifest.go) — codex
// keeps subprocess-jsonrpc here and gets PTY strictly as a per-Spawn-call
// mode, selected by Spec.Interactive != nil, not by a Transport value. See
// agent/harness.go's HarnessCaps.SupportsInteractivePTY doc comment for the
// general rule this and claude/manifest.go both follow.
func (*Provider) Manifest() agent.HarnessManifest {
	headlessEvents := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError}
	ptyEvents := []agent.EventKind{agent.EventInit, agent.EventResult}
	return agent.HarnessManifest{
		Name:        agent.HarnessCodex,
		HumanLabel:  "Codex",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: false,
			SupportsSessionResume:    true,
			SupportsToolPlugins:      true,
			AcceptsMcpServerSpec:     true,
			AcceptsAllowedToolsList:  false,
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  true,
			SupportsOneShot:          true,
			NativeJSONMode:           true, // turn/start outputSchema (app-server v2; see turnStartParams)
			ToolPermissionFormat:     "codex",
			StreamingTransport:       "none", // app-server JSON-RPC, not SSE/ndjson over the wire surface
			SupportsInteractivePTY:   true,
			// `codex app-server` (and its `codex mcp-server` sibling) is a
			// JSON-RPC control surface over a live session, so a message is a
			// method call rather than a keystroke. Handle.Inject deliberately
			// returns ErrUnsupported today (handle.go) — the CHANNEL is
			// declared here; driving it is a separate lane.
			NoticeDelivery: agent.NoticeDeliveryMCPRPC,
			Drives:         []agent.WireProtocol{agent.ProtoOpenAIResponses, agent.ProtoOpenAIChat},
			DrivesHosts:    []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect, agent.HostAzure},
			Transport:      agent.TransportSubprocessRPC,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{
			{
				ID: "codex/headless/app-server-v2", Mode: agent.PromptModeAutonomous,
				SystemDelivery: agent.PromptDeliveryCodexDeveloperInstructions, BaseAppendDelivery: agent.PromptDeliveryCodexDeveloperInstructions,
				BaseReplaceDelivery: agent.PromptDeliveryCodexBaseInstructions, ContextDelivery: agent.PromptDeliveryCodexTurnInput,
				UserDelivery: agent.PromptDeliveryCodexTurnInput, AmendmentDelivery: agent.PromptDeliveryCodexTurnInput,
			},
			{
				ID: "codex/interactive/pty-developer-instructions-v1", Mode: agent.PromptModeHumanControlled,
				SystemDelivery: agent.PromptDeliveryCodexCLIInstructions, BaseAppendDelivery: agent.PromptDeliveryCodexCLIInstructions,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryCodexPTYSeed,
				UserDelivery: agent.PromptDeliveryCodexPTYSeed, AmendmentDelivery: agent.PromptDeliveryCodexPTYSeed,
			},
		},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{
				ID: "codex/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryCodexAppServerMCP,
				NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryCodexApprovalBridge,
				MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: headlessEvents,
				ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: headlessEvents,
				CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
			},
			{
				ID: "codex/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryCodexCLIMCPConfig,
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
