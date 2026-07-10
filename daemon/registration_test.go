package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegister_StubPath_NoToken(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "https://platform.example.com",
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

// TestRegister_DefaultsToRealPath covers the real-registration default: with NO env var set and a
// valid rsk_live_* token and an http:// URL, the daemon must take the real
// path. Previously useStub defaulted to true unless
// DONMAI_DAEMON_REAL_REGISTRATION was explicitly set; that gate broke
// daemons that did not source the env in their launchd plist.
func TestRegister_DefaultsToRealPath(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "")
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "")
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

// TestRegister_ForceStubOptIn confirms DONMAI_DAEMON_FORCE_STUB still routes
// to the stub path when explicitly set, even with a real-shaped token.
func TestRegister_ForceStubOptIn(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "1")
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
// DONMAI_DAEMON_REAL_REGISTRATION=0 still routes to stub for back-compat
// with any existing test harness that explicitly disabled the real path.
func TestRegister_LegacyRealRegistrationZeroForcesStub(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "0")
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
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsp_live_" + "xxx" //nolint:gosec // synthetic test token, not a real credential
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "file:///tmp/queue",
		RegistrationToken: tok,
		MachineID:         "machine-test-host",
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
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

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
		MachineID:         "machine-test-host",
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
	// Wire contract: body is { machineId, hostname, capacity, version }.
	if capturedBody.MachineID != "machine-test-host" {
		t.Errorf("body.machineId = %q", capturedBody.MachineID)
	}
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
// the new rsk_live_* prefix that the unified mint endpoint produces,
// not just the legacy rsp_live_* shape.
func TestRegister_AcceptsRskLivePrefix(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
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
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
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
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
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

// TestWipeCachedJWT_Removes covers the happy path: a present cache file
// is removed and wiped=true is returned.
func TestWipeCachedJWT_Removes(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	if err := os.WriteFile(jwtPath, []byte(`{"workerId":"wid","runtimeToken":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wiped, err := WipeCachedJWT(jwtPath)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if !wiped {
		t.Errorf("wiped = false, want true (cache existed)")
	}
	if _, statErr := os.Stat(jwtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("cache file should be gone, stat err = %v", statErr)
	}
}

// TestWipeCachedJWT_AbsentIsNoOp confirms the helper is safe on systems
// that never had a cache file (uninstall on a never-installed host).
func TestWipeCachedJWT_AbsentIsNoOp(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "missing.jwt")
	wiped, err := WipeCachedJWT(jwtPath)
	if err != nil {
		t.Fatalf("wipe missing: %v", err)
	}
	if wiped {
		t.Errorf("wiped = true on missing cache; want false (idempotent)")
	}
}

// TestRegister_AfterWipeReregisters proves the integration: a cached
// JWT short-circuits Register(), but wiping the cache forces the next
// Register() call to walk the registration path again and write a
// fresh cache. Without this guarantee, install/uninstall paths that
// wipe the cache would not actually trigger a fresh registration
// handshake.
//
// We verify the wipe-then-rewrite semantics rather than comparing
// WorkerIDs because the stub registration path is deterministic by
// hostname (the live platform path returns fresh ids; the stub path
// returning a stable id makes the test value-based assertion
// inappropriate).
func TestRegister_AfterWipeReregisters(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "1")

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")

	// First register seeds the cache.
	if _, err := Register(context.Background(), RegistrationOptions{
		Hostname: "h", JWTPath: jwtPath,
	}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := os.Stat(jwtPath); err != nil {
		t.Fatalf("expected cache file present after first register: %v", err)
	}

	// Wipe — cache must be gone.
	wiped, err := WipeCachedJWT(jwtPath)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if !wiped {
		t.Fatalf("wipe reported false despite cache existing")
	}
	if _, statErr := os.Stat(jwtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cache file should be gone after wipe; stat err = %v", statErr)
	}

	// Re-register — cache must be re-created (proves the wipe forces
	// Register() to walk the registration path again rather than
	// short-circuiting on the now-missing cache).
	if _, err := Register(context.Background(), RegistrationOptions{
		Hostname: "h", JWTPath: jwtPath,
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if _, err := os.Stat(jwtPath); err != nil {
		t.Errorf("expected cache file recreated after wipe + register; stat err = %v", err)
	}
}

// TestRegister_ProvidesArraySentInBody verifies that the RegisterRequest body
// sent to POST /api/workers/register includes the provides[] array when
// RegistrationOptions.Provides is populated. This covers Stream H
// (pool-aware daemon) — the wire contract for substrate capability advertisement.
func TestRegister_ProvidesArraySentInBody(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var capturedBody RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &capturedBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_h",
			"runtimeToken":      "tok",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	provides := []ProvideCapability{
		{Kind: "native"},
		{Kind: "npm"},
		{Kind: "http"},
		{Kind: "mcp-server"},
		{Kind: "host-binary"},
		{Kind: "workarea"},
	}
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic test token
	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "cap-host",
		Version:           "0.6.0-dev",
		MaxAgents:         2,
		JWTPath:           jwtPath,
		Provides:          provides,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(capturedBody.Provides) != len(provides) {
		t.Fatalf("body.provides count = %d, want %d; got %+v",
			len(capturedBody.Provides), len(provides), capturedBody.Provides)
	}
	kinds := make(map[string]bool, len(capturedBody.Provides))
	for _, p := range capturedBody.Provides {
		kinds[p.Kind] = true
	}
	for _, want := range []string{"native", "npm", "http", "mcp-server", "host-binary", "workarea"} {
		if !kinds[want] {
			t.Errorf("provides[] missing %q; got %v", want, capturedBody.Provides)
		}
	}
}

// TestRegister_CapabilitiesSentInBody verifies that RegistrationOptions.Capabilities
// is serialised into the RegisterRequest body as "capabilities". This is a
// regression pin for a wire gap where Capabilities was set at the call sites but
// never copied into the request struct, so the platform stored workers.capabilities
// = [] and every capability-gated claim lane (KG-extraction, FD-4 landing's
// "merge-queue") silently no-routed.
func TestRegister_CapabilitiesSentInBody(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var capturedBody RegisterRequest
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(rawBody, &capturedBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_c",
			"runtimeToken":      "tok",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	caps := []string{"local", "sandbox", "workarea", "merge-queue"}
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic test token
	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "cap-host",
		Version:           "0.6.0-dev",
		MaxAgents:         2,
		JWTPath:           jwtPath,
		Capabilities:      caps,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(capturedBody.Capabilities) != len(caps) {
		t.Fatalf("body.capabilities = %v, want %v", capturedBody.Capabilities, caps)
	}
	got := make(map[string]bool, len(capturedBody.Capabilities))
	for _, c := range capturedBody.Capabilities {
		got[c] = true
	}
	for _, want := range caps {
		if !got[want] {
			t.Errorf("capabilities[] missing %q; got %v", want, capturedBody.Capabilities)
		}
	}
	// The JSON key must be exactly "capabilities" (the platform reads body.capabilities).
	if !bytes.Contains(rawBody, []byte(`"capabilities"`)) {
		t.Errorf("request body missing \"capabilities\" key: %s", rawBody)
	}
}

// TestRegisterRequest_CapabilitiesOmittedWhenNil verifies omitempty: a daemon
// that leaves Capabilities nil sends no "capabilities" key, so an older platform
// is byte-unaffected and the field degrades to [] server-side.
func TestRegisterRequest_CapabilitiesOmittedWhenNil(t *testing.T) {
	buf, err := json.Marshal(RegisterRequest{Hostname: "h", Capacity: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(buf, []byte(`"capabilities"`)) {
		t.Errorf("nil Capabilities should be omitted; got %s", buf)
	}
}

// TestRegisterRequest_ProjectIDsRoundTrip verifies the A3 Wire contract:
//   - When ProjectIDs is set, the field round-trips through JSON with key "projectIds".
//   - When ProjectIDs is nil, the field is omitted from the JSON body (omitempty),
//     preserving backward-compatibility with older platform versions.
func TestRegisterRequest_ProjectIDsRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		projectIDs []string
		wantKey    bool
	}{
		{
			name:       "populated projectIds round-trips",
			projectIDs: []string{"uuid-proj-1", "uuid-proj-2"},
			wantKey:    true,
		},
		{
			name:       "nil projectIds omits key (omitempty)",
			projectIDs: nil,
			wantKey:    false,
		},
		{
			name:       "empty slice omits key (omitempty)",
			projectIDs: []string{},
			wantKey:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := RegisterRequest{
				Hostname:   "test-host",
				Capacity:   2,
				ProjectIDs: tc.projectIDs,
			}
			data, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, hasKey := decoded["projectIds"]
			if hasKey != tc.wantKey {
				t.Errorf("projectIds key present=%v, want %v; body: %s", hasKey, tc.wantKey, data)
			}
			if !tc.wantKey {
				return
			}
			// Verify the values round-trip correctly.
			var back RegisterRequest
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("unmarshal back: %v", err)
			}
			if len(back.ProjectIDs) != len(tc.projectIDs) {
				t.Fatalf("ProjectIDs len = %d, want %d", len(back.ProjectIDs), len(tc.projectIDs))
			}
			for i, want := range tc.projectIDs {
				if back.ProjectIDs[i] != want {
					t.Errorf("ProjectIDs[%d] = %q, want %q", i, back.ProjectIDs[i], want)
				}
			}
		})
	}
}

func TestRegister_PopulatesProjectIDsIndependentlyOfRepositories(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	var got RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			WorkerID:          "worker-1",
			RuntimeToken:      "runtime-token",
			HeartbeatInterval: 30000,
			PollInterval:      5000,
		})
	}))
	t.Cleanup(srv.Close)

	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:         srv.URL,
		RegistrationToken:       "test-registration-token",
		Hostname:                "host",
		MaxAgents:               2,
		JWTPath:                 filepath.Join(t.TempDir(), "daemon.jwt"),
		ProjectIDs:              []string{"project-b", "project-a", "project-b"},
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		DaemonProjects:          nil,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if want := []string{"project-a", "project-b"}; !reflect.DeepEqual(got.ProjectIDs, want) {
		t.Fatalf("ProjectIDs = %v, want %v", got.ProjectIDs, want)
	}
	if got.DaemonProjects != nil {
		t.Fatalf("DaemonProjects = %v, want nil", got.DaemonProjects)
	}
	if got.ProjectAdmissionVersion != ProjectAdmissionVersionV2 {
		t.Fatalf("ProjectAdmissionVersion = %d, want 2", got.ProjectAdmissionVersion)
	}
}

// TestRegister_NilProvidesOmitsField verifies that a nil Provides slice does
// NOT add a "provides" key to the JSON body (omitempty). Older platform
// versions that don't recognise the field should still accept the request.
func TestRegister_NilProvidesOmitsField(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var rawBody json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		rawBody = buf
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_nil",
			"runtimeToken":      "tok",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic
	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "nil-host",
		Version:           "0.6.0-dev",
		MaxAgents:         2,
		JWTPath:           jwtPath,
		// Provides deliberately omitted / nil.
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, exists := decoded["provides"]; exists {
		t.Errorf("provides key should be absent from body when nil; body: %s", rawBody)
	}
}

// TestRegister_RejectsStubCacheWhenConfiguredReal is the regression test for
// the stub-cache poisoning incident: a `go test`/smoke run wrote a stub entry
// to the shared daemon.jwt; on the next boot a real-config daemon
// short-circuited on it and ran as an unregistered stub that never polled the
// platform. Register must now distrust the mismatched cache and perform a
// fresh real registration instead.
func TestRegister_RejectsStubCacheWhenConfiguredReal(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "")
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "")

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	// Poison the cache exactly as the incident did: a stub entry for a
	// "test-machine" host.
	if err := SaveCachedJWT(jwtPath, buildStubResponse("test-machine"), time.Now()); err != nil {
		t.Fatalf("seed stub cache: %v", err)
	}

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:gosec // synthetic test response
			"workerId":          "wkr_real_after_stub",
			"runtimeToken":      "real.jwt.value",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "mac-studio-local",
		Version:           "0.37.0-dev",
		MaxAgents:         8,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !called {
		t.Fatal("expected real registration endpoint to be hit (stub cache must be rejected)")
	}
	if resp.WorkerID != "wkr_real_after_stub" {
		t.Errorf("WorkerID = %q, want wkr_real_after_stub (returned the stub cache?)", resp.WorkerID)
	}
	if strings.HasPrefix(resp.RuntimeToken, "stub.") {
		t.Errorf("expected a real runtime token, got stub token %q", resp.RuntimeToken)
	}
	// The poisoned cache must have been overwritten with the real entry.
	cached, err := LoadCachedJWT(jwtPath)
	if err != nil || cached == nil {
		t.Fatalf("LoadCachedJWT after re-register: cached=%v err=%v", cached, err)
	}
	if cached.WorkerID != "wkr_real_after_stub" {
		t.Errorf("cache not refreshed: WorkerID = %q", cached.WorkerID)
	}
}

// TestRegister_KeepsRealCacheWhenConfiguredReal confirms the normal fast path
// is preserved: a valid real cache is returned without hitting the network.
func TestRegister_KeepsRealCacheWhenConfiguredReal(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "")
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "")

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	if err := SaveCachedJWT(jwtPath, &RegisterResponse{ //nolint:gosec // synthetic test token
		WorkerID:          "wkr_cached_real",
		RuntimeToken:      "real.cached.jwt",
		HeartbeatInterval: 30000,
		PollInterval:      5000,
	}, time.Now()); err != nil {
		t.Fatalf("seed real cache: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("real endpoint must NOT be hit when a valid real cache exists")
	}))
	t.Cleanup(srv.Close)

	tok := "rsk_live_" + "abc" //nolint:gosec // synthetic
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "mac-studio-local",
		Version:           "0.37.0-dev",
		MaxAgents:         8,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "wkr_cached_real" {
		t.Errorf("WorkerID = %q, want wkr_cached_real (cache hit expected)", resp.WorkerID)
	}
}

// TestRegister_KeepsStubCacheWhenConfiguredStub confirms a stub cache is still
// honoured when the daemon is itself configured for stub mode (no real token),
// so local-dev fast paths keep working.
func TestRegister_KeepsStubCacheWhenConfiguredStub(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "")
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "")

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	if err := SaveCachedJWT(jwtPath, buildStubResponse("test-host"), time.Now()); err != nil {
		t.Fatalf("seed stub cache: %v", err)
	}

	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "https://platform.example.com",
		RegistrationToken: "local-stub-no-token", // not rsk_/rsp_ -> stub mode
		Hostname:          "test-host",
		Version:           "0.37.0-dev",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "worker-test-host-stub" {
		t.Errorf("WorkerID = %q, want cached stub worker-test-host-stub", resp.WorkerID)
	}
}

// TestCachedMatchesMode exercises the cache/config consistency check directly.
func TestCachedMatchesMode(t *testing.T) {
	stub := &CachedJWT{WorkerID: "worker-h-stub", RuntimeToken: "stub.aaa.bbb.sig"} //nolint:gosec // synthetic
	realEntry := &CachedJWT{WorkerID: "wkr_abc", RuntimeToken: "real.jwt.value"}    //nolint:gosec // synthetic
	// Defensive: a stub-suffixed worker id alone (non-stub token) still counts
	// as stub — either signal trips the check.
	stubByIDOnly := &CachedJWT{WorkerID: "worker-h-stub", RuntimeToken: "real.jwt.value"} //nolint:gosec // synthetic

	tests := []struct {
		name    string
		cached  *CachedJWT
		useStub bool
		want    bool
	}{
		{"stub cache, real config -> reject", stub, false, false},
		{"stub cache, stub config -> keep", stub, true, true},
		{"real cache, real config -> keep", realEntry, false, true},
		{"real cache, stub config -> reject", realEntry, true, false},
		{"stub-by-id-only, real config -> reject", stubByIDOnly, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cachedMatchesMode(tt.cached, tt.useStub); got != tt.want {
				t.Errorf("cachedMatchesMode(%+v, useStub=%v) = %v, want %v", tt.cached, tt.useStub, got, tt.want)
			}
		})
	}
}

// TestRegister_RegionAndHostInfoWire verifies the item-8 additions round-trip
// onto the wire with the EXACT JSON key names the platform register route
// parses (register/route.ts HostInfoBody + body.region → worker_hosts columns).
// It captures the raw request body and asserts key presence/nesting rather than
// only the Go struct, since the contract is the JSON shape.
func TestRegister_RegionAndHostInfoWire(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_hi",
			"runtimeToken":      "real.jwt.value",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "hi" //nolint:gosec // synthetic
	_, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		MachineID:         "m",
		Hostname:          "h",
		Version:           "9.9.9",
		MaxAgents:         2,
		Region:            "us-west-2",
		JWTPath:           jwtPath,
		HostInfo: &HostInfo{
			IP:            "10.0.0.5",
			OS:            "linux",
			OSVersion:     "22.04",
			Arch:          "arm64",
			CPUCores:      8,
			CPUModel:      "Test CPU",
			MemTotalMB:    16384,
			DaemonVersion: "9.9.9",
			StartedAt:     "2026-07-01T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Top-level region key.
	if got, _ := raw["region"].(string); got != "us-west-2" {
		t.Errorf("body.region = %v, want us-west-2", raw["region"])
	}

	hi, ok := raw["hostInfo"].(map[string]any)
	if !ok {
		t.Fatalf("body.hostInfo missing or not an object: %v", raw["hostInfo"])
	}
	// Exact platform-parsed key names (register/route.ts:65-76).
	wantStr := map[string]string{
		"ip":            "10.0.0.5",
		"os":            "linux",
		"osVersion":     "22.04",
		"arch":          "arm64",
		"cpuModel":      "Test CPU",
		"daemonVersion": "9.9.9",
		"startedAt":     "2026-07-01T00:00:00Z",
	}
	for k, want := range wantStr {
		if got, _ := hi[k].(string); got != want {
			t.Errorf("hostInfo.%s = %v, want %q", k, hi[k], want)
		}
	}
	// Numbers decode as float64 through map[string]any.
	if got, _ := hi["cpuCores"].(float64); got != 8 {
		t.Errorf("hostInfo.cpuCores = %v, want 8", hi["cpuCores"])
	}
	if got, _ := hi["memTotalMb"].(float64); got != 16384 {
		t.Errorf("hostInfo.memTotalMb = %v, want 16384", hi["memTotalMb"])
	}
}

// TestRegister_OmitsRegionAndHostInfoWhenUnset confirms the fields are
// omitempty: a registration with no region and nil HostInfo must not emit the
// `region` / `hostInfo` keys at all (an old platform simply ignores absent
// keys — back-compat guarantee).
func TestRegister_OmitsRegionAndHostInfoWhenUnset(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_bare",
			"runtimeToken":      "real.jwt.value",
			"heartbeatInterval": 30000,
			"pollInterval":      5000,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsk_live_" + "bare" //nolint:gosec // synthetic
	if _, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "h",
		Version:           "1.0.0",
		MaxAgents:         1,
		JWTPath:           jwtPath,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, present := raw["region"]; present {
		t.Errorf("expected region key absent when unset, got %v", raw["region"])
	}
	if _, present := raw["hostInfo"]; present {
		t.Errorf("expected hostInfo key absent when nil, got %v", raw["hostInfo"])
	}
}
