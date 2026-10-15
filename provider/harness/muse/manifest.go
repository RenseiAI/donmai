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
	return agent.HarnessManifest{
		Name:              agent.HarnessMuse,
		HumanLabel:        "Muse",
		Family:            agent.FamilyHarness,
		ContractABI:       "harness/v2",
		ExecutionSecurity: museExecutionSecurity,
		Caps: agent.HarnessCaps{
			SupportsMessageInjection:         true, // turn/steer into the running turn
			SupportsSessionResume:            true,
			SupportsToolPlugins:              true,
			AcceptsMcpServerSpec:             true,
			AcceptsAllowedToolsList:          false,
			EmitsSubagentEvents:              true, // subagent items map to SubagentEvent
			SetsParentToolUseID:              false,
			SupportsReasoningEffort:          true,
			SupportsOneShot:                  true,
			NativeJSONMode:                   false, // turn/start has no structured-output primitive
			ToolPermissionFormat:             "muse",
			StreamingTransport:               "none", // serve-host JSON-RPC, not SSE/ndjson
			SupportsInteractivePTY:           false,
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
		},
	}
}
