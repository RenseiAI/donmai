package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// TestDaemonReadinessDoesNotWaitOnTheDurableSessionComposition is the whole
// point of the change: the daemon reaches its serving state and answers its
// control API while a composition that never finishes is still running.
//
// The composition here HANGS. Any design that waits for it — before New,
// inside Start, or anywhere on the path to the listener — cannot pass this
// test, because there is nothing to wait for.
func TestDaemonReadinessDoesNotWaitOnTheDurableSessionComposition(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	h.start(ctx)
	startDuration := time.Since(started)

	srv := NewServer(h.daemon)
	errCh, err := srv.Start()
	if err != nil {
		t.Fatalf("control server start: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-errCh
	})

	release := make(chan struct{})
	installDone := make(chan error, 1)
	entered := make(chan struct{})
	var enterOnce sync.Once
	go func() {
		installDone <- h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
			func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				enterOnce.Do(func() { close(entered) })
				<-release
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("composition"), AdoptionRevision: "revision-batch",
				}, nil
			}))
	}()

	select {
	case <-entered:
	case err := <-installDone:
		t.Fatalf("composition install returned before reaching adoption: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("composition install never reached the adoption pass")
	}

	// The composition is now wedged. Everything below happens while it is.
	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state during a hung composition = %q, want %q", state, StateRunning)
	}
	status := getJSON(t, "http://"+srv.Addr()+"/api/daemon/status")
	if status["status"] != string(afclient.DaemonReady) {
		t.Fatalf("control API status during a hung composition = %#v, want %q",
			status["status"], afclient.DaemonReady)
	}
	if startDuration > 5*time.Second {
		t.Fatalf("Start took %s — readiness is still coupled to something slow", startDuration)
	}

	// Positive control: the SAME hanging composition, handed to New the way it
	// used to be, does not let Start return at all. Without this the test above
	// proves only that a hang somewhere is survivable, not that moving the
	// composition is what made it survivable.
	inline := newCompositionHarness(t)
	inlineEntered := make(chan struct{})
	var inlineOnce sync.Once
	inline.daemon = New(Options{
		ConfigPath: inline.daemon.opts.ConfigPath, JWTPath: inline.daemon.opts.JWTPath,
		HTTPHost: "127.0.0.1", HTTPPort: 0, SkipWizard: true,
		SessionShim: inline.composedConfig(
			func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				inlineOnce.Do(func() { close(inlineEntered) })
				<-release
				return SessionShimAdoptionBatchReceipt{}, errors.New("released")
			}),
	})
	inlineStarted := make(chan error, 1)
	go func() { inlineStarted <- inline.daemon.Start(ctx) }()
	select {
	case <-inlineEntered:
	case err := <-inlineStarted:
		t.Fatalf("inline control: Start returned before its composition ran: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("inline control: composition never ran")
	}
	select {
	case err := <-inlineStarted:
		t.Fatalf("inline control: Start returned while its composition was hung (%v) — "+
			"the control does not discriminate", err)
	case <-time.After(250 * time.Millisecond):
	}
	if state := inline.daemon.State(); state == StateRunning {
		t.Fatalf("inline control: daemon reached %q with its composition hung", state)
	}

	close(release)
	if err := <-installDone; err != nil {
		t.Fatalf("composition install: %v", err)
	}
	<-inlineStarted
	_ = inline.daemon.Stop(context.Background())
}

