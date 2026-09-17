package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const protectedRuntimeMCPTestCapability = "example.protected-mcp/v1"

func protectedRuntimeMCPTestSelector(t *testing.T, provider agent.Provider, mode agent.PromptSessionMode) ProtectedRuntimeMCPSelector {
	t.Helper()
	harness := provider.(agent.HarnessProvider)
	profile, ok := harness.Manifest().ToolLifecycleProfile(mode)
	if !ok {
		t.Fatal("test harness has no tool lifecycle profile")
	}
	return ProtectedRuntimeMCPSelector{
		CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: harness.Manifest().Name,
		AdapterProfileID: profile.ID, Mode: mode,
	}
}

func protectedRuntimeMCPCompiled(t *testing.T, capability string, provider agent.Provider, qw QueuedWork, mode agent.PromptSessionMode) agent.CompiledCapabilityRealization {
	t.Helper()
	harness := provider.(agent.HarnessProvider)
	profile, ok := harness.Manifest().ToolLifecycleProfile(mode)
	if !ok {
		t.Fatal("test harness has no tool lifecycle profile")
	}
	return protectedRuntimeMCPCompiledForProfile(t, capability, provider, qw, profile.ID, mode)
}

func protectedRuntimeMCPCompiledForProfile(t *testing.T, capability string, provider agent.Provider, qw QueuedWork, profileID string, mode agent.PromptSessionMode) agent.CompiledCapabilityRealization {
	t.Helper()
	harness := provider.(agent.HarnessProvider)
	profile, ok := harness.Manifest().ToolLifecycleProfileByID(profileID, mode)
	if !ok {
		t.Fatalf("test harness has no tool lifecycle profile %q", profileID)
	}
	server, err := protectedRuntimeMCPServer(materializeRuntimeAuthority(qw), provider, mode)
	if err != nil {
		t.Fatal(err)
	}
	entryID, err := agent.MCPServerCapabilityEntryID(server.Name)
	if err != nil {
		t.Fatal(err)
	}
	surface := []agent.CapabilitySurfaceIdentity{
		{Kind: agent.CapabilitySurfaceMCPServer, ID: server.Name},
		{Kind: agent.CapabilitySurfaceMCPTool, ID: "example_call"},
	}
	inputDigest := agent.MCPRuntimeServerCapabilityInputDigest(server)
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID: capability, HarnessID: harness.Manifest().Name,
		AdapterVersion: profile.ID, Mode: mode, RecipeID: "example/protected-runtime-mcp/v1",
		Entries: []agent.CapabilityRecipeEntry{{
			EntryID: entryID, Channel: agent.ToolChannelMCPServer, Required: true,
			InputDigest: inputDigest, SurfaceRefs: surface,
		}},
		DeclaredSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "protected-runtime-mcp-fixture", BinaryDigest: strings.Repeat("9", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: entryID, Channel: agent.ToolChannelMCPServer, InputDigest: inputDigest}},
		ObservedSurface:  surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func protectedRuntimeMCPRealizations(t *testing.T, provider agent.Provider, qw QueuedWork, mode agent.PromptSessionMode) *agent.CapabilityRealizationRegistry {
	t.Helper()
	compiled := protectedRuntimeMCPCompiled(t, protectedRuntimeMCPTestCapability, provider, qw, mode)
	registry, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func protectedRuntimeMCPPiCompiled(t *testing.T, capability string, provider agent.Provider, mode agent.PromptSessionMode, delivery agent.ExtensionDelivery) agent.CompiledCapabilityRealization {
	t.Helper()
	harness := provider.(agent.HarnessProvider)
	profile, ok := harness.Manifest().ToolLifecycleProfile(mode)
	if !ok {
		t.Fatal("pi test harness has no tool lifecycle profile")
	}
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "fixture_author"}}
	inputDigest := agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery})
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID: capability, HarnessID: harness.Manifest().Name,
		AdapterVersion: profile.ID, Mode: mode, RecipeID: "example/plugin-authoring/v1",
		Entries: []agent.CapabilityRecipeEntry{{
			EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true,
			InputDigest: inputDigest, SurfaceRefs: surface,
		}},
		DeclaredSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "plugin-authoring-fixture", BinaryDigest: strings.Repeat("8", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: inputDigest}},
		ObservedSurface:  surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

type protectedRuntimeMCPMixedFixture struct {
	registry           *Registry
	realizations       *agent.CapabilityRealizationRegistry
	selector           ProtectedRuntimeMCPSelector
	decorate           agent.ExtensionDecorator
	piProvider         *manifestSelectorProvider
	codexProvider      *manifestSelectorProvider
	piRow              agent.CompiledCapabilityRealization
	codexHumanRow      agent.CompiledCapabilityRealization
	codexAutonomousRow agent.CompiledCapabilityRealization
}

