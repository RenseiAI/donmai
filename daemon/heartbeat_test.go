package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/interview"
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

func TestHeartbeatService_DegradedSessionShimProjectionStillPosts(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var got heartbeatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		_ = json.NewEncoder(w).Encode(heartbeatResponseBody{
			Acknowledged: true,
			SessionShim:  got.SessionShim,
		})
	}))
	t.Cleanup(srv.Close)

	projection := SessionShimHeartbeatProjection{
		Enabled:             true,
		AdoptionComplete:    true,
		WorkerHostID:        "host",
		ControllerID:        "controller",
		AdoptionRevision:    "revision",
		ReadinessState:      "unknown",
		ReadinessReason:     "resolver: upstream unavailable",
		ReadinessObservedAt: "2026-09-03T00:00:00Z",
	}
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker", Hostname: "host", OrchestratorURL: srv.URL,
		RuntimeJWT: "runtime.jwt", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
		GetStatus: func() RegistrationStatus { return RegistrationIdle },
		GetSessionShim: func() (SessionShimHeartbeatProjection, error) {
			return SessionShimHeartbeatProjection{}, errors.New("resolver: upstream unavailable")
		},
		GetSessionShimDegraded: func(err error) (SessionShimHeartbeatProjection, error) {
			if err == nil {
				t.Fatal("degraded hook received nil error")
			}
			return projection, nil
		},
	})
	if err := hs.sendOneResult(context.Background()); err != nil {
		t.Fatalf("sendOneResult: %v", err)
	}
	if got.SessionShim == nil || got.SessionShim.ReadinessState != "unknown" {
		t.Fatalf("heartbeat session shim = %+v, want unknown readiness", got.SessionShim)
	}
}

// TestHeartbeatService_HitsPlatformEndpoint verifies the heartbeat HTTP call
// targets /api/workers/<id>/heartbeat with the runtime JWT in the
// Authorization header and { activeCount, maxSessions } in the body.
func TestHeartbeatService_HitsPlatformEndpoint(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

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
	if got, _ := body["maxSessions"].(float64); got != 8 {
		t.Errorf("body.maxSessions = %v, want 8", body["maxSessions"])
	}
}

