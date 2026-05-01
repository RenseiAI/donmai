package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

// withCapturedSlog redirects slog's default logger to an in-memory buffer
// for the duration of the test, returning the buffer and a restore func.
func withCapturedSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf, func() { slog.SetDefault(prev) }
}

// TestPollItemToSessionDetail_ResolvesProjectNameToRepoURL is the v0.5.2
// regression test: when the platform sends projectName="smoke-alpha"
// (the Linear project slug, with no repository field on the wire — see
// the live Redis payload in TestPollResponse_DecodesLiveWireShape) and
// the daemon's allowlist has a matching entry, SessionDetail.repository
// MUST be the entry's GitHub URL so `git clone` succeeds. Before this
// fix the runner received "smoke-alpha" and failed with
// "fatal: repository 'smoke-alpha' does not exist" (REN-1463 / REN-1464).
func TestPollItemToSessionDetail_ResolvesProjectNameToRepoURL(t *testing.T) {
	projects := []ProjectConfig{{
		ID:         "smoke-alpha",
		Repository: "https://github.com/RenseiAI/rensei-smokes-alpha",
	}}
	item := PollWorkItem{
		SessionID:   "sess-1",
		ProjectName: "smoke-alpha",
	}

	detail := pollItemToSessionDetail(item, projects, "https://platform.example", "tok", "wkr-1")

	if got, want := detail.Repository, "https://github.com/RenseiAI/rensei-smokes-alpha"; got != want {
		t.Errorf("Repository = %q, want %q", got, want)
	}
	if got, want := detail.ProjectName, "smoke-alpha"; got != want {
		t.Errorf("ProjectName = %q, want %q", got, want)
	}
	if detail.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", detail.SessionID)
	}
	if detail.PlatformURL != "https://platform.example" {
		t.Errorf("PlatformURL = %q", detail.PlatformURL)
	}
	if detail.AuthToken != "tok" {
		t.Errorf("AuthToken = %q", detail.AuthToken)
	}
	if detail.WorkerID != "wkr-1" {
		t.Errorf("WorkerID = %q", detail.WorkerID)
	}
}

// TestPollItemToSessionDetail_FallsBackOnNoAllowlistMatch verifies the
// non-match path: the SessionDetail.repository is whatever was on the
// wire, and a Warn log is emitted so operators see the fallback.
func TestPollItemToSessionDetail_FallsBackOnNoAllowlistMatch(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	projects := []ProjectConfig{{
		ID:         "smoke-alpha",
		Repository: "https://github.com/RenseiAI/rensei-smokes-alpha",
	}}
	item := PollWorkItem{
		SessionID:   "sess-2",
		ProjectName: "smoke-charlie", // not in allowlist
	}

	detail := pollItemToSessionDetail(item, projects, "https://platform.example", "tok", "wkr-1")

	if got, want := detail.Repository, "smoke-charlie"; got != want {
		t.Errorf("Repository = %q, want %q (fallback to projectName)", got, want)
	}
	if got, want := detail.ProjectName, "smoke-charlie"; got != want {
		t.Errorf("ProjectName = %q, want %q", got, want)
	}
	logs := buf.String()
	if !strings.Contains(logs, "no allowlist match") {
		t.Errorf("expected Warn log containing 'no allowlist match'; got: %s", logs)
	}
	if !strings.Contains(logs, "smoke-charlie") {
		t.Errorf("expected log to mention the unmatched projectName; got: %s", logs)
	}
}

// TestPollItemToSessionDetail_EmptyProjectName confirms that when no
// project context is on the wire the helper returns an empty
// repository field (no resolve attempted, no log emitted).
func TestPollItemToSessionDetail_EmptyProjectName(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	projects := []ProjectConfig{{
		ID:         "smoke-alpha",
		Repository: "https://github.com/RenseiAI/rensei-smokes-alpha",
	}}
	item := PollWorkItem{SessionID: "sess-3"}

	detail := pollItemToSessionDetail(item, projects, "", "", "")

	if detail.Repository != "" {
		t.Errorf("Repository = %q, want empty", detail.Repository)
	}
	if detail.ProjectName != "" {
		t.Errorf("ProjectName = %q, want empty", detail.ProjectName)
	}
	if got := buf.String(); strings.Contains(got, "no allowlist match") {
		t.Errorf("Warn log should not fire on empty projectName; got: %s", got)
	}
}

