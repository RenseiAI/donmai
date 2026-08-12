package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/rulesetsnapshot"
)

// stubPreflightOnlyProvider satisfies ProviderRegistry and
// ExecutionPreflightProvider (so AcceptWorkWithDetail's compiler step
// succeeds) but deliberately does NOT implement ClaimGateProvider — it
// simulates a daemon composed WITHOUT any host-local claim-gate answer at
// all (e.g. "the platform is unreachable" for that seam specifically),
// distinct from a daemon with no ProviderRegistry wired whatsoever (which
// would also fail preflight for an unrelated reason).
type stubPreflightOnlyProvider struct{}

func (p *stubPreflightOnlyProvider) Names() []string { return []string{"stub"} }
func (p *stubPreflightOnlyProvider) Capabilities(string) (map[string]any, bool) {
	return map[string]any{}, true
}

func (p *stubPreflightOnlyProvider) PreflightExecution(detailJSON json.RawMessage) (json.RawMessage, error) {
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

// ---------------------------------------------------------------------------
// Test snapshot fixture — builds a wire-shape ruleset-snapshot response
// using only exported symbols (executioncell.CanonicalJSON), so this test
// exercises the SAME hashing path rulesetsnapshot's own (unexported)
// contentHash uses, from outside the package — real cross-package
// integration coverage, not a re-implementation asserted against itself.
// ---------------------------------------------------------------------------

type snapshotFixture struct {
	poolID           string
	poolStatus       string
	grantedByProfile bool
	hostID           string
	hostStatus       string
	harnessID        string
	compiledAt       time.Time
	revision         int
	signingKeyID     string
	corruptSignature bool
}

func buildSnapshotResponse(t *testing.T, priv ed25519.PrivateKey, f snapshotFixture) []byte {
	t.Helper()
	poolIDs := []string{}
	if f.grantedByProfile {
		poolIDs = []string{f.poolID}
	}
	revision := f.revision
	if revision == 0 {
		revision = 1
	}
	signingKeyID := f.signingKeyID
	if signingKeyID == "" {
		signingKeyID = "ksk_test"
	}
	compiledAt := f.compiledAt
	if compiledAt.IsZero() {
		compiledAt = time.Now().UTC()
	}

	sections := struct {
		PolicyBundle        json.RawMessage `json:"policyBundle"`
		CapacityProfiles    json.RawMessage `json:"capacityProfiles"`
		PoolHostInventory   json.RawMessage `json:"poolHostInventory"`
		ExecutionCellMatrix json.RawMessage `json:"executionCellMatrix"`
		PosteriorSummary    json.RawMessage `json:"posteriorSummary"`
	}{
		PolicyBundle: json.RawMessage(`{"workspaceId":"org1","policies":[]}`),
		CapacityProfiles: mustMarshal(t, map[string]any{
			"profiles": []map[string]any{{
				"id": "profile1", "name": "default", "orderingPolicy": "declared", "preferenceVector": nil,
				"reservationPosture":    map[string]any{"mode": "none", "timeoutMs": nil},
				"burstPosture":          "off",
				"disconnectLostAfterMs": 0, "disconnectReplace": false,
				"disconnectReconcile": "keep_original", "isOrgDefault": true, "revision": 1,
				"poolIds": poolIDs,
			}},
			"grants": []any{},
		}),
		PoolHostInventory: mustMarshal(t, map[string]any{
			"pools": []map[string]any{{
				"id": f.poolID, "providerId": "prov1", "displayName": "Pool", "servesPersistent": true,
				"servesOnDemand": true, "status": f.poolStatus, "costWeight": 1, "priority": 1,
				"substrateClass": "local", "allowedProjectIds": nil,
			}},
			"hosts": []map[string]any{{
				"id": f.hostID, "executionPoolId": f.poolID, "status": f.hostStatus,
				"capabilities": []any{}, "os": "linux", "arch": "amd64", "maxSessions": 1,
				"activeSessions": 0, "lastHeartbeatMs": 1000,
			}},
		}),
		ExecutionCellMatrix: mustMarshal(t, map[string]any{
			"providers": []map[string]any{{
				"id": "prov1", "displayName": "Prov", "configNamespace": "prov",
				"supportedAuthModes": []string{"metered"}, "category": "cli",
				"harnessByAuthMode": map[string]string{"metered": f.harnessID},
			}},
			"modelProfiles": []any{},
		}),
		PosteriorSummary: json.RawMessage(`{"posteriors":[]}`),
	}

	canonical, err := executioncell.CanonicalJSON(sections)
	if err != nil {
		t.Fatalf("canonicalize sections: %v", err)
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, hashBytes)
	if f.corruptSignature {
		sig[0] ^= 0xFF
	}

	wire := map[string]any{
		"orgId": "org1", "revision": revision, "rulesetRev": "org1@" + itoaForTest(revision),
		"contentHash": hash, "sectionDigests": map[string]string{},
		"signature": base64.StdEncoding.EncodeToString(sig), "signingKeyId": signingKeyID,
		"algorithm": "ed25519", "validators": []string{},
		"compiledAt": compiledAt.Format(time.RFC3339Nano),
		"sections":   sections,
	}
	return mustMarshal(t, wire)
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func newSnapshotTestServer(t *testing.T, respond func() []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respond())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startSnapshotClaimGateDaemon mirrors startClaimGateDaemon (claim_gate_test.go)
// but additionally wires Options.RulesetSnapshot and gives the daemon a
// non-empty WorkerID — required because narrowOnlyClaimDenial denies a blank
// PlacementID, and startClaimGateDaemon's SkipRegistration:true otherwise
// leaves WorkerID() empty.
func startSnapshotClaimGateDaemon(t *testing.T, registry ProviderRegistry, snapshot *rulesetsnapshot.Client) *Daemon {
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
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: registry,
		ExecutionPreflightStore: &countingExecutionStore{},
		RulesetSnapshot:         snapshot,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker},
		},
	})
	d.workerID = "worker-local"
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

