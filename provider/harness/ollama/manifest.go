package ollama

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the ollama provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the in-box net/http
// Ollama loop. Additive alongside Capabilities(); the agent-loop bools project
// Capabilities() (parity-tested). Its identity and capability surface are
// deliberately separate from Gemini direct: ollama drives only the Ollama
// protocol over HostLocal with NDJSON streaming and whole-response native JSON
// mode (format:"json").
func (*Provider) Manifest() agent.HarnessManifest {
	events := []agent.EventKind{agent.EventInit, agent.EventAssistantText, agent.EventResult, agent.EventError}
	return agent.HarnessManifest{
		Name:        agent.HarnessOllama,
		HumanLabel:  "Ollama (local)",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: false,
			SupportsSessionResume:    false,
			SupportsToolPlugins:      false,
			AcceptsMcpServerSpec:     false,
			AcceptsAllowedToolsList:  false,
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  false,
			SupportsOneShot:          true,
			NativeJSONMode:           true, // format:"json" (whole-response)
			ToolPermissionFormat:     "claude",
			StreamingTransport:       "ndjson",
			// A chat turn is one HTTP exchange with no session the server
			// keeps open, so there is nothing to deliver INTO: Handle.Inject
			// returns ErrUnsupported (handle.go). Declared none rather than
			// silently best-effort — the durable mailbox is this harness's
			// delivery path.
			NoticeDelivery: agent.NoticeDeliveryNone,
			Drives:         []agent.WireProtocol{agent.ProtoOllama},
			DrivesHosts:    []agent.ServingHost{agent.HostLocal},
			Transport:      agent.TransportDirectAPI,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{{
			ID: "ollama/ollama-chat/direct-api-v1", Mode: agent.PromptModeAutonomous,
			SystemDelivery: agent.PromptDeliveryOllamaSystemMessage, BaseAppendDelivery: agent.PromptDeliveryOllamaSystemMessage,
			BaseReplaceDelivery: agent.PromptDeliveryOllamaSystemMessage, ContextDelivery: agent.PromptDeliveryOllamaSystemMessage,
			UserDelivery: agent.PromptDeliveryOllamaUserMessage, AmendmentDelivery: agent.PromptDeliveryOllamaUserMessage,
		}},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "ollama/ollama-chat/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
			ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryUnsupported,
			NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
			MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
			LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: events,
			ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: events,
			CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
		}},
	}
}
