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
			SupportsMessageInjection: true,  // Lane B: Prompt on a live session (07 §7)
			SupportsSessionResume:    true,  // Lane B: Resume / create-with-session (07 §7, §9)
			SupportsToolPlugins:      false, // opencode plugins are not donmai tool plugins
			// DRIFT (code wins over 07 §7): the design tables AcceptsMcpServerSpec=true,
			// but the shipped cross-provider invariant (afcli tooluse_matrix_test:
			// AcceptsMcpServerSpec=true requires SupportsToolPlugins=true) forbids
			// mcp-accept without tool-plugin support. The §5.3 MCP config injection
			// (config.go projectMCP) is implemented + tested; only the cap
			// advertisement waits on reconciling that invariant (a cross-cutting
			// decision beyond Lane B).
			AcceptsMcpServerSpec:    false,
			AcceptsAllowedToolsList: true, // Lane B: opencode.json permission map (07 §5.2)
			EmitsSubagentEvents:     false,
			SupportsReasoningEffort: true,
			SupportsOneShot:         true,
			NativeJSONMode:          true, // /v1 honors response_format
			ToolPermissionFormat:    "claude",
			StreamingTransport:      "ndjson",
			// ONLY openai-chat — NOT anthropic-messages (cross-protocol cell not-yet-valid).
			Drives:      []agent.WireProtocol{agent.ProtoOpenAIChat},
			DrivesHosts: []agent.ServingHost{agent.HostOAuthCLI, agent.HostLocal, agent.HostDirect},
			Transport:   agent.TransportCLIInjection,
		},
	}
}