// snapshotBoundAdmission returns an admission receipt/cell matching
// claimBoundAdmission's shape but with a distinct pool id per test to avoid
// cross-test interference, and no pre-supplied ClaimReceipt (so the
// snapshot-backed ClaimGateProvider path always runs).
func snapshotBoundAdmission(t *testing.T, poolID string) (executioncell.ImmutableAdmissionReceipt, executioncell.ResolvedExecutionCell) {
	t.Helper()
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: "codex", Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "gpt-test", Author: "openai"},
		Endpoint:        executioncell.ServingEndpointRef{ID: "endpoint", Protocol: "openai-responses", Operator: "openai", Revision: "r1"},
		AuthBinding:     executioncell.AuthBindingRef{ID: "auth", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled, Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment},
		Placement:       executioncell.PlacementRef{ID: poolID, Kind: executioncell.PlacementPool, Resolution: executioncell.PlacementClaimBound},
		SessionMode:     executioncell.SessionAutonomous, GrantedCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:        executioncell.EvidenceSmoked,
		CompatibilityDigest: strings.Repeat("a", 64), RuntimeInventoryDigest: strings.Repeat("b", 64),
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-" + poolID, RequestID: "request-" + poolID,
		Decision: executioncell.AdmissionAdmitted, Cell: &cell,
		IntentDigest: strings.Repeat("c", 64), OperationalPayloadDigest: strings.Repeat("d", 64),
		ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-08-12T12:00:00Z",
	}
	admission, err := executioncell.DecodeAdmissionReceipt(mustMarshal(t, receipt))
	if err != nil {
		t.Fatal(err)
	}
	return admission, cell
}

func snapshotDetail(t *testing.T, requestID string, cell executioncell.ResolvedExecutionCell, admission executioncell.ImmutableAdmissionReceipt) *SessionDetail {
	t.Helper()
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: requestID,
		WorkerID: "worker-local", PlacementID: "worker-local", ClaimID: "claim-" + requestID,
	}
	return &SessionDetail{
		SessionID: requestID, WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), EffectiveCell: mustMarshal(t, cell),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
	}
}

// ---------------------------------------------------------------------------
// Acceptance-criteria red-first scenarios: fail-static claims from a
// cached, verified ruleset snapshot.
// ---------------------------------------------------------------------------

