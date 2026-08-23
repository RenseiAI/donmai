package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func activationTestAttestation() SessionShimHostAttestation {
	return SessionShimHostAttestation{
		Supported: true, ControllerID: "controller-activation-test",
		ProtocolMin: 1, ProtocolMax: 2,
		Capabilities: []string{"carrier-activation", "durable-host-ack", "snapshot-proxy"},
	}
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
	want := map[string]any{
		"sessionShimSupported": true, "sessionShimControllerId": att.ControllerID,
		"sessionShimProtocolMin": float64(att.ProtocolMin), "sessionShimProtocolMax": float64(att.ProtocolMax),
		"sessionShimCapabilities": []any{att.Capabilities[0], att.Capabilities[1], att.Capabilities[2]},
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
		ControllerID: attestation.ControllerID, AttestationCapabilities: attestation.Capabilities,
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
		ControllerID: attestation.ControllerID, AttestationCapabilities: attestation.Capabilities,
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
		mu    sync.Mutex
		order []string
		d     *Daemon
	)
	record := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	attestation := activationTestAttestation()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case RegisterEndpoint:
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
	d.shims.pendingSnapshots[id] = sessionshim.ControllerEvent{Kind: sessionshim.EventSnapshotFrame, Seq: 12}
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
