package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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

func TestCredentialRefresher_ReregisterUsesReloadedProjectProjection(t *testing.T) {
	t.Parallel()
	registrations := make(chan RegisterRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		var request RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		registrations <- request
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_reloaded", "runtimeToken": "reloaded.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.DaemonProjects = []ProjectAllowlistEntry{{ID: "alpha", Repository: "github.com/x/alpha"}}
	opts.Registration.ProjectIDs = []string{"alpha"}
	r := NewCredentialRefresher(opts)

	r.UpdateRegistrationProjects(
		[]ProjectAllowlistEntry{
			{ID: "alpha", Repository: "github.com/x/alpha"},
			{ID: "beta", Repository: "github.com/x/beta"},
		},
		[]string{"alpha", "beta"},
		ProjectAdmissionModeEnumerated,
	)
	if _, err := r.Reregister(context.Background()); err != nil {
		t.Fatalf("Reregister: %v", err)
	}

	select {
	case request := <-registrations:
		if got, want := request.ProjectIDs, []string{"alpha", "beta"}; !slices.Equal(got, want) {
			t.Errorf("ProjectIDs = %v, want %v", got, want)
		}
		if got := request.DaemonProjects; len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "beta" {
			t.Errorf("DaemonProjects = %+v, want merged alpha and beta", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reloaded registration did not reach orchestrator")
	}
}

func TestCredentialRefresher_ReregisterLatestProjectProjectionWins(t *testing.T) {
	t.Parallel()
	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	enteredB := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		switch request.ProjectIDs[0] {
		case "alpha":
			close(enteredA)
			<-releaseA
			_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_alpha", "runtimeToken": "alpha.jwt"})
		case "beta":
			close(enteredB)
			_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_beta", "runtimeToken": "beta.jwt"})
		default:
			t.Errorf("unexpected ProjectIDs: %v", request.ProjectIDs)
		}
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	r := NewCredentialRefresher(opts)
	lane := &recordingLane{}
	r.Attach(lane)
	r.UpdateRegistrationProjects([]ProjectAllowlistEntry{{ID: "alpha", Repository: "github.com/x/alpha"}}, []string{"alpha"}, ProjectAdmissionModeEnumerated)

	r.RequestReregister()
	select {
	case <-enteredA:
	case <-time.After(2 * time.Second):
		t.Fatal("older registration did not reach orchestrator")
	}

	r.UpdateRegistrationProjects([]ProjectAllowlistEntry{{ID: "beta", Repository: "github.com/x/beta"}}, []string{"beta"}, ProjectAdmissionModeAllRouted)
	r.RequestReregister()
	close(releaseA)
	select {
	case <-enteredB:
	case <-time.After(2 * time.Second):
		t.Fatal("newer registration did not reach orchestrator")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		workerID, jwt := r.Current()
		laneWorkerID, laneJWT, _ := lane.current()
		if workerID == "wkr_beta" && jwt == "beta.jwt" && laneWorkerID == "wkr_beta" && laneJWT == "beta.jwt" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	workerID, jwt := r.Current()
	laneWorkerID, laneJWT, _ := lane.current()
	t.Fatalf("final credentials = refresher(%q, %q) lane(%q, %q), want newest beta credentials", workerID, jwt, laneWorkerID, laneJWT)
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
