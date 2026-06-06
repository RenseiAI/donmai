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
	return agent.HarnessManifest{
		Name:        agent.HarnessOpenCode,
		HumanLabel:  "OpenCode",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: false,
			SupportsSessionResume:    false,
			SupportsToolPlugins:      false,
			AcceptsMcpServerSpec:     false,
			AcceptsAllowedToolsList:  false,
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  true,
			SupportsOneShot:          true,
			NativeJSONMode:           true, // /v1 honors response_format
			ToolPermissionFormat:     "claude",
			StreamingTransport:       "ndjson",
			// ONLY openai-chat — NOT anthropic-messages (cross-protocol cell not-yet-valid).
			Drives:      []agent.WireProtocol{agent.ProtoOpenAIChat},
			DrivesHosts: []agent.ServingHost{agent.HostOAuthCLI, agent.HostLocal, agent.HostDirect},
			Transport:   agent.TransportCLIInjection,
		},
	}
}
