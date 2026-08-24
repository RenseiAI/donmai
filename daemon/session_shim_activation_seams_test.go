package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func activationTestAttestation() SessionShimHostAttestation {
	return SessionShimHostAttestation{
		Supported: true, ControllerID: "controller-activation-test",
		ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Capabilities: RequiredSessionShimHostCapabilities(),
	}
}

func testSessionShimProofV2Readiness() (SessionShimCarrierProofV2Readiness, error) {
	return SessionShimCarrierProofV2Readiness{
		DurableCarrierProofV2Ready:          true,
		ComposingProofV1WritesClosed:        true,
		EncryptedOriginalCredentialRetained: true,
		RemainingValidityConsumeGate:        true,
		AdoptedCandidateRecovery:            true,
	}, nil
}

func activationTestCredentialReceipt(att SessionShimHostAttestation, state, host, revision string) *SessionShimCredentialReceipt {
	return &SessionShimCredentialReceipt{
		Enabled: true, State: state, WorkerHostID: host, AdoptionRevision: revision,
		ControllerID: att.ControllerID, ProtocolMin: att.ProtocolMin, ProtocolMax: att.ProtocolMax,
		Capabilities: append([]string(nil), att.Capabilities...),
	}
}

func assertFlatSessionShimAttestation(t *testing.T, raw []byte, att SessionShimHostAttestation) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request: %v (raw=%s)", err, raw)
	}
	wantCaps := make([]any, len(att.Capabilities))
	for i := range att.Capabilities {
		wantCaps[i] = att.Capabilities[i]
	}
	want := map[string]any{
		"sessionShimSupported": true, "sessionShimControllerId": att.ControllerID,
		"sessionShimProtocolMin": float64(att.ProtocolMin), "sessionShimProtocolMax": float64(att.ProtocolMax),
		"sessionShimCapabilities": wantCaps,
	}
	for key, value := range want {
		if !reflect.DeepEqual(body[key], value) {
			t.Errorf("%s = %#v, want %#v (body=%s)", key, body[key], value, raw)
		}
	}
	if _, nested := body["sessionShim"]; nested {
		t.Fatalf("request nested the flat attestation: %s", raw)
	}
}

func TestRegistrationAndEveryRefreshCarryExactSessionShimTuple(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	resetRefreshersForTest()
	t.Cleanup(resetRefreshersForTest)
	attestation := activationTestAttestation()
	var registerBody, refreshBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case RegisterEndpoint:
			registerBody = append([]byte(nil), raw...)
			_ = json.NewEncoder(w).Encode(RegisterResponse{
				WorkerID: "worker-activation", RuntimeToken: "runtime-register",
				HeartbeatInterval: 30_000, PollInterval: 10_000,
				SessionShim: activationTestCredentialReceipt(
					attestation, SessionShimCredentialStateRecovering, "stable-host-activation", "revision-register",
				),
			})
		case "/api/workers/worker-activation/refresh-token":
			refreshBody = append([]byte(nil), raw...)
			_ = json.NewEncoder(w).Encode(refreshResponse{
				RuntimeToken: "runtime-refresh", SessionShim: activationTestCredentialReceipt(
					attestation, SessionShimCredentialStateReady, "stable-host-activation", "revision-refresh",
				),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	opts := RegistrationOptions{
		OrchestratorURL: server.URL, RegistrationToken: "rsp_live_activation",
		Hostname: "host-label", MaxAgents: 1, JWTPath: filepath.Join(t.TempDir(), "daemon.jwt"),
		SessionShim: attestation, AuthOnly: true,
	}
	registered, err := Register(context.Background(), opts)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	refreshed, err := RefreshRuntimeToken(context.Background(), opts, registered.WorkerID, "test")
	if err != nil {
		t.Fatalf("RefreshRuntimeToken: %v", err)
	}
	assertFlatSessionShimAttestation(t, registerBody, attestation)
	assertFlatSessionShimAttestation(t, refreshBody, attestation)
	var registeredWire map[string]any
	_ = json.Unmarshal(registerBody, &registeredWire)
	if registeredWire["capacity"] != float64(0) {
		t.Fatalf("auth-only registration capacity = %v, want 0", registeredWire["capacity"])
	}
	if refreshed.SessionShim == nil || refreshed.SessionShim.AdoptionRevision != "revision-refresh" {
		t.Fatalf("refresh receipt = %+v", refreshed.SessionShim)
	}
	cached, err := LoadCachedJWT(opts.JWTPath)
	if err != nil || cached == nil || cached.SessionShim == nil || cached.SessionShim.AdoptionRevision != "revision-register" {
		t.Fatalf("registration cache receipt = %+v, %v", cached, err)
	}
}

func TestHostedRecoveryRefusesLegacyCacheAndPerformsAuthorityRoundTrip(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	attestation := activationTestAttestation()
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	if err := SaveCachedJWT(jwtPath, &RegisterResponse{
		WorkerID: "legacy-worker", RuntimeToken: "fresh-by-expiry", RuntimeTokenExpiresAt: "2099-01-01T00:00:00Z",
		HeartbeatInterval: 30_000, PollInterval: 10_000,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			WorkerID: "authoritative-worker", RuntimeToken: "authoritative-runtime",
			SessionShim: activationTestCredentialReceipt(
				attestation, SessionShimCredentialStateRecovering, "stable-host", "revision-current",
			),
		})
	}))
	t.Cleanup(server.Close)
	response, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: server.URL, RegistrationToken: "rsp_live_cache_refusal",
		Hostname: "host", MaxAgents: 1, JWTPath: jwtPath, SessionShim: attestation, AuthOnly: true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if calls.Load() != 1 || response.WorkerID != "authoritative-worker" {
		t.Fatalf("legacy cache was used: calls=%d response=%+v", calls.Load(), response)
	}
}

