package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegister_StubPath_NoToken(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "https://platform.rensei.dev",
		RegistrationToken: "local-stub-no-token",
		Hostname:          "test-host",
		Version:           "0.4.0-dev",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "worker-test-host-stub" {
		t.Errorf("WorkerID = %q, want worker-test-host-stub", resp.WorkerID)
	}
	if !strings.HasPrefix(resp.RuntimeToken, "stub.") {
		t.Errorf("expected stub runtime-token prefix, got %q", resp.RuntimeToken)
	}
	if resp.HeartbeatInterval == 0 {
		t.Error("expected non-zero heartbeat interval")
	}
	if resp.HeartbeatIntervalSeconds() == 0 {
		t.Error("expected non-zero heartbeat interval seconds")
	}
}

// TestRegister_DefaultsToRealPath covers REN-1444: with NO env var set and a
// valid rsk_live_* token and an http:// URL, the daemon must take the real
// path. Previously useStub defaulted to true unless
// RENSEI_DAEMON_REAL_REGISTRATION was explicitly set; that gate broke
// daemons that did not source the env in their launchd plist.
func TestRegister_DefaultsToRealPath(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_FORCE_STUB", "")
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_default",
			"runtimeToken":      "tok-default",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "default-host",
		Version:           "0.4.1-dev",
		MaxAgents:         2,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !called {
		t.Fatal("expected real endpoint to be hit by default")
	}
	if resp.WorkerID != "wkr_default" {
		t.Errorf("WorkerID = %q", resp.WorkerID)
	}
}

// TestRegister_ForceStubOptIn confirms RENSEI_DAEMON_FORCE_STUB still routes
// to the stub path when explicitly set, even with a real-shaped token.
func TestRegister_ForceStubOptIn(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_FORCE_STUB", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("real endpoint should NOT be hit when FORCE_STUB=1")
	}))
	t.Cleanup(srv.Close)
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "h", Version: "0.4.1-dev", MaxAgents: 1,
		JWTPath: jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.RuntimeToken, "stub.") {
		t.Errorf("expected stub token under FORCE_STUB=1, got %q", resp.RuntimeToken)
	}
}

// TestRegister_LegacyRealRegistrationZeroForcesStub confirms that the legacy
// RENSEI_DAEMON_REAL_REGISTRATION=0 still routes to stub for back-compat
// with any existing test harness that explicitly disabled the real path.
func TestRegister_LegacyRealRegistrationZeroForcesStub(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("real endpoint should NOT be hit when REAL_REGISTRATION=0")
	}))
	t.Cleanup(srv.Close)
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic
	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "h", Version: "0.4.1-dev", MaxAgents: 1,
		JWTPath: jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegister_FileURLForcesStub(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsp_live_" + "xxx" //nolint:gosec // synthetic test token, not a real credential
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "file:///tmp/queue",
		RegistrationToken: tok,
		Hostname:          "test-host",
		Version:           "0.4.0-dev",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.HasPrefix(resp.RuntimeToken, "stub.") {
		t.Errorf("expected stub path for file:// URL, got %q", resp.RuntimeToken)
	}
}

// TestRegister_RealEndpoint covers the wire contract against an httptest
// server playing the role of /api/workers/register: token in Authorization
// header, request body shape, response field names.
func TestRegister_RealEndpoint(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")

	var capturedAuth string
	var capturedBody RegisterRequest
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if r.URL.Path != RegisterEndpoint {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		capturedAuth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &capturedBody)
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:gosec // synthetic test response
			"workerId":              "wkr_live1",
			"runtimeToken":          "real.jwt.value",
			"runtimeTokenExpiresAt": "2099-01-01T00:00:00Z",
			"heartbeatInterval":     30000,
			"pollInterval":          5000,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsp_live_" + "xyz" //nolint:gosec // synthetic test token, not a real credential
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "test-host",
		Version:           "0.4.0-dev",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "wkr_live1" {
		t.Errorf("WorkerID = %q, want wkr_live1", resp.WorkerID)
	}
	if resp.RuntimeToken != "real.jwt.value" {
		t.Errorf("RuntimeToken = %q", resp.RuntimeToken)
	}
	if resp.HeartbeatInterval != 30000 {
		t.Errorf("heartbeatInterval(ms) = %d, want 30000", resp.HeartbeatInterval)
	}
	if got := resp.HeartbeatIntervalSeconds(); got != 30 {
		t.Errorf("HeartbeatIntervalSeconds() = %d, want 30", got)
	}
	if capturedPath != "/api/workers/register" {
		t.Errorf("endpoint path = %q, want /api/workers/register", capturedPath)
	}
	// Wire contract: Authorization header carries the token.
	if got, want := capturedAuth, "Bearer "+tok; got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
	// Wire contract: body is { hostname, capacity, version }.
	if capturedBody.Hostname != "test-host" {
		t.Errorf("body.hostname = %q", capturedBody.Hostname)
	}
	if capturedBody.Capacity != 4 {
		t.Errorf("body.capacity = %d, want 4", capturedBody.Capacity)
	}
	if capturedBody.Version != "0.4.0-dev" {
		t.Errorf("body.version = %q", capturedBody.Version)
	}
}