// TestHeartbeatService_ReregisterOn401 covers the runtime-token refresh
// path: when the server returns 401 (token expired), the service invokes
// OnReregister, swaps in the fresh credentials, and retries the heartbeat
// without losing the tick.
func TestHeartbeatService_ReregisterOn401(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

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
		OnReregister: func(_ context.Context, _ string) (string, string, error) {
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
// itself is no longer recognised: the platform returns 404 and the
// daemon must re-register. Same recovery path as 401.
func TestHeartbeatService_ReregisterOn404(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

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
		OnReregister: func(_ context.Context, _ string) (string, string, error) {
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
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
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
		OnReregister: func(_ context.Context, _ string) (string, string, error) {
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

// TestHeartbeatService_AllowlistHashOnly_OnUnchangedBeats verifies the
// Phase 1d optimisation: first beat carries the full Allowlist payload;
// subsequent beats with an unchanged hash include only AllowlistHash;
// the full payload reappears when the allowlist mutates.
func TestHeartbeatService_AllowlistHashOnly_OnUnchangedBeats(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		payloads []HeartbeatPayload
		current  = []ProjectAllowlistEntry{{ID: "alpha", Repository: "github.com/x/alpha"}}
	)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "w1", Hostname: "h", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		GetAllowlist: func() []ProjectAllowlistEntry {
			mu.Lock()
			defer mu.Unlock()
			out := make([]ProjectAllowlistEntry, len(current))
			copy(out, current)
			return out
		},
		OnHeartbeat: func(p HeartbeatPayload) {
			mu.Lock()
			payloads = append(payloads, p)
			mu.Unlock()
		},
	})

	hs.sendOne(context.Background())
	hs.sendOne(context.Background())
	hs.sendOne(context.Background())

	mu.Lock()
	if len(payloads) != 3 {
		mu.Unlock()
		t.Fatalf("got %d payloads, want 3", len(payloads))
	}
	p0, p1, p2 := payloads[0], payloads[1], payloads[2]
	mu.Unlock()

	if p0.AllowlistHash == "" {
		t.Error("first beat: AllowlistHash empty")
	}
	if len(p0.Allowlist) != 1 || p0.Allowlist[0].ID != "alpha" {
		t.Errorf("first beat: Allowlist = %v, want [{alpha, ...}]", p0.Allowlist)
	}
	if p1.AllowlistHash != p0.AllowlistHash {
		t.Errorf("second beat hash %q != first %q", p1.AllowlistHash, p0.AllowlistHash)
	}
	if p1.Allowlist != nil {
		t.Errorf("second beat: Allowlist should be nil (hash unchanged), got %v", p1.Allowlist)
	}
	if p2.Allowlist != nil {
		t.Errorf("third beat: Allowlist should be nil (hash unchanged), got %v", p2.Allowlist)
	}

	// Mutate; next beat re-includes full payload.
	mu.Lock()
	current = []ProjectAllowlistEntry{
		{ID: "alpha", Repository: "github.com/x/alpha"},
		{ID: "beta", Repository: "github.com/x/beta"},
	}
	mu.Unlock()
	hs.sendOne(context.Background())

	mu.Lock()
	p3 := payloads[3]
	mu.Unlock()
	if p3.AllowlistHash == p0.AllowlistHash {
		t.Error("fourth beat: hash should differ after mutation")
	}
	if len(p3.Allowlist) != 2 {
		t.Errorf("fourth beat: Allowlist len = %d, want 2", len(p3.Allowlist))
	}
}

// TestHeartbeatService_AllowlistOmittedWhenUnconfigured verifies that a
// daemon with no GetAllowlist callback emits payloads with empty
// AllowlistHash and nil Allowlist (pre-Phase-1d wire shape preserved).
func TestHeartbeatService_AllowlistOmittedWhenUnconfigured(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		captured HeartbeatPayload
	)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "w1", Hostname: "h", IntervalSeconds: 1,
		GetActiveCount: func() int { return 0 },
		GetMaxCount:    func() int { return 1 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
		OnHeartbeat: func(p HeartbeatPayload) {
			mu.Lock()
			captured = p
			mu.Unlock()
		},
	})
	hs.sendOne(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if captured.AllowlistHash != "" {
		t.Errorf("AllowlistHash = %q, want empty", captured.AllowlistHash)
	}
	if captured.Allowlist != nil {
		t.Errorf("Allowlist = %v, want nil", captured.Allowlist)
	}
}

// TestHeartbeatService_LoadRoundTrips confirms the item-8 per-beat load sample
// is wired end-to-end: when GetLoad reports ok, the outbound body carries a
// nested load:{cpu,memory} object with the exact key names the platform
// heartbeat route parses (heartbeat/route.ts:127-138 → last_cpu_pct/last_mem_pct).
func TestHeartbeatService_LoadRoundTrips(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu  sync.Mutex
		raw map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_load",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 1 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		GetLoad: func() (float64, float64, bool) {
			return 37.5, 62.0, true
		},
	})
	hs.sendOne(context.Background())

	mu.Lock()
	defer mu.Unlock()
	load, ok := raw["load"].(map[string]any)
	if !ok {
		t.Fatalf("body.load missing or not an object: %v", raw["load"])
	}
	if got, _ := load["cpu"].(float64); got != 37.5 {
		t.Errorf("load.cpu = %v, want 37.5", load["cpu"])
	}
	if got, _ := load["memory"].(float64); got != 62.0 {
		t.Errorf("load.memory = %v, want 62.0", load["memory"])
	}
}

// TestHeartbeatService_LoadOmittedWhenSamplerMisses confirms the load key is
// omitted entirely (omitempty on the *heartbeatLoadFields pointer) when GetLoad
// reports ok=false or is nil — so an absent sample is distinguishable from a
// genuine {cpu:0,memory:0} and an old platform simply ignores the missing key.
func TestHeartbeatService_LoadOmittedWhenSamplerMisses(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	cases := []struct {
		name    string
		getLoad func() (float64, float64, bool)
	}{
		{"nil sampler", nil},
		{"sampler miss (ok=false)", func() (float64, float64, bool) { return 12, 34, false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu  sync.Mutex
				raw map[string]any
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				buf, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(buf, &raw)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
			}))
			t.Cleanup(srv.Close)

			hs := NewHeartbeatService(HeartbeatOptions{
				WorkerID:        "wkr_noload",
				Hostname:        "h",
				OrchestratorURL: srv.URL,
				RuntimeJWT:      "runtime.jwt.value",
				IntervalSeconds: 60,
				GetActiveCount:  func() int { return 0 },
				GetMaxCount:     func() int { return 1 },
				GetStatus:       func() RegistrationStatus { return RegistrationIdle },
				GetLoad:         tc.getLoad,
			})
			hs.sendOne(context.Background())

			mu.Lock()
			defer mu.Unlock()
			if _, present := raw["load"]; present {
				t.Errorf("expected load key absent, got %v", raw["load"])
			}
		})
	}
}

