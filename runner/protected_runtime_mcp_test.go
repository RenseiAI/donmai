package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

const protectedRuntimeMCPTestCapability = "example.protected-mcp/v1"

func protectedRuntimeMCPRealizations(t *testing.T, provider agent.Provider, qw QueuedWork, mode agent.PromptSessionMode) *agent.CapabilityRealizationRegistry {
	t.Helper()
	harness := provider.(agent.HarnessProvider)
	profile, ok := harness.Manifest().ToolLifecycleProfile(mode)
	if !ok {
		t.Fatal("test harness has no tool lifecycle profile")
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
		CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: harness.Manifest().Name,
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
	registry, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func protectedRuntimeMCPPreflightFixture(t *testing.T, includeCapability bool) (*ProviderView, json.RawMessage, QueuedWork) {
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
	view, err := NewProviderViewWithProtectedRuntimeMCP(registry, nil, realizations, nil, protectedRuntimeMCPTestCapability)
	if err != nil {
		t.Fatal(err)
	}
	return view, detail, qw
}

func TestProtectedRuntimeMCPResolverIsStrictlyCapabilitySelected(t *testing.T) {
	for _, includeCapability := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary session", true: "exact selected realization"}[includeCapability], func(t *testing.T) {
			view, detail, _ := protectedRuntimeMCPPreflightFixture(t, includeCapability)
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

func TestProtectedRuntimeMCPSelectorRefusesMalformedOrUnregisteredValues(t *testing.T) {
	if _, err := NewProviderViewWithProtectedRuntimeMCP(NewRegistry(), nil, nil, nil, " malformed "); err == nil {
		t.Fatal("malformed selector accepted")
	}
	if _, err := NewProviderViewWithProtectedRuntimeMCP(NewRegistry(), nil, nil, nil, protectedRuntimeMCPTestCapability); err == nil {
		t.Fatal("unregistered selector accepted")
	}
}