// TestRegister_AcceptsRskLivePrefix confirms the stub-vs-real switch accepts
// the new rsk_live_* prefix that REN-1351's unified mint endpoint produces,
// not just the legacy rsp_live_* shape.
func TestRegister_AcceptsRskLivePrefix(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_rsk",
			"runtimeToken":      "tok",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic test token
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "rsk-host",
		Version:           "0.4.0-dev",
		MaxAgents:         2,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !called {
		t.Fatal("expected the real endpoint to be hit for an rsk_live_ token")
	}
	if resp.WorkerID != "wkr_rsk" {
		t.Errorf("unexpected WorkerID %q", resp.WorkerID)
	}
}

// TestRegister_PlainTokenForcesStub verifies non rs[pk]_live_ tokens fall
// through to the stub path even when REAL_REGISTRATION is set, so e.g. a
// laptop dev with a junk token can't accidentally hit prod.
func TestRegister_PlainTokenForcesStub(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("real endpoint should not be reached for plain token")
	}))
	t.Cleanup(srv.Close)
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: "garbage-token",
		Hostname:          "h", Version: "0.4.0-dev", MaxAgents: 1,
		JWTPath: jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.RuntimeToken, "stub.") {
		t.Errorf("expected stub path, got %q", resp.RuntimeToken)
	}
}

func TestRegister_RealEndpointError_IncludesBody(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"capacity must be a positive number"}`))
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsp_live_" + "xyz" //nolint:gosec // synthetic
	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: srv.URL, RegistrationToken: tok,
		Hostname: "h", Version: "0.4.0-dev", MaxAgents: 4,
		JWTPath: jwtPath,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "capacity must") {
		t.Errorf("expected status + body in error, got %v", err)
	}
}

func TestRegister_CachedJWTReturned(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	first, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "file:///tmp/q",
		RegistrationToken: "x",
		Hostname:          "host1",
		Version:           "0.4.0-dev",
		MaxAgents:         1,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "file:///tmp/different",
		RegistrationToken: "y",
		Hostname:          "host-different", // would produce different stub if not cached
		Version:           "0.4.0-dev",
		MaxAgents:         1,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerID != second.WorkerID {
		t.Errorf("expected cached worker id; got %q vs %q", first.WorkerID, second.WorkerID)
	}
}

func TestRegister_ForceReregister(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	first, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: "file:///tmp/q", RegistrationToken: "x",
		Hostname: "host-A", Version: "0.4.0-dev", JWTPath: jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: "file:///tmp/q", RegistrationToken: "x",
		Hostname: "host-B", Version: "0.4.0-dev", JWTPath: jwtPath,
		ForceReregister: true,
		Now:             func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerID == second.WorkerID {
		t.Errorf("force reregister should produce a fresh worker id; both = %q", first.WorkerID)
	}
}

func TestSaveAndLoadCachedJWT(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	resp := &RegisterResponse{
		WorkerID: "wid", RuntimeToken: "jwt", HeartbeatInterval: 30000, PollInterval: 10000,
	}
	if err := SaveCachedJWT(jwtPath, resp, time.Now()); err != nil {
		t.Fatalf("save: %v", err)
	}
	c, err := LoadCachedJWT(jwtPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.WorkerID != "wid" {
		t.Errorf("WorkerID = %q", c.WorkerID)
	}
	if c.RuntimeToken != "jwt" {
		t.Errorf("RuntimeToken = %q", c.RuntimeToken)
	}
	if c.HeartbeatInterval != 30000 {
		t.Errorf("HeartbeatInterval = %d", c.HeartbeatInterval)
	}
}

// TestLoadCachedJWT_LegacyFormat covers the migration path: a daemon.jwt
// file written by 0.1.0 (with runtimeJwt + heartbeatIntervalSeconds fields)
// should still load via the new struct, with the legacy fields promoted.
func TestLoadCachedJWT_LegacyFormat(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	const legacy = `{
  "workerId": "legacy-wid",
  "runtimeJwt": "legacy.jwt",
  "heartbeatIntervalSeconds": 30,
  "pollIntervalSeconds": 5,
  "cachedAt": "2026-04-01T00:00:00Z"
}`
	if err := os.WriteFile(jwtPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCachedJWT(jwtPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c == nil {
		t.Fatal("expected legacy cache to load")
	}
	if c.WorkerID != "legacy-wid" {
		t.Errorf("WorkerID = %q", c.WorkerID)
	}
	if c.RuntimeToken != "legacy.jwt" {
		t.Errorf("RuntimeToken = %q (legacy migration)", c.RuntimeToken)
	}
	if c.HeartbeatInterval != 30000 {
		t.Errorf("HeartbeatInterval(ms) = %d (want 30000 from 30s)", c.HeartbeatInterval)
	}
	if c.PollInterval != 5000 {
		t.Errorf("PollInterval(ms) = %d (want 5000 from 5s)", c.PollInterval)
	}
}

func TestLoadCachedJWT_Missing(t *testing.T) {
	c, err := LoadCachedJWT(filepath.Join(t.TempDir(), "missing.jwt"))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if c != nil {
		t.Errorf("expected nil for missing, got %+v", c)
	}
}