// TestHeartbeatService_ActiveSessionCountsRoundTrips confirms the coherent
// occupancy snapshot is wired end-to-end as distinct activeCount and
// activeInteractiveCount values.
func TestHeartbeatService_ActiveSessionCountsRoundTrips(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu  sync.Mutex
		raw map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:               "wkr_interactive",
		Hostname:               "h",
		OrchestratorURL:        srv.URL,
		RuntimeJWT:             "runtime.jwt.value",
		IntervalSeconds:        60,
		GetActiveSessionCounts: func() (int, int) { return 3, 2 },
		GetActiveCount:         func() int { return 99 },
		GetMaxCount:            func() int { return 4 },
		GetStatus:              func() RegistrationStatus { return RegistrationIdle },
	})
	hs.sendOne(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if got, _ := raw["activeCount"].(float64); got != 3 {
		t.Errorf("body.activeCount = %v, want 3", raw["activeCount"])
	}
	got, present := raw["activeInteractiveCount"]
	if !present {
		t.Fatalf("body.activeInteractiveCount missing, want 2")
	}
	if v, _ := got.(float64); v != 2 {
		t.Errorf("body.activeInteractiveCount = %v, want 2", got)
	}
}

func TestHeartbeatService_LegacyActiveInteractiveCountRoundTrips(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	for _, tc := range []struct {
		name        string
		active      int
		interactive int
	}{
		{name: "nonzero", active: 3, interactive: 2},
		{name: "classified zero", active: 0, interactive: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu  sync.Mutex
				raw map[string]any
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				buf, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(buf, &raw)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
			}))
			t.Cleanup(srv.Close)

			hs := NewHeartbeatService(HeartbeatOptions{
				WorkerID:                  "wkr_legacy_interactive",
				Hostname:                  "h",
				OrchestratorURL:           srv.URL,
				RuntimeJWT:                "runtime.jwt.value",
				IntervalSeconds:           60,
				GetActiveCount:            func() int { return tc.active },
				GetActiveInteractiveCount: func() int { return tc.interactive },
				GetMaxCount:               func() int { return 4 },
				GetStatus:                 func() RegistrationStatus { return RegistrationIdle },
			})
			hs.sendOne(context.Background())

			mu.Lock()
			defer mu.Unlock()
			if got, _ := raw["activeCount"].(float64); got != float64(tc.active) {
				t.Errorf("body.activeCount = %v, want %d", raw["activeCount"], tc.active)
			}
			got, present := raw["activeInteractiveCount"]
			if !present {
				t.Fatal("body.activeInteractiveCount missing for legacy callback")
			}
			if got != float64(tc.interactive) {
				t.Errorf("body.activeInteractiveCount = %v, want %d", got, tc.interactive)
			}
		})
	}
}

func TestHeartbeatService_LegacyActiveInteractiveCountOmitsImpossibleTornSample(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		stateMu        sync.Mutex
		activeNow      int
		interactiveNow int
	)
	activeSampled := make(chan struct{})
	releaseInteractiveSample := make(chan struct{})

	var (
		bodyMu sync.Mutex
		raw    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyMu.Lock()
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		bodyMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	var (
		warningMu     sync.Mutex
		warningFormat string
		warningArgs   []any
	)
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_legacy_torn",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveCount: func() int {
			stateMu.Lock()
			active := activeNow
			stateMu.Unlock()
			close(activeSampled)
			return active
		},
		GetActiveInteractiveCount: func() int {
			<-releaseInteractiveSample
			stateMu.Lock()
			defer stateMu.Unlock()
			return interactiveNow
		},
		GetMaxCount: func() int { return 4 },
		GetStatus:   func() RegistrationStatus { return RegistrationIdle },
		LogWarn: func(format string, args ...any) {
			warningMu.Lock()
			warningFormat = format
			warningArgs = append([]any(nil), args...)
			warningMu.Unlock()
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		hs.sendOne(context.Background())
	}()

	// The legacy active callback observed (0,0). Advance the coherent state to
	// (1,1) before releasing the separately sampled interactive callback. The
	// two valid instants therefore tear into the impossible legacy pair (0,1).
	<-activeSampled
	stateMu.Lock()
	activeNow = 1
	interactiveNow = 1
	stateMu.Unlock()
	close(releaseInteractiveSample)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not complete after releasing legacy sample barrier")
	}

	payload := hs.LastPayload()
	if payload.ActiveSessions != 0 {
		t.Fatalf("payload.ActiveSessions = %d, want sampled legacy value 0", payload.ActiveSessions)
	}
	if payload.ActiveInteractiveSessions != nil {
		t.Fatalf("payload.ActiveInteractiveSessions = %d, want nil for impossible legacy pair", *payload.ActiveInteractiveSessions)
	}

	bodyMu.Lock()
	if got := raw["activeCount"]; got != float64(0) {
		bodyMu.Unlock()
		t.Fatalf("body.activeCount = %v, want 0", got)
	}
	if got, present := raw["activeInteractiveCount"]; present {
		bodyMu.Unlock()
		t.Fatalf("body.activeInteractiveCount = %v, want key omitted for impossible legacy pair", got)
	}
	bodyMu.Unlock()

	warningMu.Lock()
	defer warningMu.Unlock()
	if !strings.Contains(warningFormat, "invalid legacy occupancy sample") {
		t.Fatalf("warning format = %q, want invalid legacy occupancy sample", warningFormat)
	}
	if len(warningArgs) != 2 || warningArgs[0] != 0 || warningArgs[1] != 1 {
		t.Fatalf("warning args = %v, want [0 1]", warningArgs)
	}
}