func newProtectedRuntimeMCPMixedFixture(t *testing.T) protectedRuntimeMCPMixedFixture {
	t.Helper()
	const capability = "example.generic-authoring/v1"
	piBase := &selectorFakeProvider{name: agent.ProviderPi, harness: agent.HarnessPi}
	piProvider := &manifestSelectorProvider{selectorFakeProvider: piBase, manifest: piManifestForTest(), capabilities: piCapabilitiesForTest()}
	codexBase := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	codexProvider := &manifestSelectorProvider{selectorFakeProvider: codexBase, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest()}
	registry := NewRegistry()
	for _, provider := range []agent.Provider{piProvider, codexProvider} {
		if err := registry.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	delivery := additionalExtensionDeliveryForTest("fixture-authoring-pack", "register fixture authoring tool")
	decorate := func(spec agent.Spec) []agent.ExtensionDelivery {
		if spec.Model == "gpt-pi-mixed-selector" {
			return []agent.ExtensionDelivery{delivery}
		}
		return nil
	}
	seed := QueuedWork{PlatformURL: "https://platform.example", McpAuthToken: "session-bearer"}
	seed.SessionID = "mixed-selector-seed"
	piRow := protectedRuntimeMCPPiCompiled(t, capability, piProvider, agent.PromptModeHumanControlled, delivery)
	codexHumanRow := protectedRuntimeMCPCompiled(t, capability, codexProvider, seed, agent.PromptModeHumanControlled)
	codexAutonomousRow := protectedRuntimeMCPCompiled(t, capability, codexProvider, seed, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{piRow, codexHumanRow, codexAutonomousRow})
	if err != nil {
		t.Fatal(err)
	}
	codexHumanProfile, _ := codexProvider.Manifest().ToolLifecycleProfile(agent.PromptModeHumanControlled)
	return protectedRuntimeMCPMixedFixture{
		registry: registry, realizations: realizations,
		selector: ProtectedRuntimeMCPSelector{
			CapabilityID: capability, HarnessID: agent.HarnessCodex,
			AdapterProfileID: codexHumanProfile.ID, Mode: agent.PromptModeHumanControlled,
		},
		decorate: decorate, piProvider: piProvider, codexProvider: codexProvider,
		piRow: piRow, codexHumanRow: codexHumanRow, codexAutonomousRow: codexAutonomousRow,
	}
}

func newProtectedRuntimeMCPProfileDriftFixture(t *testing.T) (protectedRuntimeMCPMixedFixture, *ProviderView) {
	t.Helper()
	fixture := newProtectedRuntimeMCPMixedFixture(t)
	driftManifest := codexManifestForTest()
	for i := range driftManifest.ToolLifecycle {
		if driftManifest.ToolLifecycle[i].Mode == agent.PromptModeHumanControlled {
			driftManifest.ToolLifecycle[i].ID = "codex/interactive/tool-lifecycle-drift"
		}
	}
	driftBase := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	driftProvider := &manifestSelectorProvider{selectorFakeProvider: driftBase, manifest: driftManifest, capabilities: codexCapabilitiesForTest()}
	registry := NewRegistry()
	for _, provider := range []agent.Provider{fixture.piProvider, driftProvider} {
		if err := registry.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	seed := QueuedWork{PlatformURL: "https://platform.example", McpAuthToken: "session-bearer"}
	seed.SessionID = "profile-drift-seed"
	driftRow := protectedRuntimeMCPCompiled(t, fixture.selector.CapabilityID, driftProvider, seed, agent.PromptModeHumanControlled)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{
		fixture.piRow, fixture.codexHumanRow, fixture.codexAutonomousRow, driftRow,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.registry = registry
	fixture.realizations = realizations
	fixture.codexProvider = driftProvider
	view, err := NewProviderViewWithProtectedRuntimeMCP(registry, fixture.decorate, realizations, nil, fixture.selector)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, view
}

func protectedRuntimeMCPMixedPiWork(t *testing.T, capability string) QueuedWork {
	t.Helper()
	const model = "gpt-pi-mixed-selector"
	qw := QueuedWork{}
	qw.SessionID = "mixed-selector-pi"
	qw.IssueIdentifier = "TEST-PI"
	qw.Body = "mixed selector pi path"
	qw.Mode = interactiveRunMode
	qw.InitialPrompt = "pi interactive fixture"
	qw.PlatformURL = "https://platform.example"
	qw.McpAuthToken = "session-bearer"
	qw.ResolvedProfile = ResolvedProfile{
		Harness: string(agent.HarnessPi), Model: model,
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Model: model, Protocol: agent.ProtoOpenAIChat, Host: agent.HostDirect,
			EndpointID: "endpoint:openai/direct", EndpointOperator: "vercel", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
			AuthBindingID: "auth-binding:byok", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
			AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
			AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
		},
	}
	return attachAdmittedExecutionCell(t, qw, piReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, []executioncell.CapabilityRequirement{{Name: capability}}))
}

func protectedRuntimeMCPMixedCodexWork(t *testing.T, capability string, mode executioncell.SessionMode) QueuedWork {
	t.Helper()
	qw := exactReceiptQueuedWork("mixed-selector-codex-" + string(mode))
	qw.IssueIdentifier = "TEST-CODEX"
	qw.Body = "mixed selector codex path"
	qw.PlatformURL = "https://platform.example/"
	qw.McpAuthToken = "session-bearer"
	if mode == executioncell.SessionHumanControlled {
		qw.Mode = interactiveRunMode
		qw.InitialPrompt = "codex interactive fixture"
	}
	var capabilities []executioncell.CapabilityRequirement
	if capability != "" {
		capabilities = []executioncell.CapabilityRequirement{{Name: capability}}
	}
	return attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", mode, capabilities))
}

func protectedRuntimeMCPPreflightInput(t *testing.T, qw QueuedWork) (QueuedWork, json.RawMessage) {
	t.Helper()
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	detail := rawJSONForRunner(t, map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID,
		"platformUrl": qw.PlatformURL, "mcpAuthToken": qw.McpAuthToken,
		"admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell,
		"executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload,
	})
	return qw, detail
}

