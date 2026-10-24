package matrix

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestSupportV2_DispatchRequiresVerifiedTier pins B1 for the dispatch axes:
// a unit-verified profile without production eligibility stays unverified,
// while a verified tier or production eligibility promotes with a profile ref.
func TestSupportV2_DispatchRequiresVerifiedTier(t *testing.T) {
	t.Parallel()
	verified := HarnessRow{
		Name: agent.HarnessPi,
		Caps: agent.HarnessCaps{SupportsInteractivePTY: true},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{ID: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous, LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, EvidenceTier: "native_verified", ProductionEligible: true},
		},
	}
	if level, evidence := modeDispatchSupport(verified, agent.PromptModeAutonomous, false); level != SupportSupported || len(evidence) == 0 {
		t.Fatalf("verified headless profile = %q %q; want supported with profile ref", level, evidence)
	}
	unverifiedTier := HarnessRow{
		Name: agent.HarnessPi,
		Caps: agent.HarnessCaps{SupportsInteractivePTY: true},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{ID: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous, LifecycleDelivery: agent.ToolDeliveryStructuredProviderEvents, EvidenceTier: "unit_verified", ProductionEligible: false},
		},
	}
	if level, evidence := modeDispatchSupport(unverifiedTier, agent.PromptModeAutonomous, false); level != SupportUnverified || len(evidence) != 0 {
		t.Fatalf("unit_verified headless profile = %q %q; want unverified without evidence", level, evidence)
	}
	// An interactive unit-verified profile on a PTY-capable harness also stays
	// unverified: the delivery exists but no verified tier proves it.
	interactiveUnverified := HarnessRow{
		Name: agent.HarnessPi,
		Caps: agent.HarnessCaps{SupportsInteractivePTY: true},
		ToolLifecycle: []agent.ToolLifecycleProfile{
			{ID: "pi/interactive/tool-lifecycle-v6", Mode: agent.PromptModeHumanControlled, LifecycleDelivery: agent.ToolDeliveryCoarsePTYEvents, EvidenceTier: "unit_verified", ProductionEligible: false},
		},
	}
	if level, _ := modeDispatchSupport(interactiveUnverified, agent.PromptModeHumanControlled, true); level != SupportUnverified {
		t.Fatalf("unit_verified interactive profile = %q; want unverified", level)
	}
}

// TestSupportV2_CellRequiresSmokedLane pins B1 for the cell axes: an unsmoked
// cell stays unverified, while a smoked lane-proven cell promotes with
// cell plus smoke refs.
func TestSupportV2_CellRequiresSmokedLane(t *testing.T) {
	t.Parallel()
	harness := HarnessRow{Name: agent.HarnessOpenCode}
	unsmoked := []HarnessEndpointCell{{
		Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect,
		NeedsAPIKey: true, Smoked: false,
	}}
	if level, _ := cellFlagSupport(harness, unsmoked, needsKeyCell); level != SupportUnverified {
		t.Fatalf("unsmoked credential cell = %q; want unverified", level)
	}
	// A smoked cell without proving lanes is equally unproven.
	noLanes := []HarnessEndpointCell{{
		Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect,
		NeedsAPIKey: true, Smoked: true,
	}}
	if level, _ := cellFlagSupport(harness, noLanes, needsKeyCell); level != SupportUnverified {
		t.Fatalf("lane-less smoked cell = %q; want unverified", level)
	}
	smoked := []HarnessEndpointCell{{
		Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect,
		NeedsAPIKey: true, Smoked: true,
		ProvingLanes: []SmokeLaneName{"TestOpenCodeHarnessSmoke_RealBinary_Teardown"},
	}}
	level, evidence := cellFlagSupport(harness, smoked, needsKeyCell)
	if level != SupportSupported || len(evidence) == 0 {
		t.Fatalf("smoked credential cell = %q %q; want supported with evidence", level, evidence)
	}
	joined := strings.Join(evidence, ",")
	if !strings.Contains(joined, "cell:opencode/openai/direct") || !strings.Contains(joined, "smoke:TestOpenCodeHarnessSmoke_RealBinary_Teardown") {
		t.Fatalf("smoked credential evidence %q omits cell or smoke lane ref", evidence)
	}
}