// TestHeartbeatService_ActiveInteractiveCountOmittedWhenUnclassified confirms
// the `activeInteractiveCount` key is omitted entirely (pointer + omitempty)
// when both interactive-classification callbacks are nil. It asserts key
// ABSENCE, not `== 0`, so a genuine classified zero remains distinguishable.
func TestHeartbeatService_ActiveInteractiveCountOmittedWhenUnclassified(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu  sync.Mutex
		raw map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_nointeractive",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveCount:  func() int { return 1 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		// Both interactive-classification callbacks deliberately nil.
	})
	hs.sendOne(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if _, present := raw["activeInteractiveCount"]; present {
		t.Errorf("expected activeInteractiveCount key absent, got %v", raw["activeInteractiveCount"])
	}
}

func TestHeartbeatService_ActiveSessionCountsTakePrecedence(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var (
		mu  sync.Mutex
		raw map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	var atomicCalls, legacyActiveCalls, legacyInteractiveCalls int32
	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_atomic_occupancy",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "runtime.jwt.value",
		IntervalSeconds: 60,
		GetActiveSessionCounts: func() (int, int) {
			atomic.AddInt32(&atomicCalls, 1)
			return 3, 0
		},
		GetActiveCount: func() int {
			atomic.AddInt32(&legacyActiveCalls, 1)
			return 99
		},
		GetActiveInteractiveCount: func() int {
			atomic.AddInt32(&legacyInteractiveCalls, 1)
			return 88
		},
		GetMaxCount: func() int { return 4 },
		GetStatus:   func() RegistrationStatus { return RegistrationIdle },
	})
	hs.sendOne(context.Background())

	if got := atomic.LoadInt32(&atomicCalls); got != 1 {
		t.Fatalf("GetActiveSessionCounts calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&legacyActiveCalls); got != 0 {
		t.Fatalf("GetActiveCount calls = %d, want 0 when coherent callback is configured", got)
	}
	if got := atomic.LoadInt32(&legacyInteractiveCalls); got != 0 {
		t.Fatalf("GetActiveInteractiveCount calls = %d, want 0 when coherent callback is configured", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := raw["activeCount"]; got != float64(3) {
		t.Errorf("body.activeCount = %v, want 3", got)
	}
	got, present := raw["activeInteractiveCount"]
	if !present {
		t.Fatal("body.activeInteractiveCount missing for classified zero occupancy")
	}
	if got != float64(0) {
		t.Errorf("body.activeInteractiveCount = %v, want 0", got)
	}
}

func TestHeartbeatService_ActiveSessionCountsCoherentUnderConcurrentLifecycle(t *testing.T) {
	spawner := NewWorkerSpawner(SpawnerOptions{})
	started := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		phase := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			spawner.mu.Lock()
			clear(spawner.sessions)
			switch phase {
			case 1:
				spawner.sessions["headless"] = &spawnedSession{
					spec: SessionSpec{SessionID: "headless"},
				}
			case 2:
				spawner.sessions["interview"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interview", Mode: interview.InterviewRunMode},
				}
			case 3:
				spawner.sessions["interactive"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interactive", Mode: interactiveRunMode},
				}
			case 4:
				spawner.sessions["interactive"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interactive", Mode: interactiveRunMode},
				}
				spawner.sessions["interview"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interview", Mode: interview.InterviewRunMode},
				}
				spawner.sessions["unknown"] = &spawnedSession{
					spec: SessionSpec{SessionID: "unknown", Mode: "interactive-preview"},
				}
			}
			phase = (phase + 1) % 5
			spawner.mu.Unlock()
		}
	}()
	<-started
	defer func() {
		close(stop)
		wg.Wait()
	}()

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:               "wkr_concurrent_occupancy",
		Hostname:               "h",
		RuntimeJWT:             "stub.runtime.jwt",
		GetActiveSessionCounts: spawner.ActiveSessionCounts,
		// This fallback deliberately disagrees with the live spawner. If sendOne
		// stops using the coherent callback, classification disappears and the
		// assertions below fail.
		GetActiveCount: func() int { return 99 },
		GetMaxCount:    func() int { return 4 },
		GetStatus:      func() RegistrationStatus { return RegistrationIdle },
	})

	valid := map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
		{1, 1}: true,
		{3, 2}: true,
	}
	for range 10_000 {
		hs.sendOne(context.Background())
		payload := hs.LastPayload()
		if payload.ActiveInteractiveSessions == nil {
			t.Fatal("classified occupancy omitted activeInteractiveSessions")
		}
		activeInteractive := *payload.ActiveInteractiveSessions
		if activeInteractive > payload.ActiveSessions {
			t.Fatalf("heartbeat interactive occupancy exceeds total: active=%d interactive=%d",
				payload.ActiveSessions, activeInteractive)
		}
		if !valid[[2]int{payload.ActiveSessions, activeInteractive}] {
			t.Fatalf("torn heartbeat occupancy: active=%d interactive=%d",
				payload.ActiveSessions, activeInteractive)
		}
	}
}

