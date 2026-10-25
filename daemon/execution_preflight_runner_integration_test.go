package daemon_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	providerpi "github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/runner"
)

func TestActualProviderViewReceiptPassesV2RegistrationBeforeCredential(t *testing.T) {
	registry := runner.NewRegistry()
	if err := registry.Register(&providerpi.Provider{}); err != nil {
		t.Fatal(err)
	}
	extensionSource := []byte("register an example native tool")
	extensionDigest := sha256.Sum256(extensionSource)
	delivery := agent.ExtensionDelivery{ID: "example-extension", Kind: agent.ExtensionDeliveryInline, Source: extensionSource, Basename: "example.js", Digest: hex.EncodeToString(extensionDigest[:]), Required: true}
	capabilitySurface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "example_tool"}}
	capabilityInputDigest := agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery})
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID: "example.native/v1", HarnessID: agent.HarnessPi,
		AdapterVersion: "pi/interactive/tool-lifecycle-v6", Mode: agent.PromptModeHumanControlled,
		RecipeID:        "example/inline-extension/v1",
		Entries:         []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: capabilityInputDigest, SurfaceRefs: capabilitySurface}},
		DeclaredSurface: capabilitySurface,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "actual-provider-view", BinaryDigest: strings.Repeat("b", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: capabilityInputDigest}},
		ObservedSurface:  capabilitySurface,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	decorate := func(agent.Spec) []agent.ExtensionDelivery { return []agent.ExtensionDelivery{delivery} }
	const requestID = "actual-v2-provider-view"
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(agent.HarnessPi), Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "test-model", Author: "local"},
		Endpoint: executioncell.ServingEndpointRef{
			ID: "test-local", Protocol: string(agent.ProtoOpenAIResponses), Operator: "local", Revision: "r1",
		},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "stub-auth", Mechanism: executioncell.AuthNone, CommercialMode: executioncell.CommercialSelfHosted,
			Authority: "local", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryNone,
		},
		Placement:           executioncell.PlacementRef{ID: "host-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode:         executioncell.SessionHumanControlled,
		GrantedCapabilities: []executioncell.CapabilityRequirement{{Name: "example.native/v1", ParametersDigest: strings.Repeat("5", 64)}},
		EvidenceTier:        executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
	work := runner.QueuedWork{}
	work.SessionID = requestID
	work.WorkerID = "worker-local"
	work.Body = "compile the actual provider-view authority"
	work.Mode = prompt.InteractiveRunMode
	work.InitialPrompt = "compile the actual provider-view authority"
	work.ResolvedProfile = runner.ResolvedProfile{Harness: string(agent.HarnessPi), Model: "test-model", Endpoint: &agent.EndpointBinding{
		Company: "test", Model: "test-model", Protocol: agent.ProtoOpenAIResponses, Host: agent.HostDirect,
		EndpointID: "test-local", EndpointOperator: "local", EndpointRevision: "r1", ModelAuthor: "local",
		AuthBindingID: "stub-auth", AuthAuthority: "local", AuthCommercialMode: string(executioncell.CommercialSelfHosted),
		AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
		AuthDelivery: string(executioncell.DeliveryNone), Mechanism: agent.AuthNone, Auth: agent.AuthLocal, CostModel: agent.CostLocalFree,
	}}
	operationalDigest, err := runner.DigestOperationalPayload(work)
	if err != nil {
		t.Fatal(err)
	}
	admission := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-actual-v2", RequestID: requestID,
		Decision: executioncell.AdmissionAdmitted, IntentDigest: strings.Repeat("1", 64), OperationalPayloadDigest: operationalDigest,
		Cell: &cell, ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-09-10T00:00:00Z",
	}
	admissionRaw, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	immutableAdmission, err := executioncell.DecodeAdmissionReceipt(admissionRaw)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		t.Fatal(err)
	}
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       requestID, WorkerID: work.WorkerID, PlacementID: cell.Placement.ID,
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "actual-provider-view"},
	}
	bindingRaw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	operational, err := runner.CanonicalOperationalPayload(work)
	if err != nil {
		t.Fatal(err)
	}
	view := runner.NewProviderViewWithDecoratorAndRealizations(registry, decorate, realizations)
	preflightInput, err := json.Marshal(map[string]any{
		"sessionId": requestID, "workerId": work.WorkerID, "admissionReceipt": json.RawMessage(immutableAdmission.Bytes()),
		"effectiveCell": json.RawMessage(effective), "executionRuntimeBinding": json.RawMessage(bindingRaw), "operationalPayload": json.RawMessage(operational),
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, compileErr := view.PreflightExecution(preflightInput)
	if compileErr != nil {
		t.Fatalf("actual ProviderView preflight: %v receipt=%s", compileErr, receipt)
	}
	if replayErr := view.ValidateRetainedExecution(preflightInput, receipt); replayErr != nil {
		t.Fatalf("identical realization replay refused: %v", replayErr)
	}
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "daemon.yaml")
	if err := daemon.WriteConfig(configPath, &daemon.Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: daemon.ProjectAdmissionVersionV2,
		ProjectAdmissionMode: daemon.ProjectAdmissionModeAllRouted, Machine: daemon.MachineConfig{ID: "actual-provider-view"},
		Capacity:     daemon.CapacityConfig{MaxConcurrentSessions: 8},
		Orchestrator: daemon.OrchestratorConfig{URL: "https://example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	var credentials atomic.Int32
	marker := filepath.Join(tmp, "spawned")
	receiptStore := daemon.NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts"))
	d := daemon.New(daemon.Options{
		ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"), SkipWizard: true, SkipRegistration: true,
		ProviderRegistry:            view,
		ExecutionPreflightStore:     receiptStore,
		ExecutionPreflightRegistrar: daemon.NewFileExecutionPreflightRegistrar(filepath.Join(tmp, "registrations")),
		SpawnerOptions: daemon.SpawnerOptions{WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker}, OnPreSpawn: func(_ daemon.SessionSpec, env []string) ([]string, error) {
			credentials.Add(1)
			return env, nil
		}},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	detail := &daemon.SessionDetail{
		SessionID: requestID, WorkerID: work.WorkerID, AdmissionReceipt: immutableAdmission.Bytes(),
		EffectiveCell: effective, ExecutionRuntimeBinding: bindingRaw, OperationalPayload: operational,
	}
	if _, err := d.AcceptWorkWithDetail(daemon.SessionSpec{SessionID: requestID, ProjectID: "project-v2"}, detail); err != nil {
		t.Fatal(err)
	}
	if credentials.Load() != 1 {
		t.Fatalf("credential hook calls = %d", credentials.Load())
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(detail.HostAdaptationReceipt)
	if err != nil {
		t.Fatal(err)
	}
	var prepared agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &prepared); err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidatePreparedHarness(&prepared, operationalDigest); err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidatePreparedHarnessRegistration(&prepared, operationalDigest); err != nil {
		t.Fatal(err)
	}
	if len(prepared.ToolLifecycleReceipt.CapabilityRealizations) != 1 {
		t.Fatalf("fsynced receipt realizations = %+v", prepared.ToolLifecycleReceipt.CapabilityRealizations)
	}
	result := prepared.ToolLifecycleReceipt.CapabilityRealizations[0]
	if result.Decision != "artifact_bound" || result.AdapterVersion != declaration.AdapterVersion ||
		result.BinaryDigest != observation.BinaryDigest || len(result.ObservedSurface) != 1 || result.ObservedSurface[0].ID != "example_tool" {
		t.Fatalf("fsynced receipt omitted consumed realization: %+v", result)
	}
	fsynced, err := receiptStore.Load(requestID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fsynced, detail.HostAdaptationReceipt) {
		t.Fatal("registered receipt differs from the exact fsynced bytes")
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("spawn marker absent: %v", err)
		}
	}
}

const (
	preflightRegistrationCodexFixture = "testdata/preflight-registration-request-codex.json"
	preflightRegistrationPiFixture    = "testdata/preflight-registration-request-pi.json"
	preflightRegistrationManifest     = "testdata/preflight-registration-requests.sha256"
	preflightRegistrationAccepted     = "execution-preflight-registration/v1"
)

type capturePreflightRegistrar struct {
	mu       sync.Mutex
	requests []executioncell.PreflightRegistrationRequest
}

func (c *capturePreflightRegistrar) RegisterExecutionPreflight(ctx context.Context, request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
	if err := executioncell.ValidatePreflightRegistrationRequest(request); err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	c.mu.Lock()
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	binding := request.RuntimeBinding
	raw, err := executioncell.CanonicalJSON(request)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	sum := sha256.Sum256(raw)
	return executioncell.PreflightRegistrationResponse{
		ContractVersion:       executioncell.PreflightRegistrationContractVersion,
		Decision:              "authorized",
		RegistrationID:        "preflight_registration_" + hex.EncodeToString(sum[:16]),
		ReceiptSHA256:         request.ReceiptSHA256,
		RuntimeBinding:        &binding,
		AuthorizationRevision: 2,
	}, nil
}

func (c *capturePreflightRegistrar) captured() []executioncell.PreflightRegistrationRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]executioncell.PreflightRegistrationRequest(nil), c.requests...)
}

