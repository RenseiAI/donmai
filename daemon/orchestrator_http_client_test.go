package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingOrchestratorTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	hits map[string]int
}

func (r *recordingOrchestratorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hits[request.Method+" "+request.URL.Path]++
	r.mu.Unlock()
	return r.base.RoundTrip(request)
}

func (r *recordingOrchestratorTransport) count(method, path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[method+" "+path]
}

func TestDaemonOrchestratorHTTPClientThreadsNormalRegistrationRefreshHeartbeatAndPoll(t *testing.T) {
	lockPollTestSlog(t)
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

	const (
		oldWorker = "worker-client-old"
		newWorker = "worker-client-new"
	)
	var registerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case RegisterEndpoint:
			workerID, runtimeToken := oldWorker, "runtime.client.old"
			if registerCalls.Add(1) > 1 {
				workerID, runtimeToken = newWorker, "runtime.client.new"
			}
			_ = json.NewEncoder(w).Encode(RegisterResponse{
				WorkerID: workerID, RuntimeToken: runtimeToken,
				HeartbeatInterval: 1_000, PollInterval: 1_000,
			})
		case "/api/workers/" + oldWorker + "/heartbeat":
			http.Error(w, `{"error":"Runtime token expired"}`, http.StatusUnauthorized)
		case "/api/workers/" + oldWorker + "/refresh-token":
			http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
		case "/api/workers/" + newWorker + "/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
		case "/api/workers/" + oldWorker + "/poll", "/api/workers/" + newWorker + "/poll":
			_ = json.NewEncoder(w).Encode(PollResponse{})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	transport := &recordingOrchestratorTransport{
		base: server.Client().Transport,
		hits: make(map[string]int),
	}
	redirectRefusals := atomic.Int32{}
	client := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			redirectRefusals.Add(1)
			return http.ErrUseLastResponse
		},
	}
	originalTransport, originalTimeout := client.Transport, client.Timeout

	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	config := DefaultConfig()
	config.Machine.ID = "orchestrator-client-test"
	config.Orchestrator.URL = server.URL
	config.Orchestrator.AuthToken = "rsp_live_orchestrator_client"
	if err := WriteConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	daemon := New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"), SkipWizard: true,
		OrchestratorHTTPClient: client,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := daemon.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Stop(context.Background()) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if registerCalls.Load() >= 2 &&
			transport.count(http.MethodPost, "/api/workers/"+oldWorker+"/heartbeat") >= 1 &&
			transport.count(http.MethodPost, "/api/workers/"+oldWorker+"/refresh-token") >= 1 &&
			transport.count(http.MethodPost, "/api/workers/"+newWorker+"/heartbeat") >= 1 &&
			transport.count(http.MethodGet, "/api/workers/"+newWorker+"/poll") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if registerCalls.Load() < 2 {
		t.Fatalf("registration calls = %d, want initial + full re-registration", registerCalls.Load())
	}
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, RegisterEndpoint},
		{http.MethodPost, "/api/workers/" + oldWorker + "/heartbeat"},
		{http.MethodPost, "/api/workers/" + oldWorker + "/refresh-token"},
		{http.MethodPost, "/api/workers/" + newWorker + "/heartbeat"},
		{http.MethodGet, "/api/workers/" + newWorker + "/poll"},
	} {
		if got := transport.count(request.method, request.path); got == 0 {
			t.Errorf("injected client did not observe %s %s", request.method, request.path)
		}
	}
	if daemon.heartbeat == nil || daemon.heartbeat.opts.HTTPClient != client {
		t.Fatal("heartbeat did not retain the exact injected orchestrator client")
	}
	if daemon.poller == nil || daemon.poller.opts.HTTPClient != client {
		t.Fatal("poll did not retain the exact injected orchestrator client")
	}
	if daemon.WorkerID() != newWorker {
		t.Fatalf("daemon worker id after full re-registration = %q, want %q", daemon.WorkerID(), newWorker)
	}
	if client.Transport != originalTransport || client.Timeout != originalTimeout || client.CheckRedirect == nil {
		t.Fatal("daemon cloned or mutated the injected orchestrator client")
	}
	if got := redirectRefusals.Load(); got != 0 {
		t.Fatalf("non-redirecting control server invoked redirect policy %d times", got)
	}
}