func TestHeartbeatPayload_ActiveInteractiveSessionsJSONCompatibility(t *testing.T) {
	t.Run("unclassified omits introspection field", func(t *testing.T) {
		buf, err := json.Marshal(HeartbeatPayload{ActiveSessions: 2})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(buf, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, present := raw["activeInteractiveSessions"]; present {
			t.Fatalf("activeInteractiveSessions unexpectedly present: %s", buf)
		}
	})

	t.Run("classified zero remains present", func(t *testing.T) {
		zero := 0
		buf, err := json.Marshal(HeartbeatPayload{
			ActiveSessions:            2,
			ActiveInteractiveSessions: &zero,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(buf, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, present := raw["activeInteractiveSessions"]
		if !present {
			t.Fatalf("activeInteractiveSessions missing: %s", buf)
		}
		if got != float64(0) {
			t.Fatalf("activeInteractiveSessions = %v, want 0", got)
		}
	})
}

// TestHeartbeatRequestBody_CarriesStatusOnTheWire asserts on the SERIALIZED
// request body, not on HeartbeatPayload. The daemon computes its lifecycle
// status every beat, but the struct that is actually marshalled is
// heartbeatRequestBody — a struct-level assertion on HeartbeatPayload would
// pass while the value never left the process, which is exactly how the
// status key came to be silently dropped.
//
// The server uses `status` to exclude a draining host from dispatch, and an
// absent key must stay distinguishable from an empty one: a daemon with
// nothing to report sends no key at all.
func TestHeartbeatRequestBody_CarriesStatusOnTheWire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      RegistrationStatus
		wantPresent bool
		wantValue   string
	}{
		{name: "idle", status: RegistrationIdle, wantPresent: true, wantValue: "idle"},
		{name: "busy", status: RegistrationBusy, wantPresent: true, wantValue: "busy"},
		{name: "draining", status: RegistrationDraining, wantPresent: true, wantValue: "draining"},
		{name: "unreported status omits the key", status: "", wantPresent: false},
		{name: "whitespace-only status omits the key", status: "   ", wantPresent: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				mu  sync.Mutex
				raw []byte
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buf, _ := io.ReadAll(r.Body)
				mu.Lock()
				raw = buf
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
			}))
			t.Cleanup(srv.Close)

			hs := NewHeartbeatService(HeartbeatOptions{
				WorkerID:        "wkr_status",
				Hostname:        "h",
				OrchestratorURL: srv.URL,
				RuntimeJWT:      "runtime.jwt.value",
				IntervalSeconds: 1,
				GetActiveCount:  func() int { return 0 },
				GetMaxCount:     func() int { return 1 },
				GetStatus:       func() RegistrationStatus { return tt.status },
			})
			hs.sendOne(context.Background())

			mu.Lock()
			body := append([]byte(nil), raw...)
			mu.Unlock()
			if len(body) == 0 {
				t.Fatal("heartbeat endpoint received no body")
			}

			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("unmarshal request body: %v (%s)", err, body)
			}
			got, present := decoded["status"]
			if present != tt.wantPresent {
				t.Fatalf("status key present = %v, want %v (body: %s)", present, tt.wantPresent, body)
			}
			if tt.wantPresent && got != tt.wantValue {
				t.Errorf("status = %v, want %q (body: %s)", got, tt.wantValue, body)
			}
		})
	}
}
