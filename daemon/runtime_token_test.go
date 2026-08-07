package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRefreshRuntimeToken_RefreshPathHonoured asserts that when the
// platform's POST /api/workers/<id>/refresh-token endpoint is
// available, the daemon takes the refresh path — preserving the
// workerId — instead of falling through to a full re-register.
//
// This is the green-path refresh fix: the platform side ships a
// refresh handler, the daemon picks it up automatically, and the
// 5-min `401 → re-register → 404` cycle goes away because the
// workerId is stable across token refreshes.
func TestRefreshRuntimeToken_RefreshPathHonoured(t *testing.T) {
	t.Parallel()
	const wantWorker = "wkr_existing123"
	// #nosec G101 -- test fixture token
	const wantRegToken = "rsp_live_test_registration"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Refresh probe.
		if r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+wantWorker+"/refresh-token" {
			if got := r.Header.Get("Authorization"); got != "Bearer "+wantRegToken {
				t.Errorf("refresh: wrong auth: %q", got)
			}
			// #nosec G101 -- test fixture response
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runtimeToken":          "fresh.runtime.jwt",
				"runtimeTokenExpiresAt": "2026-05-03T12:00:00Z",
				"heartbeatInterval":     30000,
				"pollInterval":          5000,
			})
			return
		}
		// Anything else → unexpected.
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	regOpts := RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: wantRegToken,
		Hostname:          "test-host",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           tmpDir + "/jwt.json",
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
		Now:               func() time.Time { return time.Unix(1_700_000_000, 0) },
	}

	result, err := RefreshRuntimeToken(context.Background(), regOpts, wantWorker, "runtime-token-expired")
	if err != nil {
		t.Fatalf("RefreshRuntimeToken err: %v", err)
	}
	if result.Mode != "refresh" {
		t.Fatalf("expected Mode=refresh, got %q", result.Mode)
	}
	if result.WorkerID != wantWorker {
		t.Fatalf("expected workerId preserved (%q), got %q", wantWorker, result.WorkerID)
	}
	if result.RuntimeToken != "fresh.runtime.jwt" {
		t.Fatalf("expected fresh.runtime.jwt, got %q", result.RuntimeToken)
	}
	if result.RegistrationTokenSwapped {
		t.Fatalf("expected no workerId swap on refresh path")
	}
	// The expiry + cadence must be surfaced so the caller can persist a
	// complete CachedJWT after the refresh (otherwise the on-disk cache keeps
	// the stale token and downstream readers 401 — the credential root cause).
	if result.RuntimeTokenExpiresAt != "2026-05-03T12:00:00Z" {
		t.Errorf("expected RuntimeTokenExpiresAt surfaced, got %q", result.RuntimeTokenExpiresAt)
	}
	if result.HeartbeatInterval != 30000 {
		t.Errorf("expected HeartbeatInterval=30000, got %d", result.HeartbeatInterval)
	}
	if result.PollInterval != 5000 {
		t.Errorf("expected PollInterval=5000, got %d", result.PollInterval)
	}
}

// TestPersistRefreshedToken_OverwritesStaleCache covers the persistence fix:
// after a refresh, the on-disk daemon.jwt must carry the FRESH token +
// expiry/cadence, replacing the stale entry, so the credential resolver and
// runner read the new token instead of 401ing on the expired one.
func TestPersistRefreshedToken_OverwritesStaleCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/daemon.jwt"

	// Seed a stale cache (an expired token) as the daemon would have on disk.
	// #nosec G101 -- test fixture token, not a real credential
	if err := SaveCachedJWT(path, &RegisterResponse{
		WorkerID:              "wkr_x",
		RuntimeToken:          "stale.expired.jwt",
		HeartbeatInterval:     30000,
		PollInterval:          5000,
		RuntimeTokenExpiresAt: "2020-01-01T00:00:00Z",
	}, time.Unix(1_600_000_000, 0)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// #nosec G101 -- test fixture token, not a real credential
	err := persistRefreshedToken(path, &RefreshTokenResult{
		Mode:                  "refresh",
		WorkerID:              "wkr_x",
		RuntimeToken:          "fresh.runtime.jwt",
		RuntimeTokenExpiresAt: "2030-01-01T00:00:00Z",
		HeartbeatInterval:     30000,
		PollInterval:          5000,
	}, func() time.Time { return time.Unix(1_700_000_000, 0) })
	if err != nil {
		t.Fatalf("persistRefreshedToken: %v", err)
	}

	got, err := LoadCachedJWT(path)
	if err != nil || got == nil {
		t.Fatalf("reload cache: %v (got=%v)", err, got)
	}
	if got.RuntimeToken != "fresh.runtime.jwt" {
		t.Errorf("token not refreshed on disk: %q", got.RuntimeToken)
	}
	if got.RuntimeTokenExpiresAt != "2030-01-01T00:00:00Z" {
		t.Errorf("expiry not refreshed on disk: %q", got.RuntimeTokenExpiresAt)
	}
	if got.WorkerID != "wkr_x" {
		t.Errorf("workerId = %q, want wkr_x", got.WorkerID)
	}
}

