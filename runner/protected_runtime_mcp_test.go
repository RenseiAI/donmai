package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
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
	view, err := NewProviderViewWithProtectedRuntimeMCP(registry, nil, realizations, nil, protectedRuntimeMCPTestCapability)
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
				CapabilityRealizations: realizations, ProtectedRuntimeMCPCapability: protectedRuntimeMCPTestCapability,
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

func TestProtectedRuntimeMCPSelectorRefusesMalformedOrUnregisteredValues(t *testing.T) {
	if _, err := NewProviderViewWithProtectedRuntimeMCP(NewRegistry(), nil, nil, nil, " malformed "); err == nil {
		t.Fatal("malformed selector accepted")
	}
	if _, err := NewProviderViewWithProtectedRuntimeMCP(NewRegistry(), nil, nil, nil, protectedRuntimeMCPTestCapability); err == nil {
		t.Fatal("unregistered selector accepted")
	}
}
