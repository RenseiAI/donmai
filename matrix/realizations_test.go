package matrix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestCompileCapabilityRealizationsConsumesObservation(t *testing.T) {
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "one"}}
	d, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: "example/v1", HarnessID: agent.HarnessPi, AdapterVersion: "pi/interactive/tool-lifecycle-v5", Mode: agent.PromptModeHumanControlled, RecipeID: "recipe/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	o, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: d, FixtureID: "real", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: strings.Repeat("b", 64)}}, ObservedSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := CompileCapabilityRealizations([]CapabilityRealizationSource{{Declaration: d, Observation: o}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	o.ObservedSurface = nil
	o.ObservationDigest = ""
	if _, err := CompileCapabilityRealizations([]CapabilityRealizationSource{{Declaration: d, Observation: o}}); err == nil {
		t.Fatal("missing fixture surface compiled")
	}
}

func TestCompileCapabilityRealizationEvidenceExecutesFixture(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	calls := 0
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		calls++
		if declaration.AdapterVersion != "pi/interactive/tool-lifecycle-v5" {
			t.Fatalf("executor declaration=%+v", declaration)
		}
		return sourceExecution(t, declaration), nil
	})
	rows, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source})
	if err != nil || calls != 1 || len(rows) != 1 {
		t.Fatalf("rows=%+v calls=%d err=%v", rows, calls, err)
	}
	if !rows[0].ProductionEligible || rows[0].EvidenceTier != agent.CapabilityEvidenceSmoked {
		t.Fatalf("eligibility=%+v", rows[0])
	}
}

func TestCompileCapabilityRealizationEvidenceRefusesMissingSkippedAndWrongEvidence(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	if _, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source}); err == nil {
		t.Fatal("missing fixture executor compiled")
	}
	source.Executor = CapabilityFixtureExecutorFunc(func(context.Context, agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		return agent.CapabilityFixtureExecution{}, ErrCapabilityFixtureSkipped
	})
	if _, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source}); !errors.Is(err, ErrCapabilityFixtureSkipped) {
		t.Fatalf("skipped fixture error=%v", err)
	}
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		execution := sourceExecution(t, declaration)
		execution.Observation.AdapterVersion = "pi/interactive/tool-lifecycle-v4"
		return execution, nil
	})
	if _, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source}); err == nil {
		t.Fatal("wrong-adapter evidence compiled")
	}
}

func TestBuildWithCapabilityRealizationsKeepsCapabilityAndChannelAxesIndependent(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		return sourceExecution(t, declaration), nil
	})
	built, err := BuildWithCapabilityRealizations(context.Background(), []CapabilityRealizationEvidenceSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Capabilities) != 1 || !built.Capabilities[0].ProductionEligible {
		t.Fatalf("derived capabilities=%+v", built.Capabilities)
	}
	var pi HarnessRow
	for _, harness := range built.Harnesses {
		if harness.Name == agent.HarnessPi {
			pi = harness
		}
	}
	var interactiveProfile agent.ToolLifecycleProfile
	for _, profile := range pi.ToolLifecycle {
		if profile.ID == "pi/interactive/tool-lifecycle-v6" {
			interactiveProfile = profile
		}
	}
	if interactiveProfile.ID != "pi/interactive/tool-lifecycle-v6" {
		t.Fatalf("current Pi interactive profile=%q, want v6", interactiveProfile.ID)
	}
	if pi.Caps.AcceptsMcpServerSpec || interactiveProfile.ProductionEligible {
		t.Fatalf("coarse channel/profile unexpectedly changed: %+v", pi)
	}
	if len(built.Matrix.Realizations) != 1 || len(built.Matrix.Capabilities) != 1 {
		t.Fatalf("full matrix omitted derived axes: %+v", built.Matrix)
	}
}