// TestPersistRefreshedToken_NoopOnEmptyPath: an empty JWT path is a no-op (no
// panic, no error) so callers without a cache path stay best-effort.
func TestPersistRefreshedToken_NoopOnEmptyPath(t *testing.T) {
	t.Parallel()
	if err := persistRefreshedToken("", &RefreshTokenResult{RuntimeToken: "x"}, nil); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

// TestRefreshRuntimeToken_FallsBackToReregisterOn404 asserts that
// when the platform's refresh endpoint returns 404 (current state —
// the platform-side companion not yet shipped), the daemon
// falls back to a full re-register and observes a NEW workerId. This
// is the canonical root-cause path — proven, logged, and
// surfaced via RegistrationTokenSwapped=true so operators see why
// in-flight heartbeats 404 in the cycle until they swap credentials.
func TestRefreshRuntimeToken_FallsBackToReregisterOn404(t *testing.T) {
	t.Parallel()
	const oldWorker = "wkr_oldworker"
	const newWorker = "wkr_freshlyminted"
	// #nosec G101 -- test fixture token
	const wantRegToken = "rsp_live_test_registration"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+oldWorker+"/refresh-token":
			// Refresh endpoint not deployed yet.
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == RegisterEndpoint:
			// Re-register mints a new workerId (matches platform
			// registerWorker() behaviour — always a new wkr_ uuid).
			if got := r.Header.Get("Authorization"); got != "Bearer "+wantRegToken {
				t.Errorf("register: wrong auth: %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			// #nosec G101 -- test fixture response
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":          newWorker,
				"runtimeToken":      "newly.minted.jwt",
				"heartbeatInterval": 30000,
				"pollInterval":      5000,
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	regOpts := RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: wantRegToken,
		Hostname:          "test-host",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           tmpDir + "/jwt.json",
		ForceReregister:   true, // skip cache so we hit the endpoint
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
		Now:               func() time.Time { return time.Unix(1_700_000_000, 0) },
	}

	result, err := RefreshRuntimeToken(context.Background(), regOpts, oldWorker, "worker-not-found")
	if err != nil {
		t.Fatalf("RefreshRuntimeToken err: %v", err)
	}
	if result.Mode != "reregister" {
		t.Fatalf("expected Mode=reregister, got %q", result.Mode)
	}
	if result.WorkerID != newWorker {
		t.Fatalf("expected new workerId %q, got %q", newWorker, result.WorkerID)
	}
	if !result.RegistrationTokenSwapped {
		t.Fatalf("expected RegistrationTokenSwapped=true on workerId swap")
	}
}

// TestRefreshRuntimeToken_WorkerNotFound_RePresentsLiveRegistration is the
// regression test for the re-registration loop.
//
// A rejection carrying reason "worker-not-found" (HTTP 404 on poll or
// heartbeat) is NOT proof that the durable registration is gone. The daemon
// used to treat it as proof and re-register immediately, which minted a new
// identity, retired the previous one, and 404ed whichever lane still held it —
// a self-perpetuating loop that produced a new registration every tick for as
// long as the process lived.
//
// The refresh endpoint answers from the durable record, so it is the
// discriminator. When it answers 200 the registration is alive: the daemon
// MUST keep the worker id and MUST NOT register again.
//
// Against the pre-fix code this test fails on the very first assertion — the
// refresh probe is never called at all — and `registerHits` is 1 with a
// brand-new worker id.
func TestRefreshRuntimeToken_WorkerNotFound_RePresentsLiveRegistration(t *testing.T) {
	t.Parallel()
	const liveWorker = "wkr_still_registered"
	var refreshHits, registerHits int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+liveWorker+"/refresh-token":
			refreshHits++
			// The durable registration is alive; a fresh JWT is minted for
			// the SAME worker id.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runtimeToken":      "fresh.jwt.same-worker",
				"heartbeatInterval": 30000,
				"pollInterval":      5000,
			})
		case r.Method == http.MethodPost && r.URL.Path == RegisterEndpoint:
			registerHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     "wkr_should_never_be_minted",
				"runtimeToken": "new.registration.jwt",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           t.TempDir() + "/jwt.json",
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
	}
	result, err := RefreshRuntimeToken(context.Background(), regOpts, liveWorker, "worker-not-found")
	if err != nil {
		t.Fatalf("RefreshRuntimeToken err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshHits != 1 {
		t.Errorf("refresh probe must be attempted for worker-not-found (it is the only way to tell an evicted cache entry from a retired registration); got %d calls", refreshHits)
	}
	if registerHits != 0 {
		t.Errorf("a live durable registration must NEVER be replaced; got %d registrations", registerHits)
	}
	if result.Mode != "refresh" {
		t.Errorf("expected Mode=refresh, got %q", result.Mode)
	}
	if result.WorkerID != liveWorker {
		t.Errorf("worker identity must survive the refresh: got %q, want %q", result.WorkerID, liveWorker)
	}
	if result.RegistrationTokenSwapped {
		t.Error("RegistrationTokenSwapped must be false when the identity was preserved")
	}
}

