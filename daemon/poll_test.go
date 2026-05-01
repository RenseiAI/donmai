package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPollService_DispatchesWork covers the happy path: poll endpoint returns
// a single work item and the OnWork handler is invoked once with the matching
// session id.
func TestPollService_DispatchesWork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/workers/wkr_test/poll") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer rt-jwt" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		// Only return work on the first call so the test is deterministic.
		if hits.Add(1) > 1 {
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{{
			SessionID:  "sess-1",
			Repository: "github.com/foo/bar",
			Ref:        "main",
		}}})
	}))
	t.Cleanup(srv.Close)

	var dispatched []PollWorkItem
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)

	p := NewPollService(PollOptions{
		WorkerID:        "wkr_test",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork: func(item PollWorkItem) error {
			mu.Lock()
			defer mu.Unlock()
			if len(dispatched) == 0 {
				wg.Done()
			}
			dispatched = append(dispatched, item)
			return nil
		},
	})
	p.Start()
	defer p.Stop()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnWork never invoked within 3s")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dispatched) == 0 {
		t.Fatal("expected at least one dispatch")
	}
	if dispatched[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", dispatched[0].SessionID)
	}
	if dispatched[0].Repository != "github.com/foo/bar" {
		t.Errorf("Repository = %q", dispatched[0].Repository)
	}
}

// TestPollService_EmptyWorkNoDispatch confirms that when work[] is empty,
// OnWork is not invoked at all.
func TestPollService_EmptyWorkNoDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
	}))
	t.Cleanup(srv.Close)

	var calls atomic.Int32
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_empty",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt",
		IntervalSeconds: 1,
		OnWork: func(_ PollWorkItem) error {
			calls.Add(1)
			return nil
		},
	})
	p.Start()
	time.Sleep(1500 * time.Millisecond) // let two ticks happen
	p.Stop()

	if got := calls.Load(); got != 0 {
		t.Errorf("OnWork called %d times for empty work[]; want 0", got)
	}
}

// TestPollService_401TriggersReregister confirms that an HTTP 401 from the
// poll endpoint triggers OnReregister and the loop continues with the fresh
// credentials returned.
func TestPollService_401TriggersReregister(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := hits.Add(1)
		if count == 1 {
			http.Error(w, `{"error":"runtime jwt expired"}`, http.StatusUnauthorized)
			return
		}
		// Subsequent calls should carry the fresh JWT.
		if r.Header.Get("Authorization") != "Bearer fresh-jwt" {
			http.Error(w, "wrong auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
	}))
	t.Cleanup(srv.Close)

	var reregistered atomic.Int32
	done := make(chan struct{})
	var doneOnce sync.Once
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_test",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "stale-jwt",
		IntervalSeconds: 1,
		OnWork:          func(_ PollWorkItem) error { return nil },
		OnReregister: func(_ context.Context) (string, string, error) {
			reregistered.Add(1)
			doneOnce.Do(func() { close(done) })
			return "wkr_test", "fresh-jwt", nil
		},
	})
	p.Start()
	defer p.Stop()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnReregister never fired within 3s")
	}

	if got := reregistered.Load(); got < 1 {
		t.Errorf("OnReregister called %d times; want >= 1", got)
	}
}

