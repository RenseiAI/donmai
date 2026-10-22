package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialRefresher_ValidationRefusalIsAtomicAndVisible(t *testing.T) {
	t.Parallel()
	srv := reregisterServer(t, "controller-exact", "after.jwt")
	defer srv.Close()
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.ValidateRefresh = func(result *RefreshTokenResult) error {
		if result.WorkerID == "controller-exact" {
			return errors.New("worker registration id aliases controller id")
		}
		return nil
	}
	r := NewCredentialRefresher(opts)
	lane := &recordingLane{}
	r.Attach(lane)
	_, _, setsBefore := lane.current()
	if _, err := r.Refresh(context.Background(), "worker-not-found"); err == nil {
		t.Fatal("aliasing refresh succeeded; want visible validation error")
	}
	if id, jwt := r.Current(); id != "wkr_before" || jwt != "before.jwt" {
		t.Fatalf("refresher changed after refusal: (%q,%q)", id, jwt)
	}
	if id, jwt, sets := lane.current(); id != "wkr_before" || jwt != "before.jwt" || sets != setsBefore {
		t.Fatalf("lane changed after refusal: (%q,%q,%d), want old credentials and %d sets", id, jwt, sets, setsBefore)
	}
}

// recordingLane is a CredentialLane that remembers every credential set it was
// handed, so a test can assert both WHAT it ended on and that it was told at
// all.
type recordingLane struct {
	mu       sync.Mutex
	workerID string
	jwt      string
	sets     int
}

func (l *recordingLane) SetCredentials(workerID, runtimeJWT string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.workerID = workerID
	l.jwt = runtimeJWT
	l.sets++
}

func (l *recordingLane) current() (string, string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.workerID, l.jwt, l.sets
}

// reregisterServer answers the refresh probe with 404 (registration retired)
// and mints a new identity on register, which is the shape that changes the
// worker id and therefore the shape that used to strand un-updated lanes.
func reregisterServer(t *testing.T, newWorkerID, newJWT string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == RegisterEndpoint {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     newWorkerID,
				"runtimeToken": newJWT,
			})
			return
		}
		http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
	}))
}

func testRefresherOptions(t *testing.T, url, workerID, jwt string) CredentialRefresherOptions {
	t.Helper()
	return CredentialRefresherOptions{
		Registration: RegistrationOptions{
			OrchestratorURL: url,
			// #nosec G101 -- test fixture
			RegistrationToken: "rsp_live_x",
			Hostname:          "label",
			Version:           Version,
			MaxAgents:         1,
			JWTPath:           t.TempDir() + "/jwt.json",
			HTTPClient:        &http.Client{Timeout: 5 * time.Second},
		},
		WorkerID:   workerID,
		RuntimeJWT: jwt,
	}
}

func TestCredentialRefresher_ReloadReregistrationKeepsNewestProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entered := make(chan string, 2)
	releaseAlpha := make(chan struct{})
	defer func() {
		select {
		case <-releaseAlpha:
		default:
			close(releaseAlpha)
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != RegisterEndpoint {
			http.NotFound(w, request)
			return
		}
		var body RegisterRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.ProjectIDs) != 1 {
			t.Errorf("decode registration: %v, project IDs %v", err, body.ProjectIDs)
			return
		}
		id := body.ProjectIDs[0]
		entered <- id
		if id == "alpha" {
			select {
			case <-releaseAlpha:
			case <-request.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_" + id, "runtimeToken": id + ".jwt"})
	}))
	defer srv.Close()
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	r := NewCredentialRefresher(opts)
	lane := &recordingLane{}
	r.Attach(lane)
	r.UpdateRegistrationProjects(nil, []string{"alpha"}, ProjectAdmissionModeEnumerated)
	done := r.RequestReregister(ctx)
	select {
	case id := <-entered:
		if id != "alpha" {
			t.Fatalf("first registration = %q, want alpha", id)
		}
	case <-ctx.Done():
		t.Fatal("alpha registration did not enter")
	}
	r.UpdateRegistrationProjects(nil, []string{"beta"}, ProjectAdmissionModeAllRouted)
	r.RequestReregister(ctx)
	close(releaseAlpha)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("coalesced registration did not finish")
	}
	select {
	case id := <-entered:
		if id != "beta" {
			t.Fatalf("second registration = %q, want beta", id)
		}
	default:
		t.Fatal("no follow-up registration used the newest projection")
	}
	if workerID, _ := r.Current(); workerID != "wkr_beta" {
		t.Errorf("final refresher worker = %q, want beta", workerID)
	}
	if workerID, _, _ := lane.current(); workerID != "wkr_beta" {
		t.Errorf("final attached lane worker = %q, want beta", workerID)
	}
}

func TestCredentialRefresher_CanceledReloadLeavesQueuedOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var registerMu sync.Mutex
	registerCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == RegisterEndpoint {
			registerMu.Lock()
			registerCount++
			registerMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_late", "runtimeToken": "late.jwt"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()
	enteredValidation := make(chan struct{})
	releaseValidation := make(chan struct{})
	defer func() {
		select {
		case <-releaseValidation:
		default:
			close(releaseValidation)
		}
	}()
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.ValidateRefresh = func(*RefreshTokenResult) error {
		close(enteredValidation)
		<-releaseValidation
		return nil
	}
	r := NewCredentialRefresher(opts)
	refreshDone := make(chan error, 1)
	go func() {
		_, err := r.Refresh(ctx, "worker-not-found")
		refreshDone <- err
	}()
	select {
	case <-enteredValidation:
	case <-ctx.Done():
		t.Fatal("refresh did not reach validation")
	}
	reloadCtx, cancelReload := context.WithCancel(ctx)
	done := r.RequestReregister(reloadCtx)
	cancelReload()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("canceled reload stayed queued behind refresh")
	}
	registerMu.Lock()
	gotRegisters := registerCount
	registerMu.Unlock()
	if gotRegisters != 0 {
		t.Errorf("registration calls after canceled queued reload = %d, want zero", gotRegisters)
	}
	close(releaseValidation)
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("original refresh: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("original refresh did not finish")
	}
	if workerID, _ := r.Current(); workerID != "wkr_before" {
		t.Errorf("worker after canceled reload = %q, want original identity", workerID)
	}
}