// TestRefreshRuntimeToken_WorkerNotFound_ReregistersOnlyWhenRecordIsGone is
// the other half of the contract: when the refresh endpoint also 404s, the
// durable registration really is gone and minting a new identity is correct.
func TestRefreshRuntimeToken_WorkerNotFound_ReregistersOnlyWhenRecordIsGone(t *testing.T) {
	t.Parallel()
	const retiredWorker = "wkr_retired"
	var refreshHits, registerHits int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+retiredWorker+"/refresh-token":
			refreshHits++
			http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == RegisterEndpoint:
			registerHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     "wkr_new_identity",
				"runtimeToken": "new.registration.jwt",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           t.TempDir() + "/jwt.json",
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
	}
	result, err := RefreshRuntimeToken(context.Background(), regOpts, retiredWorker, "worker-not-found")
	if err != nil {
		t.Fatalf("RefreshRuntimeToken err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshHits != 1 {
		t.Errorf("expected exactly one refresh probe, got %d", refreshHits)
	}
	if registerHits != 1 {
		t.Errorf("expected exactly one re-registration, got %d", registerHits)
	}
	if result.Mode != "reregister" {
		t.Errorf("expected Mode=reregister, got %q", result.Mode)
	}
	if result.WorkerID != "wkr_new_identity" {
		t.Errorf("expected the newly minted identity, got %q", result.WorkerID)
	}
	if !result.RegistrationTokenSwapped {
		t.Error("expected RegistrationTokenSwapped=true when the identity changed")
	}
}

