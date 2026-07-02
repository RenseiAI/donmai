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
// itself fell out of Redis (5-min TTL): the platform returns 404 and the
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
