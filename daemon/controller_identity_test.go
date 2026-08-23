package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func comparisonJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestRuntimeTokenJTIIsStrictComparisonOnlyAndMalformedIsAbsent(t *testing.T) {
	t.Parallel()
	valid := comparisonJWT(t, map[string]any{"jti": "correlation-7", "sub": "worker"})
	if got, ok := runtimeTokenJTI(valid); !ok || got != "correlation-7" {
		t.Fatalf("runtimeTokenJTI(valid) = %q/%v", got, ok)
	}
	for name, token := range map[string]string{
		"opaque":        "not-a-jwt",
		"bad base64":    "a.%%%.c",
		"bad json":      "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"jti":`)) + ".c",
		"trailing json": "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"jti":"x"}{}`)) + ".c",
		"missing":       comparisonJWT(t, map[string]any{"sub": "worker"}),
		"non-string":    comparisonJWT(t, map[string]any{"jti": 9}),
		"empty":         comparisonJWT(t, map[string]any{"jti": ""}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := runtimeTokenJTI(token); ok || got != "" {
				t.Fatalf("runtimeTokenJTI(%s) = %q/%v, want absent", name, got, ok)
			}
		})
	}
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{ControllerID: "controller"}})
	for _, token := range []string{"not-a-jwt", comparisonJWT(t, map[string]any{"sub": "worker"})} {
		if err := d.validateControllerCredentials("worker", token); err != nil {
			t.Fatalf("malformed/missing jti changed credential validation semantics: %v", err)
		}
	}
}

func TestDaemonStartRefusesInitialRuntimeJTIControllerAliasBeforePublication(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	const controllerID = "controller-equals-initial-jti"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			WorkerID: "worker-initial", RuntimeToken: comparisonJWT(t, map[string]any{"jti": controllerID}),
			HeartbeatInterval: 3_600_000, PollInterval: 3_600_000,
		})
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	configPath, jwtPath := filepath.Join(dir, "daemon.yaml"), filepath.Join(dir, "daemon.jwt")
	cfg := DefaultConfig()
	cfg.Machine.ID = "initial-jti-test"
	cfg.Orchestrator.URL = server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_initial_jti"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	d := New(Options{
		ConfigPath: configPath, JWTPath: jwtPath, SkipWizard: true,
		SessionShim: SessionShimConfig{ControllerID: controllerID},
	})
	if err := d.Start(context.Background()); !errors.Is(err, ErrControllerIdentityAlias) {
		t.Fatalf("Start = %v, want ErrControllerIdentityAlias", err)
	}
	if workerID, token := d.RuntimeCredentials(); workerID != "" || token != "" {
		t.Fatalf("initial alias became visible in daemon state: worker=%q tokenPresent=%v", workerID, token != "")
	}
	if cached, err := LoadCachedJWT(jwtPath); err != nil || cached != nil {
		t.Fatalf("initial alias reached credential cache: cached=%+v err=%v", cached, err)
	}
}

func TestDaemonRefreshRefusesRuntimeJTIControllerAliasAtomically(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")
	const controllerID = "controller-equals-refreshed-jti"
	initialToken := comparisonJWT(t, map[string]any{"jti": "initial-jti"})
	safeToken := comparisonJWT(t, map[string]any{"jti": "safe-refreshed-jti"})
	aliasToken := comparisonJWT(t, map[string]any{"jti": controllerID})
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == RegisterEndpoint:
			_ = json.NewEncoder(w).Encode(RegisterResponse{
				WorkerID: "worker-refresh", RuntimeToken: initialToken,
				HeartbeatInterval: 3_600_000, PollInterval: 3_600_000,
			})
		case r.URL.Path == "/api/workers/worker-refresh/refresh-token":
			token := aliasToken
			if refreshCalls.Add(1) > 1 {
				token = safeToken
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": token})
		case r.URL.Path == "/api/workers/worker-refresh/heartbeat":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/workers/worker-refresh/poll":
			_ = json.NewEncoder(w).Encode(PollResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	configPath, jwtPath := filepath.Join(dir, "daemon.yaml"), filepath.Join(dir, "daemon.jwt")
	cfg := DefaultConfig()
	cfg.Machine.ID = "refresh-jti-test"
	cfg.Orchestrator.URL = server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_refresh_jti"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	d := New(Options{
		ConfigPath: configPath, JWTPath: jwtPath, SkipWizard: true,
		SessionShim: SessionShimConfig{ControllerID: controllerID},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })

	if _, _, err := d.heartbeat.opts.OnReregister(context.Background(), "runtime-token-expired"); !errors.Is(err, ErrControllerIdentityAlias) {
		t.Fatalf("aliasing refresh = %v, want ErrControllerIdentityAlias", err)
	}
	if workerID, token := d.RuntimeCredentials(); workerID != "worker-refresh" || token != initialToken {
		t.Fatalf("daemon credentials changed after refusal: worker=%q tokenIsInitial=%v", workerID, token == initialToken)
	}
	for name, lane := range map[string]interface{ CurrentCredentials() (string, string) }{
		"heartbeat": d.heartbeat,
		"poll":      d.poller,
	} {
		workerID, token := lane.CurrentCredentials()
		if workerID != "worker-refresh" || token != initialToken {
			t.Errorf("%s lane changed after refusal: worker=%q tokenIsInitial=%v", name, workerID, token == initialToken)
		}
	}
	cached, err := LoadCachedJWT(jwtPath)
	if err != nil || cached == nil || cached.WorkerID != "worker-refresh" || cached.RuntimeToken != initialToken {
		t.Fatalf("cache changed after refusal: cached=%+v err=%v", cached, err)
	}

	workerID, token, err := d.heartbeat.opts.OnReregister(context.Background(), "runtime-token-expired")
	if err != nil || workerID != "worker-refresh" || token != safeToken {
		t.Fatalf("safe retry after refusal = worker=%q tokenIsSafe=%v err=%v", workerID, token == safeToken, err)
	}
	if refreshCalls.Load() != 2 {
		t.Fatalf("refresh calls = %d, want 2; refused alias must not be remembered/coalesced", refreshCalls.Load())
	}
}