// TestRefreshRuntimeToken_AdoptsSiblingRegistrationInsteadOfCompeting pins the
// cross-lane repair.
//
// Two lanes (heartbeat and poll) share one registration. Lane A refreshed and
// wrote the result to the shared credential cache; lane B is still holding the
// superseded id and gets rejected. Lane B must ADOPT lane A's live
// registration, not mint a third one — minting is what turns two lanes into a
// mutual-eviction loop, because each new registration retires the other lane's
// record.
//
// Against the pre-fix code lane B re-registers unconditionally: registerHits
// is 1 and the process ends up on an identity neither lane agreed on.
func TestRefreshRuntimeToken_AdoptsSiblingRegistrationInsteadOfCompeting(t *testing.T) {
	t.Parallel()
	const staleWorker = "wkr_lane_b_stale"
	const siblingWorker = "wkr_lane_a_live"
	var registerHits int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/workers/" + staleWorker + "/refresh-token":
			// Lane B's id was retired when lane A re-registered.
			http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
		case "/api/workers/" + siblingWorker + "/refresh-token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runtimeToken":      "sibling.fresh.jwt",
				"heartbeatInterval": 30000,
				"pollInterval":      5000,
			})
		case RegisterEndpoint:
			registerHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     "wkr_third_competing_identity",
				"runtimeToken": "third.jwt",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	jwtPath := t.TempDir() + "/jwt.json"
	// Lane A's refresh already landed in the shared cache.
	if err := SaveCachedJWT(jwtPath, &RegisterResponse{
		WorkerID:     siblingWorker,
		RuntimeToken: "lane.a.jwt",
	}, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           jwtPath,
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
	}
	result, err := RefreshRuntimeToken(context.Background(), regOpts, staleWorker, "worker-not-found")
	if err != nil {
		t.Fatalf("RefreshRuntimeToken err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if registerHits != 0 {
		t.Errorf("a lane must adopt its sibling's live registration, never mint a competing one; got %d registrations", registerHits)
	}
	if result.WorkerID != siblingWorker {
		t.Errorf("expected the sibling's worker id %q, got %q", siblingWorker, result.WorkerID)
	}
	if result.Mode != "refresh" {
		t.Errorf("expected Mode=refresh on adoption, got %q", result.Mode)
	}
	if result.RuntimeToken != "sibling.fresh.jwt" {
		t.Errorf("expected the freshly minted sibling token, got %q", result.RuntimeToken)
	}
}

// TestRefreshRuntimeToken_TransientProbeFailureKeepsIdentity asserts that a
// 5xx / transport failure on the refresh probe is a RETRY, never a reason to
// abandon the worker identity. Only 404 and 405 mean "there is nothing to
// re-present".
func TestRefreshRuntimeToken_TransientProbeFailureKeepsIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		probeStatus   int
		wantErr       bool
		wantRegisters int
	}{
		{name: "server error retries", probeStatus: http.StatusInternalServerError, wantErr: true, wantRegisters: 0},
		{name: "bad gateway retries", probeStatus: http.StatusBadGateway, wantErr: true, wantRegisters: 0},
		{name: "unauthorized retries", probeStatus: http.StatusUnauthorized, wantErr: true, wantRegisters: 0},
		{name: "not found reregisters", probeStatus: http.StatusNotFound, wantErr: false, wantRegisters: 1},
		{name: "method not allowed reregisters", probeStatus: http.StatusMethodNotAllowed, wantErr: false, wantRegisters: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var registerHits int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == RegisterEndpoint {
					registerHits++
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"workerId":     "wkr_new",
						"runtimeToken": "new.jwt",
					})
					return
				}
				http.Error(w, `{"error":"probe"}`, tc.probeStatus)
			}))
			defer srv.Close()

			regOpts := RegistrationOptions{
				OrchestratorURL: srv.URL,
				// #nosec G101 -- test fixture
				RegistrationToken: "rsp_live_x",
				Hostname:          "h",
				Version:           Version,
				MaxAgents:         1,
				JWTPath:           t.TempDir() + "/jwt.json",
				HTTPClient:        &http.Client{Timeout: 5 * time.Second},
			}
			_, err := RefreshRuntimeToken(context.Background(), regOpts, "wkr_current", "worker-not-found")
			if tc.wantErr && err == nil {
				t.Fatal("expected an error so the caller retries on its next tick")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if registerHits != tc.wantRegisters {
				t.Errorf("registrations = %d, want %d", registerHits, tc.wantRegisters)
			}
		})
	}
}

// TestRefreshRuntimeToken_ThrottlesRepeatedReregistration pins the safeguard
// behind the fix: even when every path legitimately concludes "re-register",
// the daemon refuses to mint identities faster than
// MinReregisterInterval. This is what bounds the blast radius of any future
// rejection mode that escapes the re-presentation logic.
func TestRefreshRuntimeToken_ThrottlesRepeatedReregistration(t *testing.T) {
	t.Parallel()
	var registerHits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == RegisterEndpoint {
			registerHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     fmt.Sprintf("wkr_minted_%d", registerHits),
				"runtimeToken": "minted.jwt",
			})
			return
		}
		http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           t.TempDir() + "/jwt.json",
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
	}

	first, err := RefreshRuntimeToken(context.Background(), regOpts, "wkr_a", "worker-not-found")
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	// A DIFFERENT stale id, so the single-flight adoption path does not
	// short-circuit: this is a genuine second re-registration attempt.
	if _, err := RefreshRuntimeToken(context.Background(), regOpts, first.WorkerID, "worker-not-found"); err == nil {
		t.Fatal("expected the second re-registration inside the cooldown to be refused")
	}

	mu.Lock()
	defer mu.Unlock()
	if registerHits != 1 {
		t.Errorf("expected the cooldown to cap registrations at 1, got %d", registerHits)
	}
}