func protectedRuntimeMCPPreflightFixture(t *testing.T, includeCapability bool) (*ProviderView, json.RawMessage, QueuedWork, *manifestSelectorProvider, *agent.CapabilityRealizationRegistry) {
	t.Helper()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	manifestProvider := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest()}
	registry := NewRegistry()
	if err := registry.Register(manifestProvider); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("protected-runtime-mcp-session")
	qw.IssueIdentifier = "TEST-1"
	qw.Body = "verify protected runtime MCP acknowledgement"
	qw.PlatformURL = "https://platform.example/"
	qw.McpAuthToken = "session-bearer"
	var capabilities []executioncell.CapabilityRequirement
	if includeCapability {
		capabilities = []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}}
	}
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, capabilities))
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	detail := rawJSONForRunner(t, map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID,
		"platformUrl": qw.PlatformURL, "mcpAuthToken": qw.McpAuthToken,
		"admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell,
		"executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload,
	})
	realizations := protectedRuntimeMCPRealizations(t, manifestProvider, qw, agent.PromptModeAutonomous)
	view, err := NewProviderViewWithProtectedRuntimeMCP(registry, nil, realizations, nil, protectedRuntimeMCPTestSelector(t, manifestProvider, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	return view, detail, qw, manifestProvider, realizations
}

func TestProtectedRuntimeMCPResolverIsStrictlyCapabilitySelected(t *testing.T) {
	for _, includeCapability := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary session", true: "exact selected realization"}[includeCapability], func(t *testing.T) {
			view, detail, _, _, _ := protectedRuntimeMCPPreflightFixture(t, includeCapability)
			receipt, err := view.PreflightExecution(detail)
			if err != nil {
				t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
			}
			requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
			if err != nil {
				t.Fatalf("ResolveExecutionPreflightProtectedRuntimeMCPRequirements: %v", err)
			}
			if includeCapability && len(requirements) != 1 {
				t.Fatalf("selected requirements = %+v, want one", requirements)
			}
			if !includeCapability && requirements != nil {
				t.Fatalf("ordinary session requirements = %+v, want nil", requirements)
			}
		})
	}
}

func TestProtectedRuntimeMCPExactSelectorPreservesMixedHarnessPreflight(t *testing.T) {
	fixture := newProtectedRuntimeMCPMixedFixture(t)
	baseline := NewProviderViewWithDecoratorAndRealizations(fixture.registry, fixture.decorate, fixture.realizations)
	selected, err := NewProviderViewWithProtectedRuntimeMCP(fixture.registry, fixture.decorate, fixture.realizations, nil, fixture.selector)
	if err != nil {
		t.Fatal(err)
	}

	piWork, piDetail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedPiWork(t, fixture.selector.CapabilityID))
	piBaseline, err := baseline.PreflightExecution(piDetail)
	if err != nil {
		t.Fatalf("baseline Pi preflight: %v", err)
	}
	piSelected, err := selected.PreflightExecution(piDetail)
	if err != nil {
		t.Fatalf("selected Pi preflight: %v", err)
	}
	if string(piSelected) != string(piBaseline) {
		t.Fatalf("Codex selector changed Pi receipt bytes\nselected=%s\nbaseline=%s", piSelected, piBaseline)
	}
	piHost, err := executioncell.DecodeHostAdaptationReceipt(piSelected)
	if err != nil || piHost.ContractVersion != executioncell.HostAdaptationContractVersion {
		t.Fatalf("Pi host receipt = %+v err=%v", piHost, err)
	}
	piRequirements, err := selected.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(piDetail, piSelected)
	if err != nil || piRequirements != nil {
		t.Fatalf("Pi protected requirements = %+v err=%v", piRequirements, err)
	}
	_ = piWork

	codexHuman, codexHumanDetail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedCodexWork(t, fixture.selector.CapabilityID, executioncell.SessionHumanControlled))
	codexHumanReceipt, err := selected.PreflightExecution(codexHumanDetail)
	if err != nil {
		t.Fatalf("Codex human preflight: %v", err)
	}
	codexRequirements, err := selected.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(codexHumanDetail, codexHumanReceipt)
	if err != nil || len(codexRequirements) != 1 {
		t.Fatalf("Codex human requirements = %+v err=%v", codexRequirements, err)
	}
	if codexRequirements[0].OperationalPayloadDigest != mustAdmissionReceipt(t, codexHuman.AdmissionReceipt).Value().OperationalPayloadDigest {
		t.Fatal("Codex protected requirement lost admission payload binding")
	}

	_, codexAutonomousDetail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedCodexWork(t, fixture.selector.CapabilityID, executioncell.SessionAutonomous))
	codexAutonomousReceipt, err := selected.PreflightExecution(codexAutonomousDetail)
	if err != nil {
		t.Fatalf("Codex autonomous preflight: %v", err)
	}
	autonomousRequirements, err := selected.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(codexAutonomousDetail, codexAutonomousReceipt)
	if err != nil || autonomousRequirements != nil {
		t.Fatalf("Codex autonomous protected requirements = %+v err=%v", autonomousRequirements, err)
	}
	autonomousHost, err := executioncell.DecodeHostAdaptationReceipt(codexAutonomousReceipt)
	if err != nil || autonomousHost.ContractVersion != executioncell.HostAdaptationContractVersion {
		t.Fatalf("Codex autonomous receipt = %+v err=%v", autonomousHost, err)
	}
}

