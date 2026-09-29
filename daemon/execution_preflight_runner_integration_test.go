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

func TestActualProviderViewProtectedRuntimeMCPIdentityMatchesDaemonV1AndV2(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version+" matching identity reaches child", func(t *testing.T) {
			credentials, spawned, err := runActualProtectedRuntimeMCPComposition(t, version, "example-platform")
			if err != nil || credentials != 1 || !spawned {
				t.Fatalf("matching %s composition credentials=%d spawned=%v err=%v", version, credentials, spawned, err)
			}
		})
		t.Run(version+" different daemon identity refuses before child", func(t *testing.T) {
			credentials, spawned, err := runActualProtectedRuntimeMCPComposition(t, version, "example-local-b-platform")
			if err == nil || !strings.Contains(err.Error(), "differs from current runtime authority") || credentials != 0 || spawned {
				t.Fatalf("different %s composition credentials=%d spawned=%v err=%v", version, credentials, spawned, err)
			}
		})
	}
}

func runActualProtectedRuntimeMCPComposition(t *testing.T, version, daemonServerName string) (int32, bool, error) {
	t.Helper()
	const (
		platformMCPServerName = "example-platform"
		capabilityID          = "example.protected-mcp/v1"
	)
	provider := &providercodex.Provider{}
	registry := runner.NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	mode := agent.PromptModeAutonomous
	sessionMode := executioncell.SessionAutonomous
	profile, ok := provider.Manifest().ToolLifecycleProfile(mode)
	if version == "v2" {
		mode = agent.PromptModeHumanControlled
		sessionMode = executioncell.SessionHumanControlled
		profile, ok = provider.Manifest().ToolLifecycleProfileByID(providercodex.InteractiveRefreshableToolLifecycleProfileID, mode)
	}
	if !ok {
		t.Fatalf("missing %s tool lifecycle profile", version)
	}
	entryID, err := agent.MCPServerCapabilityEntryID(platformMCPServerName)
	if err != nil {
		t.Fatal(err)
	}
	surface := []agent.CapabilitySurfaceIdentity{
		{Kind: agent.CapabilitySurfaceMCPServer, ID: platformMCPServerName},
		{Kind: agent.CapabilitySurfaceMCPTool, ID: "example_call"},
	}
	inputDigest := agent.MCPRuntimeServerCapabilityInputDigest(agent.MCPServerConfig{Name: platformMCPServerName, Type: executioncell.ProtectedRuntimeMCPTransportHTTP})
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID: capabilityID, HarnessID: agent.HarnessCodex, AdapterVersion: profile.ID, Mode: mode,
		RecipeID: "example/protected-runtime-mcp/v1", DeclaredSurface: surface,
		Entries: []agent.CapabilityRecipeEntry{{
			EntryID: entryID, Channel: agent.ToolChannelMCPServer, Required: true,
			InputDigest: inputDigest, SurfaceRefs: surface,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "actual-protected-runtime-mcp", BinaryDigest: strings.Repeat("b", 64),
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
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}

	viewOptions := runner.ProviderViewOptions{CapabilityRealizations: realizations, PlatformMCPServerName: platformMCPServerName}
	if version == "v1" {
		viewOptions.ProtectedRuntimeMCPSelector = runner.ProtectedRuntimeMCPSelector{
			CapabilityID: capabilityID, HarnessID: agent.HarnessCodex, AdapterProfileID: profile.ID, Mode: mode,
		}
	} else {
		viewOptions.ProtectedRuntimeMCPV2Selector = runner.ProtectedRuntimeMCPV2Selector{
			CapabilityID: capabilityID, HarnessID: agent.HarnessCodex, AdapterProfileID: profile.ID, Mode: mode,
			ConfigRequirementID: "example.session-config/v1",
		}
		viewOptions.ConfigRequirements = func(ctx runner.ExecutionPreflightConfigRequirementContext) ([]executioncell.PreflightConfigRequirementV1, error) {
			return []executioncell.PreflightConfigRequirementV1{{
				ContractVersion: executioncell.PreflightConfigRequirementContractVersion,
				RequirementID:   "example.session-config/v1", AuthorityBindingDigest: strings.Repeat("c", 64),
				OperationalPayloadDigest: ctx.OperationalPayloadDigest,
				Bindings: []executioncell.PreflightConfigBindingV1{{
					TargetEnv: executioncell.PreflightConfigSessionMCPBearerFileTarget,
					Source:    executioncell.PreflightConfigBindingSourceV1{Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, Mode: executioncell.PreflightConfigPrivateFileMode},
				}},
			}}, nil
		}
	}
	view, err := runner.NewProviderViewWithOptions(registry, viewOptions)
	if err != nil {
		t.Fatal(err)
	}

	requestID := "actual-protected-runtime-mcp-" + version + "-" + strings.ReplaceAll(daemonServerName, ".", "-")
	work := runner.QueuedWork{}
	work.SessionID, work.WorkerID = requestID, "worker-local"
	work.Body = "compile protected runtime MCP authority"
	work.PlatformURL, work.McpAuthToken = "https://platform.example/", "session-bearer"
	if mode == agent.PromptModeHumanControlled {
		work.Mode, work.InitialPrompt = prompt.InteractiveRunMode, "compile protected runtime MCP authority"
	}
	work.ResolvedProfile = runner.ResolvedProfile{Harness: string(agent.HarnessCodex), Model: "test-model", Endpoint: &agent.EndpointBinding{
		Company: "test", Model: "test-model", Protocol: agent.ProtoOpenAIResponses, Host: agent.HostDirect,
		EndpointID: "test-local", EndpointOperator: "local", EndpointRevision: "r1", ModelAuthor: "local",
		AuthBindingID: "stub-auth", AuthAuthority: "local", AuthCommercialMode: string(executioncell.CommercialSelfHosted),
		AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
		AuthDelivery: string(executioncell.DeliveryNone), Mechanism: agent.AuthNone, Auth: agent.AuthLocal, CostModel: agent.CostLocalFree,
	}}
	baseOperational, err := runner.CanonicalOperationalPayload(work)
	if err != nil {
		t.Fatal(err)
	}
	var operationalDocument map[string]any
	if err := json.Unmarshal(baseOperational, &operationalDocument); err != nil {
		t.Fatal(err)
	}
	operationalDocument["mcpAuthToken"] = work.McpAuthToken
	operational, err := executioncell.CanonicalJSON(operationalDocument)
	if err != nil {
		t.Fatal(err)
	}
	work.OperationalPayload = operational
	operationalDigest, err := runner.DigestOperationalPayload(work)
	if err != nil {
		t.Fatal(err)
	}
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(agent.HarnessCodex), Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "test-model", Author: "local"},
		Endpoint:        executioncell.ServingEndpointRef{ID: "test-local", Protocol: string(agent.ProtoOpenAIResponses), Operator: "local", Revision: "r1"},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "stub-auth", Mechanism: executioncell.AuthNone, CommercialMode: executioncell.CommercialSelfHosted,
			Authority: "local", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryNone,
		},
		Placement:   executioncell.PlacementRef{ID: "host-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode: sessionMode, GrantedCapabilities: []executioncell.CapabilityRequirement{{Name: capabilityID}},
		EvidenceTier: executioncell.EvidenceUnitVerified, CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
	admission := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-" + requestID, RequestID: requestID,
		Decision: executioncell.AdmissionAdmitted, IntentDigest: strings.Repeat("1", 64), OperationalPayloadDigest: operationalDigest,
		Cell: &cell, ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-09-19T00:00:00Z",
	}
	admissionRaw, _ := json.Marshal(admission)
	immutableAdmission, err := executioncell.DecodeAdmissionReceipt(admissionRaw)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		t.Fatal(err)
	}
	bindingRaw, _ := json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion, RequestID: requestID, WorkerID: work.WorkerID, PlacementID: cell.Placement.ID,
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "actual-protected-runtime-mcp"},
	})

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "daemon.yaml")
	if err := daemon.WriteConfig(configPath, &daemon.Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: daemon.ProjectAdmissionVersionV2,
		ProjectAdmissionMode: daemon.ProjectAdmissionModeAllRouted, Machine: daemon.MachineConfig{ID: "actual-protected-runtime-mcp"},
		Orchestrator: daemon.OrchestratorConfig{URL: "https://example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "spawned")
	var credentials atomic.Int32
	d := daemon.New(daemon.Options{
		ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"), SkipWizard: true, SkipRegistration: true,
		ProviderRegistry: view, PlatformMCPServerName: daemonServerName,
		ExecutionPreflightStore:                 daemon.NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts")),
		ExecutionPreflightRegistrar:             daemon.NewFileExecutionPreflightRegistrar(filepath.Join(tmp, "registrations")),
		ExecutionPreflightConfigDir:             filepath.Join(tmp, "preflight-config"),
		ProtectedRuntimeMCPHelperCommandBuilder: func(path string) (string, error) { return "helper --token-file " + path, nil },
		SpawnerOptions: daemon.SpawnerOptions{WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker}, OnPreSpawn: func(_ daemon.SessionSpec, env []string) ([]string, error) {
			credentials.Add(1)
			return env, nil
		}},
	})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	detail := &daemon.SessionDetail{
		SessionID: requestID, WorkerID: work.WorkerID, PlatformURL: work.PlatformURL, McpAuthToken: work.McpAuthToken,
		AdmissionReceipt: immutableAdmission.Bytes(), EffectiveCell: effective, ExecutionRuntimeBinding: bindingRaw, OperationalPayload: operational,
	}
	_, acceptErr := d.AcceptWorkWithDetail(daemon.SessionSpec{SessionID: requestID, ProjectID: "project-v2"}, detail)
	spawned := false
	if acceptErr == nil {
		for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, statErr := os.Stat(marker); statErr == nil {
				spawned = true
				break
			} else if time.Now().After(deadline) {
				break
			}
		}
	}
	return credentials.Load(), spawned, acceptErr
}
