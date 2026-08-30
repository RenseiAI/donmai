package agent_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/codex"
)

func TestPreparedHarnessIsSoleCallbackFreeProviderAuthority(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	source := agent.Spec{
		PromptMode:     agent.PromptModeHumanControlled,
		Autonomous:     false,
		SandboxEnabled: true,
		SandboxLevel:   agent.SandboxWorkspaceWrite,
		Model:          "gpt-test",
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "actual-user-task", Text: "inspect the actual input", Required: true},
		},
		MCPServers: []agent.MCPServerConfig{{
			Name: "rensei-session", Type: "http", URL: "https://runtime.invalid",
		}},
		Interactive: &agent.InteractiveSpec{},
	}
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan, err := agent.CompilePreparedHarness(source, manifest, operationalDigest, []string{"rensei-session"}, materializations)
	if err != nil {
		t.Fatalf("CompilePreparedHarness: %v", err)
	}
	if plan.Mode != agent.PromptModeHumanControlled || plan.PromptReceipt.Decision != "ready" || plan.ToolLifecycleReceipt.Decision != "ready" {
		t.Fatalf("prepared plan = %+v", plan)
	}

	promptCallbacks, toolCallbacks := 0, 0
	materialized := source
	materialized.Cwd = "/actual/worktree"
	materialized.Env = map[string]string{"RUNTIME_SECRET": "not persisted by host"}
	materialized.MCPServers = []agent.MCPServerConfig{{
		Name: "rensei-session", Type: "http", URL: "https://platform.example/api/mcp/session", Headers: map[string]string{"Authorization": "Bearer runtime"},
	}}
	materialized.Interactive = &agent.InteractiveSpec{RecordPath: "/actual/worktree/.donmai/term.cast"}
	materialized.PreparedHarness = plan
	materialized.OnPromptAdapted = func(agent.PromptDeliveryReceipt) error { promptCallbacks++; return nil }
	materialized.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error { toolCallbacks++; return nil }

	adapted, err := agent.PrepareHarness(materialized, manifest)
	if err != nil {
		t.Fatalf("provider PrepareHarness application: %v", err)
	}
	if promptCallbacks != 0 || toolCallbacks != 0 {
		t.Fatalf("provider minted a second authority: prompt callbacks=%d tool callbacks=%d", promptCallbacks, toolCallbacks)
	}
	if adapted.PromptReceipt == nil || adapted.ToolLifecycleReceipt == nil || adapted.PromptReceipt.Decision != "ready" || adapted.ToolLifecycleReceipt.Decision != "ready" {
		t.Fatalf("provider did not consume host receipts: prompt=%+v tool=%+v", adapted.PromptReceipt, adapted.ToolLifecycleReceipt)
	}
	if adapted.Cwd != materialized.Cwd || adapted.Env["RUNTIME_SECRET"] == "" || adapted.MCPServers[0].URL != materialized.MCPServers[0].URL {
		t.Fatalf("runtime materializations were not preserved: %+v", adapted)
	}

	mutated := materialized
	mutated.Model = "different-model"
	if _, err := agent.PrepareHarness(mutated, manifest); err == nil {
		t.Fatal("authority-changing child mutation was accepted")
	}
}