func runProtectedRuntimeMCPMixedChild(t *testing.T, fixture protectedRuntimeMCPMixedFixture, qw QueuedWork) (*Result, error) {
	t.Helper()
	server := mockPlatformServer(t)
	t.Cleanup(server.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: server.URL, WorkerID: qw.WorkerID, AuthToken: "worker-token",
		HTTPClient: server.Client(), BaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(Options{
		Registry: fixture.registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(),
		AdditionalExtensionDecorator: fixture.decorate, CapabilityRealizations: fixture.realizations,
		ProtectedRuntimeMCPSelector: fixture.selector,
		SkipBackstop:                true, SkipSteering: true, SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := fixture.registry.PreflightHarness(qw, fixture.realizations)
	if err != nil {
		return nil, err
	}
	return run.runLoop(context.Background(), qw, time.Now().UnixMilli(), admission)
}

func TestProtectedRuntimeMCPExactSelectorPreservesPiAndEnforcesCodexChild(t *testing.T) {
	t.Run("same capability Pi plugin remains legacy", func(t *testing.T) {
		fixture := newProtectedRuntimeMCPMixedFixture(t)
		view, err := NewProviderViewWithProtectedRuntimeMCP(fixture.registry, fixture.decorate, fixture.realizations, nil, fixture.selector)
		if err != nil {
			t.Fatal(err)
		}
		qw, detail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedPiWork(t, fixture.selector.CapabilityID))
		receipt, err := view.PreflightExecution(detail)
		if err != nil {
			t.Fatal(err)
		}
		qw.HostAdaptationReceipt = receipt
		got, runErr := runProtectedRuntimeMCPMixedChild(t, fixture, qw)
		if runErr == nil || got.Status != "failed" || fixture.piProvider.spawnCalls.Load() != 1 {
			t.Fatalf("Pi child result=%+v err=%v spawnCalls=%d", got, runErr, fixture.piProvider.spawnCalls.Load())
		}
	})

	t.Run("targeted Codex realization requires and consumes v3", func(t *testing.T) {
		fixture := newProtectedRuntimeMCPMixedFixture(t)
		view, err := NewProviderViewWithProtectedRuntimeMCP(fixture.registry, fixture.decorate, fixture.realizations, nil, fixture.selector)
		if err != nil {
			t.Fatal(err)
		}
		qw, detail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedCodexWork(t, fixture.selector.CapabilityID, executioncell.SessionHumanControlled))
		receipt, err := view.PreflightExecution(detail)
		if err != nil {
			t.Fatal(err)
		}
		requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
		if err != nil || len(requirements) != 1 {
			t.Fatalf("Codex requirements=%+v err=%v", requirements, err)
		}
		qw.HostAdaptationReceipt = attachProtectedRuntimeMCPMaterialization(t, receipt, requirements[0])
		got, runErr := runProtectedRuntimeMCPMixedChild(t, fixture, qw)
		if runErr == nil || got.Status != "failed" || fixture.codexProvider.spawnCalls.Load() != 1 {
			t.Fatalf("Codex child result=%+v err=%v spawnCalls=%d", got, runErr, fixture.codexProvider.spawnCalls.Load())
		}
	})

	t.Run("same Codex capability in autonomous mode remains legacy", func(t *testing.T) {
		fixture := newProtectedRuntimeMCPMixedFixture(t)
		view, err := NewProviderViewWithProtectedRuntimeMCP(fixture.registry, fixture.decorate, fixture.realizations, nil, fixture.selector)
		if err != nil {
			t.Fatal(err)
		}
		qw, detail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedCodexWork(t, fixture.selector.CapabilityID, executioncell.SessionAutonomous))
		receipt, err := view.PreflightExecution(detail)
		if err != nil {
			t.Fatal(err)
		}
		qw.HostAdaptationReceipt = receipt
		got, runErr := runProtectedRuntimeMCPMixedChild(t, fixture, qw)
		if runErr == nil || got.Status != "failed" || fixture.codexProvider.spawnCalls.Load() != 1 {
			t.Fatalf("Codex autonomous child result=%+v err=%v spawnCalls=%d", got, runErr, fixture.codexProvider.spawnCalls.Load())
		}
	})
}

