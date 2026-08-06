package gemini

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the gemini provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the in-box net/http
// "raw" loop as driven against Gemini direct. Additive alongside
// Capabilities(); the agent-loop bools project Capabilities() (parity-tested).
//
// gemini and ollama BOTH map to HarnessRaw; the matrix generator merges the
// two raw manifests into one harness row (union of drives/hosts). This
// package contributes the gemini-generate drive over HostDirect/HostVertex,
// direct-api transport, SSE streaming, and native JSON mode (responseSchema).
func (*Provider) Manifest() agent.HarnessManifest {
	events := []agent.EventKind{agent.EventInit, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError}
	return agent.HarnessManifest{
		Name:        agent.HarnessRaw,
		HumanLabel:  "Gemini (direct)",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true,
			SupportsSessionResume:    false,
			SupportsToolPlugins:      true,
			AcceptsMcpServerSpec:     true, // in-box MCP bridge (mcp.go + runtime/mcp)
			AcceptsAllowedToolsList:  true,
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  true,
			SupportsOneShot:          true,
			NativeJSONMode:           true, // responseSchema
			ToolPermissionFormat:     ToolPermissionFormatGemini,
			StreamingTransport:       "sse",
			Drives:                   []agent.WireProtocol{agent.ProtoGeminiGenerate},
			DrivesHosts:              []agent.ServingHost{agent.HostDirect, agent.HostVertex},
			Transport:                agent.TransportDirectAPI,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{{
			ID: "raw/gemini-generate/direct-api-v1", Mode: agent.PromptModeAutonomous,
			SystemDelivery: agent.PromptDeliveryGeminiSystemInstruction, BaseAppendDelivery: agent.PromptDeliveryGeminiSystemInstruction,
			BaseReplaceDelivery: agent.PromptDeliveryGeminiSystemInstruction, ContextDelivery: agent.PromptDeliveryGeminiSystemInstruction,
			UserDelivery: agent.PromptDeliveryGeminiUserContent, AmendmentDelivery: agent.PromptDeliveryGeminiUserContent,
		}},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "raw/gemini-generate/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
			ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryGeminiMCPBridge,
			NativeToolPolicyDelivery: agent.ToolDeliveryGeminiNativeBoundary, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
			MCPToolPolicyDelivery: agent.ToolDeliveryGeminiNativeBoundary, ToolHookDelivery: agent.ToolDeliveryUnsupported,
			LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: events,
			ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: events,
			CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
		}},
	}
}