// TestApplyPreparedHarnessNamesDriftingFields covers: on a
// materialized-Spec-differs-from-authority mismatch, ApplyPreparedHarness
// must return a *agent.AuthorityDriftError naming exactly which
// harnessAuthorityDigest projection field(s) drifted — never just the bare
// "materialized Spec differs" message a production failure could not be
// diagnosed from. Table-driven so each case pins ONE (or a known set of)
// drifting field name(s), proving the localization is precise and not just
// "any error, any field".
func TestApplyPreparedHarnessNamesDriftingFields(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	base := func() agent.Spec {
		return agent.Spec{
			PromptMode:         agent.PromptModeHumanControlled,
			SandboxEnabled:     true,
			SandboxLevel:       agent.SandboxWorkspaceWrite,
			Model:              "gpt-test",
			SystemPromptAppend: "base system append",
			PromptPlan: &agent.PromptPlan{
				ContractVersion:  agent.PromptContractVersion,
				BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
				UserPrompt:       agent.PromptContent{ID: "actual-user-task", Text: "inspect the actual input", Required: true},
			},
			Interactive: &agent.InteractiveSpec{},
		}
	}
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}

	tests := []struct {
		name   string
		mutate func(agent.Spec) agent.Spec
		want   []string
	}{
		{
			name:   "model differs (a genuine authority field, e.g. a routing swap between preflight and spawn)",
			mutate: func(s agent.Spec) agent.Spec { s.Model = "different-model"; return s },
			want:   []string{"model"},
		},
		{
			name:   "system prompt append differs (prompt adaptation applied after preflight)",
			mutate: func(s agent.Spec) agent.Spec { s.SystemPromptAppend = "mutated append"; return s },
			want:   []string{"systemPromptAppend"},
		},
		{
			name:   "allowed tools differ (capability-pack tool policy applied after preflight)",
			mutate: func(s agent.Spec) agent.Spec { s.AllowedTools = []string{"Read", "Write"}; return s },
			want:   []string{"allowedTools"},
		},
		{
			name: "a non-runtime mcp server is added (e.g. a session gateway server not in plan.RuntimeMCPNames)",
			mutate: func(s agent.Spec) agent.Spec {
				s.MCPServers = append(append([]agent.MCPServerConfig(nil), s.MCPServers...), agent.MCPServerConfig{
					Name: "session-gateway", Type: "http", URL: "https://platform.example/api/mcp/session",
					Headers: map[string]string{"Authorization": "Bearer real-bearer"},
				})
				return s
			},
			want: []string{"mcpServers"},
		},
		{
			name: "model and allowed tools both differ",
			mutate: func(s agent.Spec) agent.Spec {
				s.Model = "different-model"
				s.AllowedTools = []string{"Read", "Write"}
				return s
			},
			want: []string{"allowedTools", "model"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := base()
			plan, err := agent.CompilePreparedHarness(source, manifest, operationalDigest, nil, materializations)
			if err != nil {
				t.Fatalf("CompilePreparedHarness: %v", err)
			}
			materialized := tc.mutate(source)
			materialized.PreparedHarness = plan

			_, err = agent.PrepareHarness(materialized, manifest)
			if err == nil {
				t.Fatalf("expected an authority drift error, got nil")
			}
			var driftErr *agent.AuthorityDriftError
			if !errors.As(err, &driftErr) {
				t.Fatalf("expected *agent.AuthorityDriftError, got %T: %v", err, err)
			}
			if !reflect.DeepEqual(driftErr.Fields, tc.want) {
				t.Fatalf("Fields = %v, want %v", driftErr.Fields, tc.want)
			}
			for _, field := range tc.want {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("Error() = %q must name drifting field %q", err.Error(), field)
				}
			}
		})
	}
}

// TestPreparedHarnessDropsDeniedAdvisoryExtensionsOnApply pins the
// host/child consistency of the advisory-extensions drop: a host that
// compiles an all-advisory AdditionalExtensions batch against a profile whose
// tool_plugin channel is Unsupported persists a "ready" plan whose receipt
// records the drop (Outcome: denied, Required: false), and the child-side
// application of that SAME plan strips the deliveries from the adapted Spec —
// the exact adapter never sees a delivery the host-persisted receipt refused,
// on either the direct or the prepared-harness lane.
func TestPreparedHarnessDropsDeniedAdvisoryExtensionsOnApply(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	source := agent.Spec{
		PromptMode: agent.PromptModeAutonomous,
		Autonomous: true,
		Model:      "gpt-test",
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "actual-user-task", Text: "inspect the actual input", Required: true},
		},
		AdditionalExtensions: []agent.ExtensionDelivery{advisoryExtensionDelivery("pack-1")},
	}
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan, err := agent.CompilePreparedHarness(source, manifest, operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("an all-advisory batch must compile, got %v", err)
	}
	if plan.ToolLifecycleReceipt.Decision != "ready" {
		t.Fatalf("tool receipt decision = %q, want ready", plan.ToolLifecycleReceipt.Decision)
	}
	var found bool
	for _, entry := range plan.ToolLifecycleReceipt.Entries {
		if entry.ID != "additional-extensions" {
			continue
		}
		found = true
		if entry.Outcome != agent.ToolOutcomeDenied || entry.Required {
			t.Fatalf("entry = %+v, want denied-but-non-required", entry)
		}
	}
	if !found {
		t.Fatalf("host plan must record the additional-extensions drop; entries=%+v", plan.ToolLifecycleReceipt.Entries)
	}

	materialized := source
	materialized.Cwd = "/actual/worktree"
	materialized.PreparedHarness = plan
	adapted, err := agent.PrepareHarness(materialized, manifest)
	if err != nil {
		t.Fatalf("provider PrepareHarness application: %v", err)
	}
	if len(adapted.AdditionalExtensions) != 0 {
		t.Fatalf("dropped deliveries must be stripped on the prepared-harness lane too, got %d riding", len(adapted.AdditionalExtensions))
	}
}
