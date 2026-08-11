package agent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/agycli"
	"github.com/RenseiAI/donmai/provider/harness/amp"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/provider/harness/gemini"
	"github.com/RenseiAI/donmai/provider/harness/ollama"
	"github.com/RenseiAI/donmai/provider/harness/opencode"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/provider/harness/shell"
)

func TestToolLifecycleAdapterMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		manifest   agent.HarnessManifest
		modes      []agent.PromptSessionMode
		mcp        bool
		policy     bool
		structured bool
	}{
		{"claude", (&claude.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}, true, true, true},
		{"codex", (&codex.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}, true, false, true},
		{"gemini", (&gemini.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, true, true, true},
		{"ollama", (&ollama.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, false, false, true},
		{"amp", (&amp.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, true, false, true},
		{"agy-cli", (&agycli.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, false, false, false},
		{"opencode", (&opencode.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, true, true, true},
		{"pi", (&pi.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, false, true, true},
		{"shell", (&shell.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeHumanControlled}, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := len(tc.manifest.ToolLifecycle), len(tc.modes); got != want {
				t.Fatalf("profile count = %d, want %d", got, want)
			}
			for _, mode := range tc.modes {
				profile, ok := tc.manifest.ToolLifecycleProfile(mode)
				if !ok {
					t.Fatalf("missing %s profile", mode)
				}
				if profile.ProductionEligible {
					t.Error("unit declarations must not imply production eligibility")
				}
				if got := profile.MCPDelivery != agent.ToolDeliveryUnsupported; got != tc.mcp && mode == agent.PromptModeAutonomous {
					t.Errorf("MCP support = %v, want %v", got, tc.mcp)
				}
				if got := profile.NativeToolPolicyDelivery != agent.ToolDeliveryUnsupported; got != tc.policy && mode == agent.PromptModeAutonomous {
					t.Errorf("native policy support = %v, want %v", got, tc.policy)
				}
				if got := profile.LifecycleFidelity == agent.EvidenceStructured; got != tc.structured && mode == agent.PromptModeAutonomous {
					t.Errorf("structured lifecycle = %v, want %v", got, tc.structured)
				}
			}
		})
	}
}

func TestPrepareHarnessRejectsUnsupportedFlatSpecCapabilities(t *testing.T) {
	t.Parallel()
	manifest := (&ollama.Provider{}).Manifest()
	tests := []struct {
		name  string
		spec  agent.Spec
		field string
	}{
		{name: "reasoning effort", spec: agent.Spec{Effort: agent.EffortHigh}, field: "effort"},
		{name: "interactive PTY", spec: agent.Spec{Interactive: &agent.InteractiveSpec{}}, field: "interactive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := agent.PrepareHarness(tc.spec, manifest)
			var denial *agent.SpecAdmissionError
			if !errors.As(err, &denial) {
				t.Fatalf("PrepareHarness error = %v, want typed SpecAdmissionError", err)
			}
			if denial.Code != agent.SpecDenialCapabilityUnsupported || denial.Field != tc.field {
				t.Fatalf("denial = %+v, want capability_unsupported for %q", denial, tc.field)
			}
		})
	}
}

func TestToolLifecycleAdapterPositivePolicies(t *testing.T) {
	t.Parallel()
	stdio := agent.MCPServerConfig{Name: "tools", Command: "donmai", Args: []string{"mcp", "serve"}}
	policy := &agent.PermissionConfig{AllowPatterns: []string{"^git status$"}, DisallowPatterns: []string{"^git push"}, DefaultDecision: "deny"}
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		spec     agent.Spec
	}{
		{"claude", (&claude.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, MCPServers: []agent.MCPServerConfig{stdio}, MCPToolNames: []string{"mcp__tools__read"}}},
		{"codex", (&codex.Provider{}).Manifest(), agent.Spec{Autonomous: true, PermissionConfig: policy, MCPServers: []agent.MCPServerConfig{stdio}}},
		{"gemini", (&gemini.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, MCPServers: []agent.MCPServerConfig{stdio}, MCPToolNames: []string{"mcp__tools__read"}}},
		{"opencode", (&opencode.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, PermissionConfig: policy, MCPServers: []agent.MCPServerConfig{stdio}, MCPToolNames: []string{"mcp__tools__read"}}},
		{"pi", (&pi.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, PermissionConfig: policy}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, receipt, err := agent.AdaptToolLifecycle(tc.spec, mustProfile(t, tc.manifest, agent.PromptModeAutonomous))
			if err != nil {
				t.Fatalf("AdaptToolLifecycle: %v", err)
			}
			if receipt.Decision != "ready" || len(receipt.Entries) == 0 {
				t.Fatalf("receipt = %+v, want non-empty ready receipt", receipt)
			}
			for _, entry := range receipt.Entries {
				if entry.Outcome != agent.ToolOutcomeAdmitted {
					t.Errorf("entry %s outcome = %s, want admitted", entry.ID, entry.Outcome)
				}
			}
		})
	}
}

func TestToolLifecycleAdapterUnsupportedPoliciesDeny(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		spec     agent.Spec
		mode     agent.PromptSessionMode
	}{
		{"codex-flat-list", (&codex.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"amp", (&amp.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"agy-cli", (&agycli.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"ollama", (&ollama.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"shell", (&shell.Provider{}).Manifest(), agent.Spec{Interactive: &agent.InteractiveSpec{}, AllowedTools: []string{"Read"}}, agent.PromptModeHumanControlled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, receipt, err := agent.AdaptToolLifecycle(tc.spec, mustProfile(t, tc.manifest, tc.mode))
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported || adaptationErr.Channel != agent.ToolChannelAllowedTools {
				t.Fatalf("error = %v, want typed allowed-tools denial", err)
			}
			if receipt.Decision != "denied" || receipt.Entries[len(receipt.Entries)-1].Outcome != agent.ToolOutcomeDenied {
				t.Fatalf("receipt = %+v, want denied entry", receipt)
			}
		})
	}
}

// runtimeEvidenceCase is one harness/mode answer for the three runtime-evidence
// lanes. Every value is the exact delivery the harness profile declares, so a
// manifest that stops declaring a boundary fails here rather than silently
// downgrading what an admitted session is promised.
type runtimeEvidenceCase struct {
	name string
	// wantLifecycle and wantReplay are the deliveries for the session-boundary
	// events (init + terminal result); empty means the lane denies.
	wantLifecycle agent.ToolDeliveryKind
	wantReplay    agent.ToolDeliveryKind
	wantCleanup   agent.ToolDeliveryKind
	// wantToolEvents is the delivery for per-tool lifecycle evidence; empty
	// means the profile declares no tool_use/tool_result event.
	wantToolEvents agent.ToolDeliveryKind
}

// TestToolLifecycleRuntimeEvidenceIsPerHarness pins the truthful answer for
// every shipped harness on the lifecycle, replay, and cleanup lanes. These
// three channels used to be constructed unsupported-and-undowngradable for
// every harness alike, so a session whose admitted cell granted watch, replay,
// or cancel could never spawn anywhere — the executor refused a capability its
// own manifests declared. The delivery boundary now comes from the exact
// profile, which is the executor-attested inventory generated in this repo.
func TestToolLifecycleRuntimeEvidenceIsPerHarness(t *testing.T) {
	t.Parallel()
	const (
		structured = agent.ToolDeliveryStructuredProviderEvents
		replayed   = agent.ToolDeliveryStructuredEventReplay
		coarse     = agent.ToolDeliveryCoarsePTYEvents
		cast       = agent.ToolDeliveryTerminalCastReplay
		cleanup    = agent.ToolDeliveryHandleCleanup
	)
	tests := []struct {
		manifest agent.HarnessManifest
		mode     agent.PromptSessionMode
		fidelity agent.EvidenceFidelity
		runtimeEvidenceCase
	}{
		{(&claude.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"claude-headless", structured, replayed, cleanup, structured}},
		{(&codex.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"codex-headless", structured, replayed, cleanup, structured}},
		{(&amp.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"amp", structured, replayed, cleanup, structured}},
		{(&pi.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"pi", structured, replayed, cleanup, structured}},
		{(&opencode.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"opencode", structured, replayed, cleanup, structured}},
		{(&gemini.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"gemini", structured, replayed, cleanup, structured}},
		// A completion-only endpoint watches its own session boundary but runs
		// no tools, so the per-tool events are truthfully absent.
		{(&ollama.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"ollama", structured, replayed, cleanup, ""}},
		// A PTY mode carries the session boundary as terminal bytes and may not
		// inherit the headless mode's structured tool evidence.
		{(&claude.Provider{}).Manifest(), agent.PromptModeHumanControlled, agent.EvidenceCoarse, runtimeEvidenceCase{"claude-interactive", coarse, cast, cleanup, ""}},
		{(&codex.Provider{}).Manifest(), agent.PromptModeHumanControlled, agent.EvidenceCoarse, runtimeEvidenceCase{"codex-interactive", coarse, cast, cleanup, ""}},
		{(&shell.Provider{}).Manifest(), agent.PromptModeHumanControlled, agent.EvidenceCoarse, runtimeEvidenceCase{"shell", coarse, cast, cleanup, ""}},
		// Antigravity drives a PTY with no replay adapter at all: coarse
		// lifecycle evidence is deliverable, replay is not, and neither is a
		// structured claim over the same bytes.
		{(&agycli.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceCoarse, runtimeEvidenceCase{"agy-cli-coarse", coarse, "", cleanup, coarse}},
		{(&agycli.Provider{}).Manifest(), agent.PromptModeAutonomous, agent.EvidenceStructured, runtimeEvidenceCase{"agy-cli-structured", "", "", cleanup, ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := agent.Spec{Autonomous: true}
			if tc.mode == agent.PromptModeHumanControlled {
				spec = agent.Spec{Interactive: &agent.InteractiveSpec{}}
			}
			// Every requirement is optional so one lane's honest refusal cannot
			// mask another lane's answer; the required-entry denial is pinned
			// by TestToolLifecycleUndeliverableRequiredRuntimeEvidenceDenies.
			plan := agent.ToolLifecyclePlan{
				ContractVersion: agent.ToolLifecycleContractVersion,
				Lifecycle: []agent.LifecycleRequirement{
					{ID: "boundary-init", Event: agent.EventInit, MinimumFidelity: tc.fidelity},
					{ID: "boundary-result", Event: agent.EventResult, MinimumFidelity: tc.fidelity},
					{ID: "tool-use", Event: agent.EventToolUse, MinimumFidelity: tc.fidelity},
				},
				Replay:         &agent.LifecycleRequirement{ID: "replay", Event: agent.EventResult, MinimumFidelity: tc.fidelity},
				RequireCleanup: true,
			}
			spec.ToolLifecyclePlan = &plan
			_, receipt, err := agent.AdaptToolLifecycle(spec, mustProfile(t, tc.manifest, tc.mode))
			if err != nil {
				t.Fatalf("AdaptToolLifecycle: %v", err)
			}
			if receipt.Decision != "ready" {
				t.Fatalf("receipt decision = %q, want ready", receipt.Decision)
			}
			want := map[string]struct {
				delivery agent.ToolDeliveryKind
				pending  agent.ToolAdaptationOutcome
			}{
				"boundary-init":   {tc.wantLifecycle, agent.ToolOutcomePendingRuntime},
				"boundary-result": {tc.wantLifecycle, agent.ToolOutcomePendingRuntime},
				"tool-use":        {tc.wantToolEvents, agent.ToolOutcomePendingRuntime},
				"replay":          {tc.wantReplay, agent.ToolOutcomePendingRuntime},
				"cleanup":         {tc.wantCleanup, agent.ToolOutcomePendingCleanup},
			}
			if len(receipt.Entries) != len(want) {
				t.Fatalf("receipt entries = %d, want %d: %+v", len(receipt.Entries), len(want), receipt.Entries)
			}
			for _, entry := range receipt.Entries {
				expected, ok := want[entry.ID]
				if !ok {
					t.Fatalf("unexpected receipt entry %q", entry.ID)
				}
				delete(want, entry.ID)
				if expected.delivery == "" {
					if entry.Outcome != agent.ToolOutcomeDenied || entry.DenialCode != agent.ToolDenialDeliveryUnsupported || entry.Delivery != "" {
						t.Errorf("entry %q = %+v, want a truthful undeliverable denial", entry.ID, entry)
					}
					continue
				}
				if entry.Outcome != expected.pending || entry.Delivery != expected.delivery {
					t.Errorf("entry %q = outcome %q via %q, want %q via %q", entry.ID, entry.Outcome, entry.Delivery, expected.pending, expected.delivery)
				}
			}
			if len(want) != 0 {
				t.Fatalf("receipt omitted entries %+v", want)
			}
		})
	}
}

// A required runtime-evidence entry the exact profile cannot deliver still
// denies before any provider side effect: the contract has no warn-and-strip
// path for a required entry, and an admitted watch/replay grant the executor
// cannot honour must fail here rather than be silently dropped at runtime.
func TestToolLifecycleUndeliverableRequiredRuntimeEvidenceDenies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		mode     agent.PromptSessionMode
		plan     agent.ToolLifecyclePlan
		channel  agent.ToolLifecycleChannel
	}{
		{
			name: "replay adapter absent", manifest: (&agycli.Provider{}).Manifest(), mode: agent.PromptModeAutonomous,
			plan: agent.ToolLifecyclePlan{
				ContractVersion: agent.ToolLifecycleContractVersion,
				Replay:          &agent.LifecycleRequirement{ID: "replay", Event: agent.EventResult, Required: true, MinimumFidelity: agent.EvidenceCoarse},
			},
			channel: agent.ToolChannelReplay,
		},
		{
			name: "coarse profile cannot answer a structured demand", manifest: (&agycli.Provider{}).Manifest(), mode: agent.PromptModeAutonomous,
			plan: agent.ToolLifecyclePlan{
				ContractVersion: agent.ToolLifecycleContractVersion,
				Lifecycle:       []agent.LifecycleRequirement{{ID: "watch-init", Event: agent.EventInit, Required: true, MinimumFidelity: agent.EvidenceStructured}},
			},
			channel: agent.ToolChannelLifecycle,
		},
		{
			name: "pty mode never emits tool events", manifest: (&shell.Provider{}).Manifest(), mode: agent.PromptModeHumanControlled,
			plan: agent.ToolLifecyclePlan{
				ContractVersion: agent.ToolLifecycleContractVersion,
				Lifecycle:       []agent.LifecycleRequirement{{ID: "watch-tool", Event: agent.EventToolUse, Required: true, MinimumFidelity: agent.EvidenceCoarse}},
			},
			channel: agent.ToolChannelLifecycle,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := agent.Spec{Autonomous: true}
			if tc.mode == agent.PromptModeHumanControlled {
				spec = agent.Spec{Interactive: &agent.InteractiveSpec{}}
			}
			plan := tc.plan
			spec.ToolLifecyclePlan = &plan
			_, receipt, err := agent.AdaptToolLifecycle(spec, mustProfile(t, tc.manifest, tc.mode))
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Channel != tc.channel || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported {
				t.Fatalf("error = %v, want typed %s denial", err, tc.channel)
			}
			if receipt.Decision != "denied" || receipt.Entries[len(receipt.Entries)-1].Outcome != agent.ToolOutcomeDenied {
				t.Fatalf("receipt = %+v, want denied entry", receipt)
			}
		})
	}
}

// A fidelity shortfall is what an authorized downgrade names: the caller
// authorized coarse evidence before admission and the exact profile declares
// that alternate, so the entry is recorded downgraded against its
// authorization rather than denied. Without that authorization the same demand
// denies, so a coarse profile never inherits a structured claim by default.
func TestToolLifecycleFidelityShortfallTakesOnlyAnAuthorizedDowngrade(t *testing.T) {
	t.Parallel()
	profile := mustProfile(t, (&claude.Provider{}).Manifest(), agent.PromptModeHumanControlled)
	newPlan := func() agent.ToolLifecyclePlan {
		return agent.ToolLifecyclePlan{
			ContractVersion: agent.ToolLifecycleContractVersion,
			Lifecycle:       []agent.LifecycleRequirement{{ID: "watch-init", Event: agent.EventInit, Required: true, MinimumFidelity: agent.EvidenceStructured}},
		}
	}
	authorized := newPlan()
	authorized.AuthorizedFallbacks = []agent.ToolLifecycleFallback{
		{ID: "operator-authorized-coarse", Channel: agent.ToolChannelLifecycle, To: agent.ToolDeliveryCoarsePTYEvents},
	}
	_, receipt, err := agent.AdaptToolLifecycle(agent.Spec{Interactive: &agent.InteractiveSpec{}, ToolLifecyclePlan: &authorized}, profile)
	if err != nil {
		t.Fatalf("authorized downgrade must adapt cleanly, got %v", err)
	}
	if len(receipt.Entries) != 1 {
		t.Fatalf("receipt = %+v, want one entry", receipt)
	}
	entry := receipt.Entries[0]
	if entry.Outcome != agent.ToolOutcomeDowngraded || entry.Delivery != agent.ToolDeliveryCoarsePTYEvents || entry.FallbackAuthID != "operator-authorized-coarse" {
		t.Fatalf("entry = %+v, want a downgrade naming its authorization", entry)
	}

	unauthorized := newPlan()
	_, _, err = agent.AdaptToolLifecycle(agent.Spec{Interactive: &agent.InteractiveSpec{}, ToolLifecyclePlan: &unauthorized}, profile)
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Channel != agent.ToolChannelLifecycle {
		t.Fatalf("error = %v, want typed lifecycle denial without an authorized downgrade", err)
	}
}

// An optional runtime-evidence entry the profile cannot deliver stays visible
// on the receipt and does not fail the session: optional denial is truthful
// and visible, and only a required entry denies admission.
func TestToolLifecycleOptionalUndeliverableRuntimeEvidenceIsRecordedNotFatal(t *testing.T) {
	t.Parallel()
	plan := agent.ToolLifecyclePlan{
		ContractVersion: agent.ToolLifecycleContractVersion,
		Lifecycle: []agent.LifecycleRequirement{
			{ID: "watch-init", Event: agent.EventInit, Required: true, MinimumFidelity: agent.EvidenceCoarse},
			{ID: "watch-tool-use", Event: agent.EventToolUse, MinimumFidelity: agent.EvidenceCoarse},
		},
	}
	_, receipt, err := agent.AdaptToolLifecycle(agent.Spec{Interactive: &agent.InteractiveSpec{}, ToolLifecyclePlan: &plan}, mustProfile(t, (&claude.Provider{}).Manifest(), agent.PromptModeHumanControlled))
	if err != nil {
		t.Fatalf("optional undeliverable evidence must not fail the spawn, got %v", err)
	}
	if receipt.Decision != "ready" || len(receipt.Entries) != 2 {
		t.Fatalf("receipt = %+v, want a ready receipt with both entries", receipt)
	}
	if receipt.Entries[0].Outcome != agent.ToolOutcomePendingRuntime || receipt.Entries[0].Delivery != agent.ToolDeliveryCoarsePTYEvents {
		t.Fatalf("required boundary entry = %+v, want pending coarse delivery", receipt.Entries[0])
	}
	if receipt.Entries[1].Outcome != agent.ToolOutcomeDenied || receipt.Entries[1].DenialCode != agent.ToolDenialDeliveryUnsupported {
		t.Fatalf("optional tool entry = %+v, want a recorded denial", receipt.Entries[1])
	}
}

func TestToolLifecycleFallbackCannotInventMissingEvent(t *testing.T) {
	t.Parallel()
	manifest := (&shell.Provider{}).Manifest()
	plan := agent.ToolLifecyclePlan{
		ContractVersion: agent.ToolLifecycleContractVersion,
		Lifecycle:       []agent.LifecycleRequirement{{ID: "tool-watch", Event: agent.EventToolUse, Required: true, MinimumFidelity: agent.EvidenceCoarse}},
		AuthorizedFallbacks: []agent.ToolLifecycleFallback{
			{ID: "coarse", Channel: agent.ToolChannelLifecycle, To: agent.ToolDeliveryCoarsePTYEvents},
		},
	}
	_, _, err := agent.AdaptToolLifecycle(agent.Spec{Interactive: &agent.InteractiveSpec{}, ToolLifecyclePlan: &plan}, mustProfile(t, manifest, agent.PromptModeHumanControlled))
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Channel != agent.ToolChannelLifecycle {
		t.Fatalf("error = %v, want missing tool event denial", err)
	}
}

func TestToolLifecycleReceiptIsDigestOnlyAndPersistenceFailsClosed(t *testing.T) {
	t.Parallel()
	secret := "MCP_SERVER_TOKEN_DO_NOT_LEAK"
	manifest := (&claude.Provider{}).Manifest()
	spec := agent.Spec{
		Autonomous: true,
		MCPServers: []agent.MCPServerConfig{{Name: "secret", Command: "server", Env: map[string]string{"TOKEN": secret}}},
	}
	_, receipt, err := agent.AdaptToolLifecycle(spec, mustProfile(t, manifest, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatalf("AdaptToolLifecycle: %v", err)
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("receipt leaked MCP environment secret: %s", body)
	}

	spec.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error { return errors.New("store unavailable") }
	_, err = agent.PrepareToolLifecycle(spec, manifest)
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed {
		t.Fatalf("PrepareToolLifecycle error = %v, want application-failed denial", err)
	}
}

func TestToolLifecycleReceiptLinksAdmissionWithoutCopyingInputs(t *testing.T) {
	t.Parallel()
	secret := "RECEIPT_LINK_DO_NOT_COPY"
	command := "RECEIPT_LINK_COMMAND_DO_NOT_COPY"
	plan := agent.ToolLifecyclePlan{
		ContractVersion:          agent.ToolLifecycleContractVersion,
		AdmissionReceiptID:       "admission_test",
		OperationalPayloadDigest: strings.Repeat("a", 64),
	}
	spec := agent.Spec{
		Autonomous:        true,
		MCPServers:        []agent.MCPServerConfig{{Name: "linked", Command: command, Env: map[string]string{"TOKEN": secret}}},
		ToolLifecyclePlan: &plan,
	}
	_, receipt, err := agent.AdaptToolLifecycle(spec, mustProfile(t, (&claude.Provider{}).Manifest(), agent.PromptModeAutonomous))
	if err != nil {
		t.Fatalf("AdaptToolLifecycle: %v", err)
	}
	if receipt.Decision != "ready" || receipt.AdmissionReceiptID != plan.AdmissionReceiptID || receipt.OperationalPayloadDigest != plan.OperationalPayloadDigest {
		t.Fatalf("linked receipt = %+v", receipt)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), command) {
		t.Fatalf("receipt copied MCP secret or command: %s", raw)
	}
}

func TestToolLifecycleDeniedReceiptPersistenceFailsClosed(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Autonomous:   true,
		AllowedTools: []string{"Read"},
		OnToolLifecycleAdapted: func(receipt agent.ToolLifecycleReceipt) error {
			if receipt.Decision != "denied" {
				t.Fatalf("callback decision = %q, want denied", receipt.Decision)
			}
			return errors.New("store unavailable")
		},
	}
	_, err := agent.PrepareToolLifecycle(spec, (&amp.Provider{}).Manifest())
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed {
		t.Fatalf("PrepareToolLifecycle error = %v, want application-failed denial", err)
	}
}

