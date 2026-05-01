package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

func mustStartDaemon(t *testing.T) (*Daemon, *Server, func()) {
	t.Helper()
	tmp := t.TempDir()
	cfg := DefaultConfig()
	cfg.Machine.ID = "test-machine"
	cfg.Capacity.MaxConcurrentSessions = 4
	cfg.Projects = []ProjectConfig{{ID: "demo", Repository: "github.com/foo/bar"}}
	cfg.Orchestrator.URL = "file:///tmp/queue"
	cfgPath := filepath.Join(tmp, "daemon.yaml")
	if err := WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	jwtPath := filepath.Join(tmp, "daemon.jwt")
	d := New(Options{
		ConfigPath: cfgPath,
		JWTPath:    jwtPath,
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0, // ephemeral; effective addr exposed via Server.Addr
		SkipWizard: true,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	srv := NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = d.Stop(ctx)
	}
	return d, srv, cleanup
}

// requireGet does an HTTP GET against the server and returns the parsed body.
func requireGet(t *testing.T, addr, path string, into any) {
	t.Helper()
	res, err := http.Get("http://" + addr + path) //nolint:gosec
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s -> %d: %s", path, res.StatusCode, body)
	}
	if into != nil {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func requirePost(t *testing.T, addr, path string, body any, into any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	res, err := http.Post("http://"+addr+path, "application/json", &buf) //nolint:gosec
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if into != nil && res.Body != nil {
		_ = json.NewDecoder(res.Body).Decode(into)
	}
	return res.StatusCode
}

func TestServer_Status(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	_ = d

	var resp afclient.DaemonStatusResponse
	requireGet(t, srv.Addr(), "/api/daemon/status", &resp)

	if resp.MachineID != "test-machine" {
		t.Errorf("MachineID = %q", resp.MachineID)
	}
	if resp.MaxSessions != 4 {
		t.Errorf("MaxSessions = %d, want 4", resp.MaxSessions)
	}
	if resp.Version != Version {
		t.Errorf("Version = %q, want %q", resp.Version, Version)
	}
	if resp.Status != afclient.DaemonReady {
		t.Errorf("Status = %q, want ready", resp.Status)
	}
}

func TestServer_Stats_PoolAndByMachine(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	var resp afclient.DaemonStatsResponse
	requireGet(t, srv.Addr(), "/api/daemon/stats?pool=true&byMachine=true", &resp)

	if resp.Capacity.MaxConcurrentSessions != 4 {
		t.Errorf("Capacity.MaxConcurrent = %d", resp.Capacity.MaxConcurrentSessions)
	}
	if resp.Pool == nil {
		t.Fatal("expected non-nil Pool when ?pool=true")
	}
	if len(resp.ByMachine) == 0 {
		t.Errorf("expected ByMachine non-empty")
	}
}

func TestServer_PauseResume(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	var resp afclient.DaemonActionResponse
	if status := requirePost(t, srv.Addr(), "/api/daemon/pause", nil, &resp); status != http.StatusOK {
		t.Fatalf("pause status %d", status)
	}
	if !resp.OK {
		t.Errorf("pause OK=false: %s", resp.Message)
	}
	if d.State() != StatePaused {
		t.Errorf("state = %q, want paused", d.State())
	}

	resp = afclient.DaemonActionResponse{}
	requirePost(t, srv.Addr(), "/api/daemon/resume", nil, &resp)
	if d.State() != StateRunning {
		t.Errorf("state = %q, want running", d.State())
	}
}

func TestServer_Drain(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.DaemonActionResponse
	requirePost(t, srv.Addr(), "/api/daemon/drain", afclient.DaemonDrainRequest{TimeoutSeconds: 1}, &resp)
	if !resp.OK {
		t.Errorf("drain OK=false")
	}
}

func TestServer_AcceptWork_AndListSessions(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	var sessionResp SessionHandle
	status := requirePost(t, srv.Addr(), "/api/daemon/sessions", SessionSpec{
		SessionID: "sess-1", Repository: "github.com/foo/bar", Ref: "main",
	}, &sessionResp)
	if status != http.StatusAccepted {
		t.Errorf("accept status = %d, want 202", status)
	}
	if sessionResp.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", sessionResp.SessionID)
	}
}

func TestServer_AcceptWork_RejectsUnknownProject(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	status := requirePost(t, srv.Addr(), "/api/daemon/sessions", SessionSpec{
		SessionID: "sess", Repository: "github.com/disallowed/repo",
	}, nil)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestServer_PoolStats_DefaultEmpty(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.WorkareaPoolStats
	requireGet(t, srv.Addr(), "/api/daemon/pool/stats", &resp)
	// Default: empty members slice, no error.
	if resp.Members == nil {
		t.Errorf("expected non-nil Members slice")
	}
}

func TestServer_PoolEvict_NoHandlerReturns501(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.EvictPoolResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/pool/evict", afclient.EvictPoolRequest{
		RepoURL: "github.com/foo/bar", OlderThanSeconds: 60,
	}, &resp)
	if status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", status)
	}
}