func updatePreflightRegistrationManifest(t *testing.T) {
	t.Helper()
	lines := make([]string, 0, 2)
	if raw, err := os.ReadFile("testdata/preflight-registration-request-codex.json"); err == nil {
		sum := sha256.Sum256(raw)
		lines = append(lines, hex.EncodeToString(sum[:])+"  preflight-registration-request-codex.json\n")
	}
	if raw, err := os.ReadFile("testdata/preflight-registration-request-pi.json"); err == nil {
		sum := sha256.Sum256(raw)
		lines = append(lines, hex.EncodeToString(sum[:])+"  preflight-registration-request-pi.json\n")
	}
	if len(lines) == 0 {
		t.Fatal("no registration fixtures to manifest")
	}
	if err := os.MkdirAll("testdata", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/preflight-registration-requests.sha256", []byte(strings.Join(lines, "")), 0o600); err != nil { //nolint:gosec // Test fixture regeneration writes only fixed testdata paths.
		t.Fatal(err)
	}
}

func checkPreflightRegistrationManifest(t *testing.T, fixturePath string) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read registration fixture: %v (regenerate with UPDATE_PREFLIGHT_REGISTRATION_FIXTURES=1)", err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	manifestRaw, err := os.ReadFile(preflightRegistrationManifest)
	if err != nil {
		t.Fatalf("read registration manifest: %v", err)
	}
	want := ""
	for _, line := range strings.Split(string(manifestRaw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == filepath.Base(fixturePath) {
			want = fields[0]
		}
	}
	if want == "" {
		t.Fatalf("registration manifest has no entry for %s", filepath.Base(fixturePath))
	}
	if got != want {
		t.Fatalf("registration fixture checksum = %s, manifest pins %s", got, want)
	}
}