func TestToolLifecycleClosedDeliveryAndChannelEnums(t *testing.T) {
	t.Parallel()
	base := mustProfile(t, (&claude.Provider{}).Manifest(), agent.PromptModeAutonomous)
	unknownProfile := base
	unknownProfile.ToolPluginDelivery = agent.ToolDeliveryKind("future_delivery")
	tests := []struct {
		name    string
		profile agent.ToolLifecycleProfile
		plan    agent.ToolLifecyclePlan
	}{
		{"profile delivery", unknownProfile, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion}},
		{"fallback delivery", base, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, AuthorizedFallbacks: []agent.ToolLifecycleFallback{{ID: "f", Channel: agent.ToolChannelLifecycle, To: agent.ToolDeliveryKind("future_delivery")}}}},
		{"fallback channel", base, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, AuthorizedFallbacks: []agent.ToolLifecycleFallback{{ID: "f", Channel: agent.ToolLifecycleChannel("future_channel"), To: agent.ToolDeliveryCoarsePTYEvents}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := agent.AdaptToolLifecycle(agent.Spec{Autonomous: true, ToolLifecyclePlan: &tc.plan}, tc.profile)
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan {
				t.Fatalf("error = %v, want malformed-plan denial", err)
			}
		})
	}
}

func TestToolLifecycleUnusedPluginAndHookRequirementsDeny(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		plan     agent.ToolLifecyclePlan
		channel  agent.ToolLifecycleChannel
	}{
		{"pi plugin", (&pi.Provider{}).Manifest(), agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, RequireToolPlugins: true}, agent.ToolChannelToolPlugin},
		{"gemini plugin", (&gemini.Provider{}).Manifest(), agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, RequireToolPlugins: true}, agent.ToolChannelToolPlugin},
		{"pi hook", (&pi.Provider{}).Manifest(), agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, ToolHooks: []agent.ToolHookRequirement{{ID: "audit", Kind: "pre_tool", Required: true}}}, agent.ToolChannelToolHook},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := agent.AdaptToolLifecycle(agent.Spec{Autonomous: true, ToolLifecyclePlan: &tc.plan}, mustProfile(t, tc.manifest, agent.PromptModeAutonomous))
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported || adaptationErr.Channel != tc.channel {
				t.Fatalf("error = %v, want typed %s denial", err, tc.channel)
			}
		})
	}
}