// TestSupportV2_ProjectionGuards pins the load-bearing B2 guards with
// table-driven unit tests on the projection functions.
func TestSupportV2_ProjectionGuards(t *testing.T) {
	t.Parallel()
	t.Run("failing loaded fixture stays unverified", func(t *testing.T) {
		t.Parallel()
		harness := HarnessRow{
			Name: agent.HarnessPi,
			ToolLifecycle: []agent.ToolLifecycleProfile{
				{ID: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous},
			},
		}
		// A failed native fixture still carries the loaded delivery digest but
		// reports no subagent events: events alone never promote.
		rows := []SubagentQualificationRow{{
			Cell:           CellKey{Harness: agent.HarnessPi, Endpoint: agent.CompanyAnthropic, Host: agent.HostDirect},
			AdapterVersion: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous,
			DeliveryInputDigest: strings.Repeat("a", 64), EmitsSubagentEvents: false,
		}}
		if level, _ := nativeControlsSupport(harness, rows); level != SupportUnverified {
			t.Fatalf("failed loaded fixture = %q; want unverified", level)
		}
	})
	t.Run("interactive without PTY stays unverified", func(t *testing.T) {
		t.Parallel()
		harness := HarnessRow{
			Name: agent.HarnessMuse,
			Caps: agent.HarnessCaps{SupportsInteractivePTY: false},
			ToolLifecycle: []agent.ToolLifecycleProfile{
				{ID: "muse/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled, LifecycleDelivery: agent.ToolDeliveryCoarsePTYEvents, EvidenceTier: "native_verified", ProductionEligible: true},
			},
		}
		if level, _ := modeDispatchSupport(harness, agent.PromptModeHumanControlled, true); level != SupportUnverified {
			t.Fatalf("interactive without PTY = %q; want unverified", level)
		}
	})
	t.Run("all unsupported stays unsupported", func(t *testing.T) {
		t.Parallel()
		harness := HarnessRow{
			Name: agent.HarnessMuse,
			Caps: agent.HarnessCaps{SupportsInteractivePTY: true},
			ToolLifecycle: []agent.ToolLifecycleProfile{
				{ID: "muse/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled, LifecycleDelivery: agent.ToolDeliveryUnsupported, EvidenceTier: "unit_verified"},
			},
		}
		level, evidence := modeDispatchSupport(harness, agent.PromptModeHumanControlled, true)
		if level != SupportUnsupported || len(evidence) == 0 {
			t.Fatalf("all-unsupported interactive = %q %q; want unsupported with profile ref", level, evidence)
		}
		if !strings.Contains(strings.Join(evidence, ","), "profile:muse/interactive/tool-lifecycle-v1") {
			t.Fatalf("unsupported evidence %q omits profile ref", evidence)
		}
	})
	t.Run("brings own auth is not credential delivery", func(t *testing.T) {
		t.Parallel()
		harness := HarnessRow{Name: agent.HarnessOpenCode}
		cells := []HarnessEndpointCell{{
			Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyGoogle, Host: agent.HostLocal,
			NeedsAPIKey: true, BringsOwnAuth: true, Smoked: true,
			ProvingLanes: []SmokeLaneName{"TestSiblingContextReposSmoke"},
		}}
		if level, _ := cellFlagSupport(harness, cells, needsKeyCell); level != SupportUnverified {
			t.Fatalf("brings-own-auth key cell = %q; want unverified for credential delivery", level)
		}
	})
	t.Run("capability axis reads its own id", func(t *testing.T) {
		t.Parallel()
		harness := HarnessRow{
			Name: agent.HarnessPi,
			ToolLifecycle: []agent.ToolLifecycleProfile{
				{ID: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous},
			},
		}
		rows := []CapabilityEligibilityRow{{
			CapabilityID: supportCapA2A, HarnessID: agent.HarnessPi,
			AdapterVersion: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous,
			EvidenceTier: agent.CapabilityEvidenceSmoked, ProductionEligible: true,
		}}
		if level, _ := capabilityAxisSupport(harness, rows, supportCapA2A); level != SupportSupported {
			t.Fatalf("a2a fixture left a2a at %q; want supported", level)
		}
		if level, _ := capabilityAxisSupport(harness, rows, supportCapCode); level != SupportUnverified {
			t.Fatalf("a2a fixture promoted code intelligence to %q; want unverified", level)
		}
	})
}

// TestSupportV2_FailingLoadedFixtureEndToEnd pins B2(a) through the production
// build entry point: a loaded but failing native fixture keeps
// harness-native controls unverified.
func TestSupportV2_FailingLoadedFixtureEndToEnd(t *testing.T) {
	t.Parallel()
	failing := piSubagentSource(t, []string{"dispatch_child"}, true, true)
	built, err := BuildWithSubagentFixtures(context.Background(), nil, append(NativeSubagentFixtures(), failing))
	if err != nil {
		t.Fatalf("BuildWithSubagentFixtures(): %v", err)
	}
	for _, h := range built.Harnesses {
		if h.Name != agent.HarnessPi {
			continue
		}
		entry := supportAxisLookup(t, h.SupportV2, SupportAxisSubagentControl)
		if entry.HarnessNativeControls.Level != SupportUnverified {
			t.Fatalf("failed loaded fixture promoted native controls to %q; want unverified", entry.HarnessNativeControls.Level)
		}
	}
}

// TestSupportV2_SingleCapabilityIsolation pins B2(e) through the production
// build entry point: seeding exactly one capability promotes only its axis.
func TestSupportV2_SingleCapabilityIsolation(t *testing.T) {
	t.Parallel()
	source := realizationEvidenceFixture(t, supportCapA2A, agent.HarnessPi, "pi/headless/tool-lifecycle-v2", agent.PromptModeAutonomous)
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		return sourceExecution(t, declaration), nil
	})
	built, err := BuildWithSubagentFixtures(context.Background(), []CapabilityRealizationEvidenceSource{source}, NativeSubagentFixtures())
	if err != nil {
		t.Fatalf("BuildWithSubagentFixtures(): %v", err)
	}
	for _, h := range built.Harnesses {
		if h.Name != agent.HarnessPi {
			continue
		}
		if entry := supportAxisLookup(t, h.SupportV2, SupportAxisA2A); entry.Level != SupportSupported {
			t.Fatalf("a2a-only fixture left a2a at %q; want supported", entry.Level)
		}
		for _, axis := range []string{SupportAxisCodeIntelligence, SupportAxisArchitectureIntelligence, SupportAxisTokenCostAccuracy} {
			if entry := supportAxisLookup(t, h.SupportV2, axis); entry.Level != SupportUnverified {
				t.Fatalf("a2a-only fixture promoted %s to %q; want unverified", axis, entry.Level)
			}
		}
	}
}

