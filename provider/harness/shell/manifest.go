package shell

import "github.com/RenseiAI/donmai/agent"

// Compile-time assertion: the shell provider satisfies HarnessProvider.
var _ agent.HarnessProvider = (*Provider)(nil)

// Manifest returns the harness-family declaration for the shell spawn mode.
//
// Unlike every other harness in this repo, shell drives no model endpoint at
// all — it is a bare interactive terminal, not an agent loop — so
// Drives/DrivesHosts are intentionally empty and shell anchors NO matrix cell
// (matrix/cells.go validCells): a cell models a (harness × model-endpoint ×
// host) binding, which has no meaning for a harness that never talks to a
// model. buildHarnesses (matrix/build.go) harvests shell into the
// harnesses[] row regardless — cell coverage is optional, not required, for
// a harvested harness.
//
// shell's Transport is unconditionally TransportPTY: unlike claude
// (cli-injection) and codex (subprocess-jsonrpc), which get PTY only as an
// ADDITIONAL interactive spawn mode alongside their default headless-loop
// transport, shell has no other transport at all — TransportPTY is both its
// only mode and its declared Transport. See claude/manifest.go and
// codex/manifest.go for the contrasting "PTY is a spawn mode, not the
// declared Transport" case, and agent/harness.go's
// HarnessCaps.SupportsInteractivePTY doc comment for the general rule.
func (*Provider) Manifest() agent.HarnessManifest {
	events := []agent.EventKind{agent.EventInit, agent.EventResult}
	return agent.HarnessManifest{
		Name:        agent.HarnessShell,
		HumanLabel:  "Shell",
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
			SupportsOneShot:          false, // no headless mode at all
			NativeJSONMode:           false,
			ToolPermissionFormat:     "",
			StreamingTransport:       "none",
			SupportsInteractivePTY:   true,
			Drives:                   nil,
			DrivesHosts:              nil,
			Transport:                agent.TransportPTY,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{{
			ID: "shell/interactive/pty-seed-v1", Mode: agent.PromptModeHumanControlled,
			SystemDelivery: agent.PromptDeliveryUnsupported, BaseAppendDelivery: agent.PromptDeliveryUnsupported,
			BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryUnsupported,
			UserDelivery: agent.PromptDeliveryShellPTYSeed, AmendmentDelivery: agent.PromptDeliveryShellPTYSeed,
		}},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "shell/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled,
			ToolPluginDelivery: agent.ToolDeliveryUnsupported, MCPDelivery: agent.ToolDeliveryUnsupported,
			NativeToolPolicyDelivery: agent.ToolDeliveryUnsupported, PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
			MCPToolPolicyDelivery: agent.ToolDeliveryUnsupported, ToolHookDelivery: agent.ToolDeliveryUnsupported,
			LifecycleDelivery: agent.ToolDeliveryCoarsePTYEvents, LifecycleFidelity: agent.EvidenceCoarse, LifecycleEvents: events,
			ReplayDelivery: agent.ToolDeliveryTerminalCastReplay, ReplayFidelity: agent.EvidenceCoarse, ReplayEvents: events,
			CleanupDelivery:    agent.ToolDeliveryHandleCleanup,
			FallbackDeliveries: []agent.ToolDeliveryKind{agent.ToolDeliveryCoarsePTYEvents, agent.ToolDeliveryTerminalCastReplay},
			EvidenceTier:       "unit_verified",
		}},
	}
}
