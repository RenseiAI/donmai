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
		AdapterVersion: "pi/interactive/tool-lifecycle-v4", Mode: agent.PromptModeHumanControlled,
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