func TestRegistrationRefusesChangedSessionShimReceiptFields(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	attestation := activationTestAttestation()
	cases := map[string]func(*SessionShimCredentialReceipt){
		"disabled":         func(receipt *SessionShimCredentialReceipt) { receipt.Enabled = false },
		"bad state":        func(receipt *SessionShimCredentialReceipt) { receipt.State = "active" },
		"missing host":     func(receipt *SessionShimCredentialReceipt) { receipt.WorkerHostID = "" },
		"missing revision": func(receipt *SessionShimCredentialReceipt) { receipt.AdoptionRevision = "" },
		"controller":       func(receipt *SessionShimCredentialReceipt) { receipt.ControllerID = "changed" },
		"protocol range":   func(receipt *SessionShimCredentialReceipt) { receipt.ProtocolMax++ },
		"capability order": func(receipt *SessionShimCredentialReceipt) {
			receipt.Capabilities[0], receipt.Capabilities[1] = receipt.Capabilities[1], receipt.Capabilities[0]
		},
		"worker host alias": func(receipt *SessionShimCredentialReceipt) { receipt.WorkerHostID = "worker" },
	}
	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			receipt := activationTestCredentialReceipt(
				attestation, SessionShimCredentialStateRecovering, "stable-host", "revision",
			)
			mutate(receipt)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(RegisterResponse{
					WorkerID: "worker", RuntimeToken: "runtime", SessionShim: receipt,
				})
			}))
			t.Cleanup(server.Close)
			_, err := Register(context.Background(), RegistrationOptions{
				OrchestratorURL: server.URL, RegistrationToken: "rsp_live_changed_receipt",
				Hostname: "host", MaxAgents: 1, JWTPath: filepath.Join(t.TempDir(), "daemon.jwt"),
				SessionShim: attestation, AuthOnly: true,
			})
			if err == nil {
				t.Fatal("Register accepted changed session shim receipt")
			}
		})
	}
}

func TestRecoveryScopeHookRequiresExactCompleteNonSecretSet(t *testing.T) {
	attestation := activationTestAttestation()
	var d *Daemon
	d = New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		ControllerID:               attestation.ControllerID, AttestationCapabilities: attestation.Capabilities,
		OrgID: "org-primary", AdoptionBatchOrgIDs: []string{"org-primary", "org-secondary"},
		AcquireRecoveryScopes: func(_ context.Context, got SessionShimHostAttestation) ([]SessionShimScopeCredentialReceipt, error) {
			if !got.exactEqual(attestation) {
				return nil, errors.New("attestation changed")
			}
			return []SessionShimScopeCredentialReceipt{{
				Scope: "org-secondary", WorkerHostID: "host-secondary", AdoptionRevision: "revision-secondary",
			}}, nil
		},
	}})
	if d.sessionShimAttestationErr != nil {
		t.Fatal(d.sessionShimAttestationErr)
	}
	primary := activationTestCredentialReceipt(
		attestation, SessionShimCredentialStateRecovering, "host-primary", "revision-primary",
	)
	if err := d.acquireSessionShimRecoveryReceipts(context.Background(), primary); err != nil {
		t.Fatalf("acquireSessionShimRecoveryReceipts: %v", err)
	}
	if got := d.sessionShimCredentialReceipts(); !reflect.DeepEqual(got, []SessionShimScopeCredentialReceipt{
		{Scope: "org-primary", WorkerHostID: "host-primary", AdoptionRevision: "revision-primary"},
		{Scope: "org-secondary", WorkerHostID: "host-secondary", AdoptionRevision: "revision-secondary"},
	}) {
		t.Fatalf("scope receipts = %+v", got)
	}

	d = New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		ControllerID:               attestation.ControllerID, AttestationCapabilities: attestation.Capabilities,
		OrgID: "org-primary", AdoptionBatchOrgIDs: []string{"org-primary", "org-secondary"},
		AcquireRecoveryScopes: func(context.Context, SessionShimHostAttestation) ([]SessionShimScopeCredentialReceipt, error) {
			return nil, nil
		},
	}})
	if err := d.acquireSessionShimRecoveryReceipts(context.Background(), primary); err == nil {
		t.Fatal("partial multi-scope recovery set was accepted")
	}
}

func TestSupportedSessionShimAttestationRequiresCanonicalCompleteCapabilities(t *testing.T) {
	capabilities := RequiredSessionShimHostCapabilities()
	if !slices.Contains(capabilities, SessionShimCapabilityDurableCarrierProofV2) ||
		slices.Contains(capabilities, SessionShimCapabilityDurableCarrierProofV1) {
		t.Fatalf("new-admission capability tuple = %v, want proof-v2 only", capabilities)
	}
	for name, capabilities := range map[string][]string{
		"empty":     nil,
		"duplicate": {"a", "a"},
		"unsorted":  {"b", "a"},
		"blank":     {""},
	} {
		attestation := activationTestAttestation()
		attestation.Capabilities = capabilities
		if err := attestation.validate(); err == nil {
			t.Errorf("%s capability set was accepted", name)
		}
	}
	for name, mutate := range map[string]func(*SessionShimHostAttestation){
		"max below v3": func(attestation *SessionShimHostAttestation) { attestation.ProtocolMax = shimwire.V2 },
		"prior four-token set": func(attestation *SessionShimHostAttestation) {
			attestation.Capabilities = []string{
				SessionShimCapabilityAuthoritativeSnapshotV2,
				SessionShimCapabilityCarrierEpochPrepareCommit,
				SessionShimCapabilityFullHostFrameV3,
				SessionShimCapabilityInteractiveAttachV2,
			}
		},
		"missing capability": func(attestation *SessionShimHostAttestation) {
			attestation.Capabilities = attestation.Capabilities[:len(attestation.Capabilities)-1]
		},
		"unknown capability": func(attestation *SessionShimHostAttestation) {
			attestation.Capabilities[len(attestation.Capabilities)-1] = "unknown_host_capability"
		},
		"proof v1 only": func(attestation *SessionShimHostAttestation) {
			attestation.Capabilities[2] = SessionShimCapabilityDurableCarrierProofV1
		},
		"both proof tokens": func(attestation *SessionShimHostAttestation) {
			attestation.Capabilities = append(attestation.Capabilities, SessionShimCapabilityDurableCarrierProofV1)
			sort.Strings(attestation.Capabilities)
		},
	} {
		attestation := activationTestAttestation()
		mutate(&attestation)
		if err := attestation.validate(); err == nil {
			t.Errorf("%s attestation was accepted", name)
		}
	}
}

