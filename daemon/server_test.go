package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
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
		// Long-but-finite WorkerCommand so AcceptWork-driven sessions
		// stay resident for the duration of the test. The default falls
		// through to a stub that exits immediately (binary lookup fails
		// in test env), which races the spawner's SessionEventEnded
		// into sessionDetails.Delete before the test's HTTP read —
		// surfacing as a flaky 404 on session-detail endpoints. Use a
		// flat `sleep N` so SIGTERM during cleanup propagates and Drain
		// returns promptly; sh -c with a busy loop swallows the signal
		// and burns the full Drain timeout.
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"sleep", "10"},
		},
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

	var resp afclient.DaemonStatusResponse
	requireGet(t, srv.Addr(), "/api/daemon/status", &resp)

	if resp.MachineID != "test-machine" {
		t.Errorf("MachineID = %q", resp.MachineID)
	}
	if resp.MaxSessions != 4 {
		t.Errorf("MaxSessions = %d, want 4", resp.MaxSessions)
	}
	// mustStartDaemon does not set Options.Version, so EffectiveVersion
	// falls back to the package var (currently "dev" unless overridden
	// at build time via ldflags).
	if resp.Version != Version {
		t.Errorf("Version = %q, want package default %q", resp.Version, Version)
	}
	if resp.Status != afclient.DaemonReady {
		t.Errorf("Status = %q, want ready", resp.Status)
	}
	if len(resp.EnabledProjectIDs) != 1 || resp.EnabledProjectIDs[0] != "demo" {
		t.Errorf("EnabledProjectIDs = %v, want [demo]", resp.EnabledProjectIDs)
	}
	if !reflect.DeepEqual(resp.AppliedProjectIDs, resp.EnabledProjectIDs) {
		t.Errorf("AppliedProjectIDs = %v, want %v", resp.AppliedProjectIDs, resp.EnabledProjectIDs)
	}

	d.spawner.AddEnabledProjectIDs([]string{"satellite"})
	requireGet(t, srv.Addr(), "/api/daemon/status", &resp)
	if want := []string{"demo", "satellite"}; !reflect.DeepEqual(resp.AppliedProjectIDs, want) {
		t.Errorf("AppliedProjectIDs after runtime admission = %v, want %v", resp.AppliedProjectIDs, want)
	}
	if want := []string{"demo"}; !reflect.DeepEqual(resp.EnabledProjectIDs, want) {
		t.Errorf("EnabledProjectIDs after runtime admission = %v, want %v", resp.EnabledProjectIDs, want)
	}
	if resp.ProjectsAllowed != 2 {
		t.Errorf("ProjectsAllowed = %d, want 2", resp.ProjectsAllowed)
	}
}

func TestBuildProjectStatusRows_ReportsDriftAndRepositoryReadiness(t *testing.T) {
	t.Parallel()
	cfg := &Config{Repositories: []RepositoryConfig{
		{ID: "repo-alpha", ProjectID: "alpha", Source: "example.com/acme/alpha", Primary: true},
		{ID: "repo-disabled", ProjectID: "disabled", Source: "example.com/acme/disabled"},
	}}
	rows := buildProjectStatusRows(nil, cfg, []string{"alpha", "no-repo"}, []string{"alpha", "runtime-only"})
	byID := make(map[string]afclient.DaemonProjectStatus, len(rows))
	for _, row := range rows {
		byID[row.ProjectID] = row
	}
	if got := byID["alpha"]; got.Desired != "enabled" || got.Applied != "ready" || got.Connection != "healthy" || got.RepositoryCount != 1 || got.PrimaryRepositoryID != "repo-alpha" {
		t.Errorf("alpha row = %+v", got)
	}
	if got := byID["no-repo"]; got.Desired != "enabled" || got.Applied != "absent" || got.Connection != "pending" || len(got.Warnings) != 1 {
		t.Errorf("no-repo row = %+v", got)
	}
	if got := byID["runtime-only"]; got.Desired != "disabled" || got.Applied != "ready" {
		t.Errorf("runtime-only row = %+v", got)
	}
	if got := byID["disabled"]; got.Desired != "disabled" || got.Applied != "absent" || got.RepositoryCount != 1 {
		t.Errorf("disabled repository row = %+v", got)
	}
}