// TestPollItemToSessionDetail_RepositoryURLOnWireMatchesAllowlist
// covers the rare case where the platform already sent the canonical
// URL on the wire (forward-compat). The allowlist match still
// succeeds and the canonical URL is preserved.
func TestPollItemToSessionDetail_RepositoryURLOnWireMatchesAllowlist(t *testing.T) {
	projects := []ProjectConfig{{
		ID:         "smoke-alpha",
		Repository: "https://github.com/RenseiAI/rensei-smokes-alpha",
	}}
	item := PollWorkItem{
		SessionID:   "sess-4",
		ProjectName: "smoke-alpha",
		Repository:  "https://github.com/RenseiAI/rensei-smokes-alpha",
	}

	detail := pollItemToSessionDetail(item, projects, "", "", "")

	if got, want := detail.Repository, "https://github.com/RenseiAI/rensei-smokes-alpha"; got != want {
		t.Errorf("Repository = %q, want %q", got, want)
	}
	if got, want := detail.ProjectName, "smoke-alpha"; got != want {
		t.Errorf("ProjectName = %q, want %q", got, want)
	}
}

// TestPollItemToSessionSpec_ResolvesProjectName mirrors the
// SessionDetail test for the SessionSpec path so the spawner sees the
// resolved URL too. (Spec is what the WorkerSpawner.findProjectLocked
// matcher consumes.)
func TestPollItemToSessionSpec_ResolvesProjectName(t *testing.T) {
	projects := []ProjectConfig{{
		ID:         "smoke-alpha",
		Repository: "https://github.com/RenseiAI/rensei-smokes-alpha",
	}}
	item := PollWorkItem{
		SessionID:   "sess-5",
		ProjectName: "smoke-alpha",
		Ref:         "main",
	}

	spec := pollItemToSessionSpec(item, projects)

	if got, want := spec.Repository, "https://github.com/RenseiAI/rensei-smokes-alpha"; got != want {
		t.Errorf("Repository = %q, want %q", got, want)
	}
	if spec.SessionID != "sess-5" {
		t.Errorf("SessionID = %q", spec.SessionID)
	}
	if spec.Ref != "main" {
		t.Errorf("Ref = %q", spec.Ref)
	}
}

// TestResolveProjectFromAllowlist exercises the matcher's four match
// modes (slug, URL, URL-suffix-of-id, URL-suffix-of-repo) directly so
// future regressions in the lookup logic are caught with a small,
// readable failure rather than via the larger pollItemToSession*
// integration tests.
func TestResolveProjectFromAllowlist(t *testing.T) {
	projects := []ProjectConfig{
		{ID: "smoke-alpha", Repository: "https://github.com/RenseiAI/rensei-smokes-alpha"},
		{ID: "smoke-beta", Repository: "git@github.com:RenseiAI/rensei-smokes-beta.git"},
	}

	cases := []struct {
		name   string
		value  string
		wantID string
	}{
		{"match by slug", "smoke-alpha", "smoke-alpha"},
		{"match by URL", "https://github.com/RenseiAI/rensei-smokes-alpha", "smoke-alpha"},
		{"second entry by slug", "smoke-beta", "smoke-beta"},
		{"empty value", "", ""},
		{"unknown slug", "smoke-zeta", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := resolveProjectFromAllowlist(tc.value, projects)
			if tc.wantID == "" {
				if ok {
					t.Errorf("expected no match; got %+v", p)
				}
				return
			}
			if !ok {
				t.Fatalf("expected match for %q", tc.value)
			}
			if p.ID != tc.wantID {
				t.Errorf("matched id = %q, want %q", p.ID, tc.wantID)
			}
		})
	}
}