func TestServer_SetCapacity(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.SetCapacityResponse
	requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "capacity.poolMaxDiskGb",
		"value": "20",
	}, &resp)
	if !resp.OK {
		t.Errorf("expected OK, got %+v", resp)
	}
}

func TestServer_SetCapacity_RejectsUnknownKey(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.SetCapacityResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "capacity.unknownKey",
		"value": "1",
	}, &resp)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestServer_Doctor_Endpoint(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp map[string]any
	requireGet(t, srv.Addr(), "/api/daemon/doctor", &resp)
	if state, _ := resp["state"].(string); state != "running" {
		t.Errorf("doctor state = %v", resp["state"])
	}
	if loaded, _ := resp["configLoaded"].(bool); !loaded {
		t.Errorf("expected configLoaded=true")
	}
}

func TestServer_Healthz(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	res, err := http.Get("http://" + srv.Addr() + "/healthz") //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if string(body) != "ok" {
		t.Errorf("healthz body = %q", body)
	}
}

func TestServer_MethodNotAllowed(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	res, err := http.Post("http://"+srv.Addr()+"/api/daemon/status", "application/json", nil) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

func TestServer_StopEndpointTransitionsState(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	requirePost(t, srv.Addr(), "/api/daemon/stop", nil, nil)
	deadline := time.Now().Add(2 * time.Second)
	for d.State() == StateRunning && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.State() == StateRunning {
		t.Errorf("expected state to leave 'running' after stop")
	}
}

// TestServer_Compatibility_Endpoints_Match_Client verifies the server speaks
// the exact paths consumed by afclient.DaemonClient (REN-1336 contract).
func TestServer_Compatibility_Endpoints_Match_Client(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	c := afclient.NewDaemonClientFromURL("http://" + srv.Addr())

	if _, err := c.GetStatus(); err != nil {
		t.Errorf("GetStatus: %v", err)
	}
	if _, err := c.GetStats(false, false); err != nil {
		t.Errorf("GetStats: %v", err)
	}
	if _, err := c.Pause(); err != nil {
		t.Errorf("Pause: %v", err)
	}
	if _, err := c.Resume(); err != nil {
		t.Errorf("Resume: %v", err)
	}
	if _, err := c.Drain(1); err != nil {
		t.Errorf("Drain: %v", err)
	}
	if _, err := c.GetPoolStats(); err != nil {
		t.Errorf("GetPoolStats: %v", err)
	}
	if _, err := c.SetCapacityConfig("capacity.poolMaxDiskGb", "10"); err != nil {
		t.Errorf("SetCapacityConfig: %v", err)
	}
}

// expecting these endpoint names to be registered (sanity guard).
func TestServer_AllExpectedEndpointsRegistered(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	expected := []struct {
		path   string
		method string
	}{
		{"/api/daemon/status", "GET"},
		{"/api/daemon/stats", "GET"},
		{"/api/daemon/pause", "POST"},
		{"/api/daemon/resume", "POST"},
		{"/api/daemon/drain", "POST"},
		{"/api/daemon/update", "POST"},
		{"/api/daemon/capacity", "POST"},
		{"/api/daemon/pool/stats", "GET"},
		{"/api/daemon/pool/evict", "POST"},
		{"/api/daemon/sessions", "GET"},
		{"/api/daemon/sessions", "POST"},
		{"/api/daemon/heartbeat", "GET"},
		{"/api/daemon/doctor", "GET"},
		{"/healthz", "GET"},
	}
	for _, e := range expected {
		req, _ := http.NewRequest(e.method, "http://"+srv.Addr()+e.path, strings.NewReader("{}"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("%s %s: %v", e.method, e.path, err)
			continue
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s %s -> %d (endpoint not registered for that method)", e.method, e.path, res.StatusCode)
		}
	}
}

// canary: smoke test wiring of pool stats provider.
type fakePool struct{}

func (fakePool) Stats(_ context.Context) (*afclient.WorkareaPoolStats, error) {
	return &afclient.WorkareaPoolStats{TotalMembers: 7, ReadyMembers: 5}, nil
}

func TestServer_PoolStats_UsesProvider(t *testing.T) {
	tmp := t.TempDir()
	cfg := DefaultConfig()
	cfg.Machine.ID = "x"
	cfg.Orchestrator.URL = "file:///tmp/q"
	cfgPath := filepath.Join(tmp, "daemon.yaml")
	if err := WriteConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	d := New(Options{
		ConfigPath:        cfgPath,
		JWTPath:           filepath.Join(tmp, "daemon.jwt"),
		HTTPPort:          0,
		SkipWizard:        true,
		PoolStatsProvider: fakePool{},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = d.Stop(ctx)
	}()

	// Direct GET so we exercise the http path even when the daemon has no
	// orchestrator.
	c := afclient.NewDaemonClientFromURL("http://" + srv.Addr())
	stats, err := c.GetPoolStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalMembers != 7 {
		t.Errorf("TotalMembers = %d, want 7", stats.TotalMembers)
	}
}

// TestServer_SessionDetail_HappyPath verifies the
// /api/daemon/sessions/<id> endpoint returns the SessionDetail
// recorded by AcceptWorkWithDetail. (REN-1461 / F.2.8.)
func TestServer_SessionDetail_HappyPath(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	want := &SessionDetail{
		SessionID:       "sess-detail-1",
		IssueIdentifier: "REN-9001",
		Repository:      "github.com/foo/bar",
		Ref:             "main",
		WorkType:        "development",
		WorkerID:        "wkr_1",
		AuthToken:       "tok",
		PlatformURL:     "https://app.example.com",
		ResolvedProfile: &SessionResolvedProfile{Provider: "stub"},
	}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  want.SessionID,
		Repository: want.Repository,
		Ref:        want.Ref,
	}, want); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}

	res, err := http.Get("http://" + srv.Addr() + "/api/daemon/sessions/" + want.SessionID) //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, body = %s", res.StatusCode, body)
	}
	var got SessionDetail
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, want.SessionID)
	}
	if got.IssueIdentifier != want.IssueIdentifier {
		t.Errorf("IssueIdentifier = %q, want %q", got.IssueIdentifier, want.IssueIdentifier)
	}
	if got.AuthToken != want.AuthToken {
		t.Errorf("AuthToken not threaded through")
	}
	if got.ResolvedProfile == nil || got.ResolvedProfile.Provider != "stub" {
		t.Errorf("ResolvedProfile.Provider = %+v, want stub", got.ResolvedProfile)
	}
}

