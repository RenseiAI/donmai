package pi

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the pi provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the pi loop-driver.
// Additive alongside Capabilities(); the agent-loop bools project
// Capabilities() (parity-tested in matrix/parity_test.go). pi runs
// `pi --mode rpc` as a subprocess and drives it over LF-delimited JSONL, so
// Transport is subprocess-jsonrpc and StreamingTransport is "ndjson".
//
// pi's pi-ai layer natively speaks anthropic-messages, openai-chat,
// openai-responses, and gemini-generate (research/pi.md §3), making pi the
// broadest Drive surface in the fleet. The DrivesHosts set is intentionally
// broader than the authored matrix cells (DEC-2: declared capability is not
// claimed capability) — only pi × anthropic × direct and pi × openai ×
// direct are authored as cells today (both experimental/untested); every
// other host is a later one-row add gated on smoke coverage.
//
// DRIFT vs design §3 (code reality wins): the design listed HostGateway in
// DrivesHosts, but agent.HostGateway does not exist yet (the gateway is W3 /
// ADR-A, not merged). It is omitted here and re-added when the gateway ships.
// A `pi × local × local` cell is also NOT authored: the CompanyLocal endpoint
// only speaks ProtoOllama (provider/endpoint/local), which pi does not drive
// — a local openai-compat pi cell needs either a local-openai host on the
// local endpoint or routing via google × local, deferred as a follow-up.
func (*Provider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name:        agent.HarnessPi,
		HumanLabel:  "pi",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true,  // steer / follow_up
			SupportsSessionResume:    true,  // session file + cursor replay (get_entries since=<id>)
			SupportsToolPlugins:      true,  // pi.registerTool via the donmai extension
			AcceptsMcpServerSpec:     false, // pi has no MCP by design; Spec.MCPServers is capability-gated-ignored
			AcceptsAllowedToolsList:  true,  // enforced by OUR policy extension, not by pi
			EmitsSubagentEvents:      false,
			SupportsReasoningEffort:  true,  // set_thinking_level off…max
			SupportsOneShot:          true,  // pi --mode json single-shot lane
			NativeJSONMode:           false, // no server-constrained structured output ⇒ spawn-collect
			ToolPermissionFormat:     "claude",
			StreamingTransport:       "ndjson",
			Drives: []agent.WireProtocol{
				agent.ProtoAnthropicMessages,
				agent.ProtoOpenAIChat,
				agent.ProtoOpenAIResponses,
				agent.ProtoGeminiGenerate,
			},
			DrivesHosts: []agent.ServingHost{
				agent.HostDirect,
				agent.HostLocal,
				agent.HostBedrock,
				agent.HostVertex,
			},
			Transport: agent.TransportSubprocessRPC,
		},
	}
}

// Capabilities returns the flat agent-loop capability projection. The matrix
// parity gate asserts these agree field-for-field with Manifest().Caps.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection: true,
		SupportsSessionResume:    true,
		SupportsToolPlugins:      true,
		// pi has NO permission system of its own — the donmai policy
		// extension IS the permission config consumer, so the provider
		// requires a structured policy to adjudicate against.
		NeedsPermissionConfig:   true,
		EmitsSubagentEvents:     false,
		SupportsReasoningEffort: true,
		ToolPermissionFormat:    "claude",
		AcceptsAllowedToolsList: true,
		AcceptsMcpServerSpec:    false,
		HumanLabel:              "pi",
	}
}
