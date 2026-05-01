package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatService_StartStop(t *testing.T) {
	var count int32
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "w1", Hostname: "h", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		OnHeartbeat:    func(_ HeartbeatPayload) { atomic.AddInt32(&count, 1) },
	})
	hs.Start()
	if !hs.IsRunning() {
		t.Fatal("expected running after Start")
	}
	// Wait for the immediate first heartbeat.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&count) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&count) == 0 {
		t.Fatal("expected at least one heartbeat")
	}
	hs.Stop()
	if hs.IsRunning() {
		t.Fatal("expected not running after Stop")
	}
	got := hs.LastPayload()
	if got.WorkerID != "w1" {
		t.Errorf("LastPayload.WorkerID = %q", got.WorkerID)
	}
}

func TestHeartbeatService_IdempotentStart(_ *testing.T) {
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "x", Hostname: "h",
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
	})
	hs.Start()
	hs.Start() // should be a no-op
	hs.Stop()
}

// TestHeartbeatService_HitsPlatformEndpoint verifies the heartbeat HTTP call
// targets /api/workers/<id>/heartbeat with the runtime JWT in the
// Authorization header and { activeCount } in the body — the real platform
// contract (REN-1422 wire fix).
func TestHeartbeatService_HitsPlatformEndpoint(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu    sync.Mutex
		count int
		path  string
		auth  string
		body  map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"acknowledged":     true,
			"serverTime":       time.Now().UTC().Format(time.RFC3339),
			"pendingWorkCount": 0,
		})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_test1",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "runtime.jwt.value",
		IntervalSeconds: 1,
		GetActiveCount:  func() int { return 3 },
		GetMaxCount:     func() int { return 8 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
	})
	hs.Start()
	t.Cleanup(hs.Stop)

	// Wait for the immediate first heartbeat to round-trip.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := count
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if count == 0 {
		t.Fatal("expected at least one heartbeat HTTP call")
	}
	if path != "/api/workers/wkr_test1/heartbeat" {
		t.Errorf("path = %q, want /api/workers/wkr_test1/heartbeat", path)
	}
	if auth != "Bearer runtime.jwt.value" {
		t.Errorf("auth = %q, want Bearer runtime.jwt.value", auth)
	}
	if got, _ := body["activeCount"].(float64); got != 3 {
		t.Errorf("body.activeCount = %v, want 3", body["activeCount"])
	}
}

// TestHeartbeatService_ReregisterOn401 covers the runtime-token refresh
// path: when the server returns 401 (token expired), the service invokes
// OnReregister, swaps in the fresh credentials, and retries the heartbeat
// without losing the tick.
func TestHeartbeatService_ReregisterOn401(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")

	var (
		callsBefore atomic.Int32
		callsAfter  atomic.Int32
		reregister  atomic.Int32
		seenAuths   atomic.Value // last auth header
		seenPaths   atomic.Value
	)
	seenAuths.Store("")
	seenPaths.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuths.Store(r.Header.Get("Authorization"))
		seenPaths.Store(r.URL.Path)
		// First worker id sees a 401 (simulating expired runtime JWT).
		// After re-register the worker id changes; that path returns 200.
		if strings.Contains(r.URL.Path, "/wkr_old/") {
			callsBefore.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Runtime token expired"}`))
			return
		}
		callsAfter.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_old",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "expired.jwt",
		IntervalSeconds: 60, // long — we only want the immediate first send
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		OnReregister: func(_ context.Context) (string, string, error) {
			reregister.Add(1)
			return "wkr_new", "fresh.jwt", nil
		},
	})
	hs.Start()
	t.Cleanup(hs.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && callsAfter.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := callsBefore.Load(); got != 1 {
		t.Errorf("expected exactly 1 stale-credential call, got %d", got)
	}
	if got := reregister.Load(); got != 1 {
		t.Errorf("expected exactly 1 OnReregister call, got %d", got)
	}
	if got := callsAfter.Load(); got != 1 {
		t.Errorf("expected exactly 1 fresh-credential retry, got %d", got)
	}
	if got, _ := seenAuths.Load().(string); got != "Bearer fresh.jwt" {
		t.Errorf("final Authorization = %q, want Bearer fresh.jwt", got)
	}
	if got, _ := seenPaths.Load().(string); got != "/api/workers/wkr_new/heartbeat" {
		t.Errorf("final path = %q, want /api/workers/wkr_new/heartbeat", got)
	}
	gotID, gotJWT := hs.CurrentCredentials()
	if gotID != "wkr_new" || gotJWT != "fresh.jwt" {
		t.Errorf("CurrentCredentials = (%q, %q), want (wkr_new, fresh.jwt)", gotID, gotJWT)
	}
}

// TestHeartbeatService_ReregisterOn404 covers the case where the worker
// itself fell out of Redis (5-min TTL): the platform returns 404 and the
// daemon must re-register. Same recovery path as 401.
func TestHeartbeatService_ReregisterOn404(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")

	var (
		gotFresh atomic.Bool
		regs     atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/wkr_gone/") {
			http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
			return
		}
		gotFresh.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_gone",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "stale.jwt",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		OnReregister: func(_ context.Context) (string, string, error) {
			regs.Add(1)
			return "wkr_back", "back.jwt", nil
		},
	})
	hs.Start()
	t.Cleanup(hs.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !gotFresh.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	if !gotFresh.Load() {
		t.Fatal("expected post-reregister heartbeat to succeed")
	}
	if regs.Load() != 1 {
		t.Errorf("OnReregister calls = %d, want 1", regs.Load())
	}
}

// TestHeartbeatService_ReregisterFailure_NoCredSwap verifies that when the
// re-register itself fails, the service does NOT clobber its current
// credentials — the next tick will retry the same stale credentials and
// loop into the same recovery branch. This mirrors the bash sidecar's
// behaviour and avoids dropping into an unrecoverable state.
func TestHeartbeatService_ReregisterFailure_NoCredSwap(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	var warns atomic.Int32
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_x",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "old",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		LogWarn:         func(string, ...any) { warns.Add(1) },
		OnReregister: func(_ context.Context) (string, string, error) {
			return "", "", &reregisterErr{}
		},
	})
	hs.Start()
	t.Cleanup(hs.Stop)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && warns.Load() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	gotID, gotJWT := hs.CurrentCredentials()
	if gotID != "wkr_x" || gotJWT != "old" {
		t.Errorf("credentials clobbered after failed re-register: (%q, %q)", gotID, gotJWT)
	}
}

type reregisterErr struct{}

func (*reregisterErr) Error() string { return "no platform" }
