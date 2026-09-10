package daemon_test

import (
	"context"
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
	stub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runner"
)

func TestActualProviderViewReceiptPassesV2RegistrationBeforeCredential(t *testing.T) {
	provider, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	registry := runner.NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	const requestID = "actual-v2-provider-view"
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(agent.HarnessStub), Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "stub-model", Author: "local"},
		Endpoint: executioncell.ServingEndpointRef{
			ID: "stub-local", Protocol: string(agent.ProtoStub), Operator: "local", Revision: "r1",
		},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "stub-auth", Mechanism: executioncell.AuthNone, CommercialMode: executioncell.CommercialSelfHosted,
			Authority: "local", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryNone,
		},
		Placement:   executioncell.PlacementRef{ID: "host-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode: executioncell.SessionHumanControlled, GrantedCapabilities: []executioncell.CapabilityRequirement{}, EvidenceTier: executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
	work := runner.QueuedWork{}
	work.SessionID = requestID
	work.WorkerID = "worker-local"
	work.Body = "compile the actual provider-view authority"
	work.Mode = prompt.InteractiveRunMode
	work.InitialPrompt = "compile the actual provider-view authority"
	work.ResolvedProfile = runner.ResolvedProfile{Harness: string(agent.HarnessStub), Model: "stub-model", Endpoint: &agent.EndpointBinding{
		Company: agent.CompanyStub, Model: "stub-model", Protocol: agent.ProtoStub, Host: agent.HostLocal,
		EndpointID: "stub-local", EndpointOperator: "local", EndpointRevision: "r1", ModelAuthor: "local",
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
	view := runner.NewProviderView(registry)
	preflightInput, err := json.Marshal(map[string]any{
		"sessionId": requestID, "workerId": work.WorkerID, "admissionReceipt": json.RawMessage(immutableAdmission.Bytes()),
		"effectiveCell": json.RawMessage(effective), "executionRuntimeBinding": json.RawMessage(bindingRaw), "operationalPayload": json.RawMessage(operational),
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt, compileErr := view.PreflightExecution(preflightInput); compileErr != nil {
		t.Fatalf("actual ProviderView preflight: %v receipt=%s", compileErr, receipt)
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
	d := daemon.New(daemon.Options{
		ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"), SkipWizard: true, SkipRegistration: true,
		ProviderRegistry:            view,
		ExecutionPreflightStore:     daemon.NewFileExecutionPreflightStore(filepath.Join(tmp, "receipts")),
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
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("spawn marker absent: %v", err)
		}
	}
}