func TestCredentialRefresher_CallbackCanQueueNewerProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	seen := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != RegisterEndpoint {
			http.NotFound(w, request)
			return
		}
		var body RegisterRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.ProjectIDs) != 1 {
			t.Errorf("decode registration: %v, project IDs %v", err, body.ProjectIDs)
			return
		}
		id := body.ProjectIDs[0]
		seen <- id
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_" + id, "runtimeToken": id + ".jwt"})
	}))
	defer srv.Close()
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	var refresher *CredentialRefresher
	opts.OnRefreshed = func(result *RefreshTokenResult) {
		if result.WorkerID == "wkr_alpha" {
			refresher.UpdateRegistrationProjects(nil, []string{"beta"}, ProjectAdmissionModeAllRouted)
			refresher.RequestReregister(ctx)
		}
	}
	refresher = NewCredentialRefresher(opts)
	refresher.UpdateRegistrationProjects(nil, []string{"alpha"}, ProjectAdmissionModeEnumerated)
	done := refresher.RequestReregister(ctx)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("callback-queued follow-up registration deadlocked")
	}
	for _, want := range []string{"alpha", "beta"} {
		select {
		case got := <-seen:
			if got != want {
				t.Errorf("registration order = %q, want %q", got, want)
			}
		default:
			t.Fatalf("missing %s registration", want)
		}
	}
	if workerID, _ := refresher.Current(); workerID != "wkr_beta" {
		t.Errorf("final worker = %q, want beta", workerID)
	}
}

func TestCredentialRefresher_ProjectUpdatePreservesRegistrationAuthority(t *testing.T) {
	opts := testRefresherOptions(t, "http://127.0.0.1:1", "wkr_before", "before.jwt")
	opts.Registration.SessionShim = SessionShimStandDownAttestation()
	opts.Registration.AuthOnly = true
	opts.Registration.ValidateCredentials = func(string, string) error { return nil }
	r := NewCredentialRefresher(opts)
	r.UpdateRegistrationProjects(
		[]ProjectAllowlistEntry{{ID: "beta", Repository: "github.com/x/beta"}},
		[]string{"beta"},
		ProjectAdmissionModeAllRouted,
	)
	r.mu.Lock()
	got := r.opts.Registration
	r.mu.Unlock()
	if got.RegistrationToken != opts.Registration.RegistrationToken ||
		got.OrchestratorURL != opts.Registration.OrchestratorURL ||
		got.JWTPath != opts.Registration.JWTPath ||
		got.HTTPClient != opts.Registration.HTTPClient ||
		got.ValidateCredentials == nil || !got.AuthOnly || !got.SessionShim.StandsDown() {
		t.Fatal("project update replaced non-project registration authority")
	}
	if len(got.ProjectIDs) != 1 || got.ProjectIDs[0] != "beta" ||
		len(got.DaemonProjects) != 1 || got.DaemonProjects[0].ID != "beta" ||
		got.ProjectAdmissionMode != ProjectAdmissionModeAllRouted {
		t.Fatalf("updated project declaration = IDs %v, entries %+v, mode %q", got.ProjectIDs, got.DaemonProjects, got.ProjectAdmissionMode)
	}
}

