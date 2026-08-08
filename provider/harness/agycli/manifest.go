package agycli

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the agycli provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the Antigravity (agy)
// CLI-wrap loop-driver. Additive alongside Capabilities(); the agent-loop
// bools project Capabilities() (parity-tested). agy is driven over a pty
// (host login channel) speaking the antigravity-oauth protocol; it brings its
// own auth (host-session) and exposes no streaming framing or tool grammar.
func (*Provider) Manifest() agent.HarnessManifest {
	events := []agent.EventKind{agent.EventInit, agent.EventAssistantText, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError}
	return agent.HarnessManifest{
		Name:        agent.HarnessAntigravity,
		HumanLabel:  "Antigravity",
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
			NativeJSONMode:           false,
			ToolPermissionFormat:     "",
			StreamingTransport:       "none", // pty plaintext
			// `agy -p` is single-shot: the session ends with the answer, so
			// there is no live session to reach and Handle.Inject returns
			// ErrUnsupported (handle.go). The pty transport above is the LOGIN
			// channel, not an input surface — a write there lands in agy's own
			// UI, which is exactly the mistake pty-notice is fenced off from.
			NoticeDelivery: agent.NoticeDeliveryNone,
			Drives:         []agent.WireProtocol{agent.ProtoAntigravityOAuth},
			DrivesHosts:    []agent.ServingHost{agent.HostOAuthCLI},
			Transport:      agent.TransportPTY,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{{
			ID: "antigravity/headless/agy-pty-v1", Mode: agent.PromptModeAutonomous,
			SystemDelivery: agent.PromptDeliveryUnsupported, BaseAppendDelivery: agent.PromptDeliveryUnsupported,
			BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryUnsupported,
			UserDelivery: agent.PromptDeliveryAgyPromptFlag, AmendmentDelivery: agent.PromptDeliveryAgyPromptFlag,
		}},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "antigravity/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
			ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryUnsupported,
			NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
			MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
			LifecycleDelivery: agent.ToolDeliveryCoarsePTYEvents, LifecycleFidelity: agent.EvidenceCoarse, LifecycleEvents: events,
			ReplayDelivery: agent.ToolDeliveryUnsupported, ReplayFidelity: agent.EvidenceUnsupported,
			CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
		}},
	}
}