func TestPreflightRegistrationRequestPinsAcceptedContractVersion(t *testing.T) {
	lanes := []struct {
		name    string
		fixture string
	}{
		{name: "codex", fixture: preflightRegistrationCodexFixture},
		{name: "pi", fixture: preflightRegistrationPiFixture},
	}
	for _, lane := range lanes {
		t.Run(lane.name, func(t *testing.T) {
			var registry *runner.Registry
			var realizations *agent.CapabilityRealizationRegistry
			var decorate agent.ExtensionDecorator
			var declaration agent.CapabilityRealizationDeclaration
			var observation agent.CapabilityFixtureObservation
			var grantedCapability string
			var mcpServers []agent.MCPServerConfig
			var harness agent.HarnessName
			var err error
			requestID := "preflight-registration-" + lane.name
			challengeID := requestID + "-challenge"
			admissionID := "admission-preflight-" + lane.name
			fixtureID := requestID
			switch lane.name {
			case "codex":
				harness = agent.HarnessCodex
				registry = runner.NewRegistry()
				if err := registry.Register(&providercodex.Provider{}); err != nil {
					t.Fatal(err)
				}
				server := agent.MCPServerConfig{Name: "fixture-mcp", Command: "fixture-mcp", Args: []string{"--stdio"}}
				mcpServers = []agent.MCPServerConfig{server}
				serverDigest := agent.MCPServerCapabilityInputDigest(server)
				var entryID string
				entryID, err = agent.MCPServerCapabilityEntryID(server.Name)
				if err != nil {
					t.Fatal(err)
				}
				surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceMCPServer, ID: server.Name}, {Kind: agent.CapabilitySurfaceMCPTool, ID: "draft_create"}}
				grantedCapability = "example.named-mcp/v1"
				declaration, err = agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
					CapabilityID: grantedCapability, HarnessID: agent.HarnessCodex,
					AdapterVersion: "codex/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled,
					RecipeID:        "example/named-mcp/v1",
					Entries:         []agent.CapabilityRecipeEntry{{EntryID: entryID, Channel: agent.ToolChannelMCPServer, Required: true, InputDigest: serverDigest, SurfaceRefs: surface}},
					DeclaredSurface: surface,
				})
				if err != nil {
					t.Fatal(err)
				}
				observation, err = agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
					Declaration: declaration, FixtureID: fixtureID, BinaryDigest: strings.Repeat("a", 64),
					AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: entryID, Channel: agent.ToolChannelMCPServer, InputDigest: serverDigest}},
					ObservedSurface:  surface,
				})
				if err != nil {
					t.Fatal(err)
				}
			case "pi":
				harness = agent.HarnessPi
				registry = runner.NewRegistry()
				if err := registry.Register(&providerpi.Provider{}); err != nil {
					t.Fatal(err)
				}
				extensionSource := []byte("register an example native tool")
				extensionDigest := sha256.Sum256(extensionSource)
				delivery := agent.ExtensionDelivery{ID: "example-extension", Kind: agent.ExtensionDeliveryInline, Source: extensionSource, Basename: "example.js", Digest: hex.EncodeToString(extensionDigest[:]), Required: true}
				capabilitySurface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "example_tool"}}
				capabilityInputDigest := agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery})
				decorate = func(agent.Spec) []agent.ExtensionDelivery { return []agent.ExtensionDelivery{delivery} }
				grantedCapability = "example.native/v1"
				declaration, err = agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
					CapabilityID: grantedCapability, HarnessID: agent.HarnessPi,
					AdapterVersion: "pi/interactive/tool-lifecycle-v6", Mode: agent.PromptModeHumanControlled,
					RecipeID:        "example/inline-extension/v1",
					Entries:         []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: capabilityInputDigest, SurfaceRefs: capabilitySurface}},
					DeclaredSurface: capabilitySurface,
				})
				if err != nil {
					t.Fatal(err)
				}
				observation, err = agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
					Declaration: declaration, FixtureID: fixtureID, BinaryDigest: strings.Repeat("b", 64),
					AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: capabilityInputDigest}},
					ObservedSurface:  capabilitySurface,
				})
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown lane %q", lane.name)
			}
			compiled, err := agent.CompileCapabilityRealization(declaration, observation)
			if err != nil {
				t.Fatal(err)
			}
			realizations, err = agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
			if err != nil {
				t.Fatal(err)
			}
			cell := executioncell.ResolvedExecutionCell{
				ContractVersion: executioncell.ContractVersion,
				Harness:         executioncell.HarnessRef{ID: string(harness), Version: "harness/v2"},
				Model:           executioncell.ModelRef{ID: "test-model", Author: "local"},
				Endpoint: executioncell.ServingEndpointRef{
					ID: "test-local", Protocol: string(agent.ProtoOpenAIResponses), Operator: "local", Revision: "r1",
				},
				AuthBinding: executioncell.AuthBindingRef{
					ID: "stub-auth", Mechanism: executioncell.AuthNone, CommercialMode: executioncell.CommercialSelfHosted,
					Authority: "local", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryNone,
				},
				Placement:           executioncell.PlacementRef{ID: "host-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
				SessionMode:         executioncell.SessionHumanControlled,
				GrantedCapabilities: []executioncell.CapabilityRequirement{{Name: grantedCapability, ParametersDigest: strings.Repeat("5", 64)}},
				EvidenceTier:        executioncell.EvidenceUnitVerified,
				CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
			}
			work := runner.QueuedWork{}
			work.SessionID = requestID
			work.WorkerID = "worker-local"
			work.Body = "compile the preflight registration authority"
			work.Mode = prompt.InteractiveRunMode
			work.InitialPrompt = "compile the preflight registration authority"
			work.McpServers = mcpServers
			work.ResolvedProfile = runner.ResolvedProfile{Harness: string(harness), Model: "test-model", Endpoint: &agent.EndpointBinding{
				Company: "test", Model: "test-model", Protocol: agent.ProtoOpenAIResponses, Host: agent.HostDirect,
				EndpointID: "test-local", EndpointOperator: "local", EndpointRevision: "r1", ModelAuthor: "local",
				AuthBindingID: "stub-auth", AuthAuthority: "local", AuthCommercialMode: string(executioncell.CommercialSelfHosted),
				AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
				AuthDelivery: string(executioncell.DeliveryNone), Mechanism: agent.AuthNone, Auth: agent.AuthLocal, CostModel: agent.CostLocalFree,
			}}
			operationalDigest, err := runner.DigestOperationalPayload(work)
			if err != nil {
				t.Fatal(err)
			}
			admission := executioncell.AdmissionReceipt{
				ContractVersion: executioncell.ContractVersion, ReceiptID: admissionID, RequestID: requestID,
				Decision: executioncell.AdmissionAdmitted, IntentDigest: strings.Repeat("1", 64), OperationalPayloadDigest: operationalDigest,
				Cell: &cell, ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-09-10T00:00:00Z",
			}
			admissionRaw, err := json.Marshal(admission)
			if err != nil {
				t.Fatal(err)
			}
			immutableAdmission, err := executioncell.DecodeAdmissionReceipt(admissionRaw)
			if err != nil {
				t.Fatal(err)
			}
			effective, err := executioncell.CanonicalJSON(cell)
			if err != nil {
				t.Fatal(err)
			}
			binding := executioncell.RuntimeBinding{
				ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
				RequestID:       requestID, WorkerID: work.WorkerID, PlacementID: cell.Placement.ID,
				PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: challengeID},
			}
			bindingRaw, err := json.Marshal(binding)
			if err != nil {
				t.Fatal(err)
			}
			operational, err := runner.CanonicalOperationalPayload(work)
			if err != nil {
				t.Fatal(err)
			}
			view := runner.NewProviderViewWithDecoratorAndRealizations(registry, decorate, realizations)
			tmp := t.TempDir()
			configPath := filepath.Join(tmp, "daemon.yaml")
			if err := daemon.WriteConfig(configPath, &daemon.Config{
				APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: daemon.ProjectAdmissionVersionV2,
				ProjectAdmissionMode: daemon.ProjectAdmissionModeAllRouted, Machine: daemon.MachineConfig{ID: "preflight-registration-pin"},
				Capacity:     daemon.CapacityConfig{MaxConcurrentSessions: 8},
				Orchestrator: daemon.OrchestratorConfig{URL: "https://example.test"},
			}); err != nil {
				t.Fatal(err)
			}
			var credentials atomic.Int32
			marker := filepath.Join(tmp, "spawned")
			receiptStore := daemon.NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts"))
			registrar := &capturePreflightRegistrar{}
			d := daemon.New(daemon.Options{
				ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"), SkipWizard: true, SkipRegistration: true,
				ProviderRegistry:            view,
				ExecutionPreflightStore:     receiptStore,
				ExecutionPreflightRegistrar: registrar,
				SpawnerOptions: daemon.SpawnerOptions{WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker}, OnPreSpawn: func(_ daemon.SessionSpec, env []string) ([]string, error) {
					credentials.Add(1)
					return env, nil
				}},
			})
			if err := d.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Stop(context.Background()) })
			detail := &daemon.SessionDetail{
				SessionID: requestID, WorkerID: work.WorkerID, AdmissionReceipt: immutableAdmission.Bytes(),
				EffectiveCell: effective, ExecutionRuntimeBinding: bindingRaw, OperationalPayload: operational,
			}
			if _, err := d.AcceptWorkWithDetail(daemon.SessionSpec{SessionID: requestID, ProjectID: "project-v2"}, detail); err != nil {
				t.Fatal(err)
			}
			if credentials.Load() != 1 {
				t.Fatalf("credential hook calls = %d", credentials.Load())
			}
			captured := registrar.captured()
			if len(captured) != 1 {
				t.Fatalf("captured registration requests = %d, want 1", len(captured))
			}
			request := captured[0]
			if request.ContractVersion != preflightRegistrationAccepted {
				t.Fatalf("registration contractVersion = %q, want %q", request.ContractVersion, preflightRegistrationAccepted)
			}
			if request.RuntimeBinding.PreflightRegistration == nil || request.RuntimeBinding.PreflightRegistration.ContractVersion != preflightRegistrationAccepted {
				t.Fatalf("nested registration contractVersion = %+v, want %q", request.RuntimeBinding.PreflightRegistration, preflightRegistrationAccepted)
			}
			canonical, err := executioncell.CanonicalJSON(request)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(canonical, []byte(tmp)) {
				t.Fatal("registration fixture contains a temporary path")
			}
			if os.Getenv("UPDATE_PREFLIGHT_REGISTRATION_FIXTURES") == "1" {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				switch lane.name {
				case "codex":
					if err := os.WriteFile("testdata/preflight-registration-request-codex.json", canonical, 0o600); err != nil { //nolint:gosec // Test fixture regeneration writes only fixed testdata paths.
						t.Fatal(err)
					}
				case "pi":
					if err := os.WriteFile("testdata/preflight-registration-request-pi.json", canonical, 0o600); err != nil { //nolint:gosec // Test fixture regeneration writes only fixed testdata paths.
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown lane %q", lane.name)
				}
				updatePreflightRegistrationManifest(t)
			}
			checkPreflightRegistrationManifest(t, lane.fixture)
			golden, err := os.ReadFile(lane.fixture)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, golden) {
				t.Fatal("registration request differs from the checked-in fixture (regenerate with UPDATE_PREFLIGHT_REGISTRATION_FIXTURES=1)")
			}
			decoded, err := executioncell.DecodePreflightRegistrationRequest(golden)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.ContractVersion != preflightRegistrationAccepted {
				t.Fatalf("golden contractVersion = %q, want %q", decoded.ContractVersion, preflightRegistrationAccepted)
			}
			if decoded.RuntimeBinding.PreflightRegistration == nil || decoded.RuntimeBinding.PreflightRegistration.ContractVersion != preflightRegistrationAccepted {
				t.Fatalf("golden nested contractVersion = %+v, want %q", decoded.RuntimeBinding.PreflightRegistration, preflightRegistrationAccepted)
			}
			recanonical, err := executioncell.CanonicalJSON(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(recanonical, golden) {
				t.Fatal("golden fixture is not canonical")
			}
			host, err := executioncell.DecodeHostAdaptationReceipt(detail.HostAdaptationReceipt)
			if err != nil {
				t.Fatal(err)
			}
			var prepared agent.PreparedHarness
			if err := json.Unmarshal(host.Plan, &prepared); err != nil {
				t.Fatal(err)
			}
			if err := agent.ValidatePreparedHarness(&prepared, operationalDigest); err != nil {
				t.Fatal(err)
			}
			if err := agent.ValidatePreparedHarnessRegistration(&prepared, operationalDigest); err != nil {
				t.Fatal(err)
			}
			if len(prepared.ToolLifecycleReceipt.CapabilityRealizations) != 1 {
				t.Fatalf("fsynced receipt realizations = %+v", prepared.ToolLifecycleReceipt.CapabilityRealizations)
			}
			result := prepared.ToolLifecycleReceipt.CapabilityRealizations[0]
			if result.Decision != "artifact_bound" || result.AdapterVersion != declaration.AdapterVersion || result.BinaryDigest != observation.BinaryDigest {
				t.Fatalf("fsynced receipt omitted consumed realization: %+v", result)
			}
			if lane.name == "pi" {
				if len(result.ObservedSurface) != 1 || result.ObservedSurface[0].ID != "example_tool" {
					t.Fatalf("pi receipt observed surface = %+v", result.ObservedSurface)
				}
			} else {
				ids := map[string]bool{}
				for _, identity := range result.ObservedSurface {
					ids[identity.ID] = true
				}
				if !ids["fixture-mcp"] || !ids["draft_create"] {
					t.Fatalf("codex receipt observed surface = %+v", result.ObservedSurface)
				}
			}
			for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(marker); err == nil {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("spawn marker absent: %v", err)
				}
			}
		})
	}
}