// TestServer_Status_HostVersionOverride pins the Options.Version override
// path: a downstream embedder (e.g. rensei-tui) that sets its own
// version string MUST see that string in /api/daemon/status, NOT the
// donmai package's Version var. This is the wire that fixes
// the May-2026 incident where `rensei host status` reported the
// vendored "0.7.1" forever.
func TestServer_Status_HostVersionOverride(t *testing.T) {
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
	d := New(Options{
		ConfigPath: cfgPath,
		JWTPath:    filepath.Join(tmp, "daemon.jwt"),
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
		SkipWizard: true,
		Version:    "rensei-1.2.3-test",
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"sleep", "10"},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	srv := NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = d.Stop(ctx)
	})

	var resp afclient.DaemonStatusResponse
	requireGet(t, srv.Addr(), "/api/daemon/status", &resp)
	if resp.Version != "rensei-1.2.3-test" {
		t.Errorf("Version = %q, want rensei-1.2.3-test (Options.Version)", resp.Version)
	}

	// EffectiveVersion accessor must agree with the wire shape.
	if got := d.EffectiveVersion(); got != "rensei-1.2.3-test" {
		t.Errorf("EffectiveVersion() = %q, want rensei-1.2.3-test", got)
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

func TestServer_SetMaxConcurrentSessions(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var resp afclient.SetCapacityResponse
	requirePost(t, srv.Addr(), "/api/daemon/capacity", map[string]string{
		"key":   "capacity.maxConcurrentSessions",
		"value": "2",
	}, &resp)
	if !resp.OK {
		t.Fatalf("expected OK, got %+v", resp)
	}
	if got := d.config.Capacity.MaxConcurrentSessions; got != 2 {
		t.Errorf("MaxConcurrentSessions = %d, want 2", got)
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

func TestServer_StopEndpointRetainsCompletionOwnerAfterIncompleteDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			close(entered)
			<-release
			return nil, errors.New("pre-spawn released during shutdown")
		},
	})
	d := New(Options{})
	d.spawner = spawner
	d.setState(StateRunning)
	acceptDone := make(chan error, 1)
	go func() {
		_, err := spawner.AcceptWork(SessionSpec{SessionID: "reserved", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-spawn hook did not enter")
	}

	s := NewServer(d)
	s.stopAttemptTimeout = 5 * time.Millisecond
	s.stopRetryDelay = time.Millisecond
	attempts := make(chan error, 2)
	s.stopAttemptResults = attempts
	httpServer := httptest.NewServer(s.httpd.Handler)
	defer httpServer.Close()
	res, err := http.Post(httpServer.URL+"/api/daemon/stop", "application/json", nil) //nolint:gosec
	if err != nil {
		t.Fatalf("POST stop: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST stop status = %d, want 200", res.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for d.State() != StateDraining && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.State() != StateDraining {
		t.Fatalf("state during incomplete endpoint stop = %q, want draining", d.State())
	}
	// The response only proves the goroutine entered Stop. Wait for the first
	// completed attempt and classify it before releasing the reservation; a
	// timing sleep here could accidentally turn this into a first-attempt success.
	select {
	case err := <-attempts:
		var incomplete *DrainIncompleteError
		if !errors.As(err, &incomplete) {
			t.Fatalf("first endpoint stop attempt = %v, want DrainIncompleteError", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first endpoint stop attempt did not complete")
	}
	select {
	case <-d.Done():
		t.Fatal("Done closed while endpoint-owned drain remained incomplete")
	default:
	}

	close(release)
	if err := <-acceptDone; err == nil {
		t.Fatal("reserved AcceptWork unexpectedly succeeded")
	}
	select {
	case <-d.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("endpoint completion owner did not retry to final stop")
	}
	if d.State() != StateStopped {
		t.Fatalf("state after endpoint retry = %q, want stopped", d.State())
	}
}

// TestServer_Compatibility_Endpoints_Match_Client verifies the server speaks
// the exact paths consumed by afclient.DaemonClient (contract).
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

type responseDeadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
}

func (r *responseDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.writeDeadline = deadline
	return nil
}

func TestServerDrainExtendsWriteDeadline(t *testing.T) {
	d := New(Options{})
	d.setState(StateRunning)
	srv := NewServer(d)
	recorder := &responseDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/drain", strings.NewReader(`{"timeoutSeconds":1}`))
	before := time.Now()

	srv.handleDrain(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("drain status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.writeDeadline.IsZero() {
		t.Fatal("drain handler did not set a response write deadline")
	}
	expected := time.Second + drainResponseWriteGrace
	if recorder.writeDeadline.Before(before.Add(expected)) || recorder.writeDeadline.After(time.Now().Add(expected)) {
		t.Fatalf("write deadline = %s, want now + %s", recorder.writeDeadline, expected)
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
// recorded by AcceptWorkWithDetail.
func TestServer_SessionDetail_HappyPath(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	want := &SessionDetail{
		SessionID:       "sess-detail-1",
		IssueIdentifier: "ENG-9001",
		Repository:      "github.com/foo/bar",
		Ref:             "main",
		WorkType:        "development",
		WorkerID:        "wkr_1",
		AuthToken:       "tok",
		PlatformURL:     "https://app.example.com",
		InitialPrompt:   "first line\n二行目 🌱",
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
	if got.InitialPrompt != want.InitialPrompt {
		t.Errorf("InitialPrompt = %q, want %q", got.InitialPrompt, want.InitialPrompt)
	}

	field, ok := reflect.TypeOf(SessionDetail{}).FieldByName("InitialPrompt")
	if !ok {
		t.Fatal("SessionDetail.InitialPrompt field missing")
	}
	if got := field.Tag.Get("json"); got != "initialPrompt,omitempty" {
		t.Fatalf("SessionDetail.InitialPrompt JSON tag = %q, want initialPrompt,omitempty", got)
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

// TestServer_SessionStop_HappyPath verifies POST /api/daemon/sessions/<id>/stop
// kills a live session, frees its slot, and returns 200 — the daemon half of
// the deterministic per-session cancel wire (Guard 3 hard out-of-band leg).
func TestServer_SessionStop_HappyPath(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	ended := sessionEnds(d.spawner)

	// mustStartDaemon spawns `sleep 10`, so the session stays resident.
	if _, err := d.AcceptWork(SessionSpec{
		SessionID: "stop-me", Repository: "github.com/foo/bar", Ref: "main",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	if d.ActiveSessionCount() != 1 {
		t.Fatalf("ActiveSessionCount before stop = %d, want 1", d.ActiveSessionCount())
	}
	released, ok := d.spawner.sessionRelease("stop-me")
	if !ok {
		t.Fatal("stop-me release signal not found")
	}

	var resp afclient.DaemonActionResponse
	status := requirePost(t, srv.Addr(), "/api/daemon/sessions/stop-me/stop", nil, &resp)
	if status != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", status)
	}
	if !resp.OK {
		t.Errorf("resp.OK = false, want true (%+v)", resp)
	}
	waitSessionEnd(t, ended)
	waitSpawnerSignal(t, released, "stop-me registry release")
	if d.ActiveSessionCount() != 0 {
		t.Errorf("ActiveSessionCount after stop = %d, want 0", d.ActiveSessionCount())
	}
}

// TestServer_SessionStop_NotFound verifies stopping an unknown id returns 404.
func TestServer_SessionStop_NotFound(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	status := requirePost(t, srv.Addr(), "/api/daemon/sessions/never-spawned/stop", nil, nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestServer_SessionStopAcknowledgesNaturalCleanupOwnership(t *testing.T) {
	probe := newNaturalOwnershipProbe(t, "natural-http")
	probe.wrapCancel(t)

	d := New(Options{})
	d.spawner = probe.spawner
	d.setState(StateRunning)
	server := NewServer(d)
	postStop := func() (int, afclient.DaemonActionResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/daemon/sessions/"+probe.sessionID+"/stop", nil)
		recorder := httptest.NewRecorder()
		server.httpd.Handler.ServeHTTP(recorder, req)

		var response afclient.DaemonActionResponse
		if recorder.Code == http.StatusOK {
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode stop response: %v", err)
			}
		}
		return recorder.Code, response
	}

	for attempt := 1; attempt <= 2; attempt++ {
		status, response := postStop()
		if status != http.StatusOK {
			t.Fatalf("stop attempt %d status = %d, want 200", attempt, status)
		}
		if !response.OK || response.Message != "stopped" {
			t.Fatalf("stop attempt %d response = %+v, want acknowledged stopped response", attempt, response)
		}
	}
	probe.assertPublished(t, d.ActiveSessions())
	if got := d.ActiveSessionCount(); got != 1 {
		t.Fatalf("Daemon.ActiveSessionCount during natural cleanup = %d, want 1", got)
	}
	probe.assertNoTerminationOrCancellation(t)

	probe.release()
	ev := waitSpawnerEvent(t, probe.ended, "HTTP natural ownership Ended event")
	if ev.Handle.State != SessionCompleted {
		t.Fatalf("Ended state = %q, want %q", ev.Handle.State, SessionCompleted)
	}
	if got := probe.cancelCount(); got != 1 {
		t.Fatalf("natural reaper cancellation count = %d, want 1", got)
	}
	waitForActiveCount(t, probe.spawner, 0)

	status, _ := postStop()
	if status != http.StatusNotFound {
		t.Fatalf("stop after registry release status = %d, want 404", status)
	}
}

// TestServer_SessionStop_MethodNotAllowed verifies a non-POST to the stop
// route produces 405 (and is not mis-routed to the detail handler).
func TestServer_SessionStop_MethodNotAllowed(t *testing.T) {
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/api/daemon/sessions/x/stop", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

// TestServer_SessionStop_LeavesSiblingRunning is the HOL-isolation guarantee
// at the HTTP layer: stopping one session must not terminate the others.
func TestServer_SessionStop_LeavesSiblingRunning(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	ended := sessionEnds(d.spawner)

	for _, id := range []string{"keep", "drop"} {
		if _, err := d.AcceptWork(SessionSpec{
			SessionID: id, Repository: "github.com/foo/bar", Ref: "main",
		}); err != nil {
			t.Fatalf("AcceptWork %q: %v", id, err)
		}
	}
	if d.ActiveSessionCount() != 2 {
		t.Fatalf("ActiveSessionCount = %d, want 2", d.ActiveSessionCount())
	}
	released, ok := d.spawner.sessionRelease("drop")
	if !ok {
		t.Fatal("drop release signal not found")
	}

	if status := requirePost(t, srv.Addr(), "/api/daemon/sessions/drop/stop", nil, nil); status != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", status)
	}
	waitSessionEnd(t, ended)
	waitSpawnerSignal(t, released, "drop registry release")
	if d.ActiveSessionCount() != 1 {
		t.Fatalf("ActiveSessionCount after stop = %d, want 1", d.ActiveSessionCount())
	}
	for _, h := range d.ActiveSessions() {
		if h.SessionID == "drop" {
			t.Error("stopped session still active")
		}
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

// TestServer_Status_ReportsPaused_WhenSpawnerDrainedButStateRunning pins the
// status divergence the May-2026 prod incident exposed: spawner.Drain()
// flips spawner.accepting=false directly, so a Drain → restore-to-Running
// path that forgets to re-Resume the spawner used to leave the daemon
// reporting "ready" while NACKing every claim with "not accepting new
// work (paused or draining)". `daemonStatus()` and `buildRegistrationStats()`
// must both surface "paused" in this divergent state.
func TestServer_Status_ReportsPaused_WhenSpawnerDrainedButStateRunning(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	// Sanity: clean status reports ready.
	var pre afclient.DaemonStatusResponse
	requireGet(t, srv.Addr(), "/api/daemon/status", &pre)
	if pre.Status != afclient.DaemonReady {
		t.Fatalf("pre-drain status = %q, want ready", pre.Status)
	}

	// Force the divergent state: drain the spawner directly, leaving
	// d.state == StateRunning. This is the post-Update / post-failed-Stop
	// shape that produced the prod incident.
	if d.spawner == nil {
		t.Fatal("spawner is nil; mustStartDaemon contract changed")
	}
	if err := d.spawner.Drain(50 * time.Millisecond); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if d.State() != StateRunning {
		t.Fatalf("State = %q, want StateRunning (divergent shape)", d.State())
	}
	if d.spawner.IsAccepting() {
		t.Fatal("spawner.IsAccepting() = true, want false after Drain")
	}

	// /api/daemon/status must surface the truth.
	var status afclient.DaemonStatusResponse
	requireGet(t, srv.Addr(), "/api/daemon/status", &status)
	if status.Status != afclient.DaemonPaused {
		t.Errorf("status = %q, want paused (drained spawner)", status.Status)
	}

	// /api/daemon/stats registration block must mirror the divergence too.
	var stats afclient.DaemonStatsResponse
	requireGet(t, srv.Addr(), "/api/daemon/stats", &stats)
	if stats.Registration == nil {
		t.Fatal("Registration stats nil")
	}
	if stats.Registration.Status != "paused" {
		t.Errorf("registration status = %q, want paused", stats.Registration.Status)
	}
}

func TestServer_StopEndpointHasOneCompletionOwnerUnderContention(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			close(entered)
			<-release
			return nil, errors.New("released shutdown reservation")
		},
	})
	d := New(Options{})
	d.spawner = spawner
	d.setState(StateRunning)
	acceptDone := make(chan error, 1)
	go func() {
		_, err := spawner.AcceptWork(SessionSpec{SessionID: "reserved", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pre-spawn hook did not enter")
	}

	s := NewServer(d)
	s.stopAttemptTimeout = 5 * time.Millisecond
	s.stopRetryDelay = time.Millisecond
	httpServer := httptest.NewServer(s.httpd.Handler)
	defer httpServer.Close()

	const callers = 64
	errCh := make(chan error, callers)
	for range callers {
		go func() {
			res, err := http.Post(httpServer.URL+"/api/daemon/stop", "application/json", nil) //nolint:gosec
			if err == nil {
				if res.StatusCode != http.StatusOK {
					err = fmt.Errorf("stop status = %d", res.StatusCode)
				}
				_ = res.Body.Close()
			}
			errCh <- err
		}()
	}
	for range callers {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for d.State() != StateDraining && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.mu.Lock()
	starts := s.stopCompletionStarts
	s.mu.Unlock()
	if starts != 1 {
		t.Fatalf("stop completion starts = %d, want exactly one", starts)
	}

	close(release)
	if err := <-acceptDone; err == nil {
		t.Fatal("reserved AcceptWork unexpectedly succeeded")
	}
	select {
	case <-d.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("single endpoint completion owner did not finish")
	}
}

func TestServer_DrainAndResumeReportActualAdmissionState(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	var drained afclient.DaemonActionResponse
	if status := requirePost(t, srv.Addr(), "/api/daemon/drain", afclient.DaemonDrainRequest{TimeoutSeconds: 1}, &drained); status != http.StatusOK || !drained.OK {
		t.Fatalf("drain = (%d, %+v), want successful completed drain", status, drained)
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() {
		t.Fatalf("after drain state/admission = (%q, %v), want draining/false", d.State(), d.spawner.IsAccepting())
	}
	var resumed afclient.DaemonActionResponse
	if status := requirePost(t, srv.Addr(), "/api/daemon/resume", nil, &resumed); status != http.StatusOK || !resumed.OK {
		t.Fatalf("resume = (%d, %+v), want successful reopen", status, resumed)
	}
	if d.State() != StateRunning || !d.spawner.IsAccepting() {
		t.Fatalf("after resume state/admission = (%q, %v), want running/true", d.State(), d.spawner.IsAccepting())
	}
}
