package muse

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestManifest_CapabilityClaimsAreTruthful pins the advertised surface the
// matrix harvests: exactly one autonomous headless prompt profile and one
// autonomous headless tool-lifecycle profile, no interactive/PTY profile,
// no native structured output, no flat allow-list, no subagent parent id.
// The MSP host exposes no plan mode, no fork and no rollback — so the
// manifest must not grow a second profile or a natively-enforced claim
// for them. A future capability addition updates this test deliberately,
// not silently.
func TestManifest_CapabilityClaimsAreTruthful(t *testing.T) {
	t.Parallel()
	mf := (&Provider{}).Manifest()

	if mf.Name != agent.HarnessMuse {
		t.Errorf("name = %q, want muse", mf.Name)
	}
	if mf.Caps.SupportsMessageInjection != true {
		t.Error("inject = false, want true (turn/steer is the live-message channel)")
	}
	if mf.Caps.SupportsSessionResume != true {
		t.Error("resume = false, want true (session/resume re-attaches)")
	}
	if mf.Caps.NativeJSONMode {
		t.Error("nativeJsonMode = true, want false (turn/start has no structured-output primitive)")
	}
	if mf.Caps.AcceptsAllowedToolsList {
		t.Error("allowedToolsList = true, want false (approval-mode + bridge, not a flat list)")
	}
	if mf.Caps.SupportsInteractivePTY {
		t.Error("interactivePty = true, want false (no PTY spawn mode)")
	}
	if mf.Caps.SetsParentToolUseID {
		t.Error("parentToolUseId = true, want false (MSP exposes no parent delegation id)")
	}
	if len(mf.PromptDelivery) != 1 {
		t.Fatalf("prompt profiles = %d, want exactly 1 (no plan-mode second profile)", len(mf.PromptDelivery))
	}
	if mf.PromptDelivery[0].Mode != agent.PromptModeAutonomous {
		t.Errorf("prompt profile mode = %q, want autonomous", mf.PromptDelivery[0].Mode)
	}
	if len(mf.ToolLifecycle) != 1 {
		t.Fatalf("tool lifecycle profiles = %d, want exactly 1 (no fork/rollback second profile)", len(mf.ToolLifecycle))
	}
	if mf.ToolLifecycle[0].Mode != agent.PromptModeAutonomous {
		t.Errorf("tool lifecycle mode = %q, want autonomous", mf.ToolLifecycle[0].Mode)
	}
}

// TestManifest_DrivesOnlyMSPSessions pins the DRIVE surface the matrix
// validates cells against: muse-msp on the subscriber-login and keyed
// hosts, over subprocess JSON-RPC. A cell on any other protocol or host
// fails the generator — this test fails first, at the manifest.
func TestManifest_DrivesOnlyMSPSessions(t *testing.T) {
	t.Parallel()
	mf := (&Provider{}).Manifest()

	if len(mf.Caps.Drives) != 1 || mf.Caps.Drives[0] != agent.ProtoMuseMSP {
		t.Errorf("drives = %v, want [muse-msp]", mf.Caps.Drives)
	}
	wantHosts := map[agent.ServingHost]bool{agent.HostOAuthCLI: true, agent.HostDirect: true}
	if len(mf.Caps.DrivesHosts) != len(wantHosts) {
		t.Fatalf("drivesHosts = %v, want oauth-cli + direct", mf.Caps.DrivesHosts)
	}
	for _, h := range mf.Caps.DrivesHosts {
		if !wantHosts[h] {
			t.Errorf("drivesHosts carries %q, want only oauth-cli + direct", h)
		}
	}
	if mf.Caps.Transport != agent.TransportSubprocessRPC {
		t.Errorf("transport = %q, want subprocess-jsonrpc", mf.Caps.Transport)
	}
}