func TestDaemonStopJoinsValidationBeforePublishingTerminalState(t *testing.T) {
	controlCtx, cancelControl := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelControl()
	enteredValidation := make(chan struct{})
	releaseValidation := make(chan struct{})
	defer func() {
		select {
		case <-releaseValidation:
		default:
			close(releaseValidation)
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != RegisterEndpoint {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_new", "runtimeToken": "new.jwt"})
	}))
	defer srv.Close()
	d := New(Options{ConfigPath: "/dev/null"})
	d.setState(StateRunning)
	var retainedReceiptWorker string
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.MachineID = "test-machine"
	opts.ValidateRefresh = func(result *RefreshTokenResult) error {
		close(enteredValidation)
		<-releaseValidation
		retainedReceiptWorker = result.WorkerID
		return nil
	}
	opts.OnRefreshed = func(result *RefreshTokenResult) {
		d.mu.Lock()
		d.workerID = result.WorkerID
		d.jwt = result.RuntimeToken
		d.mu.Unlock()
	}
	credentials := NewCredentialRefresher(opts)
	reloadCtx, cancelReload := context.WithCancel(context.Background())
	stopReachedCancel := make(chan struct{})
	var signalOnce sync.Once
	d.mu.Lock()
	d.credentials = credentials
	d.workerID = "wkr_before"
	d.jwt = "before.jwt"
	d.registrationReloadCtx = reloadCtx
	d.registrationReloadCancel = func() {
		signalOnce.Do(func() { close(stopReachedCancel) })
		cancelReload()
	}
	d.mu.Unlock()
	reloadDone := credentials.RequestReregister(reloadCtx)
	select {
	case <-enteredValidation:
	case <-controlCtx.Done():
		t.Fatal("registration did not reach validation")
	}
	stopCtx, cancelStop := context.WithCancel(context.Background())
	stopDone := make(chan error, 1)
	go func() { stopDone <- d.Stop(stopCtx) }()
	select {
	case <-stopReachedCancel:
	case <-controlCtx.Done():
		t.Fatal("Stop did not cancel project reload")
	}
	cancelStop()
	select {
	case err := <-stopDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop while validation is held = %v, want context cancellation", err)
		}
	case <-controlCtx.Done():
		t.Fatal("Stop did not return after caller cancellation")
	}
	if d.State() != StateDraining {
		t.Fatalf("state after incomplete Stop = %q, want draining", d.State())
	}
	select {
	case <-d.Done():
		t.Fatal("Done closed before in-flight validation completed")
	default:
	}
	close(releaseValidation)
	select {
	case <-reloadDone:
	case <-controlCtx.Done():
		t.Fatal("registration did not finish after validation release")
	}
	workerID, jwt := credentials.Current()
	d.mu.RLock()
	daemonWorkerID, daemonJWT := d.workerID, d.jwt
	d.mu.RUnlock()
	if retainedReceiptWorker != "wkr_new" || workerID != "wkr_new" || jwt != "new.jwt" ||
		daemonWorkerID != "wkr_new" || daemonJWT != "new.jwt" {
		t.Fatal("validated receipt, refresher, and daemon credentials did not finish on the same registration")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop after joined registration: %v", err)
	}
	if d.State() != StateStopped {
		t.Fatalf("state after completed Stop = %q, want stopped", d.State())
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("Done remained open after joined Stop")
	}
}

func TestCredentialRefresher_DeclareSessionShimSerializesCanceledReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	enteredDeclaration := make(chan struct{})
	releaseDeclaration := make(chan struct{})
	defer func() {
		select {
		case <-releaseDeclaration:
		default:
			close(releaseDeclaration)
		}
	}()
	var registerMu sync.Mutex
	registerCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == RegisterEndpoint {
			registerMu.Lock()
			registerCount++
			registerMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_late", "runtimeToken": "late.jwt"})
			return
		}
		close(enteredDeclaration)
		select {
		case <-releaseDeclaration:
		case <-request.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "declared.jwt"})
	}))
	defer srv.Close()
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.SessionShim = SessionShimStandDownAttestation()
	r := NewCredentialRefresher(opts)
	declarationDone := make(chan error, 1)
	go func() {
		_, err := r.DeclareSessionShim(ctx, SessionShimStandDownAttestation(), "declaration-test")
		declarationDone <- err
	}()
	select {
	case <-enteredDeclaration:
	case <-ctx.Done():
		t.Fatal("session-shim declaration did not reach refresh endpoint")
	}
	reloadCtx, cancelReload := context.WithCancel(ctx)
	done := r.RequestReregister(reloadCtx)
	cancelReload()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("canceled reload remained behind session-shim declaration")
	}
	registerMu.Lock()
	gotRegisters := registerCount
	registerMu.Unlock()
	if gotRegisters != 0 {
		t.Errorf("registration calls while declaration held = %d, want zero", gotRegisters)
	}
	close(releaseDeclaration)
	select {
	case err := <-declarationDone:
		if err != nil {
			t.Fatalf("session-shim declaration: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("session-shim declaration did not finish")
	}
	if workerID, _ := r.Current(); workerID != "wkr_before" {
		t.Errorf("worker after canceled reload = %q, want original identity", workerID)
	}
}

// TestCredentialRefresher_EveryLaneSurvivesAReregistration is the structural
// regression test for the worker re-registration loop.
//
// Several lanes present one registration, but only the lane that gets rejected
// calls the recovery hook. Any lane left holding a superseded worker id is
// rejected on ITS next tick, re-registers, and retires the registration the
// first refresh had just settled on — the two lanes then evict each other
// forever.
//
// So: whichever lane drives the refresh, EVERY attached lane must come out of
// it on the same credentials.
func TestCredentialRefresher_EveryLaneSurvivesAReregistration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		laneCount int
	}{
		{name: "heartbeat and poll", laneCount: 2},
		{name: "an embedder with extra lanes", laneCount: 4},
		{name: "a single lane", laneCount: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const newWorker = "wkr_after_reregister"
			const newJWT = "after.jwt"
			srv := reregisterServer(t, newWorker, newJWT)
			defer srv.Close()

			r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))

			lanes := make([]*recordingLane, tc.laneCount)
			for i := range lanes {
				lanes[i] = &recordingLane{}
				r.Attach(lanes[i])
			}

			// Lane 0 is the one that took the rejection; the rest are silent.
			if _, err := r.Refresh(context.Background(), "worker-not-found"); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			for i, lane := range lanes {
				gotID, gotJWT, sets := lane.current()
				if gotID != newWorker || gotJWT != newJWT {
					t.Errorf("lane %d = (%q, %q), want (%q, %q) — a lane left on a superseded identity re-registers on its next tick and retires the registration this refresh settled on",
						i, gotID, gotJWT, newWorker, newJWT)
				}
				if sets == 0 {
					t.Errorf("lane %d was never told about the refresh", i)
				}
			}
			if gotID, _ := r.Current(); gotID != newWorker {
				t.Errorf("refresher Current() = %q, want %q", gotID, newWorker)
			}
		})
	}
}

