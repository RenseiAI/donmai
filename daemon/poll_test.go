package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		OnReregister: func(_ context.Context, _ string) (string, string, error) {
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

// TestPollService_404TriggersReregisterWithWorkerNotFoundReason confirms
// that an HTTP 404 "Worker not found" from the poll endpoint triggers
// OnReregister and passes reason="worker-not-found" so the caller can
// skip the JWT-refresh probe and go directly to full re-registration.
// This is the regression test for the Redis-TTL loop documented in the
// bug report: previously, 404 also called OnReregister, but the reason
// was passed as "auth-failure", causing RefreshRuntimeToken to probe the
// refresh endpoint first — which returned a fresh JWT for the SAME
// workerId, sending the daemon into an infinite 404 loop.
func TestPollService_404TriggersReregisterWithWorkerNotFoundReason(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := hits.Add(1)
		if count == 1 {
			http.Error(w, `{"error":"Worker not found"}`, http.StatusNotFound)
			return
		}
		// Subsequent calls succeed (new worker registered).
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
	}))
	t.Cleanup(srv.Close)

	var gotReason string
	var reasonMu sync.Mutex
	done := make(chan struct{})
	var doneOnce sync.Once
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_gone",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "valid-jwt",
		IntervalSeconds: 1,
		OnWork:          func(_ PollWorkItem) error { return nil },
		OnReregister: func(_ context.Context, reason string) (string, string, error) {
			reasonMu.Lock()
			gotReason = reason
			reasonMu.Unlock()
			doneOnce.Do(func() { close(done) })
			return "wkr_new", "new-jwt", nil
		},
	})
	p.Start()
	defer p.Stop()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnReregister never fired within 3s")
	}

	reasonMu.Lock()
	defer reasonMu.Unlock()
	if gotReason != "worker-not-found" {
		t.Errorf("OnReregister reason = %q, want %q (must be worker-not-found so RefreshRuntimeToken skips the JWT-refresh probe)", gotReason, "worker-not-found")
	}
}

