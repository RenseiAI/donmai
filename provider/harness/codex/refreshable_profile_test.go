package codex

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestInteractiveRefreshableToolLifecycleProfileIsAdditiveAndNativeProved(t *testing.T) {
	manifest := (&Provider{}).Manifest()
	legacy, ok := manifest.ToolLifecycleProfile(agent.PromptModeHumanControlled)
	if !ok || legacy.ID != "codex/interactive/tool-lifecycle-v1" {
		t.Fatalf("historical default profile = %+v ok=%v", legacy, ok)
	}
	v2, ok := manifest.ToolLifecycleProfileByID(InteractiveRefreshableToolLifecycleProfileID, agent.PromptModeHumanControlled)
	if !ok || v2.EvidenceTier != "native_verified" || !v2.ProductionEligible {
		t.Fatalf("refreshable profile = %+v ok=%v", v2, ok)
	}
	legacy.ID, v2.ID = "", ""
	legacy.EvidenceTier, v2.EvidenceTier = "", ""
	legacy.ProductionEligible, v2.ProductionEligible = false, false
	if !reflect.DeepEqual(legacy, v2) {
		t.Fatalf("refreshable profile changed unrelated delivery semantics\nlegacy=%+v\nv2=%+v", legacy, v2)
	}
}

func TestSelectedInteractiveRefreshableProfileDoesNotRelabelDefault(t *testing.T) {
	manifest := (&Provider{}).Manifest()
	base := agent.Spec{PromptMode: agent.PromptModeHumanControlled}
	selected := agent.WithToolLifecycleProfile(base, InteractiveRefreshableToolLifecycleProfileID)
	profile, ok := agent.SelectedToolLifecycleProfile(selected, manifest)
	if !ok || profile.ID != InteractiveRefreshableToolLifecycleProfileID {
		t.Fatalf("selected profile = %+v ok=%v", profile, ok)
	}
	profile, ok = agent.SelectedToolLifecycleProfile(base, manifest)
	if !ok || profile.ID != "codex/interactive/tool-lifecycle-v1" {
		t.Fatalf("default profile changed = %+v ok=%v", profile, ok)
	}
}