// TestPollResponse_DecodesLiveWireShape is the regression for the v0.4.1
// poll-decode bug: the platform's QueuedWork serialises `queuedAt` as a
// Unix-millisecond NUMBER, not a string. Before the fix this exact body
// failed with:
//
//	json: cannot unmarshal number into Go struct field
//	PollWorkItem.work.queuedAt of type string
//
// The body below mirrors the live payload pulled from the prod Redis key
// `agent:session:0b5e88d9-32d0-4aca-9f8c-caf82f2b399c` (smoke-alpha,
// workflow wf_cd531d2bc7b3, daemon wkr_4db299d9483948cf), trimmed to the
// platform's QueuedWork wire shape (work-queue.ts -> QueuedWork interface).
// Unknown fields (issueId, issueIdentifier, organizationId, etc.) must be
// silently ignored by the decoder.
func TestPollResponse_DecodesLiveWireShape(t *testing.T) {
	body := []byte(`{
		"work": [{
			"sessionId": "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
			"issueId": "08f26531-f5d2-49dc-b412-b42cef0cbffa",
			"issueIdentifier": "REN2-1",
			"priority": 4,
			"queuedAt": 1777658441780,
			"workType": "research",
			"projectName": "smoke-alpha",
			"providerSessionId": "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c"
		}],
		"hasInboxMessages": false,
		"preClaimed": true,
		"claimedSessionIds": ["0b5e88d9-32d0-4aca-9f8c-caf82f2b399c"]
	}`)

	var resp PollResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode live wire shape: %v", err)
	}
	if len(resp.Work) != 1 {
		t.Fatalf("Work len = %d, want 1", len(resp.Work))
	}
	got := resp.Work[0]
	if got.SessionID != "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.QueuedAt != 1777658441780 {
		t.Errorf("QueuedAt = %d, want 1777658441780", got.QueuedAt)
	}
	if got.Priority != 4 {
		t.Errorf("Priority = %d, want 4", got.Priority)
	}
	if got.ProjectName != "smoke-alpha" {
		t.Errorf("ProjectName = %q", got.ProjectName)
	}
	if !resp.PreClaimed {
		t.Error("PreClaimed = false, want true")
	}
	if len(resp.ClaimedSessionIDs) != 1 {
		t.Fatalf("ClaimedSessionIDs len = %d, want 1", len(resp.ClaimedSessionIDs))
	}
}

// TestPollService_DaemonIntegration covers the end-to-end wiring through
// daemon.Start: a poll-loop tick that returns a work item lands in the
// spawner's AcceptWork path. Uses a stub spawner command so the spawned
// "session" exits immediately.
func TestPollService_DaemonIntegration(t *testing.T) {
	t.Setenv("RENSEI_DAEMON_REAL_REGISTRATION", "1")

	var (
		hits        atomic.Int32
		registerHit atomic.Int32
	)
	mux := http.NewServeMux()
	//nolint:gosec // synthetic test response
	mux.HandleFunc("/api/workers/register", func(w http.ResponseWriter, _ *http.Request) {
		registerHit.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workerId":          "wkr_int",
			"runtimeToken":      "rt.fake.jwt", // non-stub prefix → poll loop starts
			"heartbeatInterval": 30000,
			"pollInterval":      1000,
		})
	})
	mux.HandleFunc("/api/workers/wkr_int/poll", func(w http.ResponseWriter, _ *http.Request) {
		count := hits.Add(1)
		if count == 1 {
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{{
				SessionID:  "int-sess-1",
				Repository: "github.com/foo/bar",
				Ref:        "main",
			}}})
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
	})
	mux.HandleFunc("/api/workers/wkr_int/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	jwtPath := filepath.Join(dir, "daemon.jwt")
	cfg := DefaultConfig()
	cfg.Machine.ID = "test-int"
	cfg.Orchestrator.URL = srv.URL
	cfg.Orchestrator.AuthToken = "rsk_live_xxx"
	cfg.Projects = []ProjectConfig{{
		ID:         "p1",
		Repository: "github.com/foo/bar",
	}}
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	d := New(Options{
		ConfigPath: configPath,
		JWTPath:    jwtPath,
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0, // unused — we don't start the server
		SkipWizard: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })

	// Wait for the poll loop to dispatch the work item and the spawner to
	// transition through started → ended (the stub /bin/sh worker exits
	// immediately).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hits.Load() < 1 {
		t.Fatal("poll endpoint never hit")
	}
	if registerHit.Load() < 1 {
		t.Errorf("register endpoint never hit; got %d", registerHit.Load())
	}
}