// TestPollService_401PassesCorrectReason confirms that an HTTP 401 from the
// poll endpoint passes reason="runtime-token-expired" (or "unauthorized")
// so RefreshRuntimeToken can try the JWT-refresh probe before falling back
// to full re-registration.
func TestPollService_401PassesCorrectReason(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := hits.Add(1)
		if count == 1 {
			http.Error(w, `{"error":"Runtime token expired; re-present registration token to refresh"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{}})
	}))
	t.Cleanup(srv.Close)

	var gotReason string
	var reasonMu sync.Mutex
	done := make(chan struct{})
	var doneOnce sync.Once
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_expired",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "expired-jwt",
		IntervalSeconds: 1,
		OnWork:          func(_ PollWorkItem) error { return nil },
		OnReregister: func(_ context.Context, reason string) (string, string, error) {
			reasonMu.Lock()
			gotReason = reason
			reasonMu.Unlock()
			doneOnce.Do(func() { close(done) })
			return "wkr_expired", "fresh-jwt", nil
		},
	})
	p.Start()
	defer p.Stop()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnReregister never fired within 3s")
	}

	reasonMu.Lock()
	defer reasonMu.Unlock()
	if gotReason != "runtime-token-expired" {
		t.Errorf("OnReregister reason = %q, want %q", gotReason, "runtime-token-expired")
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
			"issueIdentifier": "DEV-1",
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
	t.Setenv("DONMAI_DAEMON_REAL_REGISTRATION", "1")

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
// "fatal: repository 'smoke-alpha' does not exist".
func TestPollItemToSessionDetail_ResolvesProjectNameToRepoURL(t *testing.T) {
	projects := []ProjectConfig{{
		ID:         "smoke-alpha",
		Repository: "https://github.com/RenseiAI/rensei-smokes-alpha",
	}}
	item := PollWorkItem{
		SessionID:   "sess-1",
		ProjectName: "smoke-alpha",
	}

	detail := PollItemToSessionDetail(item, projects, "https://platform.example", "tok", "wkr-1")

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

// TestPollItemToSessionDetail_WorkerCapabilities covers the deterministic-
// landing (FD-3) capability wire: with no option the SessionDetail carries no
// capabilities (mixed-version-safe default); WithWorkerCapabilities advertises
// the daemon's worker capability flags through to the runner.
func TestPollItemToSessionDetail_WorkerCapabilities(t *testing.T) {
	item := PollWorkItem{SessionID: "sess-cap"}

	// Default: no option → Capabilities stays nil (every capability false).
	bare := PollItemToSessionDetail(item, nil, "", "", "")
	if bare.Capabilities != nil {
		t.Errorf("Capabilities = %v; want nil (default)", bare.Capabilities)
	}

	// An empty/nil map is a no-op (still nil — not an empty map).
	empty := PollItemToSessionDetail(item, nil, "", "", "", WithWorkerCapabilities(nil))
	if empty.Capabilities != nil {
		t.Errorf("Capabilities = %v; want nil for empty option", empty.Capabilities)
	}

	// A populated map is advertised through, defensively copied.
	src := map[string]bool{"merge-queue": true}
	withCaps := PollItemToSessionDetail(item, nil, "", "", "", WithWorkerCapabilities(src))
	if got := withCaps.Capabilities["merge-queue"]; !got {
		t.Errorf("Capabilities[merge-queue] = %v; want true", got)
	}
	// Mutating the source after the call must not affect the stored detail.
	src["merge-queue"] = false
	if !withCaps.Capabilities["merge-queue"] {
		t.Error("WithWorkerCapabilities did not defensively copy the map")
	}
}

// boolPtr is a test helper for building *bool option args.
func boolPtr(b bool) *bool { return &b }

// TestWithMergeQueueLanding covers the per-org merge-queue landing option that
// stamps Capabilities["merge-queue"] from the coordinator's per-item flag,
// replacing the org-agnostic worker capability when present:
//
//   - nil ⇒ no-op (an older coordinator that omits the field): the legacy
//     WorkerCapabilities value stands, so absent stays distinguishable from an
//     explicit false.
//   - &true ⇒ merge-queue=true (defer Delivered→Accepted to the finalizer).
//   - &false ⇒ merge-queue=false, OVERRIDING a legacy true (direct transition).
func TestWithMergeQueueLanding(t *testing.T) {
	item := PollWorkItem{SessionID: "sess-mql"}

	// nil flag is a no-op: a SessionDetail built with a legacy merge-queue=true
	// keeps that value (option must not clobber the legacy capability).
	legacy := PollItemToSessionDetail(item, nil, "", "", "",
		WithWorkerCapabilities(map[string]bool{capabilityMergeQueue: true}),
		WithMergeQueueLanding(nil))
	if got := legacy.Capabilities[capabilityMergeQueue]; !got {
		t.Errorf("nil flag clobbered legacy capability: merge-queue = %v, want true (no-op)", got)
	}

	// nil flag with NO legacy capability leaves Capabilities nil entirely.
	bare := PollItemToSessionDetail(item, nil, "", "", "", WithMergeQueueLanding(nil))
	if bare.Capabilities != nil {
		t.Errorf("nil flag created a Capabilities map: %v, want nil", bare.Capabilities)
	}

	// &true sets merge-queue=true even with no prior capability map.
	on := PollItemToSessionDetail(item, nil, "", "", "", WithMergeQueueLanding(boolPtr(true)))
	if got := on.Capabilities[capabilityMergeQueue]; !got {
		t.Errorf("&true: merge-queue = %v, want true", got)
	}

	// &false sets merge-queue=false, OVERRIDING a legacy true (per-org flag wins
	// because the option is appended after WithWorkerCapabilities).
	off := PollItemToSessionDetail(item, nil, "", "", "",
		WithWorkerCapabilities(map[string]bool{capabilityMergeQueue: true}),
		WithMergeQueueLanding(boolPtr(false)))
	if got := off.Capabilities[capabilityMergeQueue]; got {
		t.Errorf("&false: merge-queue = %v, want false (per-org flag overrides legacy true)", got)
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

	detail := PollItemToSessionDetail(item, projects, "https://platform.example", "tok", "wkr-1")

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

	detail := PollItemToSessionDetail(item, projects, "", "", "")

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

	detail := PollItemToSessionDetail(item, projects, "", "", "")

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

	spec := PollItemToSessionSpec(item, projects)

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

// TestPollItemToSessionSpec_ProjectNameFromAllowlist asserts that
// PollItemToSessionSpec populates spec.ProjectName with the matched
// ProjectConfig.ID when an allowlist entry is found, and leaves it
// empty when no entry matches. This is a table-driven extension of the
// existing TestPollItemToSessionSpec_ResolvesProjectName to cover the
// new A1/A2 field semantics.
func TestPollItemToSessionSpec_ProjectNameFromAllowlist(t *testing.T) {
	projects := []ProjectConfig{
		{ID: "smoke-alpha", Repository: "https://github.com/acme/alpha"},
		{ID: "smoke-beta", Repository: "https://github.com/acme/beta"},
	}

	cases := []struct {
		name            string
		item            PollWorkItem
		wantProjectName string
		wantRepository  string
	}{
		{
			name: "matched by project name slug",
			item: PollWorkItem{
				SessionID:   "sess-a",
				ProjectName: "smoke-alpha",
				Ref:         "main",
			},
			wantProjectName: "smoke-alpha",
			wantRepository:  "https://github.com/acme/alpha",
		},
		{
			name: "matched by repository URL",
			item: PollWorkItem{
				SessionID:  "sess-b",
				Repository: "https://github.com/acme/beta",
				Ref:        "main",
			},
			wantProjectName: "smoke-beta",
			wantRepository:  "https://github.com/acme/beta",
		},
		{
			name: "no allowlist match leaves ProjectName empty",
			item: PollWorkItem{
				SessionID:   "sess-c",
				ProjectName: "unknown-project",
				Ref:         "main",
			},
			wantProjectName: "",
			wantRepository:  "unknown-project", // falls through to item.ProjectName
		},
		{
			name: "empty item leaves ProjectName and Repository empty",
			item: PollWorkItem{
				SessionID: "sess-d",
				Ref:       "main",
			},
			wantProjectName: "",
			wantRepository:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := PollItemToSessionSpec(tc.item, projects)
			if spec.ProjectName != tc.wantProjectName {
				t.Errorf("ProjectName = %q, want %q", spec.ProjectName, tc.wantProjectName)
			}
			if spec.Repository != tc.wantRepository {
				t.Errorf("Repository = %q, want %q", spec.Repository, tc.wantRepository)
			}
			if spec.SessionID != tc.item.SessionID {
				t.Errorf("SessionID = %q, want %q", spec.SessionID, tc.item.SessionID)
			}
		})
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

// TestPollItemToSessionDetail_URLMatchSilencesProjectNameMiss covers
// the case the orchestrator's enriched dispatch produces in
// production: item.Repository carries the canonical clone URL (which
// matches an allowlist entry), but item.ProjectName carries the
// human-display name ("Yuisei") rather than the slug ("yuisei").
// The pre-fix implementation warned because it tried projectName
// first, missed (case mismatch), and emitted "no allowlist match"
// even though the URL match was about to succeed. Resolution-by-URL
// must silence the warn.
func TestPollItemToSessionDetail_URLMatchSilencesProjectNameMiss(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	projects := []ProjectConfig{{
		ID:         "yuisei",
		Repository: "https://github.com/supaku/supaku.git",
	}}
	item := PollWorkItem{
		SessionID:   "sess-display-name",
		ProjectName: "Yuisei", // display name — does NOT match slug "yuisei"
		Repository:  "https://github.com/supaku/supaku.git",
	}

	detail := PollItemToSessionDetail(item, projects, "https://platform.example", "tok", "wkr-1")

	if detail.Repository != "https://github.com/supaku/supaku.git" {
		t.Errorf("Repository = %q, want canonical URL", detail.Repository)
	}
	// projectName resolves to the canonical id from the allowlist.
	if detail.ProjectName != "yuisei" {
		t.Errorf("ProjectName = %q, want canonical id 'yuisei'", detail.ProjectName)
	}
	if logs := buf.String(); strings.Contains(logs, "no allowlist match") {
		t.Errorf("URL-match path must NOT emit 'no allowlist match' warn; got logs:\n%s", logs)
	}
}

// TestPollItemToSessionDetail_DisallowedToolsForwarded verifies that
// DisallowedTools stamped by the platform's credential-injection layer
// survives the PollWorkItem → SessionDetail forwarding step.
// Mirrors the v0.9.3 SystemPromptOverride precedent.
func TestPollItemToSessionDetail_DisallowedToolsForwarded(t *testing.T) {
	cases := []struct {
		name            string
		disallowedTools []string
		wantLen         int
	}{
		{"nil — omitted", nil, 0},
		{"empty slice", []string{}, 0},
		{"single pattern", []string{"WebSearch"}, 1},
		{"multiple patterns", []string{"WebSearch", "WebFetch", "Bash(curl:*)"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := PollWorkItem{
				SessionID:       "sess-dt",
				DisallowedTools: tc.disallowedTools,
			}
			detail := PollItemToSessionDetail(item, nil, "", "", "")
			if got := len(detail.DisallowedTools); got != tc.wantLen {
				t.Errorf("DisallowedTools len = %d, want %d; got %v", got, tc.wantLen, detail.DisallowedTools)
			}
			for i, pattern := range tc.disallowedTools {
				if i < len(detail.DisallowedTools) && detail.DisallowedTools[i] != pattern {
					t.Errorf("DisallowedTools[%d] = %q, want %q", i, detail.DisallowedTools[i], pattern)
				}
			}
		})
	}
}

// TestPollResponse_DecodesMemoryBlock proves the Wave 3 dispatch-time
// agent-memory field survives the strict JSON decode of the poll wire
// shape — the silent-drop regression guard (a field on only one
// struct is dropped by Go's decoder). Mirrors TestPollResponse_DecodesLiveWireShape.
func TestPollResponse_DecodesMemoryBlock(t *testing.T) {
	body := []byte(`{
		"work": [{
			"sessionId": "mem-sess-1",
			"workType": "development",
			"memoryBlock": "prefer the existing retry helper in afclient/retry.go"
		}]
	}`)

	var resp PollResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode wire shape: %v", err)
	}
	if len(resp.Work) != 1 {
		t.Fatalf("Work len = %d, want 1", len(resp.Work))
	}
	if got := resp.Work[0].MemoryBlock; got != "prefer the existing retry helper in afclient/retry.go" {
		t.Errorf("MemoryBlock = %q; want the platform-supplied block", got)
	}
}

// TestPollItemToSessionDetail_MemoryBlockForwarded verifies the Wave 3
// dispatch-time agent-memory context survives the PollWorkItem →
// SessionDetail forwarding step. Mirrors the DisallowedTools / v0.9.3
// SystemPromptOverride precedent.
func TestPollItemToSessionDetail_MemoryBlockForwarded(t *testing.T) {
	cases := []struct {
		name        string
		memoryBlock string
	}{
		{"empty — omitted", ""},
		{"non-empty block", "recall: this repo uses gofumpt; never reformat with gofmt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := PollWorkItem{
				SessionID:   "sess-mem",
				MemoryBlock: tc.memoryBlock,
			}
			detail := PollItemToSessionDetail(item, nil, "", "", "")
			if detail.MemoryBlock != tc.memoryBlock {
				t.Errorf("MemoryBlock = %q, want %q", detail.MemoryBlock, tc.memoryBlock)
			}
		})
	}
}

// TestPollResponse_DecodesInitialPrompt proves the optional interactive seed
// survives strict poll-wire decoding without normalization. Explicit empty and
// absent both decode to the zero value; non-empty Unicode/multiline content is
// preserved byte-for-byte.
func TestPollResponse_DecodesInitialPrompt(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "absent", body: `{"work":[{"sessionId":"seed-a"}]}`, want: ""},
		{name: "explicit empty", body: `{"work":[{"sessionId":"seed-e","initialPrompt":""}]}`, want: ""},
		{name: "unicode multiline", body: `{"work":[{"sessionId":"seed-u","initialPrompt":"こんにちは世界 🌱\nsecond line"}]}`, want: "こんにちは世界 🌱\nsecond line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp PollResponse
			if err := json.Unmarshal([]byte(tt.body), &resp); err != nil {
				t.Fatalf("decode initialPrompt wire shape: %v", err)
			}
			if len(resp.Work) != 1 {
				t.Fatalf("Work len = %d, want 1", len(resp.Work))
			}
			if got := resp.Work[0].InitialPrompt; got != tt.want {
				t.Errorf("InitialPrompt = %q, want %q", got, tt.want)
			}
		})
	}

	absentBody, err := json.Marshal(PollWorkItem{SessionID: "seed-absent"})
	if err != nil {
		t.Fatalf("marshal empty PollWorkItem: %v", err)
	}
	if bytes.Contains(absentBody, []byte(`"initialPrompt"`)) {
		t.Fatalf("empty InitialPrompt must stay omitted, got %s", absentBody)
	}
}

// TestPollItemToSessionDetail_InitialPromptForwarded verifies the first opaque
// forwarding hop preserves the exact value and does not trim whitespace.
func TestPollItemToSessionDetail_InitialPromptForwarded(t *testing.T) {
	for _, initialPrompt := range []string{"", "  ", "line one\nline two\n雪"} {
		t.Run(fmt.Sprintf("bytes-%d", len(initialPrompt)), func(t *testing.T) {
			item := PollWorkItem{SessionID: "seed-fwd", InitialPrompt: initialPrompt}
			detail := PollItemToSessionDetail(item, nil, "", "", "")
			if detail.InitialPrompt != initialPrompt {
				t.Errorf("InitialPrompt = %q, want %q", detail.InitialPrompt, initialPrompt)
			}
		})
	}
}

// TestPollItemToSessionSpec_DoesNotWarn confirms that the spec
// builder runs silently — the warn surfaces from the SessionDetail
// builder so the same poll item can't produce two identical warns
// per dispatch.
func TestPollItemToSessionSpec_DoesNotWarn(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	projects := []ProjectConfig{{ID: "alpha", Repository: "https://github.com/x/alpha"}}
	item := PollWorkItem{SessionID: "s", ProjectName: "unmatched"} // misses

	_ = PollItemToSessionSpec(item, projects)

	if logs := buf.String(); strings.Contains(logs, "no allowlist match") {
		t.Errorf("PollItemToSessionSpec must not warn (warn fires from PollItemToSessionDetail); got:\n%s", logs)
	}
}

// TestCallNackEndpoint_PostsExpectedShape pins the daemon's NACK
// wire contract against the orchestrator's nack route validation:
// POST /api/sessions/<id>/nack with `{ workerId, reason, work }`
// where work carries sessionId/issueId/issueIdentifier/priority/
// queuedAt. Catches a future regression that drops the NACK or
// reshapes the body.
func TestCallNackEndpoint_PostsExpectedShape(t *testing.T) {
	var (
		seenURL    string
		seenAuth   string
		seenMethod string
		seenBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"nacked":true,"sessionId":"s1","requeued":true}`))
	}))
	defer srv.Close()

	item := &PollWorkItem{
		SessionID:       "s1",
		IssueID:         "iss-1",
		IssueIdentifier: "OPS-1",
		Priority:        3,
		QueuedAt:        1700000000000,
	}
	err := callNackEndpoint(
		context.Background(),
		nil, // default client
		srv.URL,
		"s1",
		"wkr-1",
		"jwt-token",
		"accept work failed: allowlist mismatch",
		item,
	)
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", seenMethod)
	}
	if seenURL != "/api/sessions/s1/nack" {
		t.Errorf("URL path = %q, want /api/sessions/s1/nack", seenURL)
	}
	if seenAuth != "Bearer jwt-token" {
		t.Errorf("Authorization = %q, want Bearer jwt-token", seenAuth)
	}
	// Validate body shape against the orchestrator's NackRequestBody contract.
	if seenBody["workerId"] != "wkr-1" {
		t.Errorf("workerId = %v, want wkr-1", seenBody["workerId"])
	}
	if seenBody["reason"] == "" {
		t.Errorf("reason missing from body")
	}
	work, ok := seenBody["work"].(map[string]any)
	if !ok {
		t.Fatalf("work field missing or wrong type: %v", seenBody["work"])
	}
	for _, want := range []string{"sessionId", "issueId", "issueIdentifier", "priority", "queuedAt"} {
		if _, ok := work[want]; !ok {
			t.Errorf("work missing required field %q (orchestrator validation will reject)", want)
		}
	}
	if work["sessionId"] != "s1" {
		t.Errorf("work.sessionId = %v, want s1", work["sessionId"])
	}
}