func TestProofV2ReadinessRequiresIndependentDurableAckAndEverySupportFact(t *testing.T) {
	ready, _ := testSessionShimProofV2Readiness()
	if err := ready.validate(); err != nil {
		t.Fatalf("complete proof-v2 readiness: %v", err)
	}
	raw, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{
		`"durable_carrier_proof_v2_ready":true`, `"composingProofV1WritesClosed":true`,
		`"encryptedOriginalCredentialRetained":true`, `"remainingValidityConsumeGate":true`,
		`"adoptedCandidateRecovery":true`,
	} {
		if !bytes.Contains(raw, []byte(member)) {
			t.Fatalf("proof-v2 readiness wire omitted %s: %s", member, raw)
		}
	}
	mutations := map[string]func(*SessionShimCarrierProofV2Readiness){
		"durable ack":                func(value *SessionShimCarrierProofV2Readiness) { value.DurableCarrierProofV2Ready = false },
		"v1 writes closed":           func(value *SessionShimCarrierProofV2Readiness) { value.ComposingProofV1WritesClosed = false },
		"credential retained":        func(value *SessionShimCarrierProofV2Readiness) { value.EncryptedOriginalCredentialRetained = false },
		"remaining validity gate":    func(value *SessionShimCarrierProofV2Readiness) { value.RemainingValidityConsumeGate = false },
		"adopted candidate recovery": func(value *SessionShimCarrierProofV2Readiness) { value.AdoptedCandidateRecovery = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			value := ready
			mutate(&value)
			if err := value.validate(); err == nil {
				t.Fatal("incomplete proof-v2 readiness was accepted")
			}
		})
	}
}

func TestProofV2ReadinessWithdrawalSuspendsPollingAndCandidateOperations(t *testing.T) {
	current, _ := testSessionShimProofV2Readiness()
	var prepareCalls atomic.Int64
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		ControllerID: "readiness-controller", AttestationCapabilities: RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: func() (SessionShimCarrierProofV2Readiness, error) { return current, nil },
		PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			prepareCalls.Add(1)
			resumeFrom := preparation.LastHostSeq + 1
			return SessionShimAdoptionPreparationResult{
				State: SessionShimPreparationFreshCandidate,
				PreparedAdoption: sessionshim.PreparedAdoption{
					ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "41"}},
					ResumeFrom:           &resumeFrom,
				},
			}, nil
		},
		OnAdoptionV2: func(context.Context, SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			return SessionShimAdoptionReceipt{}, nil
		},
	}})
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true

	mutations := map[string]func(*SessionShimCarrierProofV2Readiness){
		"durable acknowledgement":       func(value *SessionShimCarrierProofV2Readiness) { value.DurableCarrierProofV2Ready = false },
		"v1 writer closure":             func(value *SessionShimCarrierProofV2Readiness) { value.ComposingProofV1WritesClosed = false },
		"original credential retention": func(value *SessionShimCarrierProofV2Readiness) { value.EncryptedOriginalCredentialRetained = false },
		"remaining-validity gate":       func(value *SessionShimCarrierProofV2Readiness) { value.RemainingValidityConsumeGate = false },
		"adopted recovery":              func(value *SessionShimCarrierProofV2Readiness) { value.AdoptedCandidateRecovery = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			value, _ := testSessionShimProofV2Readiness()
			mutate(&value)
			current = value
			if suspended, _ := d.claimSuspended(); !suspended {
				t.Fatal("claim gate remained open after proof-v2 readiness withdrawal")
			}
		})
	}

	current, _ = testSessionShimProofV2Readiness()
	current.DurableCarrierProofV2Ready = false
	var pollCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pollCalls.Add(1)
		_ = json.NewEncoder(w).Encode(PollResponse{})
	}))
	t.Cleanup(server.Close)
	poller := NewPollService(PollOptions{
		WorkerID: "worker", RuntimeJWT: "runtime", OrchestratorURL: server.URL,
		ClaimSuspended: d.claimSuspended, OnWork: func(PollWorkItem) error { return nil }, HTTPClient: server.Client(),
	})
	poller.pollOnce(context.Background())
	if pollCalls.Load() != 0 || !poller.ClaimsSuspended() {
		t.Fatalf("poll withdrawal = calls:%d suspended:%v", pollCalls.Load(), poller.ClaimsSuspended())
	}
	preparation := sessionshim.AdoptionPreparation{
		Identity:     sessionshim.Identity{OrgID: "org", SessionID: "session"},
		ControllerID: d.ControllerID(), ShimID: "shim", ProcessEpoch: 3,
		CurrentControllerGeneration: 7, LastHostSeq: 11, LocalResumeFrom: 12, SelectedVersion: shimwire.V3,
	}
	if _, err := d.prepareSessionShimAdoption(context.Background(), "host", preparation); err == nil || prepareCalls.Load() != 0 {
		t.Fatalf("withdrawn preparation = calls:%d err:%v", prepareCalls.Load(), err)
	}
	if err := d.activatePublishedSessionShimCarriers(context.Background(), nil); err == nil {
		t.Fatal("withdrawn readiness allowed carrier activation")
	}

	current, _ = testSessionShimProofV2Readiness()
	if suspended, reason := d.claimSuspended(); suspended {
		t.Fatalf("restored readiness stayed suspended: %s", reason)
	}
	if _, err := d.prepareSessionShimAdoption(context.Background(), "host", preparation); err != nil || prepareCalls.Load() != 1 {
		t.Fatalf("restored preparation = calls:%d err:%v", prepareCalls.Load(), err)
	}
	if err := d.activatePublishedSessionShimCarriers(context.Background(), nil); err != nil {
		t.Fatalf("restored empty activation: %v", err)
	}
}

