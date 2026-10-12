package muse

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the muse provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the Muse
// loop-driver. Additive alongside Capabilities(); the agent-loop bools
// project Capabilities() (parity-tested). muse runs `muse serve` as a
// per-session subprocess and drives it over MSP JSON-RPC, speaking the
// host's native session/turn protocol. No wire-level SSE/ndjson framing
// — the serve host is the transport surface, so StreamingTransport is
// "none".
func (*Provider) Manifest() agent.HarnessManifest {
	headlessEvents := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventContextUsage, agent.EventSubagent, agent.EventResult, agent.EventError}
	return agent.HarnessManifest{
		Name:              agent.HarnessMuse,
		HumanLabel:        "Muse",
		Family:            agent.FamilyHarness,
		ContractABI:       "harness/v2",
		ExecutionSecurity: museExecutionSecurity,
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true, // turn/steer into the running turn
			SupportsSessionResume:    true, // session/resume
			SupportsToolPlugins:      true,
			AcceptsMcpServerSpec:     true, // inline config.mcpServers
			AcceptsAllowedToolsList:  false,
			EmitsSubagentEvents:      true,  // subagent items map to SubagentEvent
			SetsParentToolUseID:      false, // MSP exposes no parent delegation id; the field stays empty by contract
			SupportsReasoningEffort:  true,  // turn/start.reasoningEffort
			SupportsOneShot:          true,
			NativeJSONMode:           false, // MSP turn/start carries no output-schema member; structured output stays soft
			ToolPermissionFormat:     "muse",
			StreamingTransport:       "none", // MSP JSON-RPC, not SSE/ndjson over the wire surface
			SupportsInteractivePTY:   false,
			// turn/steer delivers a message into the running turn over
			// the same JSON-RPC control surface — the mcp-rpc channel.
			NoticeDelivery: agent.NoticeDeliveryMCPRPC,
			// session/tokenUsage arrives per model step with the
			// counted-once counters plus the cumulative block; the
			// handle folds it and emits typed ContextUsageEvents from
			// session/contextUsage.
			ContextUsageSource: agent.ContextUsageSourceRPCUsage,
			Drives:             []agent.WireProtocol{agent.ProtoMuseMSP},
			DrivesHosts:        []agent.ServingHost{agent.HostOAuthCLI},
			Transport:          agent.TransportSubprocessRPC,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{
			{
				ID: "muse/headless/prompt-v1", Mode: agent.PromptModeAutonomous,
				SystemDelivery: agent.PromptDeliveryMuseLeadText, BaseAppendDelivery: agent.PromptDeliveryMuseLeadText,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryMuseLeadText,
				UserDelivery: agent.PromptDeliveryMuseTurnInput, AmendmentDelivery: agent.PromptDeliveryMuseTurnInput,
			},
		},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{
				ID: "muse/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryMuseInlineMCP,
				NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryMuseApprovalBridge,
				MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: headlessEvents,
				ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: headlessEvents,
				CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
			},
		},
	}
}