// TestCredentialRefresher_LaneAttachedLateIsBroughtCurrent covers the startup
// window. Lanes are constructed at different points while earlier ones are
// already ticking, so a lane can be attached AFTER a refresh has happened.
// Attaching it must not leave it on the credentials it was built with — those
// may already be retired.
func TestCredentialRefresher_LaneAttachedLateIsBroughtCurrent(t *testing.T) {
	t.Parallel()

	const newWorker = "wkr_after_reregister"
	const newJWT = "after.jwt"
	srv := reregisterServer(t, newWorker, newJWT)
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))

	early := &recordingLane{}
	r.Attach(early)
	if _, err := r.Refresh(context.Background(), "worker-not-found"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	late := &recordingLane{}
	r.Attach(late)

	gotID, gotJWT, sets := late.current()
	if gotID != newWorker || gotJWT != newJWT {
		t.Errorf("late lane = (%q, %q), want (%q, %q) — it must not start life on credentials that have already been superseded",
			gotID, gotJWT, newWorker, newJWT)
	}
	if sets == 0 {
		t.Error("late lane was never given credentials")
	}
}

// TestCredentialRefresher_NilLanesAreIgnored lets a caller attach optional
// services unconditionally. A panic here would take down the whole daemon.
func TestCredentialRefresher_NilLanesAreIgnored(t *testing.T) {
	t.Parallel()

	r := NewCredentialRefresher(CredentialRefresherOptions{
		WorkerID:   "wkr_a",
		RuntimeJWT: "jwt",
	})
	lane := &recordingLane{}
	r.Attach(nil, lane, nil)

	if gotID, _, _ := lane.current(); gotID != "wkr_a" {
		t.Errorf("lane = %q, want %q", gotID, "wkr_a")
	}
}

// TestCredentialRefresher_FailedRefreshLeavesLanesAlone asserts that a failed
// refresh does not half-apply. Lanes keep the credentials they had, so the
// next tick retries cleanly rather than presenting something invented.
func TestCredentialRefresher_FailedRefreshLeavesLanesAlone(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	lane := &recordingLane{}
	r.Attach(lane)
	_, setsBefore := func() (string, int) { id, _, n := lane.current(); return id, n }()

	if _, err := r.Refresh(context.Background(), "worker-not-found"); err == nil {
		t.Fatal("expected an error from a failing refresh")
	}

	gotID, gotJWT, sets := lane.current()
	if gotID != "wkr_before" || gotJWT != "before.jwt" {
		t.Errorf("lane = (%q, %q), want the pre-refresh credentials", gotID, gotJWT)
	}
	if sets != setsBefore {
		t.Errorf("lane was re-credentialed %d times during a failed refresh; want 0", sets-setsBefore)
	}
}

// TestCredentialRefresher_OnReregisterMatchesTheLaneHookShape pins that the
// adapter is directly usable as HeartbeatOptions.OnReregister /
// PollOptions.OnReregister, which is what makes "wire every lane to the same
// refresher" the path of least resistance.
func TestCredentialRefresher_OnReregisterMatchesTheLaneHookShape(t *testing.T) {
	t.Parallel()

	const newWorker = "wkr_after_reregister"
	srv := reregisterServer(t, newWorker, "after.jwt")
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))

	// Compile-time proof of shape, then a behavioural check.
	hbHook := HeartbeatOptions{OnReregister: r.OnReregister}.OnReregister
	pollHook := PollOptions{OnReregister: r.OnReregister}.OnReregister
	if hbHook == nil || pollHook == nil {
		t.Fatal("OnReregister must satisfy both lane hook signatures")
	}

	gotID, gotJWT, err := hbHook(context.Background(), "worker-not-found")
	if err != nil {
		t.Fatalf("OnReregister: %v", err)
	}
	if gotID != newWorker || gotJWT != "after.jwt" {
		t.Errorf("OnReregister = (%q, %q), want (%q, %q)", gotID, gotJWT, newWorker, "after.jwt")
	}
}

// TestCredentialRefresher_ConcurrentLanesConverge runs several lanes refreshing
// at once under -race and asserts they all land on one identity. Divergence
// here is the loop.
func TestCredentialRefresher_ConcurrentLanesConverge(t *testing.T) {
	t.Parallel()

	const newWorker = "wkr_after_reregister"
	srv := reregisterServer(t, newWorker, "after.jwt")
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	lanes := []*recordingLane{{}, {}, {}}
	for _, lane := range lanes {
		r.Attach(lane)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = r.Refresh(context.Background(), "worker-not-found")
		}()
	}
	close(start)
	wg.Wait()

	for i, lane := range lanes {
		if gotID, _, _ := lane.current(); gotID != newWorker {
			t.Errorf("lane %d = %q, want %q — all lanes must converge on one identity", i, gotID, newWorker)
		}
	}
}

