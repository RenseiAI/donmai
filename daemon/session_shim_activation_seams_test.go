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
		Supported: SessionShimSupported, ControllerID: "controller-activation-test",
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
		AcquireRecoveryScopes: func(_ context.Context, got SessionShimHostAttestation, primary SessionShimScopeCredentialReceipt) ([]SessionShimScopeCredentialReceipt, error) {
			if !got.exactEqual(attestation) {
				return nil, errors.New("attestation changed")
			}
			if primary != (SessionShimScopeCredentialReceipt{
				Scope: "org-primary", WorkerHostID: "host-primary", AdoptionRevision: "revision-primary",
			}) {
				return nil, fmt.Errorf("primary receipt = %+v", primary)
			}
			return []SessionShimScopeCredentialReceipt{{
				Scope: "org-secondary", WorkerHostID: "host-secondary", AdoptionRevision: "revision-secondary",
			}}, nil
		},
	}})
	if d.sessionShimAttestationError() != nil {
		t.Fatal(d.sessionShimAttestationError())
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
		AcquireRecoveryScopes: func(context.Context, SessionShimHostAttestation, SessionShimScopeCredentialReceipt) ([]SessionShimScopeCredentialReceipt, error) {
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

func TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail(t *testing.T) {
	mutations := map[string]func(*SessionShimCarrierProofV2Readiness){
		"durable acknowledgement":       func(value *SessionShimCarrierProofV2Readiness) { value.DurableCarrierProofV2Ready = false },
		"v1 writer closure":             func(value *SessionShimCarrierProofV2Readiness) { value.ComposingProofV1WritesClosed = false },
		"original credential retention": func(value *SessionShimCarrierProofV2Readiness) { value.EncryptedOriginalCredentialRetained = false },
		"remaining validity gate":       func(value *SessionShimCarrierProofV2Readiness) { value.RemainingValidityConsumeGate = false },
		"adopted recovery":              func(value *SessionShimCarrierProofV2Readiness) { value.AdoptedCandidateRecovery = false },
	}
	newDaemon := func(t *testing.T, mutate func(*SessionShimCarrierProofV2Readiness)) *Daemon {
		t.Helper()
		readiness, _ := testSessionShimProofV2Readiness()
		mutate(&readiness)
		d := New(Options{SessionShim: SessionShimConfig{
			EnableAdoption: true, RequireCredentialAttestation: true,
			ControllerID: "live-readiness-controller", AttestationCapabilities: RequiredSessionShimHostCapabilities(),
			GetCarrierProofV2Readiness: func() (SessionShimCarrierProofV2Readiness, error) { return readiness, nil },
		}})
		d.setState(StateRunning)
		d.shims.adoptionComplete = true
		d.shims.carrierActivationComplete = true
		d.spawner = NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: 1})
		d.spawner.Resume()
		return d
	}
	assertWithdrawn := func(t *testing.T, d *Daemon) {
		t.Helper()
		if !d.sessionShimReadinessWithdrawn.Load() || d.State() != StateRecovering ||
			d.spawner.IsAccepting() || d.RegistrationStatus() != RegistrationDraining {
			t.Fatalf("withdrawal state = withdrawn:%v state:%s accepting:%v registration:%s",
				d.sessionShimReadinessWithdrawn.Load(), d.State(), d.spawner.IsAccepting(), d.RegistrationStatus())
		}
	}
	for fact, mutate := range mutations {
		t.Run(fact+"/claim-and-poll", func(t *testing.T) {
			d := newDaemon(t, mutate)
			if blocked, _ := d.PollClaimGate()(); !blocked {
				t.Fatal("live claim check stayed open")
			}
			assertWithdrawn(t, d)
			var pollCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				pollCalls.Add(1)
				_ = json.NewEncoder(w).Encode(PollResponse{})
			}))
			t.Cleanup(server.Close)
			poller := NewPollService(PollOptions{
				WorkerID: "worker", RuntimeJWT: "runtime", OrchestratorURL: server.URL,
				ClaimSuspended: d.PollClaimGate(), OnWork: func(PollWorkItem) error { return nil }, HTTPClient: server.Client(),
			})
			poller.pollOnce(context.Background())
			if pollCalls.Load() != 0 || !poller.ClaimsSuspended() {
				t.Fatalf("poll withdrawal = calls:%d suspended:%v", pollCalls.Load(), poller.ClaimsSuspended())
			}
		})
		t.Run(fact+"/direct-admission", func(t *testing.T) {
			d := newDaemon(t, mutate)
			if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: "must-refuse"}, nil); err == nil {
				t.Fatal("live direct admission check stayed open")
			}
			assertWithdrawn(t, d)
		})
		t.Run(fact+"/activation", func(t *testing.T) {
			d := newDaemon(t, mutate)
			if err := d.activatePublishedSessionShimCarriers(context.Background(), nil); err == nil {
				t.Fatal("live activation check stayed open")
			}
			assertWithdrawn(t, d)
		})
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
	if err := validateSessionShimAdoptionPreparationResult(result, 3, now); err != nil {
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
}