// TestSupportV2_ValidateGuards pins B3: the Render-time guard rejects a
// caller-mutated harness row and a skewed matrix mirror.
func TestSupportV2_ValidateGuards(t *testing.T) {
	t.Parallel()
	t.Run("mutated harness row", func(t *testing.T) {
		t.Parallel()
		built, err := Build()
		if err != nil {
			t.Fatalf("Build(): %v", err)
		}
		built.Harnesses[0].SupportV2.Axes[0].Level = SupportSupported
		built.Harnesses[0].SupportV2.Axes[0].Evidence = []string{"profile:mutated"}
		if _, err := built.Render(); err == nil || !strings.Contains(err.Error(), "does not match executed evidence") {
			t.Fatalf("mutated harness Render() error = %v; want executed-evidence mismatch", err)
		}
	})
	t.Run("skewed matrix mirror", func(t *testing.T) {
		t.Parallel()
		built, err := Build()
		if err != nil {
			t.Fatalf("Build(): %v", err)
		}
		built.Matrix.Harnesses = append([]HarnessRow(nil), built.Matrix.Harnesses...)
		// Replace the whole contract so the Axes backing shared with
		// Built.Harnesses is not mutated through the alias. Render
		// checks the subagent projection (full-row DeepEqual) before
		// the support guard, so the mirror skew is exercised directly
		// against validateHarnessSupportV2, the load-bearing check.
		built.Matrix.Harnesses[0].SupportV2 = HarnessSupportV2{
			ContractVersion: HarnessSupportContractVersion,
			Axes:            []SupportAxisEntry{{Axis: "tampered", Level: SupportSupported, Evidence: []string{"profile:mutated"}}},
		}
		if err := built.validateHarnessSupportV2(); err == nil || !strings.Contains(err.Error(), "mirror does not match") {
			t.Fatalf("skewed mirror validate error = %v; want mirror mismatch", err)
		}
		// The same skew is also rejected at Render time (via the
		// subagent full-row mirror check shadowing it).
		if _, err := built.Render(); err == nil {
			t.Fatal("skewed mirror Render() passed; want rejection")
		}
	})
	t.Run("mirror without harness row", func(t *testing.T) {
		t.Parallel()
		built, err := Build()
		if err != nil {
			t.Fatalf("Build(): %v", err)
		}
		// validateHarnessSupportV2 runs before any artifact is emitted;
		// Render checks the subagent projection first, so an orphan
		// mirror row is exercised directly against the support guard.
		ghost := built.Harnesses[0]
		ghost.Name = "ghost-harness"
		built.Matrix.Harnesses = append(append([]HarnessRow(nil), built.Matrix.Harnesses...), ghost)
		if err := built.validateHarnessSupportV2(); err == nil || !strings.Contains(err.Error(), "has no harness row") {
			t.Fatalf("orphan mirror validate error = %v; want missing-row rejection", err)
		}
	})
}