// TestCredentialRefresher_UpdateRegistrationTokenTakesEffectOnNextRefresh pins
// the token-swap contract: after UpdateRegistrationToken, the next refresh
// presents the NEW token on the wire, and the refresh result carries the
// platform's registration-token lifetime hints when present. A response that
// omits both fields leaves them empty (older platforms do not send them).
//
// Against the pre-change code this test fails twice over: there is no setter
// to call (compile error), and without the swap the server sees the stale
// token.
func TestCredentialRefresher_UpdateRegistrationTokenTakesEffectOnNextRefresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sendHints bool
	}{
		{name: "lifetime hints surfaced", sendHints: true},
		{name: "absent hints stay empty", sendHints: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var gotAuth []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
					http.NotFound(w, r)
					return
				}
				mu.Lock()
				gotAuth = append(gotAuth, r.Header.Get("Authorization"))
				mu.Unlock()
				body := map[string]any{"runtimeToken": "refreshed.jwt"}
				if tc.sendHints {
					body["registrationTokenExpiresAt"] = "2030-06-01T00:00:00Z"
					body["registrationTokenRenewAfter"] = "2030-05-01T00:00:00Z"
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()

			r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
			// #nosec G101 -- test fixture token
			r.UpdateRegistrationToken("rsp_live_rotated")

			result, err := r.Refresh(context.Background(), "runtime-token-expired")
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(gotAuth) != 1 || gotAuth[0] != "Bearer rsp_live_rotated" {
				t.Errorf("refresh presented auth %v, want exactly [Bearer rsp_live_rotated] — the swap did not take effect", gotAuth)
			}
			if tc.sendHints {
				if result.RegistrationTokenExpiresAt != "2030-06-01T00:00:00Z" {
					t.Errorf("RegistrationTokenExpiresAt = %q, want the platform hint", result.RegistrationTokenExpiresAt)
				}
				if result.RegistrationTokenRenewAfter != "2030-05-01T00:00:00Z" {
					t.Errorf("RegistrationTokenRenewAfter = %q, want the platform hint", result.RegistrationTokenRenewAfter)
				}
			} else if result.RegistrationTokenExpiresAt != "" || result.RegistrationTokenRenewAfter != "" {
				t.Errorf("absent hints must stay empty, got (%q, %q)", result.RegistrationTokenExpiresAt, result.RegistrationTokenRenewAfter)
			}
		})
	}
}

// TestCredentialRefresher_UpdateRegistrationTokenIsSafeDuringRefresh hammers
// the setter against an in-flight refresh under -race. The refresh in flight
// may legitimately present either the old or the new token, but the refresh
// AFTER the swap settles must present the new one — and nothing may race.
func TestCredentialRefresher_UpdateRegistrationTokenIsSafeDuringRefresh(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var mu sync.Mutex
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		<-release
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))

	refreshDone := make(chan error, 1)
	go func() {
		_, err := r.Refresh(context.Background(), "runtime-token-expired")
		refreshDone <- err
	}()

	// Swap while the first refresh is blocked inside the endpoint handler.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// #nosec G101 -- test fixture token
			r.UpdateRegistrationToken("rsp_live_rotated")
		}()
	}
	wg.Wait()
	close(release)
	if err := <-refreshDone; err != nil {
		t.Fatalf("in-flight refresh: %v", err)
	}

	if _, err := r.Refresh(context.Background(), "runtime-token-expired"); err != nil {
		t.Fatalf("post-swap refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) != 2 {
		t.Fatalf("refresh calls = %d, want 2", len(gotAuth))
	}
	if gotAuth[1] != "Bearer rsp_live_rotated" {
		t.Errorf("post-swap refresh presented %q, want Bearer rsp_live_rotated", gotAuth[1])
	}
}

// TestCredentialRefresher_UpdateRegistrationTokenPreservesOtherFields keeps
// the token setter aligned with its UpdateRegistrationProjects precedent: only
// the token changes, everything else retains its current value.
func TestCredentialRefresher_UpdateRegistrationTokenPreservesOtherFields(t *testing.T) {
	t.Parallel()

	opts := testRefresherOptions(t, "http://127.0.0.1:1", "wkr_before", "before.jwt")
	opts.Registration.MaxAgents = 3
	r := NewCredentialRefresher(opts)
	// #nosec G101 -- test fixture token
	r.UpdateRegistrationToken("rsp_live_rotated")

	r.mu.Lock()
	got := r.opts.Registration
	r.mu.Unlock()
	if got.RegistrationToken != "rsp_live_rotated" {
		t.Fatalf("token = %q, want the rotated value", got.RegistrationToken)
	}
	if got.OrchestratorURL != opts.Registration.OrchestratorURL ||
		got.JWTPath != opts.Registration.JWTPath ||
		got.MaxAgents != 3 {
		t.Fatal("token update replaced non-token registration fields")
	}
}

// TestCredentialRefresher_RotateRegistrationTokenRefusesBadShape pins that a
// fail-open swap cannot demote the refresher: a blank, padded, or
// foreign-shaped token is refused with an error, the previous generation
// stays in place, and no request reaches the wire. On the pre-fix setter
// alone each case succeeds silently and the refresher holds the bad token.
func TestCredentialRefresher_RotateRegistrationTokenRefusesBadShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
	}{
		{name: "blank", token: ""},
		{name: "leading whitespace", token: " rsp_live_rotated"},
		{name: "trailing whitespace", token: "rsp_live_rotated "},
		{name: "unprefixed", token: "operator-pasted-wrong-value"},
		{name: "stub-shaped", token: "local-stub-no-token"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
			}))
			defer srv.Close()

			r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
			// #nosec G101 -- test fixture token
			if _, err := r.RotateRegistrationToken(context.Background(), tc.token, "runtime-token-expired"); err == nil {
				t.Fatalf("RotateRegistrationToken(%q) succeeded; want refusal", tc.token)
			}

			r.mu.Lock()
			held := r.opts.Registration.RegistrationToken
			r.mu.Unlock()
			if held != "rsp_live_x" {
				t.Errorf("refresher holds %q, want the previous generation kept", held)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 0 {
				t.Errorf("refused rotation made %d wire request(s); want none", requests)
			}
		})
	}
}