// TestSessionShimIsNotAnnouncedActiveBeforeAdoptionCompletes is the care point.
// A control plane's heartbeat preflight demands exact agreement and demotes a
// host that disagrees, so the window between "configuration installed" and
// "adoption complete" must carry NO projection and hand no session to a shim.
func TestSessionShimIsNotAnnouncedActiveBeforeAdoptionCompletes(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	type midAdoption struct {
		attestationSupported bool
		adoptionComplete     bool
		ownsInteractive      bool
		beatCarriedShim      bool
		beatErr              error
	}
	var observed midAdoption

	err := h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
		func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			// The configuration IS live here — the adoption pass reads it — so
			// this is exactly the moment an early announcement would happen.
			observed.attestationSupported = h.daemon.SessionShimHostAttestation().Supports()
			observed.adoptionComplete = h.daemon.SessionShimAdoptionComplete()
			observed.ownsInteractive = h.daemon.SessionShimOwnsSession(SessionSpec{
				SessionID: "session-mid-adoption", Mode: interactiveRunMode,
			})
			before := len(h.heartbeats())
			observed.beatErr = h.daemon.heartbeat.SendNow(ctx)
			beats := h.heartbeats()
			if len(beats) > before {
				observed.beatCarriedShim = beats[len(beats)-1].SessionShim != nil
			}
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("composition"), AdoptionRevision: "revision-batch",
			}, nil
		}))
	if err != nil {
		t.Fatalf("composition install: %v", err)
	}

	if !observed.attestationSupported {
		t.Fatal("the adoption pass ran without the composed attestation installed")
	}
	if observed.adoptionComplete {
		t.Fatal("adoption reported complete from inside the adoption pass")
	}
	if observed.beatErr != nil {
		t.Fatalf("a heartbeat sent during the adoption pass failed: %v", observed.beatErr)
	}
	if observed.beatCarriedShim {
		t.Fatal("a heartbeat sent BEFORE adoption completed announced the session shim")
	}
	if observed.ownsInteractive {
		t.Fatal("an interactive session was claimed by the shim before adoption completed")
	}

	// And the announcement does land, once it is true.
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatalf("no projected heartbeat after the composition installed: %+v", beat)
	}
	if !beat.SessionShim.AdoptionComplete {
		t.Fatalf("projected heartbeat adoptionComplete = false: %+v", beat.SessionShim)
	}
	if !h.daemon.SessionShimOwnsSession(SessionSpec{SessionID: "session-after", Mode: interactiveRunMode}) {
		t.Fatal("an interactive session was not claimed by the shim after the composition installed")
	}
}

// TestCompositionDeclarationOrderIsStandDownThenAttested pins the two wire
// facts that make the deferral safe: the ordinary registration carries the
// explicit stand-down, and the composed attestation reaches the control plane
// on THIS worker identity through the refresh lane — never a second
// registration, which would mint a competing worker and retire the one every
// lane is holding.
func TestCompositionDeclarationOrderIsStandDownThenAttested(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	registrationsBeforeInstall := len(h.registrations())
	var declaredBeforeAdoption bool
	if err := h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
		func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			declaredBeforeAdoption = len(h.refreshes()) > 0
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("composition"), AdoptionRevision: "revision-batch",
			}, nil
		})); err != nil {
		t.Fatalf("composition install: %v", err)
	}

	registrations := h.registrations()
	if len(registrations) != 1 {
		t.Fatalf("registrations = %d, want exactly the one stand-down registration", len(registrations))
	}
	if len(registrations) != registrationsBeforeInstall {
		t.Fatal("installing a composition performed a second worker registration")
	}
	present := shimKeysIn(t, registrations[0])
	if len(present) != 1 || present["sessionShimSupported"] != false {
		t.Fatalf("registration presented %#v, want exactly sessionShimSupported=false: %s",
			present, registrations[0])
	}

	refreshes := h.refreshes()
	if len(refreshes) == 0 {
		t.Fatal("the composed attestation was never declared to the control plane")
	}
	if !declaredBeforeAdoption {
		t.Fatal("the adoption pass ran before the composition was declared")
	}
	var declared SessionShimHostAttestation
	if err := json.Unmarshal(refreshes[len(refreshes)-1], &declared); err != nil {
		t.Fatalf("decode declaring refresh: %v", err)
	}
	if !declared.exactEqual(h.attestation) {
		t.Fatalf("declaring refresh presented %#v, want the composed attestation %#v",
			declared, h.attestation)
	}
}