// TestCallNackEndpoint_PropagatesServerError confirms a non-2xx
// response surfaces as an error so the daemon can log the NACK
// failure (without aborting the local rejection that already
// happened).
func TestCallNackEndpoint_PropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"workerId mismatch"}`))
	}))
	defer srv.Close()

	err := callNackEndpoint(
		context.Background(),
		nil,
		srv.URL,
		"s1",
		"wkr-1",
		"jwt",
		"reason",
		&PollWorkItem{SessionID: "s1", IssueID: "i", IssueIdentifier: "OPS-1", Priority: 1, QueuedAt: 1},
	)
	if err == nil {
		t.Fatalf("expected error on HTTP 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error should mention HTTP 400; got: %v", err)
	}
}

// TestCallNackEndpoint_RejectsMissingArgs guards against caller
// mistakes that would silently no-op (and leave a session in stale
// claimed state) instead of surfacing the bug.
func TestCallNackEndpoint_RejectsMissingArgs(t *testing.T) {
	cases := []struct {
		name       string
		sessionID  string
		workerID   string
		work       *PollWorkItem
		wantSubstr string
	}{
		{name: "missing-session", sessionID: "", workerID: "w", work: &PollWorkItem{}, wantSubstr: "session id"},
		{name: "missing-worker", sessionID: "s", workerID: "", work: &PollWorkItem{}, wantSubstr: "worker id"},
		{name: "missing-work", sessionID: "s", workerID: "w", work: nil, wantSubstr: "work item"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := callNackEndpoint(
				context.Background(),
				nil, "http://x", tc.sessionID, tc.workerID, "j", "r", tc.work,
			)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q should mention %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestPollService_RoutesLandingWork verifies that the per-(orgId,repoId)
// landing-trigger lane (`landingWork[]`) is decoded and routed through OnWork
// as a synthesized LandingWorkType item — the gap this change closes (the
// orchestrator emitted landingWork[] but the daemon decoded only work[], so
// triggers were dropped and the per-tenant serializer never started).
//
// Cases:
//   - a body with landingWork[] → OnWork called once per item with
//     WorkType==LandingWorkType and the right OrganizationID/Repository;
//   - absent/empty landingWork → zero extra OnWork calls (inert);
//   - malformed items (missing orgId or repoId) → skipped, no OnWork call.
func TestPollService_RoutesLandingWork(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    []PollWorkItem // expected OnWork items, in order
		wantLen int
	}{
		{
			name: "single landing trigger routed",
			body: `{
				"work": [],
				"landingWork": [{
					"batchJobId": "batch:landing:org_1:foo/bar",
					"workType": "landing-run",
					"contractVersion": 1,
					"orgId": "org_1",
					"repoId": "foo/bar"
				}]
			}`,
			want: []PollWorkItem{
				{WorkType: LandingWorkType, OrganizationID: "org_1", Repository: "foo/bar"},
			},
			wantLen: 1,
		},
		{
			name: "multiple landing triggers routed",
			body: `{
				"landingWork": [
					{"batchJobId": "b1", "workType": "landing-run", "contractVersion": "1", "orgId": "org_a", "repoId": "a/one"},
					{"batchJobId": "b2", "workType": "landing-run", "contractVersion": 1, "orgId": "org_b", "repoId": "b/two"}
				]
			}`,
			want: []PollWorkItem{
				{WorkType: LandingWorkType, OrganizationID: "org_a", Repository: "a/one"},
				{WorkType: LandingWorkType, OrganizationID: "org_b", Repository: "b/two"},
			},
			wantLen: 2,
		},
		{
			name:    "absent landingWork is inert",
			body:    `{"work": []}`,
			want:    nil,
			wantLen: 0,
		},
		{
			name:    "empty landingWork is inert",
			body:    `{"work": [], "landingWork": []}`,
			want:    nil,
			wantLen: 0,
		},
		{
			name: "missing orgId is skipped",
			body: `{
				"landingWork": [{"batchJobId": "b", "workType": "landing-run", "repoId": "foo/bar"}]
			}`,
			want:    nil,
			wantLen: 0,
		},
		{
			name: "missing repoId is skipped",
			body: `{
				"landingWork": [{"batchJobId": "b", "workType": "landing-run", "orgId": "org_1"}]
			}`,
			want:    nil,
			wantLen: 0,
		},
		{
			name: "valid trigger routed even when one sibling is malformed",
			body: `{
				"landingWork": [
					{"batchJobId": "b1", "workType": "landing-run", "orgId": "", "repoId": "x/y"},
					{"batchJobId": "b2", "workType": "landing-run", "orgId": "org_ok", "repoId": "ok/repo"}
				]
			}`,
			want: []PollWorkItem{
				{WorkType: LandingWorkType, OrganizationID: "org_ok", Repository: "ok/repo"},
			},
			wantLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			var got []PollWorkItem
			p := NewPollService(PollOptions{
				WorkerID:        "wkr_landing",
				OrchestratorURL: srv.URL,
				RuntimeJWT:      "rt",
				IntervalSeconds: 1,
				OnWork: func(item PollWorkItem) error {
					got = append(got, item)
					return nil
				},
			})
			// Drive a single synchronous poll so the assertion is deterministic
			// (no ticker timing). pollOnce decodes the body and routes both
			// work[] and landingWork[].
			p.pollOnce(context.Background())

			if len(got) != tc.wantLen {
				t.Fatalf("OnWork called %d times; want %d (items=%+v)", len(got), tc.wantLen, got)
			}
			for i, w := range tc.want {
				if got[i].WorkType != w.WorkType {
					t.Errorf("item[%d].WorkType = %q, want %q", i, got[i].WorkType, w.WorkType)
				}
				if got[i].OrganizationID != w.OrganizationID {
					t.Errorf("item[%d].OrganizationID = %q, want %q", i, got[i].OrganizationID, w.OrganizationID)
				}
				if got[i].Repository != w.Repository {
					t.Errorf("item[%d].Repository = %q, want %q", i, got[i].Repository, w.Repository)
				}
				if got[i].SessionID != "" {
					t.Errorf("item[%d].SessionID = %q, want empty (trigger is not a session)", i, got[i].SessionID)
				}
			}
		})
	}
}

// TestPollResponse_DecodesLandingWork confirms the wire shape decodes the
// fields the daemon uses (orgId, repoId, workType, batchJobId) and that an
// unknown/extra field (contractVersion, as either number or string) is
// harmlessly ignored — PollResponse does not use DisallowUnknownFields.
func TestPollResponse_DecodesLandingWork(t *testing.T) {
	body := []byte(`{
		"work": [],
		"landingWork": [{
			"batchJobId": "batch:landing:org_1:foo/bar",
			"workType": "landing-run",
			"contractVersion": 1,
			"orgId": "org_1",
			"repoId": "foo/bar"
		}]
	}`)

	var resp PollResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode landingWork wire shape: %v", err)
	}
	if len(resp.LandingWork) != 1 {
		t.Fatalf("LandingWork len = %d, want 1", len(resp.LandingWork))
	}
	lw := resp.LandingWork[0]
	if lw.OrgID != "org_1" {
		t.Errorf("OrgID = %q, want org_1", lw.OrgID)
	}
	if lw.RepoID != "foo/bar" {
		t.Errorf("RepoID = %q, want foo/bar", lw.RepoID)
	}
	if lw.WorkType != "landing-run" {
		t.Errorf("WorkType = %q, want landing-run", lw.WorkType)
	}
	if lw.BatchJobID != "batch:landing:org_1:foo/bar" {
		t.Errorf("BatchJobID = %q", lw.BatchJobID)
	}
}

// TestPollResponse_DecodesMergeQueueLanding proves the per-org merge-queue
// landing flag survives the strict poll wire decode into a *bool, so that
// absent (older coordinator, nil) is distinguishable from explicit false. This
// is the silent-drop regression guard — the v0.9.3 SystemPromptOverride
// precedent — for the per-session merge-queue capability.
func TestPollResponse_DecodesMergeQueueLanding(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *bool // nil ⇒ absent
	}{
		{
			name: "true",
			body: `{"work":[{"sessionId":"mql-t","mergeQueueLanding":true}]}`,
			want: boolPtr(true),
		},
		{
			name: "false",
			body: `{"work":[{"sessionId":"mql-f","mergeQueueLanding":false}]}`,
			want: boolPtr(false),
		},
		{
			name: "absent",
			body: `{"work":[{"sessionId":"mql-a"}]}`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp PollResponse
			if err := json.Unmarshal([]byte(tt.body), &resp); err != nil {
				t.Fatalf("decode mergeQueueLanding wire shape: %v", err)
			}
			if len(resp.Work) != 1 {
				t.Fatalf("Work len = %d, want 1", len(resp.Work))
			}
			got := resp.Work[0].MergeQueueLanding
			switch {
			case tt.want == nil:
				if got != nil {
					t.Errorf("MergeQueueLanding = %v, want nil (absent must stay nil)", *got)
				}
			case got == nil:
				t.Errorf("MergeQueueLanding = nil, want %v", *tt.want)
			case *got != *tt.want:
				t.Errorf("MergeQueueLanding = %v, want %v", *got, *tt.want)
			}
		})
	}
}

// TestPollResponse_DecodesWS5Fidelity proves the WS5 agent-card → runner
// fidelity fields (allowedTools, mcpServers, skills) survive the strict
// JSON decode of the poll wire shape — the silent-drop regression guard.
// Mirrors the v0.9.3 SystemPromptOverride precedent. Note the MCP transport
// discriminator on the wire is "type" (NOT "transport") — it reuses the
// agent.MCPServerConfig shape.
func TestPollResponse_DecodesWS5Fidelity(t *testing.T) {
	body := []byte(`{
		"work": [{
			"sessionId": "ws5-sess-1",
			"workType": "development",
			"allowedTools": ["Bash(pnpm:*)", "Edit", "Read"],
			"mcpServers": [
				{"name": "linear", "type": "stdio", "command": "pnpm", "args": ["af-linear"], "env": {"FOO": "bar"}},
				{"name": "remote", "type": "http", "url": "https://example.test/mcp", "headers": {"Authorization": "Bearer x"}}
			],
			"skills": [
				{"id": "spring", "body": "Check @SpringBootTest first.", "disallowedTools": ["Bash(npm publish *)"]}
			]
		}]
	}`)

	var resp PollResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode wire shape: %v", err)
	}
	if len(resp.Work) != 1 {
		t.Fatalf("Work len = %d, want 1", len(resp.Work))
	}
	w := resp.Work[0]
	if len(w.AllowedTools) != 3 || w.AllowedTools[0] != "Bash(pnpm:*)" {
		t.Errorf("AllowedTools = %v, want 3 entries leading with Bash(pnpm:*)", w.AllowedTools)
	}
	if len(w.McpServers) != 2 {
		t.Fatalf("McpServers len = %d, want 2", len(w.McpServers))
	}
	if w.McpServers[0].Name != "linear" || w.McpServers[0].Type != "stdio" ||
		w.McpServers[0].Command != "pnpm" || len(w.McpServers[0].Args) != 1 ||
		w.McpServers[0].Env["FOO"] != "bar" {
		t.Errorf("McpServers[0] stdio shape wrong: %+v", w.McpServers[0])
	}
	if w.McpServers[1].Name != "remote" || w.McpServers[1].Type != "http" ||
		w.McpServers[1].URL != "https://example.test/mcp" ||
		w.McpServers[1].Headers["Authorization"] != "Bearer x" {
		t.Errorf("McpServers[1] http shape wrong: %+v", w.McpServers[1])
	}
	if len(w.Skills) != 1 {
		t.Fatalf("Skills len = %d, want 1", len(w.Skills))
	}
	if w.Skills[0].ID != "spring" || w.Skills[0].Body != "Check @SpringBootTest first." ||
		len(w.Skills[0].DisallowedTools) != 1 || w.Skills[0].DisallowedTools[0] != "Bash(npm publish *)" {
		t.Errorf("Skills[0] shape wrong: %+v", w.Skills[0])
	}
}

// TestPollItemToSessionDetail_WS5FidelityForwarded verifies the WS5
// agent-card fields survive the PollWorkItem → SessionDetail forwarding step.
// Mirrors the DisallowedTools / v0.9.3 SystemPromptOverride precedent.
func TestPollItemToSessionDetail_WS5FidelityForwarded(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		item := PollWorkItem{
			SessionID:    "ws5-fwd",
			AllowedTools: []string{"Edit", "Read"},
			McpServers: []PollMCPServer{
				{Name: "linear", Type: "stdio", Command: "pnpm", Args: []string{"af-linear"}},
			},
			Skills: []PollSkill{
				{ID: "s1", Body: "body", DisallowedTools: []string{"Bash(rm:*)"}},
			},
		}
		detail := PollItemToSessionDetail(item, nil, "", "", "")
		if len(detail.AllowedTools) != 2 {
			t.Errorf("AllowedTools len = %d, want 2", len(detail.AllowedTools))
		}
		if len(detail.McpServers) != 1 || detail.McpServers[0].Name != "linear" {
			t.Errorf("McpServers = %+v, want 1 named linear", detail.McpServers)
		}
		if len(detail.Skills) != 1 || detail.Skills[0].ID != "s1" ||
			len(detail.Skills[0].DisallowedTools) != 1 {
			t.Errorf("Skills = %+v, want 1 with disallow", detail.Skills)
		}
	})
	t.Run("absent — omitted", func(t *testing.T) {
		detail := PollItemToSessionDetail(PollWorkItem{SessionID: "bare"}, nil, "", "", "")
		if detail.AllowedTools != nil || detail.McpServers != nil || detail.Skills != nil {
			t.Errorf("WS5 fields must be nil when absent: allowed=%v mcp=%v skills=%v",
				detail.AllowedTools, detail.McpServers, detail.Skills)
		}
	})
}
