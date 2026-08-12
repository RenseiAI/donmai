package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

// stubClaimGateProvider satisfies ProviderRegistry, ExecutionPreflightProvider
// (as a permissive stub that always answers "ready" once the wire binding
// decodes), and ClaimGateProvider. It records call counts and the last
// SessionDetail projection the preflight compiler saw, so tests can assert
// whether the compiler ran at all and, when it did, that it received the
// daemon-computed claim.
type stubClaimGateProvider struct {
	preflightCalls atomic.Int32
	realityCalls   atomic.Int32

	realityJSON json.RawMessage
	realityErr  error

	lastPreflightInput atomic.Pointer[[]byte]
}

func (p *stubClaimGateProvider) Names() []string { return []string{"stub"} }
func (p *stubClaimGateProvider) Capabilities(string) (map[string]any, bool) {
	return map[string]any{}, true
}

// PreflightExecution builds a real, schema-valid "ready" HostAdaptationReceipt
// from whatever request/worker/placement/claim identity the wire input
// actually carries, so AcceptWorkWithDetail's own decode-and-cross-check of
// the compiler's answer succeeds regardless of which test's identities are in
// play. It intentionally ignores the admission/claim/effective-cell content —
// this stub tests the daemon's claim-gate seam, not the exact-harness
// compiler downstream of it.
func (p *stubClaimGateProvider) PreflightExecution(detailJSON json.RawMessage) (json.RawMessage, error) {
	p.preflightCalls.Add(1)
	captured := append([]byte(nil), detailJSON...)
	p.lastPreflightInput.Store(&captured)

	var wire struct {
		SessionID               string          `json:"sessionId"`
		WorkerID                string          `json:"workerId"`
		ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding"`
	}
	if err := json.Unmarshal(detailJSON, &wire); err != nil {
		return nil, err
	}
	binding, err := executioncell.DecodeRuntimeBinding(wire.ExecutionRuntimeBinding)
	if err != nil {
		return nil, err
	}
	plan := json.RawMessage(`{"plan":true}`)
	sum := sha256.Sum256(plan)
	receipt := executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       wire.SessionID, WorkerID: wire.WorkerID,
		PlacementID: binding.PlacementID, ClaimID: binding.ClaimID,
		Decision: "ready", Plan: plan, PlanDigest: hex.EncodeToString(sum[:]),
		PromptReceipt:        json.RawMessage(`{"decision":"ready"}`),
		ToolLifecycleReceipt: json.RawMessage(`{"decision":"ready"}`),
	}
	return json.Marshal(receipt)
}

func (p *stubClaimGateProvider) ResolveClaimLocalReality(json.RawMessage) (json.RawMessage, error) {
	p.realityCalls.Add(1)
	return p.realityJSON, p.realityErr
}

// claimBoundAdmission returns an admitted, claim-bound AdmissionReceipt built
// straight from the executioncell package, plus its cell, so daemon-level
// tests don't depend on runner or platform fixtures.
func claimBoundAdmission(t *testing.T) (executioncell.ImmutableAdmissionReceipt, executioncell.ResolvedExecutionCell) {
	t.Helper()
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: "codex", Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "gpt-test", Author: "openai"},
		Endpoint:        executioncell.ServingEndpointRef{ID: "endpoint", Protocol: "openai-responses", Operator: "openai", Revision: "r1"},
		AuthBinding:     executioncell.AuthBindingRef{ID: "auth", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled, Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment},
		Placement:       executioncell.PlacementRef{ID: "pool-1", Kind: executioncell.PlacementPool, Resolution: executioncell.PlacementClaimBound},
		SessionMode:     executioncell.SessionAutonomous, GrantedCapabilities: []executioncell.CapabilityRequirement{{Name: "watch"}},
		EvidenceTier:        executioncell.EvidenceSmoked,
		CompatibilityDigest: strings.Repeat("a", 64), RuntimeInventoryDigest: strings.Repeat("b", 64),
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-claim-gate-1", RequestID: "request-claim-gate-1",
		Decision: executioncell.AdmissionAdmitted, Cell: &cell,
		IntentDigest: strings.Repeat("c", 64), OperationalPayloadDigest: strings.Repeat("d", 64),
		ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-08-12T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return admission, cell
}

