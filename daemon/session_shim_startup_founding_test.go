package daemon

// session_shim_startup_founding_test.go — what a refused STARTUP founding
// leaves behind.
//
// The deferred install already classifies a refused founding declaration or
// first projected heartbeat as a retryable founding refusal and rolls back so
// another composition can found. The startup path constructs the daemon WITH
// the composed configuration, so there is no prior generation to roll back
// to: before this change a platform that refused the startup founding
// registration or first heartbeat failed Start outright — durable sessions
// off for every scope until the next start, and the next start presented the
// same founder. These tests drive the production Start entry point and pin
// the in-process fallback instead: Start succeeds stood down, the refusal is
// retained for diagnostics, the host-wide claim gate is open, and a later
// deferred install with another composed configuration founds the
// composition.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startupFoundingHarness is one fake control plane plus a daemon constructed
// WITH the composed configuration, the way the inline startup path runs: the
// founding registration carries the composed attestation from the first round
// trip. The fake plane can refuse that registration, or the first heartbeat
// carrying the projection, with a definite client error.
type startupFoundingHarness struct {
	t *testing.T

	daemon      *Daemon
	attestation SessionShimHostAttestation
	orgID       string
	registryDir string
	server      *httptest.Server

	mu                        sync.Mutex
	registerBodies            [][]byte
	refreshBodies             [][]byte
	heartbeatBodies           []heartbeatRequestBody
	workerForgotten           bool
	refuseRegisterComposed    bool
	refuseFirstProjectedBeat  bool
	refuseFirstProjectedBeats int
	heartbeatRequireRevision  string
	secondAttestation         SessionShimHostAttestation
	secondOrgID               string
	refuseRegisterForSecond   bool
}

// setRefuseComposedRegistration arms (or, with false, disarms) the
// registration endpoint's refusal of a registration presenting the composed
// attestation. It models the platform refusing one founder's startup founding
// registration while still accepting another's.
func (h *startupFoundingHarness) setRefuseComposedRegistration(refuse bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseRegisterComposed = refuse
}

// setRefuseFirstProjectedHeartbeat arms (or, with 0, disarms) the heartbeat
// endpoint's refusal of the next n beats carrying a session-shim projection.
// It models the platform refusing one founder's first startup heartbeat while
// still accepting another's.
func (h *startupFoundingHarness) setRefuseFirstProjectedHeartbeat(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseFirstProjectedBeats = n
	h.refuseFirstProjectedBeat = n > 0
}

func (h *startupFoundingHarness) lastHeartbeat() (heartbeatRequestBody, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.heartbeatBodies) == 0 {
		return heartbeatRequestBody{}, false
	}
	return h.heartbeatBodies[len(h.heartbeatBodies)-1], true
}

func (h *startupFoundingHarness) registrations() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.registerBodies...)
}

func (h *startupFoundingHarness) refreshes() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.refreshBodies...)
}

// forgetWorker makes the refresh endpoint answer 404, the way a control plane
// that no longer holds this worker's registration does, so the refresher falls
// through to a full re-registration with the options it holds.
func (h *startupFoundingHarness) forgetWorker() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workerForgotten = true
}