// TestRefreshRuntimeToken_ConcurrentLanesProduceOneRegistration asserts that
// two lanes reacting to the same rejection at the same instant collapse into a
// single refresh. Without the single-flight each lane registers, and each
// registration retires the other's record — the loop, in miniature.
func TestRefreshRuntimeToken_ConcurrentLanesProduceOneRegistration(t *testing.T) {
	resetRefreshersForTest()
	t.Cleanup(resetRefreshersForTest)

	var registerHits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == RegisterEndpoint {
			mu.Lock()
			registerHits++
			id := fmt.Sprintf("wkr_minted_%d", registerHits)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     id,
				"runtimeToken": "minted.jwt",
			})
			return
		}
		http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           t.TempDir() + "/jwt.json",
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
	}

	const lanes = 2
	var wg sync.WaitGroup
	results := make([]*RefreshTokenResult, lanes)
	errs := make([]error, lanes)
	start := make(chan struct{})
	for i := range lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = RefreshRuntimeToken(
				context.Background(), regOpts, "wkr_shared_stale", "worker-not-found")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("lane %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if registerHits != 1 {
		t.Errorf("concurrent lanes must coalesce into ONE registration; got %d", registerHits)
	}
	if results[0].WorkerID != results[1].WorkerID {
		t.Errorf("lanes converged on different identities: %q vs %q", results[0].WorkerID, results[1].WorkerID)
	}
}

// TestRefreshRuntimeToken_ProbedBeforeReregister asserts that on every
// auth-failure the daemon HITS the refresh endpoint FIRST. This is
// the acceptance check: "assert refresh path is hit BEFORE
// re-register". When the platform side ships the handler the daemon
// flips automatically.
func TestRefreshRuntimeToken_ProbedBeforeReregister(t *testing.T) {
	t.Parallel()
	const oldWorker = "wkr_old"
	var refreshHits, registerHits int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+oldWorker+"/refresh-token":
			refreshHits++
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == RegisterEndpoint:
			registerHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     "wkr_new",
				"runtimeToken": "fresh",
			})
		}
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           tmpDir + "/jwt.json",
		ForceReregister:   true,
		HTTPClient:        &http.Client{Timeout: 5 * time.Second},
	}
	if _, err := RefreshRuntimeToken(context.Background(), regOpts, oldWorker, "test"); err != nil {
		t.Fatalf("RefreshRuntimeToken err: %v", err)
	}
	if refreshHits != 1 {
		t.Errorf("expected refresh probe to be hit exactly once, got %d", refreshHits)
	}
	if registerHits != 1 {
		t.Errorf("expected register fallback to be hit exactly once, got %d", registerHits)
	}
}