func testAdoptedCandidateRecoveryResult(t *testing.T, now time.Time) SessionShimAdoptionPreparationResult {
	t.Helper()
	credential, err := attachclient.NewV2RetainedCredential([]byte("original-adopted-candidate-bearer"))
	if err != nil {
		t.Fatal(err)
	}
	correlation, err := NewSessionShimRecoveryCorrelation([]byte("opaque-adopted-recovery-correlation"))
	if err != nil {
		t.Fatal(err)
	}
	resumeFrom := uint64(13)
	snapshot := attachwire.Frame{
		Type: attachwire.TypeSnapshot, Seq: 12,
		Payload: (attachwire.SnapshotEnvelope{
			AtSeq: 11, SnapFormat: attachwire.SnapFormatScreen, Snap: []byte("snapshot-never-json"),
		}).Encode(),
	}.Encode()
	return SessionShimAdoptionPreparationResult{
		State: SessionShimPreparationAdoptedCandidateRecovery,
		PreparedAdoption: sessionshim.PreparedAdoption{
			ControllerGeneration: 99, ResumeFrom: &resumeFrom,
			Extensions: shimwire.Extensions{
				Required: []string{shimwire.ExtCarrierEpoch},
				Values:   map[string]string{shimwire.ExtCarrierEpoch: "9"},
			},
		},
		AdoptedCandidateRecovery: &SessionShimAdoptedCandidateRecovery{
			Credential: credential, RecoveryCorrelation: correlation,
			CarrierEpoch: 9, PreStageAckSeq: 10, StagedHighWater: 12, ResumeFrom: 13,
			CredentialExpiresAt: now.Add(time.Hour),
			ResumeDisposition: attachclient.V2ResumeDisposition{
				ProofSchemaVersion: attachclient.V2ProofSchemaV2,
				Authority:          attachclient.V2ResumeAdoptedCandidateRecovery,
				State:              attachclient.V2ResumeReceiptStored,
				PTYEpoch:           3, CarrierEpoch: 9, AckSeq: 10,
				CandidateSnapshotSeq: 12, CandidateSnapshot: snapshot,
				GapFromSeq: 11, GapToSeq: 11, GapReason: attachwirev2.GapControllerUnforwarded,
			},
		},
	}
}

func TestAdoptedCandidateRecoveryIsExactOpaqueAndControllerGenerationIndependent(t *testing.T) {
	now := time.Now()
	result := testAdoptedCandidateRecoveryResult(t, now)
	if err := validateSessionShimAdoptionPreparationResult(result, now, 3); err != nil {
		t.Fatalf("valid adopted candidate recovery: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"original-adopted", "opaque-adopted", "snapshot-never-json", "PreparedAdoption", "AdoptedCandidateRecovery"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("typed recovery JSON leaked %q: %s", forbidden, encoded)
		}
	}
	for _, rendered := range []string{
		fmt.Sprint(result.AdoptedCandidateRecovery.Credential),
		fmt.Sprintf("%+v", result.AdoptedCandidateRecovery.RecoveryCorrelation),
		fmt.Sprintf("%+v", *result.AdoptedCandidateRecovery),
		fmt.Sprintf("%#v", result),
	} {
		if strings.Contains(rendered, "original-adopted") || strings.Contains(rendered, "opaque-adopted") ||
			strings.Contains(rendered, "snapshot-never-json") || !strings.Contains(rendered, "redacted") {
			t.Fatalf("typed recovery formatting leaked or omitted redaction: %q", rendered)
		}
	}

	for _, generation := range []shimwire.Generation{1, 999, ^shimwire.Generation(0) - 1} {
		d := New(Options{SessionShim: SessionShimConfig{
			EnableAdoption: true, RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
			AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
			GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
			PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
				if preparation.CurrentControllerGeneration != generation {
					t.Fatalf("controller generation = %d, want %d", preparation.CurrentControllerGeneration, generation)
				}
				resolved := cloneSessionShimAdoptionPreparationResult(result)
				resolved.PreparedAdoption.ControllerGeneration = generation + 1
				return resolved, nil
			},
		}})
		prepared, err := d.prepareSessionShimAdoption(context.Background(), "stable-host", sessionshim.AdoptionPreparation{
			Identity: sessionshim.Identity{OrgID: "org", SessionID: "session"}, ControllerID: d.ControllerID(),
			ShimID: "shim", ProcessEpoch: 3, CurrentControllerGeneration: generation,
			LocalResumeFrom: 11, LastHostSeq: 12, SelectedVersion: shimwire.V3,
		})
		if err != nil || prepared.State != SessionShimPreparationAdoptedCandidateRecovery ||
			prepared.PreparedAdoption.ResumeFrom == nil || *prepared.PreparedAdoption.ResumeFrom != 13 {
			t.Fatalf("generation %d recovery = %+v err=%v", generation, prepared, err)
		}
	}
	mismatch := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		PrepareAdoptionV2: func(context.Context, SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			return result, nil
		},
	}})
	if _, err := mismatch.prepareSessionShimAdoption(context.Background(), "stable-host", sessionshim.AdoptionPreparation{
		Identity: sessionshim.Identity{OrgID: "org", SessionID: "session"}, ControllerID: mismatch.ControllerID(),
		ShimID: "shim", ProcessEpoch: 4, CurrentControllerGeneration: 1,
		LocalResumeFrom: 11, LastHostSeq: 12, SelectedVersion: shimwire.V3,
	}); err == nil {
		t.Fatal("adopted-candidate recovery accepted a PTY epoch from another authenticated preparation")
	}
}