// newStartupFoundingHarness stands up the fake control plane and the daemon
// constructed with the composed configuration. The daemon is NOT started;
// callers do that so they can observe the timing.
func newStartupFoundingHarness(t *testing.T) *startupFoundingHarness {
	t.Helper()
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	dir := t.TempDir()
	h := &startupFoundingHarness{
		t:           t,
		attestation: activationTestAttestation(),
		orgID:       "org-startup-founding",
		registryDir: filepath.Join(dir, "registry"),
		secondAttestation: SessionShimHostAttestation{
			Supported: SessionShimSupported, ControllerID: "controller-second-founder",
			ProtocolMin: 1, ProtocolMax: 3,
			Capabilities: RequiredSessionShimHostCapabilities(),
		},
		secondOrgID: "org-second-founding",
	}
	const workerID = "worker-startup-founding"

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case RegisterEndpoint:
			raw, _ := io.ReadAll(r.Body)
			var presented SessionShimHostAttestation
			_ = json.Unmarshal(raw, &presented)
			h.mu.Lock()
			h.registerBodies = append(h.registerBodies, append([]byte(nil), raw...))
			refuseComposed := h.refuseRegisterComposed
			refuseSecond := h.refuseRegisterForSecond
			h.mu.Unlock()

			if presented.Supports() && refuseComposed && presented.ControllerID == h.attestation.ControllerID {
				http.Error(w, "startup founding registration refused", http.StatusForbidden)
				return
			}
			if presented.Supports() && refuseSecond && presented.ControllerID == h.secondAttestation.ControllerID {
				http.Error(w, "startup founding registration refused", http.StatusForbidden)
				return
			}
			// The stood-down re-registration after a refused founding is
			// answered normally: the platform refused one founder's
			// declaration, not this host's right to serve. A stand-down
			// presents no controller, so it never matches the refusal arm
			// above, which keys on the refused founder's controller id.
			resp := RegisterResponse{
				WorkerID: workerID, RuntimeToken: "runtime-startup-founding",
				HeartbeatInterval: 3_600_000, PollInterval: 3_600_000,
			}
			if presented.Supports() {
				resp.SessionShim = activationTestCredentialReceipt(
					presented, SessionShimCredentialStateRecovering,
					"stable-host-startup", "revision-declared",
				)
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/api/workers/" + workerID + "/refresh-token":
			raw, _ := io.ReadAll(r.Body)
			var presented SessionShimHostAttestation
			_ = json.Unmarshal(raw, &presented)
			h.mu.Lock()
			h.refreshBodies = append(h.refreshBodies, append([]byte(nil), raw...))
			forgotten := h.workerForgotten
			h.mu.Unlock()
			if forgotten {
				http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
				return
			}
			token := "runtime-startup-founding-refreshed"
			resp := refreshResponse{RuntimeToken: token}
			if presented.Supports() {
				resp.SessionShim = activationTestCredentialReceipt(
					presented, SessionShimCredentialStateRecovering,
					"stable-host-startup", "revision-declared",
				)
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/api/workers/" + workerID + "/heartbeat":
			var body heartbeatRequestBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode heartbeat: %v", err)
			}
			h.mu.Lock()
			h.heartbeatBodies = append(h.heartbeatBodies, body)
			refuse := h.refuseFirstProjectedBeat && body.SessionShim != nil
			if refuse {
				h.refuseFirstProjectedBeats--
				h.refuseFirstProjectedBeat = h.refuseFirstProjectedBeats > 0
			}
			requireRevision := h.heartbeatRequireRevision
			h.mu.Unlock()
			if refuse {
				http.Error(w, "first startup heartbeat refused", http.StatusForbidden)
				return
			}
			if requireRevision != "" &&
				(body.SessionShim == nil || body.SessionShim.AdoptionRevision != requireRevision) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"SESSION_SHIM_ADOPTION_REVISION_STALE"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(heartbeatResponseBody{
				Acknowledged: true, SessionShim: body.SessionShim,
			})
		case "/api/workers/" + workerID + "/poll":
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.server.Close)

	configPath := filepath.Join(dir, "daemon.yaml")
	cfg := DefaultConfig()
	cfg.Machine.ID = "startup-founding-test"
	cfg.Orchestrator.URL = h.server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_startup_founding"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h.daemon = New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"),
		HTTPHost: "127.0.0.1", HTTPPort: 0, SkipWizard: true,
		OrchestratorHTTPClient: h.server.Client(),
		SessionShim: SessionShimConfig{
			EnableAdoption:               true,
			EnableOwnership:              true,
			RequireCredentialAttestation: true,
			GetCarrierProofV2Readiness:   testSessionShimProofV2Readiness,
			ControllerID:                 h.attestation.ControllerID,
			AttestationCapabilities:      h.attestation.Capabilities,
			OrgID:                        h.orgID,
			RegistryDir:                  h.registryDir,
			OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("startup"), AdoptionRevision: "revision-batch",
				}, nil
			},
			OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
				return nil, nil
			},
			OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {},
		},
	})
	return h
}

