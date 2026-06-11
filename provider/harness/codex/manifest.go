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
func (*Provider) Manifest() agent.HarnessManifest {
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
			Drives:                   []agent.WireProtocol{agent.ProtoOpenAIResponses, agent.ProtoOpenAIChat},
			DrivesHosts:              []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect, agent.HostAzure},
			Transport:                agent.TransportSubprocessRPC,
		},
	}
}
