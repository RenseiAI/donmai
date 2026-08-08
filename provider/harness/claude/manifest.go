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
	headlessEvents := []agent.EventKind{agent.EventInit, agent.EventSystem, agent.EventAssistantText, agent.EventLlmCall, agent.EventToolUse, agent.EventToolResult, agent.EventToolProgress, agent.EventResult, agent.EventError}
	ptyEvents := []agent.EventKind{agent.EventInit, agent.EventResult}
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
			// The claude CLI invokes its own lifecycle hooks (`--bare`'s help
			// text enumerates hooks among the things it skips), and the Stop
			// hook is the point at which a message can be handed to a session
			// that is already running. That is the declared channel.
			//
			// It is NOT the same thing as the shipped `claude --resume <id>`
			// path (provider/harness/clijsonl/handle.go Inject), which starts
			// a SECOND invocation continuing a finished conversation — real
			// delivery, but not delivery into the live process, which is why
			// the axis distinguishes them.
			//
			// This declaration is DRIVEN: an interactive spawn establishes the
			// channel (stophook.go) and exposes it as agent.NoticeChannel on
			// the handle, which the runner's interactive supervisor collects
			// through. The declaration and the channel are separate facts on
			// purpose — a session whose channel could not be established still
			// runs, and every message aimed at it is dead-lettered rather than
			// accepted into a door that does not exist.
			NoticeDelivery: agent.NoticeDeliveryHook,
			Drives:         []agent.WireProtocol{agent.ProtoAnthropicMessages},
			DrivesHosts: []agent.ServingHost{
				agent.HostOAuthCLI, agent.HostDirect, agent.HostBedrock, agent.HostVertex,
			},
			Transport: agent.TransportCLIInjection,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{
			{
				ID: "claude-code/headless/cli-v1", Mode: agent.PromptModeAutonomous,
				SystemDelivery: agent.PromptDeliveryClaudeSystemAppend, BaseAppendDelivery: agent.PromptDeliveryClaudeSystemAppend,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryClaudeSystemAppend,
				UserDelivery: agent.PromptDeliveryClaudeUserStdin, AmendmentDelivery: agent.PromptDeliveryClaudeUserStdin,
			},
			{
				ID: "claude-code/interactive/pty-v1", Mode: agent.PromptModeHumanControlled,
				SystemDelivery: agent.PromptDeliveryClaudeSystemAppend, BaseAppendDelivery: agent.PromptDeliveryClaudeSystemAppend,
				BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryClaudeSystemAppend,
				UserDelivery: agent.PromptDeliveryClaudePTYSeed, AmendmentDelivery: agent.PromptDeliveryClaudePTYSeed,
			},
		},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{
				ID: "claude-code/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryClaudeMCPConfig,
				NativeToolPolicyDelivery: agent.ToolDeliveryClaudeCLIAllowDeny, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
				MCPToolPolicyDelivery: agent.ToolDeliveryClaudeCLIAllowDeny, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: headlessEvents,
				ReplayDelivery: agent.ToolDeliveryStructuredEventReplay, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: headlessEvents,
				CleanupDelivery: agent.ToolDeliveryHandleCleanup, EvidenceTier: "unit_verified",
			},
			{
				ID: "claude-code/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled,
				ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryClaudeMCPConfig,
				NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
				MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
				LifecycleDelivery: agent.ToolDeliveryCoarsePTYEvents, LifecycleFidelity: agent.EvidenceCoarse, LifecycleEvents: ptyEvents,
				ReplayDelivery: agent.ToolDeliveryTerminalCastReplay, ReplayFidelity: agent.EvidenceCoarse, ReplayEvents: ptyEvents,
				CleanupDelivery:    agent.ToolDeliveryHandleCleanup,
				FallbackDeliveries: []agent.ToolDeliveryKind{agent.ToolDeliveryCoarsePTYEvents, agent.ToolDeliveryTerminalCastReplay},
				EvidenceTier:       "unit_verified",
			},
		},
	}
}