// secondConfig is the deferred composition another founder would hand the seam
// once the startup founder is refused: a different scope and controller.
func (h *startupFoundingHarness) secondConfig() SessionShimConfig {
	return SessionShimConfig{
		EnableAdoption:               true,
		EnableOwnership:              true,
		RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness:   testSessionShimProofV2Readiness,
		ControllerID:                 h.secondAttestation.ControllerID,
		AttestationCapabilities:      h.secondAttestation.Capabilities,
		OrgID:                        h.secondOrgID,
		RegistryDir:                  h.registryDir,
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("second"), AdoptionRevision: "revision-second",
			}, nil
		},
		OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			return nil, nil
		},
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {},
	}
}

// TestRefusedStartupFoundingRegistrationStandsDownAndServes is the startup
// half of the retry contract: when the platform refuses the startup founding
// registration with a definite client error, Start succeeds stood down — the
// daemon serves direct-owned sessions with the claim gate open — instead of
// failing startup. The refusal is retained for diagnostics, and a later
// deferred install with another composed configuration founds the
// composition in process.
func TestRefusedStartupFoundingRegistrationStandsDownAndServes(t *testing.T) {
	h := newStartupFoundingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.setRefuseComposedRegistration(true)

	if err := h.daemon.Start(ctx); err != nil {
		t.Fatalf("Start after a refused startup founding registration: %v", err)
	}
	t.Cleanup(func() { _ = h.daemon.Stop(context.Background()) })

	// The daemon serves, stood down — not failed, not recovering.
	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state after a refused startup founding = %q, want %q", state, StateRunning)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
		t.Fatalf("attestation after a refused startup founding = %#v, want the stand-down", got)
	}
	if h.daemon.sessionShimEnabled() {
		t.Fatal("a refused startup founding left the shim enabled")
	}
	if h.daemon.SessionShimCompositionPending() {
		t.Fatal("a refused startup founding is still reported pending")
	}
	retained := h.daemon.SessionShimDurabilityRefusal()
	if retained == nil || retained.Scope != h.orgID || retained.Reason == "" {
		t.Fatalf("retained refusal = %+v, want the refused scope and a reason", retained)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal == nil {
		t.Fatal("diagnostics hide the retained startup founding refusal")
	}
	// The host-wide new-work claim gate is open: a refused founder for one
	// scope must not pause claims for every scope.
	if suspended, reason := h.daemon.PollClaimGate()(); suspended {
		t.Fatalf("claim gate closed after a refused startup founding: %s", reason)
	}
	if _, err := h.daemon.AcceptWorkWithDetail(SessionSpec{}, &SessionDetail{}); err == nil ||
		containsWithdrawn(err.Error()) {
		t.Fatalf("work admission after a refused startup founding: err = %v, want the benign spec refusal", err)
	}

	// Exactly two registrations went out: the refused founding registration,
	// then the stood-down re-registration that gives the serving daemon its
	// worker identity. The second presents the explicit stand-down — never a
	// second founder nobody configured.
	registrations := h.registrations()
	if len(registrations) != 2 {
		t.Fatalf("registrations = %d, want the refused founding plus the stood-down re-registration", len(registrations))
	}
	second := shimKeysIn(t, registrations[1])
	if len(second) != 1 || second["sessionShimSupported"] != false {
		t.Fatalf("second registration presented %#v, want exactly sessionShimSupported=false: %s",
			second, registrations[1])
	}

	// A later deferred install with another founder founds the composition in
	// process: the refused startup founder never blocks it.
	h.setRefuseComposedRegistration(false)
	if err := h.daemon.InstallSessionShimComposition(ctx, h.secondConfig()); err != nil {
		t.Fatalf("a refused startup founder blocked a later healthy founder: %v", err)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the deferred install = %#v, want the composed attestation", got)
	}
	if got := h.daemon.sessionShimConfig().orgID(); got != h.secondOrgID {
		t.Fatalf("installed configuration scope = %q, want %q", got, h.secondOrgID)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
		t.Fatal("host status still reports a refusal the deferred install has since recovered from")
	}
	if suspended, reason := h.daemon.PollClaimGate()(); suspended {
		t.Fatalf("claim gate closed after the deferred install founded: %s", reason)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the deferred install never reached a projected heartbeat")
	}
	if beat.SessionShim.ControllerID != h.secondAttestation.ControllerID {
		t.Fatalf("projected heartbeat controller = %q, want %q",
			beat.SessionShim.ControllerID, h.secondAttestation.ControllerID)
	}
}