// startClaimGateDaemon starts a daemon whose project admission is
// "all-routed" (the machine owner trusts the org's own routing), so
// AcceptWorkWithDetail's spawner step admits SessionSpec.ProjectID without
// requiring a pre-enumerated project or a resolvable repository — the claim
// gate under test sits entirely upstream of that decision.
func startClaimGateDaemon(t *testing.T, provider *stubClaimGateProvider) *Daemon {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "daemon.yaml")
	cfg := Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon",
		ProjectAdmissionVersion: ProjectAdmissionVersionV2, ProjectAdmissionMode: ProjectAdmissionModeAllRouted,
		Machine: MachineConfig{ID: "claim-gate-machine"}, Orchestrator: OrchestratorConfig{URL: "https://example.test"},
	}
	if err := WriteConfig(configPath, &cfg); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "spawned")
	d := New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"),
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider,
		ExecutionPreflightStore: &countingExecutionStore{},
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

func TestEvaluateNarrowOnlyClaim_PreSuppliedReceiptMustNarrow(t *testing.T) {
	admission, cell := claimBoundAdmission(t)
	widened := cell
	widened.Placement = executioncell.PlacementRef{ID: "worker-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact}
	widened.Model.ID = "a-different-model" // an immutable axis the claim must not change
	badClaim := executioncell.ClaimReceipt{
		ContractVersion: executioncell.ContractVersion, ClaimReceiptID: "claim-1",
		AdmissionReceiptID: admission.Value().ReceiptID, ClaimID: "claim-active",
		Decision: executioncell.ClaimClaimed, EffectiveCell: &widened, RecordedAt: "2026-08-12T12:00:00Z",
	}
	badClaimRaw, err := json.Marshal(badClaim)
	if err != nil {
		t.Fatal(err)
	}

	provider := &stubClaimGateProvider{}
	d := startClaimGateDaemon(t, provider)
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-claim-gate-1",
		WorkerID: "worker-local", PlacementID: "worker-local", ClaimID: "claim-active",
	}
	detail := &SessionDetail{
		SessionID: "request-claim-gate-1", WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), ClaimReceipt: badClaimRaw, EffectiveCell: mustMarshal(t, widened),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
	}

	_, err = d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID}, detail)
	if err == nil {
		t.Fatal("a claim receipt that widens the admitted cell was accepted")
	}
	if !strings.Contains(err.Error(), "does not narrow admission") {
		t.Fatalf("error = %v, want a narrow-only violation", err)
	}
	if provider.preflightCalls.Load() != 0 {
		t.Fatalf("preflight compiler ran despite a non-narrowing claim receipt: calls=%d", provider.preflightCalls.Load())
	}
}

func TestEvaluateNarrowOnlyClaim_ComputesFromLocalReality_Denied(t *testing.T) {
	admission, cell := claimBoundAdmission(t)
	reality := executioncell.ClaimLocalReality{
		PlacementID: "worker-local", HarnessAvailable: true, EndpointReachable: true,
		AuthBindingAvailable:  false, // denies with auth_unavailable
		AvailableCapabilities: cell.GrantedCapabilities, EvidenceTier: cell.EvidenceTier,
		RuntimeInventoryDigest: strings.Repeat("e", 64),
	}
	provider := &stubClaimGateProvider{realityJSON: mustMarshal(t, reality)}
	d := startClaimGateDaemon(t, provider)
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-claim-gate-1",
		WorkerID: "worker-local", PlacementID: "worker-local", ClaimID: "claim-active",
	}
	detail := &SessionDetail{
		SessionID: "request-claim-gate-1", WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), EffectiveCell: mustMarshal(t, cell),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
	}

	_, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID}, detail)
	if err == nil {
		t.Fatal("a claim denied by local reality was accepted")
	}
	if !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("error = %v, want an auth_unavailable denial", err)
	}
	if provider.preflightCalls.Load() != 0 {
		t.Fatalf("preflight compiler ran despite a locally denied claim: calls=%d", provider.preflightCalls.Load())
	}
	if provider.realityCalls.Load() != 1 {
		t.Fatalf("local reality was resolved %d times, want 1", provider.realityCalls.Load())
	}
}