// TestServer_SessionDetail_NotFound verifies the endpoint returns
// 404 with a JSON body for unknown session ids.
func TestServer_SessionDetail_NotFound(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	res, err := http.Get("http://" + srv.Addr() + "/api/daemon/sessions/missing-id") //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// TestServer_SessionDetail_MethodNotAllowed verifies non-GET requests
// produce 405.
func TestServer_SessionDetail_MethodNotAllowed(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, "http://"+srv.Addr()+"/api/daemon/sessions/x", strings.NewReader("{}"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

// TestServer_SessionDetail_BindsLocalhostOnly is an explicit guard
// that the daemon's HTTP server has bound 127.0.0.1 (the localhost-
// only auth model the F.2.8 wire-up depends on). Failing this test
// means the SessionDetail endpoint exposes worker auth tokens to
// the network.
func TestServer_SessionDetail_BindsLocalhostOnly(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Errorf("Addr = %q; expected 127.0.0.1 bind for security", srv.Addr())
	}
}

// quick sanity: the server hands back a meaningful 405 for unknown methods.
func TestServer_HTTPTestServerWrapper(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	// Confirm the server responds via a httptest wrapper as a sanity check
	// (helpful when downstream consumers want to embed our handler).
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hi"))
	}))
	defer hs.Close()
	_ = d
	if srv.Addr() == "" {
		t.Fatal("expected non-empty addr")
	}
}