// TestRefusedStartupFirstHeartbeatStandsDownAndServes is the same contract
// for the other startup founding leg: when the platform refuses the first
// heartbeat presenting the startup composition, Start succeeds stood down
// and a later deferred install founds the composition.
func TestRefusedStartupFirstHeartbeatStandsDownAndServes(t *testing.T) {
	h := newStartupFoundingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.setRefuseFirstProjectedHeartbeat(1)

	if err := h.daemon.Start(ctx); err != nil {
		t.Fatalf("Start after a refused startup first heartbeat: %v", err)
	}
	t.Cleanup(func() { _ = h.daemon.Stop(context.Background()) })

	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state after a refused startup first heartbeat = %q, want %q", state, StateRunning)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
		t.Fatalf("attestation after a refused startup first heartbeat = %#v, want the stand-down", got)
	}
	retained := h.daemon.SessionShimDurabilityRefusal()
	if retained == nil || retained.Scope != h.orgID || retained.Reason == "" {
		t.Fatalf("retained refusal = %+v, want the refused scope and a reason", retained)
	}
	if suspended, reason := h.daemon.PollClaimGate()(); suspended {
		t.Fatalf("claim gate closed after a refused startup first heartbeat: %s", reason)
	}

	if err := h.daemon.InstallSessionShimComposition(ctx, h.secondConfig()); err != nil {
		t.Fatalf("a refused startup first heartbeat blocked a later healthy founder: %v", err)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the deferred install = %#v, want the composed attestation", got)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
		t.Fatal("host status still reports a refusal the deferred install has since recovered from")
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the deferred install never reached a projected heartbeat")
	}
}

// TestRefusedStartupFirstHeartbeatWithdrawsTheCompositionFromTheCredentialLane
// pins what the heartbeat leg owes the control plane that the registration leg
// does not. The refused founder's registration was ACCEPTED, so the control
// plane records the host as composed and the shared credential lane was built
// presenting that attestation. Standing down locally is not enough: the
// stand-down goes on the wire exactly once, the lane presents it from then on,
// and a full re-registration mints an ordinary identity with the host's real
// capacity rather than an auth-only one for the abandoned composition.
func TestRefusedStartupFirstHeartbeatWithdrawsTheCompositionFromTheCredentialLane(t *testing.T) {
	h := newStartupFoundingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.setRefuseFirstProjectedHeartbeat(1)

	if err := h.daemon.Start(ctx); err != nil {
		t.Fatalf("Start after a refused startup first heartbeat: %v", err)
	}
	t.Cleanup(func() { _ = h.daemon.Stop(context.Background()) })

	refreshes := h.refreshes()
	if len(refreshes) != 1 {
		t.Fatalf("refreshes during Start = %d, want exactly the stand-down re-declaration", len(refreshes))
	}
	if keys := shimKeysIn(t, refreshes[0]); len(keys) != 1 || keys["sessionShimSupported"] != false {
		t.Fatalf("re-declaration presented %#v, want exactly sessionShimSupported=false: %s", keys, refreshes[0])
	}
	if got := h.daemon.credentials.SessionShimAttestation(); !got.StandsDown() {
		t.Fatalf("credential lane attestation after the stand-down = %#v, want the stand-down", got)
	}

	// The control plane forgets the worker, so the lane falls through to a
	// full re-registration with the options it now holds.
	before := len(h.registrations())
	h.forgetWorker()
	if _, err := h.daemon.credentials.Refresh(ctx, "worker-not-found"); err != nil {
		t.Fatalf("a stood-down daemon could not re-register: %v", err)
	}
	registrations := h.registrations()
	if len(registrations) != before+1 {
		t.Fatalf("re-registrations = %d, want exactly 1", len(registrations)-before)
	}
	var request RegisterRequest
	if err := json.Unmarshal(registrations[before], &request); err != nil {
		t.Fatalf("decode re-registration: %v", err)
	}
	if !request.SessionShimHostAttestation.StandsDown() {
		t.Fatalf("re-registration presented %#v, want the stand-down", request.SessionShimHostAttestation)
	}
	if request.Capacity <= 0 {
		t.Fatalf("re-registration capacity = %d, want the host's real capacity, not auth-only", request.Capacity)
	}
}

