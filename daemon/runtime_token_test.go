package daemon

import (
	"context"
	"encoding/json"
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
// This is the green-path REN-1481 fix: the platform side ships a
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
}

// TestRefreshRuntimeToken_FallsBackToReregisterOn404 asserts that
// when the platform's refresh endpoint returns 404 (current state —
// REN-1481 platform-side companion not yet shipped), the daemon
// falls back to a full re-register and observes a NEW workerId. This
// is the canonical REN-1481 root-cause path — proven, logged, and
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

// TestRefreshRuntimeToken_ProbedBeforeReregister asserts that on every
// auth-failure the daemon HITS the refresh endpoint FIRST. This is
// the REN-1481 acceptance check: "assert refresh path is hit BEFORE
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
// REN-1481.
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
// REN-1481 acceptance criterion: when the platform returns
// 401 "Runtime token expired" on a heartbeat, the daemon's
// OnReregister callback (which the daemon wires through
// RefreshRuntimeToken) HITS the refresh endpoint before falling back
// to a full re-register. The heartbeat resumes with the refreshed
// JWT against the SAME workerId.
func TestHeartbeatService_RefreshOn401Probe(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
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
	reregister := func(rctx context.Context) (string, string, error) {
		result, err := RefreshRuntimeToken(rctx, regOpts, currentWorkerID, "test")
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