// TestFailedCompositionLeavesTheDaemonServingAndStoodDown is the degrade path.
// A host that cannot bring durable sessions up loses one feature; it must not
// lose the host, and it must not leave the control plane believing in a
// composition that is not there.
func TestFailedCompositionLeavesTheDaemonServingAndStoodDown(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	srv := NewServer(h.daemon)
	errCh, err := srv.Start()
	if err != nil {
		t.Fatalf("control server start: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-errCh
	})

	installErr := h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
		func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{}, errors.New("durable adoption store unavailable")
		}))
	if installErr == nil {
		t.Fatal("a composition whose adoption failed reported success")
	}

	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state after a failed composition = %q, want %q", state, StateRunning)
	}
	status := getJSON(t, "http://"+srv.Addr()+"/api/daemon/status")
	if status["status"] != string(afclient.DaemonReady) {
		t.Fatalf("control API status after a failed composition = %#v", status["status"])
	}
	attestation := h.daemon.SessionShimHostAttestation()
	if !attestation.StandsDown() || attestation.Supports() {
		t.Fatalf("attestation after a failed composition = %#v, want the explicit stand-down", attestation)
	}
	if h.daemon.sessionShimConfig().EnableOwnership {
		t.Fatal("a failed composition left session ownership enabled")
	}
	if h.daemon.SessionShimOwnsSession(SessionSpec{SessionID: "session-degraded", Mode: interactiveRunMode}) {
		t.Fatal("a failed composition left an interactive session claimed by the shim")
	}

	// The withdrawal has to reach the wire too, on both lanes.
	if err := h.daemon.heartbeat.SendNow(ctx); err != nil {
		t.Fatalf("heartbeat after a failed composition: %v", err)
	}
	beat, ok := h.lastHeartbeat()
	if !ok {
		t.Fatal("no heartbeat after a failed composition")
	}
	if beat.SessionShim != nil {
		t.Fatalf("a heartbeat after a failed composition still announced the shim: %+v", beat.SessionShim)
	}
	refreshes := h.refreshes()
	if len(refreshes) == 0 {
		t.Fatal("the failed composition never reached the declaring refresh")
	}
	var lastDeclared SessionShimHostAttestation
	if err := json.Unmarshal(refreshes[len(refreshes)-1], &lastDeclared); err != nil {
		t.Fatalf("decode last refresh: %v", err)
	}
	if lastDeclared.Supports() {
		t.Fatalf("the last declaration after a failed composition still claimed support: %#v", lastDeclared)
	}
}

// TestInstalledCompositionEndsWithTheAttestationDeclared is the success shape,
// asserted at every surface that has to agree: the local tuple, the credential
// lane, and the heartbeat projection.
func TestInstalledCompositionEndsWithTheAttestationDeclared(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
		t.Fatalf("attestation before the composition = %#v, want the explicit stand-down", got)
	}

	if err := h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
		func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("composition"), AdoptionRevision: "revision-batch",
			}, nil
		})); err != nil {
		t.Fatalf("composition install: %v", err)
	}

	if got := h.daemon.SessionShimHostAttestation(); !got.exactEqual(h.attestation) {
		t.Fatalf("attestation after the composition = %#v, want %#v", got, h.attestation)
	}
	if !h.daemon.SessionShimAdoptionComplete() || !h.daemon.SessionShimCarrierActivationComplete() {
		t.Fatalf("readiness after the composition: adoption=%v activation=%v",
			h.daemon.SessionShimAdoptionComplete(), h.daemon.SessionShimCarrierActivationComplete())
	}
	if h.daemon.SessionShimCompositionPending() {
		t.Fatal("the composition is still reported pending after a successful install")
	}
	if got := h.daemon.credentials.SessionShimAttestation(); !got.exactEqual(h.attestation) {
		t.Fatalf("credential lane attestation = %#v, want the composed attestation", got)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the installed composition never reached a projected heartbeat")
	}
	if beat.SessionShim.ControllerID != h.attestation.ControllerID {
		t.Fatalf("projection controller = %q, want %q",
			beat.SessionShim.ControllerID, h.attestation.ControllerID)
	}
	if beat.SessionShim.AdoptionRevision != "revision-batch" {
		t.Fatalf("projection adoption revision = %q, want the batch receipt's",
			beat.SessionShim.AdoptionRevision)
	}

	// A second composition is refused rather than layered on the first.
	if err := h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
		func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{}, nil
		})); err == nil {
		t.Fatal("a second composition install was accepted")
	}
}