func TestEvaluateNarrowOnlyClaim_ComputesFromLocalReality_ClaimedFlowsIntoCompiler(t *testing.T) {
	admission, cell := claimBoundAdmission(t)
	reality := executioncell.ClaimLocalReality{
		PlacementID: "worker-local", HarnessAvailable: true, EndpointReachable: true,
		AuthBindingAvailable: true, AvailableCapabilities: cell.GrantedCapabilities,
		EvidenceTier: cell.EvidenceTier, RuntimeInventoryDigest: strings.Repeat("e", 64),
	}
	provider := &stubClaimGateProvider{realityJSON: mustMarshal(t, reality)}
	d := startClaimGateDaemon(t, provider)
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-claim-gate-1",
		WorkerID: "worker-local", PlacementID: "worker-local", ClaimID: "claim-active",
	}
	detail := &SessionDetail{
		SessionID: "request-claim-gate-1", WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), EffectiveCell: mustMarshal(t, cell),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
	}

	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "claim-gate-project"}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	if provider.preflightCalls.Load() != 1 {
		t.Fatalf("preflight compiler calls = %d, want 1", provider.preflightCalls.Load())
	}
	if len(detail.ClaimReceipt) == 0 {
		t.Fatal("daemon-computed claim receipt was not attached to the session detail")
	}
	claim, err := executioncell.DecodeClaimReceipt(detail.ClaimReceipt)
	if err != nil {
		t.Fatalf("decode computed claim receipt: %v", err)
	}
	value := claim.Value()
	if value.Decision != executioncell.ClaimClaimed {
		t.Fatalf("computed claim decision = %q, want %q", value.Decision, executioncell.ClaimClaimed)
	}
	if value.EffectiveCell == nil || value.EffectiveCell.Placement.ID != "worker-local" {
		t.Fatalf("computed claim did not bind placement to this host: %+v", value.EffectiveCell)
	}
	input := provider.lastPreflightInput.Load()
	if input == nil || !strings.Contains(string(*input), "worker-local") {
		t.Fatal("preflight compiler did not receive the daemon-computed claim/effective cell")
	}
}

func TestEvaluateNarrowOnlyClaim_ExactAdmissionIsNoOp(t *testing.T) {
	// Mixed-version-safe default: the gate only applies to a claim-bound
	// placement. An exact admission — the overwhelming majority of admissions
	// today — must behave exactly as it did before this gate existed: the
	// daemon layer does not newly deny anything it previously accepted, and
	// never even asks a wired ClaimGateProvider for local reality.
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: "codex", Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "gpt-test", Author: "openai"},
		Endpoint:        executioncell.ServingEndpointRef{ID: "endpoint", Protocol: "openai-responses", Operator: "openai", Revision: "r1"},
		AuthBinding:     executioncell.AuthBindingRef{ID: "auth", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled, Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment},
		Placement:       executioncell.PlacementRef{ID: "worker-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode:     executioncell.SessionAutonomous, GrantedCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:        executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("a", 64), RuntimeInventoryDigest: strings.Repeat("b", 64),
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-exact-1", RequestID: "request-claim-gate-1",
		Decision: executioncell.AdmissionAdmitted, Cell: &cell,
		IntentDigest: strings.Repeat("c", 64), OperationalPayloadDigest: strings.Repeat("d", 64),
		ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-08-12T12:00:00Z",
	}
	admission, err := executioncell.DecodeAdmissionReceipt(mustMarshal(t, receipt))
	if err != nil {
		t.Fatal(err)
	}
	provider := &stubClaimGateProvider{}
	d := startClaimGateDaemon(t, provider)
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-claim-gate-1",
		WorkerID: "worker-local", PlacementID: "worker-local",
	}
	detail := &SessionDetail{
		SessionID: "request-claim-gate-1", WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), EffectiveCell: mustMarshal(t, cell),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
	}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "claim-gate-project"}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	if provider.realityCalls.Load() != 0 {
		t.Fatalf("local reality was resolved for a non-claim-bound admission: calls=%d", provider.realityCalls.Load())
	}
	if provider.preflightCalls.Load() != 1 {
		t.Fatalf("preflight compiler calls = %d, want 1", provider.preflightCalls.Load())
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
