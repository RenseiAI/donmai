package stub

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the stub provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*provider)(nil)

// Manifest returns the harness-family declaration for the test stub. Additive
// alongside Capabilities(). Unlike the real providers (whose Capabilities()
// is hardcoded and state-free), the stub's Capabilities() returns the
// CONFIGURABLE p.caps; to keep the manifest a faithful projection of the live
// declaration for ANY instance (including the zero-value the matrix parity
// test constructs), the agent-loop bools are derived from p.caps here rather
// than hardcoded. The DRIVE surface (the axis link) is fixed: the stub drives
// the sentinel "stub" protocol over HostLocal via direct-api.
//
// New() seeds p.caps with defaultCapabilities() (all-on), so the matrix
// generator — which harvests via New() — sees the all-on projection per
// P1-SPEC §2.8.
func (p *provider) Manifest() agent.HarnessManifest {
	c := p.caps
	events := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventToolProgress, agent.EventResult, agent.EventError}
	return agent.HarnessManifest{
		Name:        agent.HarnessStub,
		HumanLabel:  "Test Stub",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: c.SupportsMessageInjection,
			SupportsSessionResume:    c.SupportsSessionResume,
			SupportsToolPlugins:      c.SupportsToolPlugins,
			AcceptsMcpServerSpec:     c.AcceptsMcpServerSpec,
			AcceptsAllowedToolsList:  c.AcceptsAllowedToolsList,
			EmitsSubagentEvents:      c.EmitsSubagentEvents,
			SupportsReasoningEffort:  c.SupportsReasoningEffort,
			SupportsOneShot:          true,
			NativeJSONMode:           true,
			ToolPermissionFormat:     c.ToolPermissionFormat,
			StreamingTransport:       "none",
			// The stub's scripted loop lives in this process and its
			// Handle.Inject appends to it directly (handle.go), so the honest
			// declaration is the in-box one — the same answer the real in-box
			// harness (gemini-direct) gives.
			NoticeDelivery: agent.NoticeDeliveryInBoxLoop,
			Drives:         []agent.WireProtocol{agent.ProtoStub},
			DrivesHosts:    []agent.ServingHost{agent.HostLocal},
			Transport:      agent.TransportDirectAPI,
		},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "stub/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
			ToolPluginDelivery: agent.ToolDeliveryStubOracle, MCPDelivery: agent.ToolDeliveryStubOracle,
			NativeToolPolicyDelivery: agent.ToolDeliveryStubOracle, PermissionConfigDelivery: agent.ToolDeliveryStubOracle,
			MCPToolPolicyDelivery: agent.ToolDeliveryStubOracle, ToolHookDelivery: agent.ToolDeliveryStubOracle,
			LifecycleDelivery: agent.ToolDeliveryStubOracle, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: events,
			ReplayDelivery: agent.ToolDeliveryStubOracle, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: events,
			CleanupDelivery: agent.ToolDeliveryStubOracle, EvidenceTier: "unit_verified",
		}},
	}
}