// TestStartupFoundingRefusalClassifierCoversTheRegistrationLeg pins the new
// carrier: a refused startup founding registration classifies as a retryable
// founding refusal — never the batch refusal — while every status that is not
// a heard-and-answered client error keeps its ordinary error.
func TestStartupFoundingRefusalClassifierCoversTheRegistrationLeg(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		definite bool
	}{
		{name: "registration bad request", err: &registerHTTPError{status: 400, body: "bad"}, definite: true},
		{name: "registration forbidden", err: &registerHTTPError{status: 403, body: "no"}, definite: true},
		{name: "registration plain conflict", err: &registerHTTPError{status: 409, body: "conflict"}, definite: true},
		{name: "registration unprocessable", err: &registerHTTPError{status: 422, body: "no"}, definite: true},
		{
			name: "registration carrying the stale code is behind, not refused",
			err:  &registerHTTPError{status: 409, body: "SESSION_SHIM_ADOPTION_REVISION_STALE"},
		},
		{name: "registration unauthorized stays ordinary", err: &registerHTTPError{status: 401, body: "expired"}},
		{name: "registration not found stays ordinary", err: &registerHTTPError{status: 404, body: "gone"}},
		{name: "registration server error stays ordinary", err: &registerHTTPError{status: 503, body: "down"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionShimFoundingRefusalIsDefinite(tc.err); got != tc.definite {
				t.Fatalf("sessionShimFoundingRefusalIsDefinite(%v) = %v, want %v", tc.err, got, tc.definite)
			}
			refused := newSessionShimFoundingRefused("scope", tc.err)
			if (refused != nil) != tc.definite {
				t.Fatalf("newSessionShimFoundingRefused(%v) classified = %v, want %v", tc.err, refused != nil, tc.definite)
			}
			if refused != nil {
				var batchRefused *SessionShimDurabilityRefused
				if errors.As(refused, &batchRefused) {
					t.Fatalf("founding refusal %v matches the batch refusal type", refused)
				}
				if !refused.Retryable() {
					t.Fatal("a founding refusal must always be retryable")
				}
			}
		})
	}
}

// TestStartupTransportFailureStillFailsStartup is the negative side: a
// transport-level registration failure is recovered by the supervised restart
// that an error triggers, so it keeps its ordinary error and fails Start —
// standing the composition down over a relay blip would trade a transient
// outage for a silent one.
func TestStartupTransportFailureStillFailsStartup(t *testing.T) {
	h := newStartupFoundingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.server.Close()

	if err := h.daemon.Start(ctx); err == nil {
		t.Fatal("Start over an unreachable control plane succeeded")
	} else {
		var refused *SessionShimFoundingRefused
		if errors.As(err, &refused) {
			t.Fatalf("transport failure classified as a founding refusal: %v", err)
		}
	}
}

// TestOrphanedFirstHeartbeatFenceDoesNotCloseTheClaimGate pins the host-wide
// claim-gate half: a first-heartbeat admission fence for a scope the daemon
// holds no authority receipt for is an orphan no heartbeat can clear. The
// claim gate must not stay closed for every scope behind it.
func TestOrphanedFirstHeartbeatFenceDoesNotCloseTheClaimGate(t *testing.T) {
	h := newStartupFoundingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.daemon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.daemon.Stop(context.Background()) })

	scope := h.daemon.sessionShimConfig().orgID()
	if scope == "" {
		t.Fatal("startup founding left no composed scope")
	}
	// Plant the orphan the way a refused founding leaves it: the scope's
	// receipt is gone while its first-heartbeat acknowledgement is still
	// pending and the fence is raised.
	h.daemon.shims.mu.Lock()
	delete(h.daemon.shims.credentialReceipts, scope)
	h.daemon.shims.pendingHeartbeatAcks[scope] = "revision-orphan"
	h.daemon.shims.mu.Unlock()
	h.daemon.sessionShimReadinessWithdrawn.Store(true)

	if suspended, _ := h.daemon.PollClaimGate()(); suspended {
		t.Fatal("the claim gate stayed closed behind an orphaned first-heartbeat fence")
	}
	h.daemon.shims.mu.RLock()
	_, stillPending := h.daemon.shims.pendingHeartbeatAcks[scope]
	h.daemon.shims.mu.RUnlock()
	if stillPending {
		t.Fatal("the claim gate reopened without clearing the orphaned fence")
	}
	if h.daemon.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("the orphaned fence is still raised after the claim gate reconciled it")
	}
	if _, err := h.daemon.AcceptWorkWithDetail(SessionSpec{}, &SessionDetail{}); err == nil ||
		containsWithdrawn(err.Error()) {
		t.Fatalf("work admission behind an orphaned fence: err = %v, want the benign spec refusal", err)
	}
}