// TestFailStatic_PlatformUnreachableWithinTTL_ClaimSucceedsFromCache proves
// the headline acceptance criterion: with NO live ClaimGateProvider wired at
// all (simulating "the platform is unreachable" — nothing answers host-local
// facts), a valid cached snapshot whose pool is active/granted and whose
// harness is known lets the claim succeed.
func TestFailStatic_PlatformUnreachableWithinTTL_ClaimSucceedsFromCache(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateTestKey(t)
	srv := newSnapshotTestServer(t, func() []byte {
		return buildSnapshotResponse(t, priv, snapshotFixture{
			poolID: "pool-ttl-ok", poolStatus: "active", grantedByProfile: true,
			hostID: "worker-local", hostStatus: "ready", harnessID: "codex",
		})
	})
	client, err := rulesetsnapshot.NewClient(rulesetsnapshot.Config{
		Endpoint: srv.URL, TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath: filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	d := startSnapshotClaimGateDaemon(t, &stubPreflightOnlyProvider{}, client)
	admission, cell := snapshotBoundAdmission(t, "pool-ttl-ok")
	detail := snapshotDetail(t, "request-ttl-ok", cell, admission)

	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "p1"}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	if len(detail.ClaimReceipt) == 0 {
		t.Fatal("no claim receipt was attached")
	}
	claim, err := executioncell.DecodeClaimReceipt(detail.ClaimReceipt)
	if err != nil {
		t.Fatalf("decode claim receipt: %v", err)
	}
	if claim.Value().Decision != executioncell.ClaimClaimed {
		t.Fatalf("claim decision = %q, want claimed", claim.Value().Decision)
	}

	// The claim decision must be visible on the routing explain surface with
	// the snapshot's rev and degraded=false.
	_, _, status, ok := d.RoutingTraces().ExplainWithSnapshot(detail.SessionID)
	if !ok {
		t.Fatal("routing explain has no record for this claim decision")
	}
	if status == nil {
		t.Fatal("routing explain recorded no ruleset-snapshot status")
	}
	if status.Rev != "org1@1" {
		t.Fatalf("recorded snapshot rev = %q, want org1@1", status.Rev)
	}
	if status.Degraded {
		t.Fatal("recorded snapshot status is degraded=true for a fresh snapshot")
	}
}

// TestFailStatic_TTLExpired_ClaimRefusesTyped proves the second acceptance
// criterion: once the cached snapshot's age exceeds RefuseAfter, the claim
// is refused with a loud, typed error naming the expiry — never a silent
// stall, never fail-open.
func TestFailStatic_TTLExpired_ClaimRefusesTyped(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateTestKey(t)
	compiledAt := time.Now().Add(-time.Hour) // already old at fetch time
	srv := newSnapshotTestServer(t, func() []byte {
		return buildSnapshotResponse(t, priv, snapshotFixture{
			poolID: "pool-expired", poolStatus: "active", grantedByProfile: true,
			hostID: "worker-local", hostStatus: "ready", harnessID: "codex",
			compiledAt: compiledAt,
		})
	})
	client, err := rulesetsnapshot.NewClient(rulesetsnapshot.Config{
		Endpoint: srv.URL, TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath:     filepath.Join(t.TempDir(), "snap.json"),
		DegradedAfter: 10 * time.Second,
		RefuseAfter:   time.Minute, // compiledAt is already an hour old
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	d := startSnapshotClaimGateDaemon(t, &stubPreflightOnlyProvider{}, client)
	admission, cell := snapshotBoundAdmission(t, "pool-expired")
	detail := snapshotDetail(t, "request-expired", cell, admission)

	_, err = d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "p1"}, detail)
	if err == nil {
		t.Fatal("a claim against an expired cached snapshot was accepted")
	}
	var expired *rulesetsnapshot.ExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("error = %v, want an *rulesetsnapshot.ExpiredError in its chain", err)
	}
}

// TestFailStatic_TamperedSnapshotNeverAdopted proves the fourth acceptance
// criterion end to end through the daemon: a client that only ever saw a
// tampered (bad-signature) response has NO usable cached snapshot, and the
// claim gate refuses rather than trusting it.
func TestFailStatic_TamperedSnapshotNeverAdopted(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateTestKey(t)
	srv := newSnapshotTestServer(t, func() []byte {
		return buildSnapshotResponse(t, priv, snapshotFixture{
			poolID: "pool-tampered", poolStatus: "active", grantedByProfile: true,
			hostID: "worker-local", hostStatus: "ready", harnessID: "codex",
			corruptSignature: true,
		})
	})
	client, err := rulesetsnapshot.NewClient(rulesetsnapshot.Config{
		Endpoint: srv.URL, TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath: filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh accepted a tampered snapshot")
	}

	d := startSnapshotClaimGateDaemon(t, &stubPreflightOnlyProvider{}, client)
	admission, cell := snapshotBoundAdmission(t, "pool-tampered")
	detail := snapshotDetail(t, "request-tampered", cell, admission)

	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "p1"}, detail); err == nil {
		t.Fatal("a claim was accepted with no verified cached snapshot")
	}
}