func TestToolLifecycleMalformedMCPFailsBeforeDelivery(t *testing.T) {
	t.Parallel()
	manifest := (&claude.Provider{}).Manifest()
	_, _, err := agent.AdaptToolLifecycle(agent.Spec{
		Autonomous: true,
		MCPServers: []agent.MCPServerConfig{{Name: "missing-command"}},
	}, mustProfile(t, manifest, agent.PromptModeAutonomous))
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan || adaptationErr.Channel != agent.ToolChannelMCPServer {
		t.Fatalf("error = %v, want malformed MCP denial", err)
	}
}

func TestToolLifecycleMalformedPermissionRegexFailsBeforeDelivery(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	_, _, err := agent.AdaptToolLifecycle(agent.Spec{
		Autonomous:       true,
		PermissionConfig: &agent.PermissionConfig{AllowPatterns: []string{"("}},
	}, mustProfile(t, manifest, agent.PromptModeAutonomous))
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan || adaptationErr.Channel != agent.ToolChannelPermissionConfig {
		t.Fatalf("error = %v, want malformed permission denial", err)
	}
}

// Agent cards express permissions as tool designators
// ("Bash(git *)", "Bash(*)", "Read"), the runner's AllowedTools bridge
// forwards them into PermissionConfig verbatim, and the codex approval
// bridge consumes that grammar natively — so adaptation must accept
// designators even where they are not valid regexes ("*" after "(" made
// every headless codex spawn fail closed before delivery).
func TestToolLifecycleDesignatorPermissionPatternsSpawn(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	_, receipt, err := agent.AdaptToolLifecycle(agent.Spec{
		Autonomous: true,
		PermissionConfig: &agent.PermissionConfig{
			AllowPatterns:    []string{"Bash(git *)", "Bash(*)", "Read", "mcp__linear__list_issues", "^git status$"},
			DisallowPatterns: []string{"Write", "Bash(rm *)"},
			DefaultDecision:  "deny",
		},
	}, mustProfile(t, manifest, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatalf("designator permission patterns must adapt cleanly, got %v", err)
	}
	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q, want ready", receipt.Decision)
	}
}