func TestProtectedRuntimeMCPExactSelectorRefusesProfileDriftAndUnselectedV3(t *testing.T) {
	t.Run("target adapter profile drift", func(t *testing.T) {
		fixture, view := newProtectedRuntimeMCPProfileDriftFixture(t)
		_, detail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedCodexWork(t, fixture.selector.CapabilityID, executioncell.SessionHumanControlled))
		receipt, err := view.PreflightExecution(detail)
		if err != nil {
			t.Fatalf("drift preflight compile: %v", err)
		}
		if _, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt); err == nil || !strings.Contains(err.Error(), "target adapter profile changed") {
			t.Fatalf("profile drift requirement error = %v", err)
		}
	})

	t.Run("profile drift without granted capability remains legacy", func(t *testing.T) {
		fixture, view := newProtectedRuntimeMCPProfileDriftFixture(t)
		baseline := NewProviderViewWithDecoratorAndRealizations(fixture.registry, fixture.decorate, fixture.realizations)
		qw, detail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedCodexWork(t, "", executioncell.SessionHumanControlled))
		baselineReceipt, err := baseline.PreflightExecution(detail)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := view.PreflightExecution(detail)
		if err != nil {
			t.Fatalf("capability-absent drift preflight: %v", err)
		}
		if string(receipt) != string(baselineReceipt) {
			t.Fatal("capability-absent profile drift changed legacy receipt bytes")
		}
		requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
		if err != nil || requirements != nil {
			t.Fatalf("capability-absent profile drift requirements=%+v err=%v", requirements, err)
		}
		qw.HostAdaptationReceipt = receipt
		got, runErr := runProtectedRuntimeMCPMixedChild(t, fixture, qw)
		if runErr == nil || got.Status != "failed" || fixture.codexProvider.spawnCalls.Load() != 1 {
			t.Fatalf("capability-absent drift child result=%+v err=%v spawnCalls=%d", got, runErr, fixture.codexProvider.spawnCalls.Load())
		}
	})

	t.Run("carried v3 on unselected Pi child", func(t *testing.T) {
		fixture := newProtectedRuntimeMCPMixedFixture(t)
		view, err := NewProviderViewWithProtectedRuntimeMCP(fixture.registry, fixture.decorate, fixture.realizations, nil, fixture.selector)
		if err != nil {
			t.Fatal(err)
		}
		qw, detail := protectedRuntimeMCPPreflightInput(t, protectedRuntimeMCPMixedPiWork(t, fixture.selector.CapabilityID))
		receipt, err := view.PreflightExecution(detail)
		if err != nil {
			t.Fatal(err)
		}
		admission := mustAdmissionReceipt(t, qw.AdmissionReceipt).Value()
		requirement := executioncell.ProtectedRuntimeMCPConfigRequirementV1{
			ContractVersion: executioncell.ProtectedRuntimeMCPConfigContractVersion,
			RequirementID:   "protected-runtime-mcp/v1", AuthorityBindingDigest: strings.Repeat("a", 64),
			OperationalPayloadDigest: admission.OperationalPayloadDigest,
			ServerName:               "fixture-platform", Transport: executioncell.ProtectedRuntimeMCPTransportHTTP,
			EndpointDigest: strings.Repeat("b", 64),
			Headers:        []executioncell.ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: strings.Repeat("c", 64)}},
		}
		qw.HostAdaptationReceipt = attachProtectedRuntimeMCPMaterialization(t, receipt, requirement)
		got, runErr := runProtectedRuntimeMCPMixedChild(t, fixture, qw)
		if runErr == nil || got.Status != "failed" || fixture.piProvider.spawnCalls.Load() != 0 || !strings.Contains(runErr.Error(), "unselected realization") {
			t.Fatalf("unselected v3 result=%+v err=%v spawnCalls=%d", got, runErr, fixture.piProvider.spawnCalls.Load())
		}
	})
}