// TestHealthyFirstHeartbeatFenceStillClosesTheClaimGate is the control: a
// pending first-heartbeat acknowledgement for a scope the daemon DOES hold
// authority for keeps the host-wide claim gate closed until the exact
// heartbeat is acknowledged. The orphan rule above must not loosen the live
// fence.
func TestHealthyFirstHeartbeatFenceStillClosesTheClaimGate(t *testing.T) {
	h := newStartupFoundingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.daemon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.daemon.Stop(context.Background()) })

	scope := h.daemon.sessionShimConfig().orgID()
	h.daemon.shims.mu.Lock()
	_, retained := h.daemon.shims.credentialReceipts[scope]
	h.daemon.shims.mu.Unlock()
	if !retained {
		t.Fatal("startup founding retained no authority receipt; the control proves nothing")
	}
	h.daemon.shims.mu.Lock()
	h.daemon.shims.pendingHeartbeatAcks[scope] = "revision-live"
	h.daemon.shims.mu.Unlock()
	h.daemon.sessionShimReadinessWithdrawn.Store(true)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if suspended, _ := h.daemon.PollClaimGate()(); suspended {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the claim gate opened behind a live first-heartbeat fence")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRefusedFirstHeartbeatLeavesNoFenceForTheNextOrg pins the claim-gate half
// on the sequence that really produces an orphaned fence, not on planted state:
// a deferred install's boot adoption raises its scope's first-heartbeat fence,
// the platform refuses that first projected heartbeat, and a DIFFERENT org then
// founds the composition. The refused scope's fence must not survive the
// rollback, and once the new founder's first beat is acknowledged the
// host-wide claim gate is open. Without either reconciliation the refused
// scope's fence kept the gate closed for every scope, with no heartbeat left
// that could clear it.
func TestRefusedFirstHeartbeatLeavesNoFenceForTheNextOrg(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	h.setRefuseFirstProjectedHeartbeat(true)
	first := h.composedConfig(acceptingBatch)
	var refused *SessionShimFoundingRefused
	if err := h.daemon.InstallSessionShimComposition(ctx, first); !errors.As(err, &refused) {
		t.Fatalf("refused first-heartbeat install error = %v, want a founding refusal", err)
	}
	h.daemon.shims.mu.RLock()
	leftover := len(h.daemon.shims.pendingHeartbeatAcks)
	h.daemon.shims.mu.RUnlock()
	if leftover != 0 {
		t.Fatalf("the refused install left %d first-heartbeat fence(s) behind", leftover)
	}

	second := h.composedConfig(acceptingBatch)
	second.OrgID = "org-second-composition"
	second.ControllerID = "controller-second-org"
	second.AttestationCapabilities = append([]string(nil), first.AttestationCapabilities...)
	if err := h.daemon.InstallSessionShimComposition(ctx, second); err != nil {
		t.Fatalf("a refused first heartbeat blocked another org: %v", err)
	}
	if suspended, reason := h.daemon.PollClaimGate()(); suspended {
		t.Fatalf("the claim gate stayed closed after another org founded and was acknowledged: %s", reason)
	}
}

func containsWithdrawn(s string) bool {
	lowered := strings.ToLower(s)
	for _, token := range []string{"withdrawn", "not ready", "readiness"} {
		if strings.Contains(lowered, token) {
			return true
		}
	}
	return false
}