// MCP tool names are a narrowing hint, not the mount boundary: a harness
// whose profile cannot deliver the name-policy channel (codex auto-discovers
// tools from mounted servers) must record the drop on the receipt and spawn,
// not fail the whole run for an undeliverable ergonomic hint.
func TestToolLifecycleMCPToolNamesUndeliverableIsRecordedNotFatal(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	stdio := agent.MCPServerConfig{Name: "tools", Command: "mcp-tools"}
	_, receipt, err := agent.AdaptToolLifecycle(agent.Spec{
		Autonomous:   true,
		MCPServers:   []agent.MCPServerConfig{stdio},
		MCPToolNames: []string{"mcp__tools__read"},
	}, mustProfile(t, manifest, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatalf("undeliverable MCP tool names must not fail the spawn, got %v", err)
	}
	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q, want ready", receipt.Decision)
	}
	var found bool
	for _, entry := range receipt.Entries {
		if entry.ID == "mcp-tool-names" {
			found = true
			if entry.Outcome != agent.ToolOutcomeDenied {
				t.Fatalf("mcp-tool-names outcome = %q, want denied-but-recorded", entry.Outcome)
			}
			if entry.Required {
				t.Fatalf("mcp-tool-names entry must be non-required")
			}
		}
	}
	if !found {
		t.Fatalf("receipt must still record the mcp-tool-names entry")
	}
}

func mustProfile(t *testing.T, manifest agent.HarnessManifest, mode agent.PromptSessionMode) agent.ToolLifecycleProfile {
	t.Helper()
	profile, ok := manifest.ToolLifecycleProfile(mode)
	if !ok {
		t.Fatalf("manifest %s has no %s tool/lifecycle profile", manifest.Name, mode)
	}
	return profile
}