func TestAdoptedCandidateRecoveryRejectsRemintAndSecondCursorShapes(t *testing.T) {
	now := time.Now()
	mutations := map[string]func(*SessionShimAdoptionPreparationResult){
		"missing retained bearer": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.Credential = attachclient.V2RetainedCredential{}
		},
		"missing recovery correlation": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.RecoveryCorrelation = SessionShimRecoveryCorrelation{}
		},
		"expired original bearer": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.CredentialExpiresAt = now
		},
		"second cursor":           func(result *SessionShimAdoptionPreparationResult) { result.AdoptedCandidateRecovery.ResumeFrom++ },
		"changed candidate":       func(result *SessionShimAdoptionPreparationResult) { result.AdoptedCandidateRecovery.CarrierEpoch++ },
		"changed pre-stage ack":   func(result *SessionShimAdoptionPreparationResult) { result.AdoptedCandidateRecovery.PreStageAckSeq++ },
		"changed staged snapshot": func(result *SessionShimAdoptionPreparationResult) { result.AdoptedCandidateRecovery.StagedHighWater++ },
		"new proof correlation": func(result *SessionShimAdoptionPreparationResult) {
			result.PreparedAdoption.Correlation = []byte("new-proof-or-receipt")
		},
		"changed carrier extension": func(result *SessionShimAdoptionPreparationResult) {
			result.PreparedAdoption.Extensions.Values[shimwire.ExtCarrierEpoch] = "10"
		},
		"changed pty epoch": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.ResumeDisposition.PTYEpoch++
		},
		"proof v1": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.ResumeDisposition.ProofSchemaVersion = attachclient.V2ProofSchemaV1
		},
		"same-handoff authority": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.ResumeDisposition.Authority = attachclient.V2ResumeSameHandoff
		},
		"active changed-controller rebind": func(result *SessionShimAdoptionPreparationResult) {
			result.AdoptedCandidateRecovery.ResumeDisposition.State = attachclient.V2ResumeActive
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			result := testAdoptedCandidateRecoveryResult(t, now)
			mutate(&result)
			if err := validateSessionShimAdoptionPreparationResult(result, now, 3); err == nil {
				t.Fatal("invalid adopted-candidate recovery was accepted")
			}
		})
	}
}

func TestPrepareAdoptionCallbacksAreMutuallyExclusive(t *testing.T) {
	cfg := SessionShimConfig{
		PrepareAdoption: func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			return sessionshim.PreparedAdoption{}, nil
		},
		PrepareAdoptionV2: func(context.Context, SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			return SessionShimAdoptionPreparationResult{}, nil
		},
		OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			return SessionShimAdoptionReceipt{}, nil
		},
		OnAdoptionV2: func(context.Context, SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			return SessionShimAdoptionReceipt{}, nil
		},
	}
	if err := cfg.validateSnapshotCarrier(); err == nil {
		t.Fatal("old and new prepare callbacks were accepted together")
	}
}

func TestOnAdoptionV2ReceivesExactEphemeralRecoveryAuthority(t *testing.T) {
	now := time.Now()
	prepared := testAdoptedCandidateRecoveryResult(t, now)
	originalSnapshot := append([]byte(nil), prepared.AdoptedCandidateRecovery.ResumeDisposition.CandidateSnapshot...)
	var calls, snapshotAuthorityCalls int
	d := New(Options{SessionShim: SessionShimConfig{
		OnAdoptionV2: func(_ context.Context, got SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			calls++
			if got.Evidence.SnapshotProxy != nil {
				snapshotAuthorityCalls++
			}
			if got.PreparationResult.State != SessionShimPreparationAdoptedCandidateRecovery {
				return SessionShimAdoptionReceipt{}, fmt.Errorf("recovery evidence state changed: %+v", got)
			}
			token, err := got.PreparationResult.AdoptedCandidateRecovery.Credential.TokenSource()(context.Background())
			if err != nil || token != "original-adopted-candidate-bearer" {
				return SessionShimAdoptionReceipt{}, fmt.Errorf("original credential changed: token=%q err=%v", token, err)
			}
			got.PreparationResult.AdoptedCandidateRecovery.ResumeDisposition.CandidateSnapshot[0] ^= 0xff
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("exact-replayed-adoption")}, nil
		},
	}})
	receipt, err := d.completeSessionShimAdoption(context.Background(), SessionShimAdoptionEvidence{
		CarrierCompatible: true,
	}, prepared)
	if err != nil || calls != 1 || string(receipt.DurableCorrelation) != "exact-replayed-adoption" {
		t.Fatalf("OnAdoptionV2 calls=%d receipt=%+v err=%v", calls, receipt, err)
	}
	if !bytes.Equal(prepared.AdoptedCandidateRecovery.ResumeDisposition.CandidateSnapshot, originalSnapshot) {
		t.Fatal("OnAdoptionV2 received a mutable alias of the retained Snapshot")
	}
	if snapshotAuthorityCalls != 0 {
		t.Fatal("adopted-candidate recovery callback received new Snapshot authority")
	}
	if _, err := d.completeSessionShimAdoption(context.Background(), SessionShimAdoptionEvidence{
		CarrierCompatible: true, SnapshotProxy: &SessionShimSnapshotProxy{},
	}, prepared); err == nil || snapshotAuthorityCalls != 0 || calls != 1 {
		t.Fatal("adopted-candidate recovery accepted new Snapshot authority")
	}
}

func TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot(t *testing.T) {
	first := newShimSpawnFixture(t)
	spec := first.interactiveSpec("adopted-recovery-activation")
	if _, err := first.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("initial AcceptWork: %v", err)
	}
	id := first.identity(spec.SessionID)
	initialHighWater := first.exchange(t, id, "before-controller-replacement")
	if initialHighWater == 0 {
		t.Fatal("initial controller did not establish a positive durable high-water")
	}
	first.daemon.ReleaseAdoptedSessionShims()

	var (
		secondSnapshots atomic.Int64
		batchCalls      int
		activationCalls int
		retained        SessionShimCarrierActivationReceipt
	)
	replacement := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: first.registry,
		ControllerID: "replacement-controller", HostID: "stable-recovery-host",
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			highWater := preparation.LastHostSeq
			if highWater < initialHighWater || highWater == 0 {
				return SessionShimAdoptionPreparationResult{}, fmt.Errorf(
					"replacement Hello high-water = %d, want at least %d", highWater, initialHighWater)
			}
			credential, err := attachclient.NewV2RetainedCredential([]byte("exact-original-recovery-bearer"))
			if err != nil {
				return SessionShimAdoptionPreparationResult{}, err
			}
			correlation, err := NewSessionShimRecoveryCorrelation([]byte("exact-recovery-correlation"))
			if err != nil {
				return SessionShimAdoptionPreparationResult{}, err
			}
			resumeFrom := highWater + 1
			snapshot := attachwire.Frame{
				Type: attachwire.TypeSnapshot, Seq: highWater,
				Payload: (attachwire.SnapshotEnvelope{
					AtSeq: highWater - 1, SnapFormat: attachwire.SnapFormatScreen, Snap: []byte("retained"),
				}).Encode(),
			}.Encode()
			return SessionShimAdoptionPreparationResult{
				State: SessionShimPreparationAdoptedCandidateRecovery,
				PreparedAdoption: sessionshim.PreparedAdoption{
					ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					Extensions: shimwire.Extensions{
						Required: []string{shimwire.ExtCarrierEpoch},
						Values:   map[string]string{shimwire.ExtCarrierEpoch: "29"},
					},
					ResumeFrom: &resumeFrom,
				},
				AdoptedCandidateRecovery: &SessionShimAdoptedCandidateRecovery{
					Credential: credential, RecoveryCorrelation: correlation,
					CarrierEpoch: 29, PreStageAckSeq: highWater - 1,
					StagedHighWater: highWater, ResumeFrom: resumeFrom,
					CredentialExpiresAt: time.Now().Add(time.Hour),
					ResumeDisposition: attachclient.V2ResumeDisposition{
						ProofSchemaVersion: attachclient.V2ProofSchemaV2,
						Authority:          attachclient.V2ResumeAdoptedCandidateRecovery,
						State:              attachclient.V2ResumeReceiptStored,
						PTYEpoch:           preparation.ProcessEpoch, CarrierEpoch: 29,
						AckSeq: highWater - 1, CandidateSnapshotSeq: highWater,
						CandidateSnapshot: snapshot,
					},
				},
			}, nil
		},
		OnAdoptionV2: func(_ context.Context, got SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			if got.Evidence.SnapshotProxy != nil || got.PreparationResult.State != SessionShimPreparationAdoptedCandidateRecovery {
				return SessionShimAdoptionReceipt{}, errors.New("recovered adoption gained fresh Snapshot authority")
			}
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("replayed-adoption")}, nil
		},
		OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
			if event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeSnapshot {
				secondSnapshots.Add(1)
			}
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			batchCalls++
			if len(batch.Adopted) != 1 || batch.Adopted[0].RetainedCarrierActivation == nil {
				return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("batch retained activation = %+v", batch.Adopted)
			}
			retained = *batch.Adopted[0].RetainedCarrierActivation
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("replayed-batch"), AdoptionRevision: "replayed-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			activationCalls++
			if len(publication.Carriers) != 1 || publication.Carriers[0] != retained.Activation {
				return nil, fmt.Errorf("publication carriers = %+v, retained = %+v", publication.Carriers, retained)
			}
			return []SessionShimCarrierActivationReceipt{retained}, nil
		},
	}})
	enableHostedFullHostFramesForTest(t, replacement, id.OrgID)
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("replacement adoption: %v", err)
	}
	if batchCalls != 1 || activationCalls != 1 || secondSnapshots.Load() != 0 ||
		!replacement.SessionShimCarrierActivationComplete() {
		t.Fatalf("recovery completion = batch:%d activation:%d snapshots:%d complete:%v",
			batchCalls, activationCalls, secondSnapshots.Load(), replacement.SessionShimCarrierActivationComplete())
	}
	if retained.Activation != (SessionShimCarrierActivation{OrgID: id.OrgID, SessionID: id.SessionID, CarrierEpoch: 29}) ||
		retained.AckSeq < initialHighWater {
		t.Fatalf("retained carrier activation = %+v, initial high-water = %d", retained, initialHighWater)
	}
	replacement.shims.mu.RLock()
	pending, staging, gates := len(replacement.shims.pendingSnapshots), len(replacement.shims.stagingSnapshots), len(replacement.shims.activationGates)
	entry := replacement.shims.adopted[id]
	replacement.shims.mu.RUnlock()
	if pending != 0 || staging != 0 || gates != 0 || entry.retainedCarrierActivation == nil ||
		*entry.retainedCarrierActivation != retained || !entry.carrierActivationComplete {
		t.Fatalf("retained activation state = pending:%d staging:%d gates:%d entry:%+v", pending, staging, gates, entry.retainedCarrierActivation)
	}
	if got := replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID); got < retained.AckSeq {
		t.Fatalf("replacement forwarded cursor = %d, want at least retained carrier_active ack %d", got, retained.AckSeq)
	}
	replacement.shims.mu.RLock()
	replayedEntries := make(map[sessionshim.Identity]adoptedShim, len(replacement.shims.adopted))
	for identity, adopted := range replacement.shims.adopted {
		replayedEntries[identity] = adopted
	}
	replacement.shims.mu.RUnlock()
	if err := replacement.activatePublishedSessionShimCarriers(context.Background(), replayedEntries); err != nil {
		t.Fatalf("already-active retained carrier exclusion: %v", err)
	}
	if activationCalls != 1 || secondSnapshots.Load() != 0 {
		t.Fatalf("already-active retained carrier = calls:%d snapshots:%d", activationCalls, secondSnapshots.Load())
	}
	if err := replacement.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("after-recovery\r")); err != nil {
		t.Fatalf("post-activation input: %v", err)
	}
}

func TestFutureProtocolMaxRequiresAndAcceptsOnlyItsExactReceiptEcho(t *testing.T) {
	attestation := activationTestAttestation()
	attestation.ProtocolMax = shimwire.V3 + 1
	receipt := activationTestCredentialReceipt(
		attestation, SessionShimCredentialStateRecovering, "stable-host", "revision",
	)
	if err := validateSessionShimCredentialReceipt(attestation, receipt, "worker"); err != nil {
		t.Fatalf("future max exact echo: %v", err)
	}
	receipt.ProtocolMax = shimwire.V3
	if err := validateSessionShimCredentialReceipt(attestation, receipt, "worker"); err == nil {
		t.Fatal("future max attestation accepted a downgraded receipt echo")
	}
}

