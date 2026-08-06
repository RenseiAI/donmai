package opencode

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the opencode provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the OpenCode
// loop-driver. Additive alongside Capabilities(); the agent-loop bools
// project Capabilities() (parity-tested). opencode runs `opencode run
// --format json` (ndjson) over cli-injection. It drives ONLY openai-chat —
// NOT anthropic-messages — so there is no opencode×anthropic cell; the
// north-star opencode×google cell rides Google's local /v1 (openai-chat) host.
func (*Provider) Manifest() agent.HarnessManifest {
	events := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError}
	return agent.HarnessManifest{
		Name:        agent.HarnessOpenCode,
		HumanLabel:  "OpenCode",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true,  // Lane B: Prompt on a live session (07 §7)
			SupportsSessionResume:    true,  // Lane B: Resume / create-with-session (07 §7, §9)
			SupportsToolPlugins:      false, // OpenCode plugins are independent of MCP config acceptance.
			AcceptsMcpServerSpec:     true,  // provider-owned per-session config; see config.go projectMCP.
			AcceptsAllowedToolsList:  true,  // Lane B: opencode.json permission map (07 §5.2)
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  true,
			SupportsOneShot:          true,
			NativeJSONMode:           true, // /v1 honors response_format
			ToolPermissionFormat:     "claude",
			StreamingTransport:       "ndjson",
			// ONLY openai-chat — NOT anthropic-messages (cross-protocol cell not-yet-valid).
			Drives: []agent.WireProtocol{agent.ProtoOpenAIChat},
			// HostGateway added (08 §2 / ADR-2026-07-24): opencode is the M1
			// gateway consumer, reaching an OpenAI model through the loopback
			// gateway's openai-chat surface. Same-protocol in M1; the gateway
			// makes cross-protocol reach legal for other harnesses in M2.
			DrivesHosts: []agent.ServingHost{agent.HostOAuthCLI, agent.HostLocal, agent.HostDirect, agent.HostGateway},
			Transport:   agent.TransportCLIInjection,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{{
			ID: "opencode/headless/prompt-v1", Mode: agent.PromptModeAutonomous,
			SystemDelivery: agent.PromptDeliveryUnsupported, BaseAppendDelivery: agent.PromptDeliveryUnsupported,
			BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryUnsupported,
			UserDelivery: agent.PromptDeliveryOpenCodePrompt, AmendmentDelivery: agent.PromptDeliveryOpenCodePrompt,
		}},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "opencode/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
			ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryOpenCodeProjectMCP,
			NativeToolPolicyDelivery: agent.ToolDeliveryOpenCodePermissionMap, PermissionConfigDelivery: agent.ToolDeliveryOpenCodePermissionMap,
			MCPToolPolicyDelivery: agent.ToolDeliveryOpenCodePermissionMap, ToolHookDelivery: agent.ToolDeliveryUnsupported,
			LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: events,
			ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: events,
			CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
		}},
	}
}