// TestDeclareSessionShimRestoresThePreviousAttestationOnFailure covers the
// refresher on its own. A refusal must not leave the lane presenting a claim
// authority rejected — the next unattended expiry refresh would present it
// again, with nobody watching — and it must not burn the worker identity to
// deliver an attestation nobody asked for.
func TestDeclareSessionShimRestoresThePreviousAttestationOnFailure(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		reason  string
	}{
		"authority refuses the attestation": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "attestation refused", http.StatusBadRequest)
			},
			reason: "declaration-refused",
		},
		"authority cannot re-present this identity": {
			// A 404 is what RefreshRuntimeToken treats as "the registration is
			// gone" and answers with a FULL re-register. A declaration must not
			// take that path: it would mint a competing worker identity and
			// retire the one every lane is presenting, for a feature this host
			// was merely offering.
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "worker not found", http.StatusNotFound)
			},
			reason: "declaration-unavailable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var registrations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == RegisterEndpoint {
					registrations.Add(1)
					_ = json.NewEncoder(w).Encode(RegisterResponse{
						WorkerID: "worker-competing", RuntimeToken: "runtime-competing",
					})
					return
				}
				tc.handler(w, r)
			}))
			t.Cleanup(server.Close)

			refresher := NewCredentialRefresher(CredentialRefresherOptions{
				Registration: RegistrationOptions{
					OrchestratorURL: server.URL, RegistrationToken: "rsp_live_declare",
					JWTPath:     filepath.Join(t.TempDir(), "daemon.jwt"),
					SessionShim: SessionShimStandDownAttestation(),
				},
				WorkerID: "worker-declare", RuntimeJWT: "runtime-declare",
			})
			if _, err := refresher.DeclareSessionShim(
				context.Background(), activationTestAttestation(), tc.reason,
			); err == nil {
				t.Fatal("a refused declaration reported success")
			}
			if got := refresher.SessionShimAttestation(); !got.StandsDown() || got.Supports() {
				t.Fatalf("attestation after a refused declaration = %#v, want the previous stand-down", got)
			}
			if got := registrations.Load(); got != 0 {
				t.Fatalf("a failed declaration performed %d full re-registrations, want none", got)
			}
			if workerID, _ := refresher.Current(); workerID != "worker-declare" {
				t.Fatalf("worker identity after a failed declaration = %q, want the one it already held", workerID)
			}
		})
	}
}

// TestCompositionInstallDoesNotRaceLiveConfigurationReaders keeps the race
// detector honest about the one thing this change made mutable after New: the
// live durable-session configuration, read by every ownership decision while an
// install swaps it.
func TestCompositionInstallDoesNotRaceLiveConfigurationReaders(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	var reads atomic.Int64
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = h.daemon.sessionShimConfig().orgID()
				_ = h.daemon.SessionShimHostAttestation()
				_ = h.daemon.SessionShimOwnsSession(SessionSpec{SessionID: "s", Mode: interactiveRunMode})
				reads.Add(1)
			}
		}()
	}

	err := h.daemon.InstallSessionShimComposition(ctx, h.composedConfig(
		func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("composition"), AdoptionRevision: "revision-batch",
			}, nil
		}))
	close(stop)
	readers.Wait()
	if err != nil {
		t.Fatalf("composition install: %v", err)
	}
	if reads.Load() == 0 {
		t.Fatal("no concurrent reads ran — the race probe proved nothing")
	}
	if got := h.daemon.sessionShimConfig().orgID(); got != h.orgID {
		t.Fatalf("installed configuration org = %q, want %q", got, h.orgID)
	}
}