func TestAdoptedCandidateRecoveryAcceptsExplicitServerRetainedDisposition(t *testing.T) {
	now := time.Now()
	result := testAdoptedCandidateRecoveryResult(t, now)
	resume := &result.AdoptedCandidateRecovery.ResumeDisposition
	resume.State = attachclient.V2ResumeServerRetained
	resume.CandidateSnapshot = nil

	if err := validateSessionShimAdoptionPreparationResult(result, 3, now); err != nil {
		t.Fatalf("server-retained adopted candidate recovery: %v", err)
	}
	cloned := cloneSessionShimAdoptionPreparationResult(result)
	if cloned.AdoptedCandidateRecovery.ResumeDisposition.State != attachclient.V2ResumeServerRetained ||
		len(cloned.AdoptedCandidateRecovery.ResumeDisposition.CandidateSnapshot) != 0 {
		t.Fatal("server-retained recovery grew client-retained Snapshot authority")
	}
}

func TestAdoptedCandidateRecoveryRequiresAuthenticatedLivePTYEpoch(t *testing.T) {
	const liveProcessEpoch = uint64(3)
	for _, test := range []struct {
		name       string
		ptyEpoch   uint64
		wantRefuse bool
	}{
		{name: "exact", ptyEpoch: liveProcessEpoch},
		{name: "zero", ptyEpoch: 0, wantRefuse: true},
		{name: "lower_3_to_2", ptyEpoch: 2, wantRefuse: true},
		{name: "higher_3_to_4", ptyEpoch: 4, wantRefuse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := testAdoptedCandidateRecoveryResult(t, time.Now())
			result.AdoptedCandidateRecovery.ResumeDisposition.PTYEpoch = test.ptyEpoch
			d := New(Options{SessionShim: SessionShimConfig{
				EnableAdoption: true, RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
				AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
				GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
				PrepareAdoptionV2: func(context.Context, SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
					return cloneSessionShimAdoptionPreparationResult(result), nil
				},
			}})
			_, err := d.prepareSessionShimAdoption(context.Background(), "stable-host", sessionshim.AdoptionPreparation{
				Identity: sessionshim.Identity{OrgID: "org", SessionID: "session"}, ControllerID: d.ControllerID(),
				ShimID: "shim", ProcessEpoch: liveProcessEpoch, CurrentControllerGeneration: 7,
				LocalResumeFrom: 11, LastHostSeq: 12, SelectedVersion: shimwire.V3,
			})
			if test.wantRefuse && err == nil {
				t.Fatalf("adopted-candidate recovery accepted live PTY epoch %d with disposition epoch %d", liveProcessEpoch, test.ptyEpoch)
			}
			if !test.wantRefuse && err != nil {
				t.Fatalf("exact live PTY epoch was refused: %v", err)
			}
		})
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
			if err := validateSessionShimAdoptionPreparationResult(result, 3, now); err == nil {
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

func TestHostedSnapshotCarrierRequiresHeartbeatDelayedLocalRelease(t *testing.T) {
	cfg := SessionShimConfig{
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		PrepareAdoption: func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			return sessionshim.PreparedAdoption{}, nil
		},
		OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			return SessionShimAdoptionReceipt{}, nil
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{}, nil
		},
		OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			return nil, nil
		},
	}
	if err := cfg.validateSnapshotCarrier(); err == nil || !strings.Contains(err.Error(), "OnCarrierActivationAcknowledged") {
		t.Fatalf("hosted config without delayed release hook error = %v", err)
	}
	cfg.OnCarrierActivationAcknowledged = func(SessionShimPublishedBatchReceipt) {}
	if err := cfg.validateSnapshotCarrier(); err != nil {
		t.Fatalf("complete hosted delayed release config: %v", err)
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
	readiness, _ := testSessionShimProofV2Readiness()
	projection := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true,
		WorkerHostID: "stable-host", ControllerID: "controller", AdoptionRevision: "revision",
		SessionShimCarrierProofV2Readiness: readiness,
		QuarantinedSessions: []SessionShimQuarantinedSession{{
			OrgID: "org", SessionID: "session", ShimID: "shim", ProcessEpoch: 4,
			ControllerGeneration: "18446744073709551615", ProtocolMin: 1, ProtocolMax: 1,
			Reason: "protocol_mismatch", AgeSeconds: 5, ConsumesCapacity: true,
		}},
	}
	var rawBody []byte
	var changedEcho atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		echo := projection
		switch changedEcho.Load() {
		case 1:
			echo.AdoptionRevision = "changed"
		case 2:
			echo.WorkerHostID = "changed-host"
		case 3:
			echo.DurableCarrierProofV2Ready = false
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
	for _, member := range []string{
		`"durable_carrier_proof_v2_ready":true`, `"composingProofV1WritesClosed":true`,
		`"encryptedOriginalCredentialRetained":true`, `"remainingValidityConsumeGate":true`,
		`"adoptedCandidateRecovery":true`,
	} {
		if !bytes.Contains(body["sessionShim"], []byte(member)) {
			t.Fatalf("heartbeat sessionShim omitted readiness member %s: %s", member, body["sessionShim"])
		}
	}

	withoutReadiness := projection
	withoutReadiness.SessionShimCarrierProofV2Readiness = SessionShimCarrierProofV2Readiness{}
	if err := withoutReadiness.validateReady(); err == nil {
		t.Fatal("capability-bearing session-shim authority was accepted without dynamic proof-v2 readiness")
	}

	changedEcho.Store(1)
	if err := service.sendOneResult(context.Background()); err == nil {
		t.Fatal("heartbeat accepted a changed adoption revision echo")
	}
	changedEcho.Store(2)
	if err := service.sendOneResult(context.Background()); err == nil {
		t.Fatal("heartbeat accepted a changed stable-host authority echo")
	}
	changedEcho.Store(3)
	if err := service.sendOneResult(context.Background()); err == nil {
		t.Fatal("heartbeat accepted malformed proof-v2 readiness echo")
	}
}

