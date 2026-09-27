package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