func TestRenderedCapabilityEvidenceBuildsRuntimeRegistryWithoutExecutingFixture(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	executions := 0
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		executions++
		return sourceExecution(t, declaration), nil
	})
	built, err := BuildWithCapabilityRealizations(context.Background(), []CapabilityRealizationEvidenceSource{source})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := built.Render()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Realizations []agent.CompiledCapabilityRealizationEvidence `json:"realizations"`
		Capabilities []CapabilityEligibilityRow                    `json:"capabilities"`
	}
	if err := json.Unmarshal(artifacts.Files[FileCapabilityRealizations], &document); err != nil {
		t.Fatal(err)
	}
	if executions != 1 || len(document.Realizations) != 1 || len(document.Capabilities) != 1 {
		t.Fatalf("executions=%d document=%+v", executions, document)
	}
	registry, err := agent.NewCapabilityRealizationRegistryFromEvidence(document.Realizations)
	if err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatal("runtime artifact validation re-executed the fixture")
	}
	declaration := source.Declaration
	if _, ok := registry.Resolve(declaration.CapabilityID, declaration.HarnessID, declaration.AdapterVersion, declaration.Mode); !ok {
		t.Fatal("generated row did not reach runtime registry")
	}
}

func TestCompileCapabilityRealizationEvidencePreflightRejectsDuplicateTupleWithoutExecuting(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	calls := 0
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		calls++
		return sourceExecution(t, declaration), nil
	})
	_, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source, source})
	if err == nil || !strings.Contains(err.Error(), "duplicate capability realization evidence tuple") {
		t.Fatalf("duplicate preflight error=%v", err)
	}
	if calls != 0 {
		t.Fatalf("duplicate preflight executed %d fixtures, want 0", calls)
	}
}

func TestCompileCapabilityRealizationEvidencePreflightRejectsMalformedDeclarationWithoutExecuting(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	calls := 0
	source.Executor = CapabilityFixtureExecutorFunc(func(context.Context, agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		calls++
		return agent.CapabilityFixtureExecution{}, nil
	})
	source.Declaration = agent.CapabilityRealizationDeclaration{}
	if _, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source}); err == nil {
		t.Fatal("malformed preflight declaration compiled")
	}
	if calls != 0 {
		t.Fatalf("malformed preflight executed %d fixtures, want 0", calls)
	}
}

func TestCompileCapabilityRealizationEvidencePreflightKeepsUniqueExecutionOrder(t *testing.T) {
	first := realizationEvidenceFixture(t, "example.workflow/b", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	second := realizationEvidenceFixture(t, "example.workflow/a", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	firstCalls, secondCalls := 0, 0
	first.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		firstCalls++
		return sourceExecution(t, declaration), nil
	})
	second.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		secondCalls++
		return sourceExecution(t, declaration), nil
	})
	rows, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || secondCalls != 1 || len(rows) != 2 {
		t.Fatalf("calls=%d/%d rows=%d", firstCalls, secondCalls, len(rows))
	}
	if rows[0].Compiled.Declaration.CapabilityID != "example.workflow/a" || rows[1].Compiled.Declaration.CapabilityID != "example.workflow/b" {
		t.Fatalf("order=%+v", rows)
	}
}

func TestCompileCapabilityRealizationEvidenceRejectsDuplicateTuple(t *testing.T) {
	source := realizationEvidenceFixture(t, "example.workflow/v1", agent.HarnessPi, "pi/interactive/tool-lifecycle-v5", agent.PromptModeHumanControlled)
	source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		return sourceExecution(t, declaration), nil
	})
	if _, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source, source}); err == nil {
		t.Fatal("duplicate evidence tuple compiled")
	}
}

func realizationEvidenceFixture(t *testing.T, capability string, harness agent.HarnessName, adapter string, mode agent.PromptSessionMode) CapabilityRealizationEvidenceSource {
	t.Helper()
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "one"}}
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID: capability, HarnessID: harness, AdapterVersion: adapter, Mode: mode, RecipeID: "recipe/v1",
		Entries:         []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}},
		DeclaredSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	return CapabilityRealizationEvidenceSource{Declaration: declaration}
}