// TestAuthFailureReason classifies the platform's specific 401
// "Runtime token expired" message — the smoking-gun signal for
// the runtime-token refresh path.
func TestAuthFailureReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "runtime-token-expired",
			err:  &heartbeatHTTPError{status: 401, body: `{"error":"Runtime token expired; re-present registration token to refresh"}`},
			want: "runtime-token-expired",
		},
		{
			name: "generic-401",
			err:  &heartbeatHTTPError{status: 401, body: `{"error":"unauthorized"}`},
			want: "unauthorized",
		},
		{
			name: "worker-not-found",
			err:  &heartbeatHTTPError{status: 404, body: `{"error":"Worker not found"}`},
			want: "worker-not-found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := authFailureReason(tc.err); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPollAuthFailureReason mirrors the heartbeat path for the
// poll-loop classification.
func TestPollAuthFailureReason(t *testing.T) {
	t.Parallel()
	err := &PollHTTPError{Status: 401, Body: `{"error":"Runtime token expired; re-present registration token to refresh"}`}
	if got := pollAuthFailureReason(err); got != "runtime-token-expired" {
		t.Fatalf("got %q, want runtime-token-expired", got)
	}
}

// TestHeartbeatService_RefreshOn401Probe asserts the
// refresh-before-reregister acceptance criterion: when the platform returns
// 401 "Runtime token expired" on a heartbeat, the daemon's
// OnReregister callback (which the daemon wires through
// RefreshRuntimeToken) HITS the refresh endpoint before falling back
// to a full re-register. The heartbeat resumes with the refreshed
// JWT against the SAME workerId.
func TestHeartbeatService_RefreshOn401Probe(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	const workerID = "wkr_persistent"

	var (
		mu             sync.Mutex
		hbHits         int
		refreshHits    int
		registerHits   int
		bearerHistory  []string
		expiredOnFirst = true
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		bearerHistory = append(bearerHistory, r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+workerID+"/heartbeat":
			hbHits++
			if expiredOnFirst {
				expiredOnFirst = false
				http.Error(w, `{"error":"Runtime token expired; re-present registration token to refresh"}`, http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+workerID+"/refresh-token":
			refreshHits++
			// #nosec G101 -- test fixture response
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runtimeToken": "fresh.runtime.jwt",
			})
		case r.Method == http.MethodPost && r.URL.Path == RegisterEndpoint:
			registerHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId":     "wkr_NEW",
				"runtimeToken": "should-not-be-used",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           t.TempDir() + "/jwt.json",
		HTTPClient:        srv.Client(),
	}

	// reregister callback — same shape as daemon.go uses.
	currentWorkerID := workerID
	currentJWT := "stale.runtime.jwt"
	reregister := func(rctx context.Context, reason string) (string, string, error) {
		result, err := RefreshRuntimeToken(rctx, regOpts, currentWorkerID, reason)
		if err != nil {
			return "", "", err
		}
		currentWorkerID = result.WorkerID
		currentJWT = result.RuntimeToken
		return result.WorkerID, result.RuntimeToken, nil
	}

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        workerID,
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      currentJWT,
		IntervalSeconds: 1,
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 1 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		HTTPClient:      srv.Client(),
		OnReregister:    reregister,
	})
	hs.Start()
	defer hs.Stop()

	// Wait for: first heartbeat (401) → refresh probe → second heartbeat OK.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := hbHits >= 2 && refreshHits == 1
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshHits != 1 {
		t.Errorf("expected refresh endpoint to be hit exactly once before re-register, got %d", refreshHits)
	}
	if registerHits != 0 {
		t.Errorf("expected NO register fallback when refresh succeeds, got %d", registerHits)
	}
	if hbHits < 2 {
		t.Errorf("expected at least 2 heartbeat attempts (initial 401 + refreshed retry), got %d", hbHits)
	}
	// workerId must NOT have been swapped on the refresh path.
	if currentWorkerID != workerID {
		t.Errorf("expected workerId to be preserved via refresh, got %q (was %q)", currentWorkerID, workerID)
	}
}

// TestRefreshRuntimeToken_NetworkErrorReturnsErr asserts that an
// unrelated network failure on the refresh probe surfaces as an
// error rather than silently re-registering. This protects against
// "platform partially down → daemon burns workerIds" failure mode.
func TestRefreshRuntimeToken_NetworkErrorReturnsErr(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/refresh-token") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// register would succeed if reached, but the assertion below
		// catches the case where we DID reach it.
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"workerId":"x","runtimeToken":"y"}`))
	}))
	defer srv.Close()

	regOpts := RegistrationOptions{
		OrchestratorURL: srv.URL,
		// #nosec G101 -- test fixture
		RegistrationToken: "rsp_live_x",
		Hostname:          "h",
		Version:           Version,
		MaxAgents:         1,
		JWTPath:           t.TempDir() + "/jwt.json",
		HTTPClient:        &http.Client{Timeout: 2 * time.Second},
	}
	_, err := RefreshRuntimeToken(context.Background(), regOpts, "wkr_x", "test")
	if err == nil {
		t.Fatalf("expected error on 5xx refresh probe (avoid burning workerId)")
	}
}