func TestHeartbeatCarriesLiveProofV2ReadinessFromDaemonProjection(t *testing.T) {
	attestation := activationTestAttestation()
	var (
		readinessCalls atomic.Int64
		readinessError atomic.Bool
		endpointCalls  atomic.Int64
		delayedRelease atomic.Int64
	)
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		ControllerID: attestation.ControllerID, AttestationCapabilities: attestation.Capabilities,
		OrgID: "org-readiness-wire",
		GetCarrierProofV2Readiness: func() (SessionShimCarrierProofV2Readiness, error) {
			readinessCalls.Add(1)
			if readinessError.Load() {
				return SessionShimCarrierProofV2Readiness{}, errors.New("readiness authority unavailable")
			}
			return testSessionShimProofV2Readiness()
		},
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) { delayedRelease.Add(1) },
	}})
	if err := d.retainSessionShimCredentialReceipts([]SessionShimScopeCredentialReceipt{{
		Scope: "org-readiness-wire", WorkerHostID: "stable-host-readiness-wire", AdoptionRevision: "17",
	}}); err != nil {
		t.Fatal(err)
	}
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true

	var (
		rawBody                []byte
		sourceProjection       SessionShimHeartbeatProjection
		acknowledgedProjection SessionShimHeartbeatProjection
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpointCalls.Add(1)
		rawBody, _ = io.ReadAll(r.Body)
		var body heartbeatRequestBody
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Errorf("decode heartbeat request: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(heartbeatResponseBody{Acknowledged: true, SessionShim: body.SessionShim})
	}))
	t.Cleanup(server.Close)

	service := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker-readiness-wire", OrchestratorURL: server.URL, RuntimeJWT: "runtime-readiness-wire",
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationDraining },
		GetSessionShim: func() (SessionShimHeartbeatProjection, error) {
			projection, err := d.SessionShimHeartbeatProjection("org-readiness-wire")
			sourceProjection = projection
			return projection, err
		},
		OnSessionShimAcknowledged: func(projection SessionShimHeartbeatProjection) {
			acknowledgedProjection = projection
		},
		HTTPClient: server.Client(),
	})
	if err := service.sendOneResult(context.Background()); err != nil {
		t.Fatalf("send live readiness heartbeat: %v", err)
	}
	if readinessCalls.Load() != 1 {
		t.Fatalf("live readiness resolutions = %d, want exactly 1", readinessCalls.Load())
	}
	if delayedRelease.Load() != 0 {
		t.Fatalf("readiness-only heartbeat invoked carrier release hook %d times", delayedRelease.Load())
	}
	assertCanonicalEmptyQuarantine := func(label string, projectionBytes []byte) {
		t.Helper()
		var projectionBody map[string]json.RawMessage
		if err := json.Unmarshal(projectionBytes, &projectionBody); err != nil {
			t.Fatalf("decode %s projection: %v (raw=%s)", label, err, projectionBytes)
		}
		if got := projectionBody["quarantinedSessions"]; !bytes.Equal(got, []byte("[]")) {
			t.Fatalf("%s quarantinedSessions bytes = %s, want [] (projection=%s)", label, got, projectionBytes)
		}
	}
	sourceProjectionBytes, err := json.Marshal(sourceProjection)
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalEmptyQuarantine("authority source", sourceProjectionBytes)
	for _, member := range []string{
		`"durable_carrier_proof_v2_ready":true`, `"composingProofV1WritesClosed":true`,
		`"encryptedOriginalCredentialRetained":true`, `"remainingValidityConsumeGate":true`,
		`"adoptedCandidateRecovery":true`, `"workerHostId":"stable-host-readiness-wire"`,
	} {
		if !bytes.Contains(rawBody, []byte(member)) {
			t.Fatalf("live heartbeat omitted %s: %s", member, rawBody)
		}
	}
	var heartbeatBody map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &heartbeatBody); err != nil {
		t.Fatal(err)
	}
	assertCanonicalEmptyQuarantine("raw heartbeat", heartbeatBody["sessionShim"])
	acknowledgedProjectionBytes, err := json.Marshal(acknowledgedProjection)
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalEmptyQuarantine("acknowledgement callback", acknowledgedProjectionBytes)
	var legacyProjection struct {
		Enabled             bool                            `json:"enabled"`
		AdoptionComplete    bool                            `json:"adoptionComplete"`
		WorkerHostID        string                          `json:"workerHostId"`
		ControllerID        string                          `json:"controllerId"`
		AdoptionRevision    string                          `json:"adoptionRevision"`
		QuarantinedSessions []SessionShimQuarantinedSession `json:"quarantinedSessions"`
	}
	if err := json.Unmarshal(heartbeatBody["sessionShim"], &legacyProjection); err != nil {
		t.Fatalf("legacy reader rejected additive readiness fields: %v", err)
	}
	if !legacyProjection.Enabled || !legacyProjection.AdoptionComplete ||
		legacyProjection.WorkerHostID != "stable-host-readiness-wire" ||
		legacyProjection.ControllerID != attestation.ControllerID || legacyProjection.AdoptionRevision != "17" {
		t.Fatalf("additive readiness changed legacy authority projection: %+v", legacyProjection)
	}
	if _, err := d.SessionShimHeartbeatProjection("other-org"); err == nil {
		t.Fatal("readiness projection crossed organization authority")
	}

	readinessError.Store(true)
	if err := service.sendOneResult(context.Background()); err != nil {
		t.Fatalf("degraded readiness heartbeat: %v", err)
	}
	if endpointCalls.Load() != 2 {
		t.Fatalf("endpoint calls after readiness failure = %d, want 2", endpointCalls.Load())
	}
	var degraded heartbeatRequestBody
	if err := json.Unmarshal(rawBody, &degraded); err != nil {
		t.Fatalf("decode degraded heartbeat: %v", err)
	}
	if degraded.SessionShim == nil || degraded.SessionShim.ReadinessState != "unknown" ||
		degraded.SessionShim.ReadinessReason == "" || degraded.SessionShim.ReadinessObservedAt == "" {
		t.Fatalf("degraded session-shim projection = %+v, want unknown with reason and timestamp", degraded.SessionShim)
	}
	readinessError.Store(false)
	if err := service.sendOneResult(context.Background()); err != nil {
		t.Fatalf("recovered readiness heartbeat: %v", err)
	}
	var recovered heartbeatRequestBody
	if err := json.Unmarshal(rawBody, &recovered); err != nil {
		t.Fatalf("decode recovered heartbeat: %v", err)
	}
	if endpointCalls.Load() != 3 || recovered.SessionShim == nil || recovered.SessionShim.ReadinessState != "ready" {
		t.Fatalf("recovered heartbeat calls/projection = %d/%+v, want 3/ready", endpointCalls.Load(), recovered.SessionShim)
	}
}