// TestFailStatic_PermissionRevokedDeniesEvenWithLiveProvider proves the
// permission re-check (the pool/host inventory and capacity-profile
// projection) overrides a Live provider that would otherwise say yes: a
// pool absent from every capacity profile's pool list is refused even
// though the wired Live stub reports full host-local availability.
func TestFailStatic_PermissionRevokedDeniesEvenWithLiveProvider(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateTestKey(t)
	srv := newSnapshotTestServer(t, func() []byte {
		return buildSnapshotResponse(t, priv, snapshotFixture{
			poolID: "pool-revoked", poolStatus: "active", grantedByProfile: false, // NOT granted
			hostID: "worker-local", hostStatus: "ready", harnessID: "codex",
		})
	})
	client, err := rulesetsnapshot.NewClient(rulesetsnapshot.Config{
		Endpoint: srv.URL, TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub},
		StatePath: filepath.Join(t.TempDir(), "snap.json"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	admission, cell := snapshotBoundAdmission(t, "pool-revoked")
	reality := executioncell.ClaimLocalReality{
		PlacementID: "worker-local", HarnessAvailable: true, EndpointReachable: true,
		AuthBindingAvailable: true, AvailableCapabilities: cell.GrantedCapabilities,
		EvidenceTier: cell.EvidenceTier, RuntimeInventoryDigest: strings.Repeat("e", 64),
	}
	liveProvider := &stubClaimGateProvider{realityJSON: mustMarshal(t, reality)}
	d := startSnapshotClaimGateDaemon(t, liveProvider, client)
	detail := snapshotDetail(t, "request-revoked", cell, admission)

	_, err = d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "p1"}, detail)
	if err == nil {
		t.Fatal("a claim against a pool absent from every capacity profile was accepted despite a permissive Live provider")
	}
	var refused *rulesetsnapshot.PermissionRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a *rulesetsnapshot.PermissionRefusedError in its chain", err)
	}
	if liveProvider.realityCalls.Load() != 0 {
		t.Fatalf("Live provider was consulted %d times despite an earlier permission refusal — it should never have been asked", liveProvider.realityCalls.Load())
	}
}

// TestFailStatic_Unconfigured_NoBehaviorChange pins the "no configured
// snapshot source, no behaviour change" invariant: a daemon with
// Options.RulesetSnapshot left nil behaves byte-identically to a daemon
// built without ever knowing this feature exists — including using the
// SAME code path (claimGateProvider falls through to the original type
// assertion) rather than merely producing the same externally-observed
// result.
func TestFailStatic_Unconfigured_NoBehaviorChange(t *testing.T) {
	t.Parallel()
	admission, cell := claimBoundAdmission(t)
	reality := executioncell.ClaimLocalReality{
		PlacementID: "worker-local", HarnessAvailable: true, EndpointReachable: true,
		AuthBindingAvailable: true, AvailableCapabilities: cell.GrantedCapabilities,
		EvidenceTier: cell.EvidenceTier, RuntimeInventoryDigest: strings.Repeat("e", 64),
	}
	provider := &stubClaimGateProvider{realityJSON: mustMarshal(t, reality)}
	d := startSnapshotClaimGateDaemon(t, provider, nil) // RulesetSnapshot: nil

	got, ok := d.claimGateProvider()
	if !ok {
		t.Fatal("claimGateProvider() ok=false with a wired Live provider and no snapshot source")
	}
	if _, wrapped := got.(*FailStaticClaimGateProvider); wrapped {
		t.Fatal("claimGateProvider() wrapped the Live provider despite Options.RulesetSnapshot being nil — an unconfigured daemon must take the ORIGINAL code path, not just an equivalent result")
	}

	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-claim-gate-1",
		WorkerID: "worker-local", PlacementID: "worker-local", ClaimID: "claim-active",
	}
	detail := &SessionDetail{
		SessionID: "request-claim-gate-1", WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), EffectiveCell: mustMarshal(t, cell),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
	}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "p1"}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	// The pre-existing (unrelated) OSS routing-decision recorder may still
	// record an ordinary LLM/sandbox decision for this session — that is
	// not this feature. What must never happen is THIS feature attaching a
	// ruleset-snapshot status when none is configured.
	if _, _, status, ok := d.RoutingTraces().ExplainWithSnapshot(detail.SessionID); ok && status != nil {
		t.Fatal("a ruleset-snapshot status was recorded for an unconfigured snapshot source — an unconfigured daemon must have no new side effect")
	}
}

func mustGenerateTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}
