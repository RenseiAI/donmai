package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		Version:           "0.1.0",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "worker-test-host-stub" {
		t.Errorf("WorkerID = %q, want worker-test-host-stub", resp.WorkerID)
	}
	if !strings.HasPrefix(resp.RuntimeJWT, "stub.") {
		t.Errorf("expected stub JWT prefix, got %q", resp.RuntimeJWT)
	}
	if resp.HeartbeatIntervalSeconds == 0 {
		t.Error("expected non-zero heartbeat interval")
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
		Version:           "0.1.0",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.HasPrefix(resp.RuntimeJWT, "stub.") {
		t.Errorf("expected stub path for file:// URL, got %q", resp.RuntimeJWT)
	}
}

func TestRegister_RealEndpoint(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RegisterEndpoint {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			WorkerID:                 "live-worker-1",
			RuntimeJWT:               "real.jwt.value",
			HeartbeatIntervalSeconds: 15,
			PollIntervalSeconds:      5,
		})
	}))
	t.Cleanup(srv.Close)

	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	tok := "rsp_live_" + "xyz" //nolint:gosec // synthetic test token, not a real credential
	resp, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   srv.URL,
		RegistrationToken: tok,
		Hostname:          "test-host",
		Version:           "0.1.0",
		MaxAgents:         4,
		JWTPath:           jwtPath,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.WorkerID != "live-worker-1" {
		t.Errorf("WorkerID = %q, want live-worker-1", resp.WorkerID)
	}
	if resp.HeartbeatIntervalSeconds != 15 {
		t.Errorf("heartbeat = %d, want 15", resp.HeartbeatIntervalSeconds)
	}
}

func TestRegister_CachedJWTReturned(t *testing.T) {
	jwtPath := filepath.Join(t.TempDir(), "daemon.jwt")
	first, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "file:///tmp/q",
		RegistrationToken: "x",
		Hostname:          "host1",
		Version:           "0.1.0",
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
		Version:           "0.1.0",
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
		Hostname: "host-A", Version: "0.1.0", JWTPath: jwtPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL: "file:///tmp/q", RegistrationToken: "x",
		Hostname: "host-B", Version: "0.1.0", JWTPath: jwtPath,
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
		WorkerID: "wid", RuntimeJWT: "jwt", HeartbeatIntervalSeconds: 30, PollIntervalSeconds: 10,
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