func TestDaemonStartAuthOnlyOrderingBeforeAdoptionHeartbeatAndPoll(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var (
		mu                 sync.Mutex
		order              []string
		d                  *Daemon
		readinessCalls     atomic.Int64
		readinessOK        atomic.Bool
		readinessError     atomic.Bool
		driftRevision      atomic.Bool
		heartbeatCount     atomic.Int64
		pollCount          atomic.Int64
		activationReleases atomic.Int64
		lastHeartbeat      heartbeatRequestBody
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
			mu.Lock()
			lastHeartbeat = body
			mu.Unlock()
			if driftRevision.CompareAndSwap(true, false) {
				d.shims.mu.Lock()
				receipt := d.shims.credentialReceipts["org-order"]
				receipt.AdoptionRevision = "revision-drifted-in-flight"
				d.shims.credentialReceipts["org-order"] = receipt
				d.shims.mu.Unlock()
			}
			heartbeatCount.Add(1)
			record("heartbeat")
			_ = json.NewEncoder(w).Encode(heartbeatResponseBody{Acknowledged: true, SessionShim: body.SessionShim})
		case "/api/workers/worker-order/poll":
			pollCount.Add(1)
			record("poll")
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	orchestratorTransport := &recordingOrchestratorTransport{
		base: server.Client().Transport,
		hits: make(map[string]int),
	}
	var redirectRefusals atomic.Int32
	orchestratorHTTPClient := &http.Client{
		Transport: orchestratorTransport,
		Timeout:   3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			redirectRefusals.Add(1)
			return http.ErrUseLastResponse
		},
	}
	originalTransport, originalTimeout := orchestratorHTTPClient.Transport, orchestratorHTTPClient.Timeout
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
		OrchestratorHTTPClient: orchestratorHTTPClient,
		SessionShim: SessionShimConfig{
			EnableAdoption: true, RequireCredentialAttestation: true,
			GetCarrierProofV2Readiness: func() (SessionShimCarrierProofV2Readiness, error) {
				readinessCalls.Add(1)
				if readinessError.Load() {
					return SessionShimCarrierProofV2Readiness{}, errors.New("proof-v2 store unavailable")
				}
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
			OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {
				activationReleases.Add(1)
			},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	satelliteClaimGate := d.PollClaimGate()
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
	if d.heartbeat == nil || d.heartbeat.opts.HTTPClient != orchestratorHTTPClient {
		t.Fatal("auth-only heartbeat did not retain the exact injected orchestrator client")
	}
	if d.poller == nil || d.poller.opts.HTTPClient != orchestratorHTTPClient {
		t.Fatal("auth-only poll did not retain the exact injected orchestrator client")
	}
	if orchestratorHTTPClient.Transport != originalTransport || orchestratorHTTPClient.Timeout != originalTimeout {
		t.Fatal("auth-only startup mutated the injected orchestrator client")
	}
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, RegisterEndpoint},
		{http.MethodPost, "/api/workers/worker-order/heartbeat"},
		{http.MethodGet, "/api/workers/worker-order/poll"},
	} {
		if got := orchestratorTransport.count(request.method, request.path); got == 0 {
			t.Errorf("auth-only injected client did not observe %s %s", request.method, request.path)
		}
	}
	if got := redirectRefusals.Load(); got != 0 {
		t.Fatalf("auth-only non-redirecting control server invoked redirect policy %d times", got)
	}
	if suspended, reason := satelliteClaimGate(); suspended || reason != "" {
		t.Fatalf("exported poll claim gate at ready startup = (%v, %q), want open", suspended, reason)
	}
	if readinessCalls.Load() < 2 {
		t.Fatalf("proof-v2 readiness was not checked at registration and heartbeat: calls=%d", readinessCalls.Load())
	}
	mu.Lock()
	startupHeartbeat := lastHeartbeat
	mu.Unlock()
	wantStartupReadiness, _ := testSessionShimProofV2Readiness()
	if startupHeartbeat.SessionShim == nil ||
		startupHeartbeat.SessionShim.SessionShimCarrierProofV2Readiness != wantStartupReadiness ||
		startupHeartbeat.SessionShim.WorkerHostID != "stable-host-order" {
		t.Fatalf("startup heartbeat omitted authority-bound live readiness: %+v", startupHeartbeat.SessionShim)
	}
	assertClosed := func(t *testing.T, reason string, priorHeartbeats, priorPolls int64) {
		t.Helper()
		t.Run(reason+"/non-ready", func(t *testing.T) {
			if d.State() != StateRecovering || d.RegistrationStatus() != RegistrationDraining {
				t.Fatalf("withdrawal left daemon ready: state=%s registration=%s", d.State(), d.RegistrationStatus())
			}
		})
		t.Run(reason+"/spawn", func(t *testing.T) {
			if d.spawner.IsAccepting() {
				t.Fatal("withdrawal left spawner admission open")
			}
			if _, err := d.AcceptWork(SessionSpec{SessionID: "must-refuse-" + reason}); err == nil {
				t.Fatal("withdrawal left Daemon.AcceptWork admission open")
			}
		})
		t.Run(reason+"/claim", func(t *testing.T) {
			if blocked, _ := satelliteClaimGate(); !blocked {
				t.Fatal("withdrawal left claim admission open")
			}
		})
		t.Run(reason+"/capacity", func(t *testing.T) {
			if got := heartbeatCount.Load(); got != priorHeartbeats {
				t.Fatalf("withdrawal published a heartbeat/capacity claim: got %d want %d", got, priorHeartbeats)
			}
		})
		t.Run(reason+"/poll", func(t *testing.T) {
			d.poller.pollOnce(context.Background())
			if got := pollCount.Load(); got != priorPolls {
				t.Fatalf("withdrawal allowed poll/claim: got %d want %d", got, priorPolls)
			}
		})
	}
	reopenAfterAcknowledgedHeartbeat := func(t *testing.T, reason string) {
		t.Helper()
		readinessOK.Store(true)
		readinessError.Store(false)
		if _, err := d.SessionShimHeartbeatProjection("org-order"); err != nil {
			t.Fatalf("%s readiness revalidation: %v", reason, err)
		}
		t.Run(reason+"/pre-ack", func(t *testing.T) {
			if d.State() != StateRecovering || d.spawner.IsAccepting() {
				t.Fatalf("reopened before a fresh acknowledged heartbeat: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
			}
			if suspended, _ := satelliteClaimGate(); !suspended {
				t.Fatal("exported poll claim gate reopened before a fresh acknowledged heartbeat")
			}
		})
		t.Run(reason+"/recovery-heartbeat", func(t *testing.T) {
			if err := d.heartbeat.sendOneResult(context.Background()); err != nil {
				t.Fatalf("recovery heartbeat: %v", err)
			}
			mu.Lock()
			recoveryBeat := lastHeartbeat
			mu.Unlock()
			if recoveryBeat.Status != string(RegistrationDraining) || recoveryBeat.MaxSessions != 0 {
				t.Fatalf("recovery heartbeat advertised open capacity: status=%q maxSessions=%d", recoveryBeat.Status, recoveryBeat.MaxSessions)
			}
			if d.State() != StateRunning || !d.spawner.IsAccepting() {
				t.Fatalf("acknowledged heartbeat did not reopen admission: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
			}
			if suspended, reason := satelliteClaimGate(); suspended || reason != "" {
				t.Fatalf("exported poll claim gate after acknowledged heartbeat = (%v, %q), want open", suspended, reason)
			}
		})
	}

	priorHeartbeats, priorPolls := heartbeatCount.Load(), pollCount.Load()
	readinessOK.Store(false)
	if err := d.heartbeat.sendOneResult(context.Background()); err == nil {
		t.Fatal("heartbeat remained eligible after durable proof-v2 readiness became false")
	}
	assertClosed(t, "false", priorHeartbeats, priorPolls)
	if err := d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{SessionShim: &SessionShimCredentialReceipt{
		State: SessionShimCredentialStateReady, WorkerHostID: "stable-host-order", AdoptionRevision: "revision-refresh",
	}}); err == nil {
		t.Fatal("refresh remained eligible after durable proof-v2 readiness became false")
	}
	reopenAfterAcknowledgedHeartbeat(t, "false")

	priorHeartbeats, priorPolls = heartbeatCount.Load(), pollCount.Load()
	readinessError.Store(true)
	if err := d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{SessionShim: &SessionShimCredentialReceipt{
		State: SessionShimCredentialStateReady, WorkerHostID: "stable-host-order", AdoptionRevision: "revision-refresh-error",
	}}); err == nil {
		t.Fatal("refresh remained eligible after proof-v2 readiness resolver error")
	}
	assertClosed(t, "error", priorHeartbeats, priorPolls)
	reopenAfterAcknowledgedHeartbeat(t, "error")

	t.Run("stale-acknowledgement-cannot-reopen", func(t *testing.T) {
		readinessOK.Store(false)
		if err := d.heartbeat.sendOneResult(context.Background()); err == nil {
			t.Fatal("heartbeat remained eligible before stale-acknowledgement control")
		}
		readinessOK.Store(true)
		driftRevision.Store(true)
		if err := d.heartbeat.sendOneResult(context.Background()); err != nil {
			t.Fatalf("stale but server-echoed heartbeat: %v", err)
		}
		if d.State() != StateRecovering || d.spawner.IsAccepting() {
			t.Fatalf("stale acknowledged revision reopened admission: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
		}
		if suspended, _ := satelliteClaimGate(); !suspended {
			t.Fatal("stale acknowledged revision reopened exported poll claim gate")
		}
		if err := d.heartbeat.sendOneResult(context.Background()); err != nil {
			t.Fatalf("fresh current-revision heartbeat: %v", err)
		}
		if d.State() != StateRunning || !d.spawner.IsAccepting() {
			t.Fatalf("fresh current-revision acknowledgement did not reopen: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
		}
		if suspended, reason := satelliteClaimGate(); suspended || reason != "" {
			t.Fatalf("fresh current-revision acknowledgement left exported gate closed: (%v, %q)", suspended, reason)
		}
	})

	t.Run("race/heartbeat-poll-refresh", func(t *testing.T) {
		priorHeartbeats = heartbeatCount.Load()
		readinessError.Store(true)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				switch i % 4 {
				case 0:
					_ = d.heartbeat.sendOneResult(context.Background())
				case 1:
					_ = d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{SessionShim: &SessionShimCredentialReceipt{
						State: SessionShimCredentialStateReady, WorkerHostID: "stable-host-order", AdoptionRevision: "revision-race",
					}})
				case 2:
					d.poller.pollOnce(context.Background())
				case 3:
					_, _ = satelliteClaimGate()
				}
			}(i)
		}
		wg.Wait()
		pollAfterRace := pollCount.Load()
		assertClosed(t, "race", priorHeartbeats, pollAfterRace)
		reopenAfterAcknowledgedHeartbeat(t, "race")
	})
	if activationReleases.Load() != 0 {
		t.Fatalf("readiness-only recovery invoked delayed carrier release %d times", activationReleases.Load())
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

func TestCarrierActivationPublishesOnlyExactUnresolvedAuthority(t *testing.T) {
	activeID := sessionshim.Identity{OrgID: "org-filter", SessionID: "active"}
	pendingID := sessionshim.Identity{OrgID: "org-filter", SessionID: "pending"}
	recoveryID := sessionshim.Identity{OrgID: "org-filter", SessionID: "recovery"}
	carrierEntry := func(id sessionshim.Identity, epoch uint64) adoptedShim {
		return adoptedShim{adoption: SessionShimAdoptionEvidence{
			Identity: id, CarrierCompatible: true,
			Extensions: shimwire.Extensions{Values: map[string]string{
				shimwire.ExtCarrierEpoch: fmt.Sprintf("%d", epoch),
			}},
		}}
	}
	active := carrierEntry(activeID, 7)
	active.carrierActivationResolved = true
	pending := carrierEntry(pendingID, 8)
	recovery := carrierEntry(recoveryID, 9)
	recovery.adoptionReceipt = SessionShimAdoptionReceipt{DurableCorrelation: []byte("recovery-adoption")}
	recovery.consumedRecovery = &sessionShimConsumedRecovery{
		preStageAckSeq: 20, stagedHighWater: 21,
		adoptionReceipt: cloneSessionShimAdoptionReceipt(recovery.adoptionReceipt),
	}
	wantCarriers := []SessionShimCarrierActivation{
		{OrgID: pendingID.OrgID, SessionID: pendingID.SessionID, CarrierEpoch: 8},
		{OrgID: recoveryID.OrgID, SessionID: recoveryID.SessionID, CarrierEpoch: 9},
	}
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, ControllerID: "controller-filter",
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			if !reflect.DeepEqual(publication.Carriers, wantCarriers) {
				return nil, fmt.Errorf("activation carriers = %+v, want %+v", publication.Carriers, wantCarriers)
			}
			return []SessionShimCarrierActivationReceipt{
				{Activation: wantCarriers[0], AckSeq: 12},
				{Activation: wantCarriers[1], AckSeq: 21},
			}, nil
		},
	}})
	d.shims.adoptionComplete = true
	d.shims.batchReceipts[activeID.OrgID] = SessionShimAdoptionBatchReceipt{
		DurableCorrelation: []byte("batch-filter"), AdoptionRevision: "revision-filter",
	}
	d.shims.adopted[activeID] = active
	d.shims.adopted[pendingID] = pending
	d.shims.adopted[recoveryID] = recovery
	d.shims.pendingSnapshots[pendingID] = sessionshim.ControllerEvent{
		Kind: sessionshim.EventHostFrame, FrameType: attachwire.TypeSnapshot, RequestID: 44, Seq: 12,
	}
	d.shims.activationGates[pendingID] = newShimAdoptionGate()
	published := map[sessionshim.Identity]adoptedShim{
		activeID: active, pendingID: pending, recoveryID: recovery,
	}
	if err := d.activatePublishedSessionShimCarriers(context.Background(), published); err != nil {
		t.Fatalf("candidate-only activation: %v", err)
	}
	for _, id := range []sessionshim.Identity{activeID, pendingID, recoveryID} {
		if !d.shims.adopted[id].carrierActivationResolved {
			t.Errorf("carrier %s is not marked active after exact resolution", id)
		}
	}
	if d.shims.adopted[recoveryID].consumedRecovery != nil {
		t.Fatal("exact consumed recovery remained pending after activation")
	}
	if _, pending := d.shims.pendingSnapshots[pendingID]; pending {
		t.Fatal("exact pending Snapshot remained after activation")
	}
}