func attachProtectedRuntimeMCPMaterialization(t *testing.T, receipt json.RawMessage, requirement executioncell.ProtectedRuntimeMCPConfigRequirementV1) json.RawMessage {
	t.Helper()
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	materialization := executioncell.ProtectedRuntimeMCPConfigMaterializationV1{
		ContractVersion: requirement.ContractVersion, RequirementID: requirement.RequirementID,
		AuthorityBindingDigest: requirement.AuthorityBindingDigest, OperationalPayloadDigest: requirement.OperationalPayloadDigest,
		ServerName: requirement.ServerName, Transport: requirement.Transport, EndpointDigest: requirement.EndpointDigest,
		Headers: append([]executioncell.ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...),
	}
	materialization.ConfigReferenceDigest, err = executioncell.DigestProtectedRuntimeMCPConfigReference(materialization)
	if err != nil {
		t.Fatal(err)
	}
	host.ContractVersion = executioncell.HostAdaptationV3ContractVersion
	host.ProtectedRuntimeMCPConfigs = []executioncell.ProtectedRuntimeMCPConfigMaterializationV1{materialization}
	raw, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executioncell.DecodeHostAdaptationReceipt(raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProtectedRuntimeMCPChildRefusesDowngradeAndRotatedBearerBeforeSpawn(t *testing.T) {
	for _, tc := range []struct {
		name                string
		mutate              func(*QueuedWork)
		wantPreflightDenial bool
	}{
		{name: "selected receipt missing", mutate: func(qw *QueuedWork) { qw.HostAdaptationReceipt = nil }, wantPreflightDenial: true},
		{name: "v1 attachment removed", mutate: func(qw *QueuedWork) {
			host, err := executioncell.DecodeHostAdaptationReceipt(qw.HostAdaptationReceipt)
			if err != nil {
				t.Fatal(err)
			}
			host.ContractVersion = executioncell.HostAdaptationContractVersion
			host.ProtectedRuntimeMCPConfigs = nil
			qw.HostAdaptationReceipt, _ = json.Marshal(host)
		}},
		{name: "bearer rotated after materialization", mutate: func(qw *QueuedWork) { qw.McpAuthToken = "rotated-bearer" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, detail, qw, provider, realizations := protectedRuntimeMCPPreflightFixture(t, true)
			receipt, err := view.PreflightExecution(detail)
			if err != nil {
				t.Fatal(err)
			}
			requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
			if err != nil || len(requirements) != 1 {
				t.Fatalf("requirements = %+v err=%v", requirements, err)
			}
			qw.HostAdaptationReceipt = attachProtectedRuntimeMCPMaterialization(t, receipt, requirements[0])
			tc.mutate(&qw)

			server := mockPlatformServer(t)
			defer server.Close()
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: qw.WorkerID, AuthToken: "worker-token", HTTPClient: server.Client(), BaseDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			registry := NewRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			run, err := New(Options{
				Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(),
				CapabilityRealizations: realizations, ProtectedRuntimeMCPSelector: protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous),
				SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			admission, err := registry.PreflightHarness(qw, realizations)
			if tc.wantPreflightDenial {
				if err == nil || provider.spawnCalls.Load() != 0 {
					t.Fatalf("missing selected receipt admission=%+v err=%v spawnCalls=%d", admission, err, provider.spawnCalls.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := run.runLoop(context.Background(), qw, time.Now().UnixMilli(), admission)
			if runErr == nil || result.Status != "failed" || provider.spawnCalls.Load() != 0 {
				t.Fatalf("run result=%+v err=%v spawnCalls=%d", result, runErr, provider.spawnCalls.Load())
			}
		})
	}
}

func TestProtectedRuntimeMCPAbsentSelectorPreservesLegacyNonReceiptRun(t *testing.T) {
	harness := newRunnerHarness(t)
	qw := harness.queuedWork("REN-PROTECTED-MCP-LEGACY")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := harness.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("legacy non-receipt Run: %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Fatalf("legacy non-receipt result = %+v", got)
	}
}

func TestProtectedRuntimeMCPCarriedV3RefusesMissingChildSelectorBeforeSpawn(t *testing.T) {
	view, detail, qw, provider, realizations := protectedRuntimeMCPPreflightFixture(t, true)
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
	if err != nil || len(requirements) != 1 {
		t.Fatalf("requirements = %+v err=%v", requirements, err)
	}
	qw.HostAdaptationReceipt = attachProtectedRuntimeMCPMaterialization(t, receipt, requirements[0])

	server := mockPlatformServer(t)
	defer server.Close()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: qw.WorkerID, AuthToken: "worker-token", HTTPClient: server.Client(), BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	run, err := New(Options{
		Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(),
		CapabilityRealizations: realizations,
		SkipBackstop:           true, SkipSteering: true, SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := registry.PreflightHarness(qw, realizations)
	if err != nil {
		t.Fatal(err)
	}
	got, runErr := run.runLoop(context.Background(), qw, time.Now().UnixMilli(), admission)
	if runErr == nil || got.Status != "failed" || provider.spawnCalls.Load() != 0 {
		t.Fatalf("run result=%+v err=%v spawnCalls=%d", got, runErr, provider.spawnCalls.Load())
	}
}

func TestProtectedRuntimeMCPSelectorRefusesMalformedOrUnregisteredValues(t *testing.T) {
	if _, err := NewProviderViewWithProtectedRuntimeMCP(NewRegistry(), nil, nil, nil, ProtectedRuntimeMCPSelector{CapabilityID: "incomplete"}); err == nil {
		t.Fatal("incomplete selector accepted")
	}
	unregistered := ProtectedRuntimeMCPSelector{CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: agent.HarnessCodex, AdapterProfileID: "codex/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous}
	if _, err := NewProviderViewWithProtectedRuntimeMCP(NewRegistry(), nil, nil, nil, unregistered); err == nil {
		t.Fatal("unregistered selector accepted")
	}
}

func TestProtectedRuntimeMCPV2ResolverBindsExactCommonRequirement(t *testing.T) {
	legacyView, detail, qw, provider, _ := protectedRuntimeMCPPreflightFixture(t, true)
	retainedV1, err := legacyView.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	base := protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous)
	const profileID = "codex/headless/tool-lifecycle-v2-fixture"
	manifest := provider.manifest
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	profile.ID = profileID
	profile.EvidenceTier = "native_verified"
	profile.ProductionEligible = true
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, profile)
	provider.manifest = manifest
	compiled := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, qw, profileID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	selector := ProtectedRuntimeMCPV2Selector{
		CapabilityID: base.CapabilityID, HarnessID: base.HarnessID,
		AdapterProfileID: profileID, Mode: base.Mode,
		ConfigRequirementID: "example.session-config/v1",
	}
	view, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPV2Selector: selector,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ValidateRetainedExecution(detail, retainedV1); err == nil || !strings.Contains(err.Error(), "toolLifecyclePlan") {
		t.Fatalf("targeted retained v1 receipt was not explicitly refused: %v", err)
	}
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var toolReceipt agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &toolReceipt); err != nil || toolReceipt.ProfileID != profileID {
		t.Fatalf("tool receipt=%+v err=%v", toolReceipt, err)
	}
	v1, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
	if err != nil || v1 != nil {
		t.Fatalf("v1 requirements = %+v err=%v, want nil", v1, err)
	}
	v2, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2(detail, receipt)
	if err != nil || len(v2) != 1 {
		t.Fatalf("v2 requirements = %+v err=%v", v2, err)
	}
	requirement := v2[0]
	if requirement.ContractVersion != executioncell.ProtectedRuntimeMCPConfigContractVersionV2 ||
		requirement.RequirementID != protectedRuntimeMCPRequirementIDV2 ||
		requirement.AuthorizationSource != (executioncell.ProtectedRuntimeMCPAuthorizationSourceV2{
			Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, ConfigRequirementID: selector.ConfigRequirementID,
			TargetEnv: executioncell.PreflightConfigSessionMCPBearerFileTarget, Mode: executioncell.PreflightConfigPrivateFileMode,
		}) {
		t.Fatalf("v2 requirement = %+v", requirement)
	}
	if err := executioncell.ValidateProtectedRuntimeMCPConfigRequirementV2(requirement); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedRuntimeMCPV2ProfileIntentLeavesUntargetedSessionOnV1(t *testing.T) {
	legacyView, detail, qw, provider, _ := protectedRuntimeMCPPreflightFixture(t, false)
	base := protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous)
	const profileID = "codex/headless/tool-lifecycle-v2-fixture"
	manifest := provider.manifest
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	profile.ID, profile.EvidenceTier, profile.ProductionEligible = profileID, "native_verified", true
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, profile)
	provider.manifest = manifest
	compiled := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, qw, profileID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	selector := ProtectedRuntimeMCPV2Selector{
		CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: base.HarnessID,
		AdapterProfileID: profileID, Mode: base.Mode, ConfigRequirementID: "example.session-config/v1",
	}
	if bound := BindProtectedRuntimeMCPV2ProfileIntent(qw, selector); bound.toolLifecycleProfileID != "" {
		t.Fatalf("untargeted session selected profile %q", bound.toolLifecycleProfileID)
	}
	view, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPV2Selector: selector,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var toolReceipt agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &toolReceipt); err != nil || toolReceipt.ProfileID != base.AdapterProfileID {
		t.Fatalf("untargeted tool receipt=%+v err=%v", toolReceipt, err)
	}
	if requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2(detail, receipt); err != nil || requirements != nil {
		t.Fatalf("untargeted V2 requirements=%+v err=%v", requirements, err)
	}
}

func TestProtectedRuntimeMCPV2SelectorRefusesAmbiguousOrInvalidConfiguration(t *testing.T) {
	legacyView, _, _, provider, realizations := protectedRuntimeMCPPreflightFixture(t, true)
	base := protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous)
	v2 := ProtectedRuntimeMCPV2Selector{
		CapabilityID: base.CapabilityID, HarnessID: base.HarnessID,
		AdapterProfileID: base.AdapterProfileID, Mode: base.Mode,
		ConfigRequirementID: "example.session-config/v1",
	}
	if _, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPSelector: base, ProtectedRuntimeMCPV2Selector: v2,
	}); err == nil {
		t.Fatal("ambiguous v1/v2 selectors accepted")
	}
	v2.ConfigRequirementID = "bad requirement id"
	if _, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPV2Selector: v2,
	}); err == nil {
		t.Fatal("invalid v2 config requirement ID accepted")
	}
}