func sourceExecution(t *testing.T, declaration agent.CapabilityRealizationDeclaration) agent.CapabilityFixtureExecution {
	t.Helper()
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "real-binary", BinaryDigest: strings.Repeat("a", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: strings.Repeat("b", 64)}},
		ObservedSurface:  declaration.Recipe.DeclaredSurface,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent.CapabilityFixtureExecution{
		Producer:    agent.CapabilityFixtureProducer{ID: "example.real-binary-fixture/v1", Source: []byte("actual fixture producer source"), ReleaseGate: "real-binary-no-skip"},
		Observation: observation,
	}
}

func TestCompileCapabilityRealizationsOrdersFullTupleAndRejectsDuplicates(t *testing.T) {
	base := func(capability string, h agent.HarnessName, adapter string, mode agent.PromptSessionMode) CapabilityRealizationSource {
		surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "one"}}
		d, _ := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: capability, HarnessID: h, AdapterVersion: adapter, Mode: mode, RecipeID: "recipe/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface})
		o, _ := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: d, FixtureID: "real", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: strings.Repeat("b", 64)}}, ObservedSurface: surface})
		return CapabilityRealizationSource{Declaration: d, Observation: o}
	}
	a := base("same/v1", agent.HarnessPi, "z/v1", agent.PromptModeHumanControlled)
	b := base("same/v1", agent.HarnessCodex, "a/v1", agent.PromptModeAutonomous)
	rows, err := CompileCapabilityRealizations([]CapabilityRealizationSource{a, b})
	if err != nil || rows[0].Compiled.Declaration.HarnessID != agent.HarnessCodex {
		t.Fatalf("order=%+v err=%v", rows, err)
	}
	if _, err := CompileCapabilityRealizations([]CapabilityRealizationSource{a, a}); err == nil {
		t.Fatal("duplicate tuple compiled")
	}
}

func TestNamedExtensionRealizationRequiresExactProfileOptIn(t *testing.T) {
	deliveryID := "synthetic-pack"
	entryID, err := agent.AdditionalExtensionCapabilityEntryID(deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	binderSource := []byte("synthetic matrix binder")
	binderDigest := sha256.Sum256(binderSource)
	contract := &agent.CapabilityParameterContractV1{ContractVersion: agent.CapabilityParameterContractVersionV1, ID: "example.matrix-binder/v1", BinderSourceDigest: hex.EncodeToString(binderDigest[:]), SurfaceProjection: "subset", RuntimeObservation: "before_first_turn"}
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "tool"}}
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: "example.named/v1", HarnessID: agent.HarnessPi, AdapterVersion: "pi/interactive/tool-lifecycle-v6", Mode: agent.PromptModeHumanControlled, RecipeID: "example/named/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: entryID, Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface, ParameterContract: contract})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: declaration, FixtureID: "synthetic", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: entryID, Channel: agent.ToolChannelToolPlugin, InputDigest: strings.Repeat("b", 64)}}, ObservedSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileCapabilityRealizations([]CapabilityRealizationSource{{Declaration: declaration, Observation: observation}}); err == nil || !strings.Contains(err.Error(), "does not opt in") {
		t.Fatalf("named declaration parity error=%v", err)
	}
	calls := 0
	source := CapabilityRealizationEvidenceSource{Declaration: declaration, Executor: CapabilityFixtureExecutorFunc(func(context.Context, agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		calls++
		return agent.CapabilityFixtureExecution{}, nil
	})}
	if _, err := CompileCapabilityRealizationEvidence(context.Background(), []CapabilityRealizationEvidenceSource{source}); err == nil || !strings.Contains(err.Error(), "does not opt in") {
		t.Fatalf("named evidence parity error=%v", err)
	}
	if calls != 0 {
		t.Fatalf("profile parity executed %d fixtures before refusal", calls)
	}
}