func TestCarrierActivationRefusesCompatibleUnresolvedEntryWithoutCorrelation(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org-missing-correlation", SessionID: "session-missing-correlation"}
	entry := adoptedShim{adoption: SessionShimAdoptionEvidence{
		Identity: id, CarrierCompatible: true,
		Extensions: shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "31"}},
	}}
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, ControllerID: "controller-missing-correlation",
		OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			t.Fatal("missing activation authority reached publication callback")
			return nil, nil
		},
	}})
	d.shims.adoptionComplete = true
	d.shims.adopted[id] = entry
	d.shims.batchReceipts[id.OrgID] = SessionShimAdoptionBatchReceipt{
		DurableCorrelation: []byte("batch"), AdoptionRevision: "revision",
	}
	if err := d.activatePublishedSessionShimCarriers(
		context.Background(), map[sessionshim.Identity]adoptedShim{id: entry},
	); err == nil || !strings.Contains(err.Error(), "no pending Snapshot or consumed recovery") {
		t.Fatalf("missing activation authority error = %v", err)
	}
	if d.shims.adopted[id].carrierActivationResolved {
		t.Fatal("missing activation authority marked carrier active")
	}
}

func TestConsumedRecoveryActivationRequiresOriginalHighWaterReceiptAndCurrentEntry(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org-recovery-bind", SessionID: "session-recovery-bind"}
	carrier := SessionShimCarrierActivation{OrgID: id.OrgID, SessionID: id.SessionID, CarrierEpoch: 17}
	originalReceipt := SessionShimAdoptionReceipt{DurableCorrelation: []byte("original-replayed-adoption")}

	tests := map[string]func(*Daemon, map[sessionshim.Identity]adoptedShim, *[]SessionShimCarrierActivationReceipt){
		"changed staged high-water": func(_ *Daemon, _ map[sessionshim.Identity]adoptedShim, activated *[]SessionShimCarrierActivationReceipt) {
			(*activated)[0].AckSeq = 28
		},
		"changed adoption receipt": func(d *Daemon, _ map[sessionshim.Identity]adoptedShim, _ *[]SessionShimCarrierActivationReceipt) {
			entry := d.shims.adopted[id]
			entry.adoptionReceipt.DurableCorrelation = []byte("changed-replayed-adoption")
			d.shims.adopted[id] = entry
		},
		"replaced adopted entry": func(d *Daemon, _ map[sessionshim.Identity]adoptedShim, _ *[]SessionShimCarrierActivationReceipt) {
			entry := d.shims.adopted[id]
			entry.controller = &sessionshim.Controller{}
			d.shims.adopted[id] = entry
		},
		"second staged Snapshot": func(d *Daemon, _ map[sessionshim.Identity]adoptedShim, _ *[]SessionShimCarrierActivationReceipt) {
			d.shims.pendingSnapshots[id] = sessionshim.ControllerEvent{
				Kind: sessionshim.EventHostFrame, FrameType: attachwire.TypeSnapshot, RequestID: 88, Seq: 29,
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			activated := []SessionShimCarrierActivationReceipt{{Activation: carrier, AckSeq: 29}}
			d := New(Options{SessionShim: SessionShimConfig{
				EnableAdoption: true, ControllerID: "controller-recovery-bind",
				OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
					return append([]SessionShimCarrierActivationReceipt(nil), activated...), nil
				},
			}})
			d.shims.adoptionComplete = true
			d.shims.batchReceipts[id.OrgID] = SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("batch"), AdoptionRevision: "revision",
			}
			ctrl := &sessionshim.Controller{}
			entry := adoptedShim{
				controller: ctrl,
				adoption: SessionShimAdoptionEvidence{
					Identity: id, CarrierCompatible: true,
					Extensions: shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "17"}},
				},
				adoptionReceipt: cloneSessionShimAdoptionReceipt(originalReceipt),
				consumedRecovery: &sessionShimConsumedRecovery{
					preStageAckSeq: 27, stagedHighWater: 29,
					adoptionReceipt: cloneSessionShimAdoptionReceipt(originalReceipt),
				},
			}
			d.shims.adopted[id] = entry
			published := map[sessionshim.Identity]adoptedShim{id: entry}
			mutate(d, published, &activated)
			if err := d.activatePublishedSessionShimCarriers(context.Background(), published); err == nil {
				t.Fatal("consumed recovery activation accepted changed original evidence")
			}
			if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
				t.Fatalf("refused consumed recovery advanced cursor = %d", got)
			}
		})
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
	readiness, _ := testSessionShimProofV2Readiness()
	projection := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true, WorkerHostID: "host", ControllerID: "controller", AdoptionRevision: "revision",
		SessionShimCarrierProofV2Readiness: readiness,
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
