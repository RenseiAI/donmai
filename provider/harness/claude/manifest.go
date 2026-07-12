package claude

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the claude provider satisfies the additive
// HarnessProvider interface (Provider + Manifest()). Manifest() is a
// state-free method on (*Provider) so the matrix generator can harvest it
// from a zero-value instance with no probe.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the Claude Code
// loop-driver. It is PURELY ADDITIVE alongside Capabilities(): the agent-loop
// bools below are a faithful projection of Capabilities() (asserted by the
// matrix parity test), and the method adds the DRIVE surface (which wire
// protocols / serving hosts this harness can drive) + transport.
//
// claude spawns the `claude` CLI with --output-format stream-json (JSONL
// framing = ndjson), driving the anthropic-messages protocol over
// cli-injection. It can ride host login (oauth-cli) or keyed direct/bedrock/
// vertex hosts.
//
// SupportsInteractivePTY (W4) declares an ADDITIONAL spawn mode: bare
// `claude` under a PTY (interactive.go), NOT a change to the declared
// Transport above. Transport names how the DEFAULT headless loop runs
// (cli-injection, unchanged); TransportPTY is used only where PTY is a
// harness's ONLY transport (see provider/harness/shell/manifest.go) — claude
// keeps cli-injection here and gets PTY strictly as a per-Spawn-call mode,
// selected by Spec.Interactive != nil, not by a Transport value. See
// agent/harness.go's HarnessCaps.SupportsInteractivePTY doc comment for the
// general rule this and codex/manifest.go both follow.
func (*Provider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name:        agent.HarnessClaudeCode,
		HumanLabel:  "Claude Code",
		Family:      agent.FamilyHarness,
		ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true,
			SupportsSessionResume:    false,
			SupportsToolPlugins:      true,
			AcceptsMcpServerSpec:     true,
			AcceptsAllowedToolsList:  true,
			EmitsSubagentEvents:      true,
			SupportsReasoningEffort:  true,
			SupportsOneShot:          true,
			NativeJSONMode:           false,
			ToolPermissionFormat:     "claude",
			StreamingTransport:       "ndjson", // claude CLI JSONL = ndjson framing
			SupportsInteractivePTY:   true,
			Drives:                   []agent.WireProtocol{agent.ProtoAnthropicMessages},
			DrivesHosts: []agent.ServingHost{
				agent.HostOAuthCLI, agent.HostDirect, agent.HostBedrock, agent.HostVertex,
			},
			Transport: agent.TransportCLIInjection,
		},
	}
}