func TestZeroValueSessionShimPreservesRegistrationAndRefreshBytes(t *testing.T) {
	request, err := json.Marshal(RegisterRequest{Hostname: "h", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(request) != `{"hostname":"h","capacity":1}` {
		t.Fatalf("zero-value RegisterRequest bytes = %s", request)
	}
	var refreshBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(refreshResponse{RuntimeToken: "runtime"})
	}))
	t.Cleanup(server.Close)
	if _, err := callRefreshEndpoint(context.Background(), RegistrationOptions{
		OrchestratorURL: server.URL, RegistrationToken: "rsp_live_zero", HTTPClient: server.Client(),
	}, "worker"); err != nil {
		t.Fatal(err)
	}
	if string(refreshBody) != `{}` {
		t.Fatalf("zero-value refresh body = %s, want {}", refreshBody)
	}
	statusBytes, err := json.Marshal(New(Options{SkipRegistration: true}).SessionShimDiagnostics())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(statusBytes, []byte("carrierActivationComplete")) {
		t.Fatalf("zero-value daemon status gained activation bytes: %s", statusBytes)
	}
}

func TestHeartbeatSessionShimProjectionIsCoherentAndRequiresExactEcho(t *testing.T) {
	projection := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true,
		WorkerHostID: "stable-host", ControllerID: "controller", AdoptionRevision: "revision",
		QuarantinedSessions: []SessionShimQuarantinedSession{{
			OrgID: "org", SessionID: "session", ShimID: "shim", ProcessEpoch: 4,
			ControllerGeneration: "18446744073709551615", ProtocolMin: 1, ProtocolMax: 1,
			Reason: "protocol_mismatch", AgeSeconds: 5, ConsumesCapacity: true,
		}},
	}
	var rawBody []byte
	var changedEcho atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		echo := projection
		if changedEcho.Load() {
			echo.AdoptionRevision = "changed"
		}
		_ = json.NewEncoder(w).Encode(heartbeatResponseBody{Acknowledged: true, SessionShim: &echo})
	}))
	t.Cleanup(server.Close)
	service := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker", OrchestratorURL: server.URL, RuntimeJWT: "runtime",
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationDraining },
		GetSessionShim: func() (SessionShimHeartbeatProjection, error) { return projection, nil },
		HTTPClient:     server.Client(),
	})
	if err := service.sendOneResult(context.Background()); err != nil {
		t.Fatalf("sendOne: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body["sessionShim"], []byte("carrierActivationComplete")) {
		t.Fatalf("heartbeat invented carrierActivationComplete wire key: %s", body["sessionShim"])
	}
	if !bytes.Contains(body["sessionShim"], []byte(`"controllerGeneration":"18446744073709551615"`)) {
		t.Fatalf("controllerGeneration was not a canonical decimal string: %s", body["sessionShim"])
	}

	changedEcho.Store(true)
	if err := service.sendOneResult(context.Background()); err == nil {
		t.Fatal("heartbeat accepted a changed adoption revision echo")
	}
}

func TestDaemonStartAuthOnlyOrderingBeforeAdoptionHeartbeatAndPoll(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var (
		mu             sync.Mutex
		order          []string
		d              *Daemon
		readinessCalls atomic.Int64
		readinessOK    atomic.Bool
	)
	readinessOK.Store(true)
	record := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	attestation := activationTestAttestation()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case RegisterEndpoint:
			if readinessCalls.Load() != 1 {
				t.Errorf("proof-v2 readiness calls before registration = %d, want 1", readinessCalls.Load())
			}
			raw, _ := io.ReadAll(r.Body)
			assertFlatSessionShimAttestation(t, raw, attestation)
			var request RegisterRequest
			if err := json.Unmarshal(raw, &request); err != nil || request.Capacity != 0 {
				t.Errorf("auth-only registration capacity = %d, err=%v; want 0", request.Capacity, err)
			}
			record("register")
			_ = json.NewEncoder(w).Encode(RegisterResponse{
				WorkerID: "worker-order", RuntimeToken: "runtime-order",
				HeartbeatInterval: 3_600_000, PollInterval: 3_600_000,
				SessionShim: activationTestCredentialReceipt(
					attestation, SessionShimCredentialStateRecovering, "stable-host-order", "revision-register",
				),
			})
		case "/api/workers/worker-order/heartbeat":
			var body heartbeatRequestBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode heartbeat: %v", err)
			}
			record("heartbeat")
			_ = json.NewEncoder(w).Encode(heartbeatResponseBody{Acknowledged: true, SessionShim: body.SessionShim})
		case "/api/workers/worker-order/poll":
			record("poll")
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	cfg := DefaultConfig()
	cfg.Machine.ID = "auth-only-order"
	cfg.Orchestrator.URL = server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_auth_only"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	d = New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"), SkipWizard: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true, RequireCredentialAttestation: true,
			GetCarrierProofV2Readiness: func() (SessionShimCarrierProofV2Readiness, error) {
				readinessCalls.Add(1)
				ready, _ := testSessionShimProofV2Readiness()
				ready.DurableCarrierProofV2Ready = readinessOK.Load()
				return ready, nil
			},
			ControllerID: attestation.ControllerID, AttestationCapabilities: attestation.Capabilities,
			OrgID: "org-order", RegistryDir: filepath.Join(dir, "registry"),
			OnAdoptionBatch: func(_ context.Context, _ SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				if d.State() != StateRecovering || d.spawner != nil || d.heartbeat != nil || d.poller != nil {
					return SessionShimAdoptionBatchReceipt{}, errors.New("serving lane started during auth-only recovery")
				}
				record("batch")
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("batch-order"), AdoptionRevision: "revision-batch",
				}, nil
			},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seenPoll := false
		for _, step := range order {
			seenPoll = seenPoll || step == "poll"
		}
		mu.Unlock()
		if seenPoll {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) < 4 || !reflect.DeepEqual(got[:4], []string{"register", "batch", "heartbeat", "poll"}) {
		t.Fatalf("startup order = %v, want register -> batch -> heartbeat -> poll", got)
	}
	if d.State() != StateRunning || !d.SessionShimAdoptionComplete() || !d.SessionShimCarrierActivationComplete() {
		t.Fatalf("daemon readiness = state:%s adoption:%v activation:%v", d.State(), d.SessionShimAdoptionComplete(), d.SessionShimCarrierActivationComplete())
	}
	if readinessCalls.Load() < 2 {
		t.Fatalf("proof-v2 readiness was not checked at registration and heartbeat: calls=%d", readinessCalls.Load())
	}
	readinessOK.Store(false)
	if _, err := d.SessionShimHeartbeatProjection("org-order"); err == nil {
		t.Fatal("heartbeat projection remained eligible after durable proof-v2 readiness was withdrawn")
	}
	if err := d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{SessionShim: &SessionShimCredentialReceipt{
		State: SessionShimCredentialStateReady, WorkerHostID: "stable-host-order", AdoptionRevision: "revision-refresh",
	}}); err == nil {
		t.Fatal("refresh remained eligible after durable proof-v2 readiness was withdrawn")
	}
}