// TestCredentialRefresher_RotateRegistrationTokenProvesThenAdopts pins the
// validate-before-switching half: a new generation the control plane honours
// is presented on the rotation's own round trip (no second refresh needed),
// fans out to every lane, and persists as the refresher's token. Against the
// raw setter this fails at the first assertion — nothing reaches the wire.
func TestCredentialRefresher_RotateRegistrationTokenProvesThenAdopts(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runtimeToken":                "refreshed.jwt",
			"registrationTokenExpiresAt":  "2030-06-01T00:00:00Z",
			"registrationTokenRenewAfter": "2030-05-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	lane := &recordingLane{}
	r.Attach(lane)
	// #nosec G101 -- test fixture token
	result, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired")
	if err != nil {
		t.Fatalf("RotateRegistrationToken: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer rsp_live_rotated" {
		t.Fatalf("rotation presented auth %v, want exactly [Bearer rsp_live_rotated]", gotAuth)
	}
	if result.Mode != "refresh" {
		t.Errorf("rotation mode = %q, want refresh (rotation must never mint a competing identity)", result.Mode)
	}
	if result.RegistrationTokenExpiresAt != "2030-06-01T00:00:00Z" || result.RegistrationTokenRenewAfter != "2030-05-01T00:00:00Z" {
		t.Errorf("rotation result hints = (%q, %q), want the proven generation's hints", result.RegistrationTokenExpiresAt, result.RegistrationTokenRenewAfter)
	}
	if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "refreshed.jwt" {
		t.Errorf("lane = (%q, %q), want the proven credentials fanned out", id, jwt)
	}
	if token, _ := r.Registration(); token != "rsp_live_rotated" {
		t.Errorf("refresher holds %q, want the proven generation", token)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenRestoresPreviousOnRefusal
// pins the restore-on-failure half: when the control plane refuses the new
// generation, the rotation surfaces the error and the refresher still holds
// the previous working token, so the next ordinary refresh keeps working.
// Against the raw setter plus a refresh the old token is already lost.
func TestCredentialRefresher_RotateRegistrationTokenRestoresPreviousOnRefusal(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		refused := len(gotAuth) == 1
		mu.Unlock()
		if refused {
			http.Error(w, `{"error":"unknown registration"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	lane := &recordingLane{}
	r.Attach(lane)
	// #nosec G101 -- test fixture token
	if _, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rejected", "runtime-token-expired"); err == nil {
		t.Fatal("rotation of a refused token succeeded; want the error surfaced")
	}

	if token, _ := r.Registration(); token != "rsp_live_x" {
		t.Fatalf("refresher holds %q after refusal, want the previous generation restored", token)
	}

	result, err := r.Refresh(context.Background(), "runtime-token-expired")
	if err != nil {
		t.Fatalf("refresh after refused rotation: %v", err)
	}
	if result.RuntimeToken != "refreshed.jwt" {
		t.Errorf("refresh after refused rotation minted %q, want the working generation's credentials", result.RuntimeToken)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer rsp_live_rejected", "Bearer rsp_live_x"}
	if len(gotAuth) != len(want) {
		t.Fatalf("wire auth = %v, want %v", gotAuth, want)
	}
	for i := range want {
		if gotAuth[i] != want[i] {
			t.Fatalf("wire auth = %v, want %v", gotAuth, want)
		}
	}
	if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "refreshed.jwt" {
		t.Errorf("lane = (%q, %q), want the working generation's credentials", id, jwt)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenNeedsAnIdentity pins that
// rotation without a current worker identity refuses and leaves the token
// unchanged, instead of adopting a generation nothing has proven.
func TestCredentialRefresher_RotateRegistrationTokenNeedsAnIdentity(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "", "before.jwt"))
	// #nosec G101 -- test fixture token
	if _, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired"); err == nil {
		t.Fatal("rotation without an identity succeeded; want refusal")
	}
	if token, _ := r.Registration(); token != "rsp_live_x" {
		t.Errorf("refresher holds %q, want the previous generation kept", token)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenFailedKeepsConcurrentUpdate
// pins that a failed rotation never clobbers a newer generation: when the
// control plane refuses the rotated token AFTER a concurrent
// UpdateRegistrationToken lands, the refresher keeps the concurrent
// generation instead of restoring the pre-rotation one. Against an
// unconditional restore the refresher holds the stale previous generation
// and the test goes red.
func TestCredentialRefresher_RotateRegistrationTokenFailedKeepsConcurrentUpdate(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		http.Error(w, `{"error":"unknown registration"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	// Unblock the handler before srv.Close waits out outstanding requests.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	rotateDone := make(chan error, 1)
	go func() {
		// #nosec G101 -- test fixture token
		_, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired")
		rotateDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not reach the wire")
	}

	// A newer generation lands while the prove round trip is in flight.
	// #nosec G101 -- test fixture token
	r.UpdateRegistrationToken("rsk_live_concurrent")
	close(release)

	select {
	case err := <-rotateDone:
		if err == nil {
			t.Fatal("rotation of a refused token succeeded; want the error surfaced")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return")
	}

	if token, _ := r.Registration(); token != "rsk_live_concurrent" {
		t.Fatalf("refresher holds %q after a failed rotation, want the concurrent generation kept", token)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenWaitsForInflightReregister
// pins the operation-slot serialization the rotation doc comment claims: a
// rotation started while a re-registration is in flight does not reach the
// wire until the re-registration settles, then re-presents the NEW worker
// identity the re-registration minted. Without the slot the rotation
// presents the stale identity concurrently and the test goes red.
func TestCredentialRefresher_RotateRegistrationTokenWaitsForInflightReregister(t *testing.T) {
	t.Parallel()

	reregisterEntered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var mu sync.Mutex
	var refreshPaths []string
	var refreshAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == RegisterEndpoint {
			enterOnce.Do(func() { close(reregisterEntered) })
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     "wkr_after",
				"runtimeToken": "after.jwt",
			})
			return
		}
		if !strings.HasSuffix(req.URL.Path, "/refresh-token") {
			http.NotFound(w, req)
			return
		}
		mu.Lock()
		refreshPaths = append(refreshPaths, req.URL.Path)
		refreshAuth = append(refreshAuth, req.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()
	// Unblock the handler before srv.Close waits out outstanding requests.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	lane := &recordingLane{}
	r.Attach(lane)

	reregDone := make(chan error, 1)
	go func() {
		_, err := r.Reregister(context.Background())
		reregDone <- err
	}()

	select {
	case <-reregisterEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("re-registration did not reach the wire")
	}

	rotateRes := make(chan *RefreshTokenResult, 1)
	rotateErr := make(chan error, 1)
	go func() {
		// #nosec G101 -- test fixture token
		res, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired")
		if err != nil {
			rotateErr <- err
			return
		}
		rotateRes <- res
	}()

	// Give a slot-less rotation a chance to (incorrectly) proceed.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	early := len(refreshPaths)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("rotation reached the wire while a re-registration was in flight (%d refresh requests)", early)
	}
	close(release)

	select {
	case err := <-reregDone:
		if err != nil {
			t.Fatalf("re-registration: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("re-registration did not return")
	}
	select {
	case err := <-rotateErr:
		t.Fatalf("rotation after re-registration: %v", err)
	case res := <-rotateRes:
		if res.WorkerID != "wkr_after" {
			t.Errorf("rotation worker = %q, want the re-registered identity re-presented", res.WorkerID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(refreshPaths) != 1 || refreshPaths[0] != "/api/workers/wkr_after/refresh-token" {
		t.Fatalf("rotation presented %v, want exactly [/api/workers/wkr_after/refresh-token]", refreshPaths)
	}
	// #nosec G101 -- test fixture token asserted, not embedded
	if len(refreshAuth) != 1 || refreshAuth[0] != "Bearer rsp_live_rotated" {
		t.Fatalf("rotation presented auth %v, want exactly [Bearer rsp_live_rotated]", refreshAuth)
	}
	if token, _ := r.Registration(); token != "rsp_live_rotated" {
		t.Errorf("refresher holds %q, want the proven generation", token)
	}
	if id, jwt, _ := lane.current(); id != "wkr_after" || jwt != "refreshed.jwt" {
		t.Errorf("lane = (%q, %q), want the proven credentials fanned out", id, jwt)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenHidesUnprovenGeneration pins
// that the candidate is proven on a copy: while the prove round trip is in
// flight, Registration() still reports the previous generation. Against the
// mutate-before-prove shape the refresher exposes the rotated token
// mid-flight and this test goes red.
func TestCredentialRefresher_RotateRegistrationTokenHidesUnprovenGeneration(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	done := make(chan error, 1)
	go func() {
		// #nosec G101 -- test fixture token
		_, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired")
		done <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not reach the wire")
	}
	if token, _ := r.Registration(); token != "rsp_live_x" {
		t.Fatalf("refresher exposes %q mid-flight, want the previous generation until proven", token)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rotation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return")
	}
	if token, _ := r.Registration(); token != "rsp_live_rotated" {
		t.Fatalf("refresher holds %q after proof, want the proven generation", token)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenSuccessKeepsConcurrentUpdate
// pins the success-path race: when a concurrent UpdateRegistrationToken lands
// while the prove round trip is in flight, the rotation reports that it lost
// the race instead of fanning out credentials proven under the rotated
// generation alongside the concurrent one. The refresher keeps the newer
// generation and the lanes stay on their current credentials until the next
// refresh presents it. Against a publish-before-prove swap the lanes move to
// the rotated credentials while the token ends on the concurrent generation.
func TestCredentialRefresher_RotateRegistrationTokenSuccessKeepsConcurrentUpdate(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var mu sync.Mutex
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	r := NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))
	lane := &recordingLane{}
	r.Attach(lane)
	rotateDone := make(chan error, 1)
	go func() {
		// #nosec G101 -- test fixture token
		_, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired")
		rotateDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not reach the wire")
	}
	// A newer generation lands while the prove round trip is in flight.
	// #nosec G101 -- test fixture token
	r.UpdateRegistrationToken("rsk_live_concurrent")
	close(release)

	select {
	case err := <-rotateDone:
		if err == nil {
			t.Fatal("rotation succeeded over a concurrent update; want the race reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return")
	}
	if token, _ := r.Registration(); token != "rsk_live_concurrent" {
		t.Fatalf("refresher holds %q, want the concurrent generation kept", token)
	}
	if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "before.jwt" {
		t.Errorf("lane = (%q, %q), want the pre-rotation credentials kept — nothing proven was fanned out", id, jwt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer rsp_live_rotated" {
		t.Fatalf("wire auth = %v, want exactly [Bearer rsp_live_rotated] — the proof ran under the candidate", gotAuth)
	}
}

// TestCredentialRefresher_RotateRegistrationTokenRefusesStubPromotion pins
// that rotation never promotes a stub-booted refresher live: the running
// generation must itself be platform-shaped, sharing the watcher's
// stub-promotion refusal through the same shape door.
func TestCredentialRefresher_RotateRegistrationTokenRefusesStubPromotion(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	// #nosec G101 -- test fixture token
	opts.Registration.RegistrationToken = "local-stub-no-token"
	r := NewCredentialRefresher(opts)
	// #nosec G101 -- test fixture token
	if _, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired"); err == nil {
		t.Fatal("rotation promoting a stub registration succeeded; want refusal")
	}
	if token, _ := r.Registration(); token != "local-stub-no-token" {
		t.Errorf("refresher holds %q, want the stub generation kept", token)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Errorf("refused promotion made %d wire request(s); want none", requests)
	}
}

// TestDaemon_YamlWatcherProvesRotations pins that the config watcher routes a
// reloaded registration token through the proven rotation entry point instead
// of adopting it raw: a generation the control plane refuses leaves the
// running generation (and the filed pointer) in place, while a generation it
// honours is adopted and fanned out to every lane. Against the raw setter the
// refused generation would be adopted and every later refresh would present
// it.
func TestDaemon_YamlWatcherProvesRotations(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer rsp_live_rejected" {
			http.Error(w, `{"error":"unknown registration"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()

	newWatched := func(t *testing.T) (*Daemon, *recordingLane) {
		t.Helper()
		base := &Config{Orchestrator: OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_x"}}
		d := &Daemon{config: base, credentials: NewCredentialRefresher(testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt"))}
		lane := &recordingLane{}
		d.credentials.Attach(lane)
		return d, lane
	}

	t.Run("refused generation stays", func(t *testing.T) {
		d, lane := newWatched(t)
		// #nosec G101 -- test fixture token
		reloaded := *d.config
		reloaded.Orchestrator.AuthToken = "rsp_live_rejected"
		d.onYamlChanged(&reloaded)
		if token, _ := d.credentials.Registration(); token != "rsp_live_x" {
			t.Fatalf("refresher holds %q after a refused rotation, want the running generation kept", token)
		}
		if d.config.Orchestrator.AuthToken != "rsp_live_x" {
			t.Fatalf("filed token = %q after a refused rotation, want the running generation kept", d.config.Orchestrator.AuthToken)
		}
		if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "before.jwt" {
			t.Errorf("lane = (%q, %q), want the running credentials kept", id, jwt)
		}
	})

	t.Run("proven generation adopts", func(t *testing.T) {
		d, lane := newWatched(t)
		// #nosec G101 -- test fixture token
		reloaded := *d.config
		reloaded.Orchestrator.AuthToken = "rsp_live_rotated"
		d.onYamlChanged(&reloaded)
		if token, _ := d.credentials.Registration(); token != "rsp_live_rotated" {
			t.Fatalf("refresher holds %q after a proven rotation, want the rotated generation", token)
		}
		if d.config.Orchestrator.AuthToken != "rsp_live_rotated" {
			t.Fatalf("filed token = %q after a proven rotation, want the rotated generation", d.config.Orchestrator.AuthToken)
		}
		if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "refreshed.jwt" {
			t.Errorf("lane = (%q, %q), want the proven credentials fanned out", id, jwt)
		}
	})
}

// TestCredentialRefresher_RotateRegistrationTokenRestoresPreviousOnAdoptRefusal
// pins that a rotation the local validator refuses is not adopted: the
// control plane honours the candidate (200) but ValidateRefresh rejects the
// result, so the rotation reports the error and the refresher keeps
// presenting the previous generation while the lanes stay on the running
// credentials. Against store-then-adopt the refresher holds the refused
// generation and every later refresh presents it.
func TestCredentialRefresher_RotateRegistrationTokenRestoresPreviousOnAdoptRefusal(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/wkr_before/refresh-token" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	// #nosec G101 -- test fixture token
	opts.Registration.RegistrationToken = "rsp_live_before"
	opts.ValidateRefresh = func(*RefreshTokenResult) error {
		return errors.New("controller credentials refused")
	}
	r := NewCredentialRefresher(opts)
	lane := &recordingLane{}
	r.Attach(lane)
	// #nosec G101 -- test fixture token
	if _, err := r.RotateRegistrationToken(context.Background(), "rsp_live_rotated", "runtime-token-expired"); err == nil {
		t.Fatal("rotation refused by the local validator succeeded; want the error surfaced")
	}
	if token, _ := r.Registration(); token != "rsp_live_before" {
		t.Fatalf("refresher holds %q after a validator-refused rotation, want the previous generation kept", token)
	}
	if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "before.jwt" {
		t.Fatalf("lane = (%q, %q), want the running credentials kept", id, jwt)
	}

	// The previous generation still works: with the refusal cleared, the
	// next ordinary refresh presents it.
	r.opts.ValidateRefresh = nil
	result, err := r.Refresh(context.Background(), "runtime-token-expired")
	if err != nil {
		t.Fatalf("refresh after a validator-refused rotation: %v", err)
	}
	if result.RuntimeToken != "refreshed.jwt" {
		t.Errorf("refresh after a validator-refused rotation minted %q, want the working generation's credentials", result.RuntimeToken)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer rsp_live_rotated", "Bearer rsp_live_before"}
	if len(gotAuth) != len(want) {
		t.Fatalf("wire auth = %v, want %v", gotAuth, want)
	}
	for i := range want {
		if gotAuth[i] != want[i] {
			t.Fatalf("wire auth = %v, want %v", gotAuth, want)
		}
	}
}