func TestProtectedRuntimeMCPV2RefusesLegacyToolLifecycleProfile(t *testing.T) {
	legacyView, detail, _, provider, realizations := protectedRuntimeMCPPreflightFixture(t, true)
	base := protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous)
	selector := ProtectedRuntimeMCPV2Selector{
		CapabilityID: base.CapabilityID, HarnessID: base.HarnessID,
		AdapterProfileID: base.AdapterProfileID, Mode: base.Mode,
		ConfigRequirementID: "example.session-config/v1",
	}
	view, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPV2Selector: selector,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2(detail, receipt); err == nil || !strings.Contains(err.Error(), "native-verified") {
		t.Fatalf("legacy profile V2 resolution err=%v", err)
	}
}

func TestApplyProtectedRuntimeMCPV2JoinsActualFileAndAttachesPrivateHelper(t *testing.T) {
	legacyView, detail, qw, provider, _ := protectedRuntimeMCPPreflightFixture(t, true)
	base := protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous)
	const profileID = "codex/headless/tool-lifecycle-v2-fixture"
	manifest := provider.manifest
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	profile.ID, profile.EvidenceTier, profile.ProductionEligible = profileID, "native_verified", true
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, profile)
	provider.manifest = manifest
	compiled := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, qw, profileID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	selector := ProtectedRuntimeMCPV2Selector{
		CapabilityID: base.CapabilityID, HarnessID: base.HarnessID,
		AdapterProfileID: profileID, Mode: base.Mode,
		ConfigRequirementID: "example.session-config/v1",
	}
	view, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPV2Selector: selector,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2(detail, receipt)
	if err != nil || len(requirements) != 1 {
		t.Fatalf("requirements=%+v err=%v", requirements, err)
	}

	path := filepath.Join(t.TempDir(), "mcp-token")
	if err := os.WriteFile(path, []byte(qw.McpAuthToken), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := canonicalProtectedRuntimeMCPHelperCommand(path)
	if err != nil {
		t.Fatal(err)
	}
	common := executioncell.PreflightConfigMaterializationV1{
		ContractVersion: executioncell.PreflightConfigMaterializationContractVersion,
		RequirementID:   selector.ConfigRequirementID, AuthorityBindingDigest: strings.Repeat("a", 64),
		OperationalPayloadDigest: requirements[0].OperationalPayloadDigest,
		Bindings: []executioncell.PreflightConfigBindingMaterializationV1{{
			TargetEnv:           executioncell.PreflightConfigSessionMCPBearerFileTarget,
			Source:              executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: executioncell.PreflightConfigPrivateFileMode},
			BearerContentDigest: digestProtectedRuntimeValue(qw.McpAuthToken), FileReferenceDigest: digestProtectedRuntimeValue(path),
			Mode: executioncell.PreflightConfigPrivateFileMode,
		}},
	}
	common.ConfigReferenceDigest, err = executioncell.DigestPreflightConfigReference(common)
	if err != nil {
		t.Fatal(err)
	}
	materialization := executioncell.ProtectedRuntimeMCPConfigMaterializationV2{
		ContractVersion: requirements[0].ContractVersion, RequirementID: requirements[0].RequirementID,
		AuthorityBindingDigest: requirements[0].AuthorityBindingDigest, OperationalPayloadDigest: requirements[0].OperationalPayloadDigest,
		ServerName: requirements[0].ServerName, Transport: requirements[0].Transport,
		EndpointDigest: requirements[0].EndpointDigest, Headers: requirements[0].Headers,
		AuthorizationSource: executioncell.ProtectedRuntimeMCPAuthorizationSourceMaterializationV2{
			Kind: requirements[0].AuthorizationSource.Kind, ConfigRequirementID: selector.ConfigRequirementID,
			TargetEnv: requirements[0].AuthorizationSource.TargetEnv, Mode: requirements[0].AuthorizationSource.Mode,
			ConfigReferenceDigest: common.ConfigReferenceDigest, FileReferenceDigest: digestProtectedRuntimeValue(path),
			HelperCommandDigest: digestProtectedRuntimeValue(command),
		},
	}
	materialization.ConfigReferenceDigest, err = executioncell.DigestProtectedRuntimeMCPConfigReferenceV2(materialization)
	if err != nil {
		t.Fatal(err)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	host.ContractVersion = executioncell.HostAdaptationV3ContractVersion
	host.ConfigMaterializations = []executioncell.PreflightConfigMaterializationV1{common}
	host.ProtectedRuntimeMCPConfigsV2 = []executioncell.ProtectedRuntimeMCPConfigMaterializationV2{materialization}
	qw.HostAdaptationReceipt = rawJSONForRunner(t, host)
	qw = BindProtectedRuntimeMCPV2ProfileIntent(qw, selector)
	admission, err := legacyView.reg.PreflightHarness(qw, realizations)
	if err != nil {
		t.Fatal(err)
	}
	servers := defaultMCPServersForHarness(qw, "/tmp/worktree", provider, agent.PromptModeAutonomous)
	got, err := applyProtectedRuntimeMCPV2(qw, admission.selection, realizations, selector, servers, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Headers) != 0 {
		t.Fatalf("servers=%+v", got)
	}
	if helper, ok := agent.ProtectedRuntimeMCPHeadersHelper(got[0]); !ok || helper != command {
		t.Fatalf("helper=%q ok=%v", helper, ok)
	}
	if _, ok := agent.ProtectedRuntimeMCPHeadersHelper(servers[0]); ok || servers[0].Headers["Authorization"] == "" {
		t.Fatal("input slice mutated")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*executioncell.HostAdaptationReceipt, *[]agent.MCPServerConfig, *string)
	}{
		{"missing common", func(h *executioncell.HostAdaptationReceipt, _ *[]agent.MCPServerConfig, _ *string) {
			h.ConfigMaterializations = nil
		}},
		{"changed file path", func(_ *executioncell.HostAdaptationReceipt, _ *[]agent.MCPServerConfig, p *string) { *p += ".changed" }},
		{"duplicate server", func(_ *executioncell.HostAdaptationReceipt, s *[]agent.MCPServerConfig, _ *string) {
			*s = append(*s, (*s)[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changedHost := host
			changedServers := append([]agent.MCPServerConfig(nil), servers...)
			changedPath := path
			tc.mutate(&changedHost, &changedServers, &changedPath)
			changedWork := qw
			changedWork.HostAdaptationReceipt = rawJSONForRunner(t, changedHost)
			if _, err := applyProtectedRuntimeMCPV2(changedWork, admission.selection, realizations, selector, changedServers, changedPath); err == nil {
				t.Fatal("changed authority accepted")
			}
		})
	}
}
