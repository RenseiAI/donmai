package amp

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the amp provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the Amp loop-driver.
// Additive alongside Capabilities(); the agent-loop bools project
// Capabilities() (parity-tested). amp runs the `amp` CLI with --stream-json
// (reusing claude's JSONL mapper = ndjson) over cli-injection, driving
// anthropic-messages against a keyed, metered direct host (AMP_API_KEY) — it
// is NOT a host-subscription cell.
func (*Provider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name:        agent.HarnessAmp,
		HumanLabel:  "Amp",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: false,
			SupportsSessionResume:    false,
			SupportsToolPlugins:      true,
			AcceptsMcpServerSpec:     true,
			AcceptsAllowedToolsList:  false,
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  false,
			SupportsOneShot:          true,
			NativeJSONMode:           false,
			ToolPermissionFormat:     "claude",
			StreamingTransport:       "ndjson", // reuses claude JSONL mapper
			Drives:                   []agent.WireProtocol{agent.ProtoAnthropicMessages},
			DrivesHosts:              []agent.ServingHost{agent.HostDirect},
			Transport:                agent.TransportCLIInjection,
		},
	}
}