func TestCarrierActivationExactSetAndAckResolvePendingSnapshot(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org", SessionID: "session"}
	carrier := SessionShimCarrierActivation{OrgID: id.OrgID, SessionID: id.SessionID, CarrierEpoch: 7}
	returned := []SessionShimCarrierActivationReceipt{{Activation: carrier, AckSeq: 12}}
	var d *Daemon
	d = New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, ControllerID: "controller", OnAdoptionPublished: func(_ context.Context, _ SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			if !d.SessionShimAdoptionComplete() {
				return nil, errors.New("activation ran before local publication")
			}
			return append([]SessionShimCarrierActivationReceipt(nil), returned...), nil
		},
	}})
	d.shims.adoptionComplete = true
	d.shims.batchReceipts[id.OrgID] = SessionShimAdoptionBatchReceipt{
		DurableCorrelation: []byte("batch"), AdoptionRevision: "revision",
	}
	d.shims.adopted[id] = adoptedShim{adoption: SessionShimAdoptionEvidence{
		Identity: id, CarrierCompatible: true,
		Extensions: shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "7"}},
	}}
	d.shims.pendingSnapshots[id] = sessionshim.ControllerEvent{
		Kind: sessionshim.EventHostFrame, FrameType: attachwire.TypeSnapshot, RequestID: 99, Seq: 12,
	}
	d.shims.activationGates[id] = newShimAdoptionGate()
	entries := map[sessionshim.Identity]adoptedShim{id: d.shims.adopted[id]}

	returned = []SessionShimCarrierActivationReceipt{{
		Activation: SessionShimCarrierActivation{OrgID: id.OrgID, SessionID: id.SessionID, CarrierEpoch: 8},
		AckSeq:     12,
	}}
	if err := d.activatePublishedSessionShimCarriers(context.Background(), entries); err == nil {
		t.Fatal("activation accepted a changed carrier correlation")
	}
	returned = nil
	if err := d.activatePublishedSessionShimCarriers(context.Background(), entries); err == nil {
		t.Fatal("activation accepted an incomplete returned carrier set")
	}
	if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("incomplete set advanced forwarded cursor = %d", got)
	}
	returned = []SessionShimCarrierActivationReceipt{{Activation: carrier, AckSeq: 11}}
	if err := d.activatePublishedSessionShimCarriers(context.Background(), entries); err == nil {
		t.Fatal("activation accepted ack before the staged Snapshot sequence")
	}
	if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("premature forwarded cursor = %d", got)
	}
	returned[0].AckSeq = 12
	if err := d.activatePublishedSessionShimCarriers(context.Background(), entries); err != nil {
		t.Fatalf("exact activation: %v", err)
	}
	if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 12 {
		t.Fatalf("forwarded cursor after exact carrier_active = %d, want 12", got)
	}
	if !d.SessionShimCarrierActivationComplete() {
		t.Fatal("carrierActivationComplete remained false after exact complete set")
	}
}

type gatedSessionShimCursorAck struct {
	started chan uint64
	release chan struct{}
}

func (*gatedSessionShimCursorAck) SupportsFullHostFrames() bool { return true }

func (g *gatedSessionShimCursorAck) Heartbeat(sequence uint64) error {
	g.started <- sequence
	<-g.release
	return nil
}

func TestForwardedCursorWaitsForExactShimPersistenceReceipt(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	id := sessionshim.Identity{OrgID: "org-ack-gate", SessionID: "session-ack-gate"}
	ack := &gatedSessionShimCursorAck{started: make(chan uint64, 1), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- d.recordShimForwardedSeqForController(id, ack, 42) }()
	select {
	case sequence := <-ack.started:
		if sequence != 42 {
			t.Fatalf("persistence request sequence = %d, want 42", sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("cursor persistence was not requested")
	}
	if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("forwarded cursor advanced before persistence receipt = %d", got)
	}
	select {
	case err := <-done:
		t.Fatalf("cursor update returned before persistence receipt: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(ack.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 42 {
		t.Fatalf("forwarded cursor after persistence receipt = %d, want 42", got)
	}
}

func TestHeartbeatQuarantineOrderingUsesNumericControllerGeneration(t *testing.T) {
	projection := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true, WorkerHostID: "host", ControllerID: "controller", AdoptionRevision: "revision",
		QuarantinedSessions: []SessionShimQuarantinedSession{
			{OrgID: "org", SessionID: "session", ShimID: "shim", ProcessEpoch: 1, ControllerGeneration: "2", Reason: "x", ConsumesCapacity: true},
			{OrgID: "org", SessionID: "session", ShimID: "shim", ProcessEpoch: 1, ControllerGeneration: "10", Reason: "x", ConsumesCapacity: true},
		},
	}
	if err := projection.validateReady(); err != nil {
		t.Fatalf("numeric quarantine order: %v", err)
	}
	sort.Slice(projection.QuarantinedSessions, func(i, j int) bool {
		return sessionShimQuarantineLess(projection.QuarantinedSessions[i], projection.QuarantinedSessions[j])
	})
	if projection.QuarantinedSessions[0].ControllerGeneration != "2" {
		t.Fatalf("numeric order = %+v", projection.QuarantinedSessions)
	}
}
