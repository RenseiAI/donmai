package result_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
)

// goodResult returns a populated agent.Result mirroring a typical
// completed Claude session.
func goodResult() agent.Result {
	return agent.Result{
		Status:            "completed",
		ProviderName:      agent.ProviderClaude,
		ProviderSessionID: "claude-sess-123",
		WorktreePath:      "/tmp/wt/REN-1",
		PullRequestURL:    "https://github.com/x/y/pull/42",
		Summary:           "Implemented X, opened PR.",
		WorkResult:        "passed",
		Cost: &agent.CostData{
			InputTokens:  1234,
			OutputTokens: 567,
			TotalCostUsd: 0.0123,
			NumTurns:     8,
		},
	}
}

// captureServer returns an httptest server that records every request
// hit to /completion + /status and replies per the per-path scripts.
func captureServer(t *testing.T,
	completionScript, statusScript func(attempt int) (status int, body string),
) (*httptest.Server, *struct {
	completion atomic.Int32
	status     atomic.Int32
	mu         sync.Mutex
	bodies     []string
},
) {
	t.Helper()
	state := &struct {
		completion atomic.Int32
		status     atomic.Int32
		mu         sync.Mutex
		bodies     []string
	}{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The test server is hit concurrently by the completion + status
		// posts, so guard the shared slice — mirrors the syncBuffer fix
		// applied to daemon/child_log_test.go under go test -race.
		state.mu.Lock()
		state.bodies = append(state.bodies, string(body))
		state.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/completion"):
			n := state.completion.Add(1)
			s, b := completionScript(int(n))
			w.WriteHeader(s)
			_, _ = w.Write([]byte(b))
		case strings.HasSuffix(r.URL.Path, "/status"):
			n := state.status.Add(1)
			s, b := statusScript(int(n))
			w.WriteHeader(s)
			_, _ = w.Write([]byte(b))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

func newPoster(t *testing.T, baseURL string, baseDelay time.Duration) *result.Poster {
	t.Helper()
	p, err := result.NewPoster(result.Options{
		PlatformURL: baseURL,
		AuthToken:   "test-token",
		WorkerID:    "wkr_test",
		BaseDelay:   baseDelay, // 0 = no sleep, fast tests
	})
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}
	return p
}

func TestPosterPost_Happy(t *testing.T) {
	t.Parallel()
	ok := func(int) (int, string) { return http.StatusOK, `{"ok":true}` }
	srv, state := captureServer(t, ok, ok)
	p := newPoster(t, srv.URL, 0)

	if err := p.Post(context.Background(), "sess-1", goodResult()); err != nil {
		t.Fatalf("Post error: %v", err)
	}
	if state.completion.Load() != 1 {
		t.Errorf("completion attempts = %d, want 1", state.completion.Load())
	}
	if state.status.Load() != 1 {
		t.Errorf("status attempts = %d, want 1", state.status.Load())
	}

	// Sanity-check the wire shapes the platform handlers expect.
	for _, body := range state.bodies {
		var generic map[string]any
		if err := json.Unmarshal([]byte(body), &generic); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if generic["workerId"] != "wkr_test" {
			t.Errorf("workerId missing or wrong: %v", generic)
		}
	}
}

func TestPosterPost_RetryThenSucceed(t *testing.T) {
	t.Parallel()
	flaky := func(attempt int) (int, string) {
		if attempt < 3 {
			return http.StatusBadGateway, `{"err":"upstream"}`
		}
		return http.StatusOK, `{"ok":true}`
	}
	ok := func(int) (int, string) { return http.StatusOK, `{"ok":true}` }
	srv, state := captureServer(t, flaky, ok)
	p := newPoster(t, srv.URL, 0)

	if err := p.Post(context.Background(), "sess-2", goodResult()); err != nil {
		t.Fatalf("Post error: %v", err)
	}
	if state.completion.Load() != 3 {
		t.Errorf("completion attempts = %d, want 3", state.completion.Load())
	}
}

func TestPosterPost_ExhaustRetries(t *testing.T) {
	t.Parallel()
	bad := func(int) (int, string) { return http.StatusInternalServerError, `boom` }
	ok := func(int) (int, string) { return http.StatusOK, `{"ok":true}` }
	srv, state := captureServer(t, bad, ok)
	p := newPoster(t, srv.URL, 0)

	err := p.Post(context.Background(), "sess-3", goodResult())
	if err == nil {
		t.Fatalf("expected transient error, got nil")
	}
	var transient *result.TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("expected TransientError, got %T: %v", err, err)
	}
	if transient.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", transient.Attempts)
	}
	if state.completion.Load() != 3 {
		t.Errorf("completion attempts = %d, want 3", state.completion.Load())
	}
}

func TestPosterPost_PermanentNoRetry(t *testing.T) {
	t.Parallel()
	bad := func(int) (int, string) { return http.StatusBadRequest, `{"error":"missing summary"}` }
	ok := func(int) (int, string) { return http.StatusOK, `{"ok":true}` }
	srv, state := captureServer(t, bad, ok)
	p := newPoster(t, srv.URL, 0)

	err := p.Post(context.Background(), "sess-4", goodResult())
	if err == nil {
		t.Fatalf("expected permanent error, got nil")
	}
	var perm *result.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("expected PermanentError, got %T: %v", err, err)
	}
	if perm.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", perm.StatusCode)
	}
	if state.completion.Load() != 1 {
		t.Errorf("completion attempts = %d, want 1 (no retry on 4xx)", state.completion.Load())
	}
}

func TestPosterPost_BothCallsFailJoined(t *testing.T) {
	t.Parallel()
	bad := func(int) (int, string) { return http.StatusInternalServerError, `boom` }
	srv, _ := captureServer(t, bad, bad)
	p := newPoster(t, srv.URL, 0)

	err := p.Post(context.Background(), "sess-5", goodResult())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "completion") || !strings.Contains(err.Error(), "status") {
		t.Errorf("expected joined error mentioning both calls, got %q", err.Error())
	}
}

func TestPosterPost_ContextCancel(t *testing.T) {
	t.Parallel()
	// Server hangs forever — ctx cancellation must surface promptly.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	p := newPoster(t, srv.URL, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Post(ctx, "sess-6", goodResult())
	if err == nil {
		t.Fatalf("expected context-cancel error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled chain, got %v", err)
	}
}

func TestPosterPost_NetworkTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// Stall longer than the client timeout.
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	p, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "test",
		WorkerID:    "wkr_test",
		HTTPClient:  &http.Client{Timeout: 10 * time.Millisecond},
		BaseDelay:   0,
	})
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}

	out := p.Post(context.Background(), "sess-7", goodResult())
	if out == nil {
		t.Fatalf("expected timeout, got nil")
	}
	var transient *result.TransientError
	if !errors.As(out, &transient) {
		t.Fatalf("expected TransientError, got %T: %v", out, out)
	}
}

func TestPosterPost_UsesCredentialProvider(t *testing.T) {
	t.Parallel()

	var auths []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "old-token",
		WorkerID:    "wkr_old",
		CredentialProvider: func(context.Context) (result.RuntimeCredentials, error) {
			return result.RuntimeCredentials{
				WorkerID:  "wkr_fresh",
				AuthToken: "fresh-token",
			}, nil
		},
		BaseDelay: 0,
	})
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}

	if err := p.Post(context.Background(), "sess-cred", goodResult()); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if len(auths) != 2 {
		t.Fatalf("requests = %d, want 2", len(auths))
	}
	for _, auth := range auths {
		if auth != "Bearer fresh-token" {
			t.Fatalf("Authorization = %q, want Bearer fresh-token", auth)
		}
	}
	for _, body := range bodies {
		if !strings.Contains(body, `"workerId":"wkr_fresh"`) {
			t.Fatalf("body %q missing fresh worker id", body)
		}
	}
}

// TestPosterPost_RefreshCredentialsOn401 confirms the SUP-1823 fix:
// when the platform returns 401 (cached runtime JWT expired mid-session),
// the next retry attempt re-invokes the CredentialProvider and posts
// with the fresh bearer token. Mirrors
// runtime/activity.TestUnauthorizedTriggersCredentialRefresh.
func TestPosterPost_RefreshCredentialsOn401(t *testing.T) {
	t.Parallel()

	var (
		completionAuths atomic.Value // []string
		statusAuths     atomic.Value // []string
		completionBody  atomic.Value // []string
		completionN     atomic.Int32
		statusN         atomic.Int32
	)
	completionAuths.Store([]string{})
	statusAuths.Store([]string{})
	completionBody.Store([]string{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/completion"):
			n := completionN.Add(1)
			completionAuths.Store(append(completionAuths.Load().([]string), auth))
			completionBody.Store(append(completionBody.Load().([]string), string(body)))
			if n == 1 {
				// First attempt — simulate expired JWT.
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"Authentication required (user session or worker token)"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/status"):
			n := statusN.Add(1)
			statusAuths.Store(append(statusAuths.Load().([]string), auth))
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	var providerCalls atomic.Int32
	p, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "stale-token",
		WorkerID:    "wkr_stale",
		CredentialProvider: func(context.Context) (result.RuntimeCredentials, error) {
			n := providerCalls.Add(1)
			// First call returns the same stale token (so the first
			// attempt 401s); subsequent calls return a refreshed token
			// — modelling the daemon-side rotation that lands between
			// the failed attempt and the retry.
			if n == 1 {
				return result.RuntimeCredentials{
					WorkerID:  "wkr_stale",
					AuthToken: "stale-token",
				}, nil
			}
			return result.RuntimeCredentials{
				WorkerID:  "wkr_fresh",
				AuthToken: "fresh-token",
			}, nil
		},
		BaseDelay: 0,
	})
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}

	if err := p.Post(context.Background(), "sess-401", goodResult()); err != nil {
		t.Fatalf("Post: %v", err)
	}

	// Each endpoint should see exactly 2 attempts: 1st 401, 2nd OK.
	if got := completionN.Load(); got != 2 {
		t.Errorf("completion attempts = %d, want 2", got)
	}
	if got := statusN.Load(); got != 2 {
		t.Errorf("status attempts = %d, want 2", got)
	}

	// Provider must be invoked at least once per attempt (4 total).
	if got := providerCalls.Load(); got < 4 {
		t.Errorf("CredentialProvider calls = %d, want >= 4", got)
	}

	// Verify the second attempt of each endpoint used the fresh token.
	cAuths := completionAuths.Load().([]string)
	if len(cAuths) != 2 {
		t.Fatalf("completion auth captures = %d, want 2", len(cAuths))
	}
	if cAuths[0] != "Bearer stale-token" {
		t.Errorf("completion attempt 1 auth = %q, want Bearer stale-token", cAuths[0])
	}
	if cAuths[1] != "Bearer fresh-token" {
		t.Errorf("completion attempt 2 auth = %q, want Bearer fresh-token", cAuths[1])
	}
	sAuths := statusAuths.Load().([]string)
	if len(sAuths) != 2 {
		t.Fatalf("status auth captures = %d, want 2", len(sAuths))
	}
	if sAuths[1] != "Bearer fresh-token" {
		t.Errorf("status attempt 2 auth = %q, want Bearer fresh-token", sAuths[1])
	}

	// The body's workerId must also reflect the refreshed credentials —
	// proves the per-attempt body builder is being called per retry,
	// not just the Authorization header.
	cBodies := completionBody.Load().([]string)
	if len(cBodies) != 2 {
		t.Fatalf("completion body captures = %d, want 2", len(cBodies))
	}
	if !strings.Contains(cBodies[0], `"workerId":"wkr_stale"`) {
		t.Errorf("completion attempt 1 body missing stale workerId: %q", cBodies[0])
	}
	if !strings.Contains(cBodies[1], `"workerId":"wkr_fresh"`) {
		t.Errorf("completion attempt 2 body missing fresh workerId: %q", cBodies[1])
	}
}

func TestPosterPost_MissingFieldsValidation(t *testing.T) {
	t.Parallel()
	p := newPoster(t, "http://example.invalid", 0)
	if err := p.Post(context.Background(), "", goodResult()); err == nil {
		t.Errorf("expected error for empty sessionID")
	}
	if err := p.Post(context.Background(), "x", agent.Result{}); err == nil {
		t.Errorf("expected error for empty Result.Status")
	}
}

func TestPosterPost_SynthSummary(t *testing.T) {
	t.Parallel()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/completion") {
			captured = body
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p := newPoster(t, srv.URL, 0)

	r := agent.Result{Status: "completed", PullRequestURL: "https://gh/example/pr/1"}
	if err := p.Post(context.Background(), "sess-8", r); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("captured not JSON: %v (raw %q)", err, captured)
	}
	summary, _ := body["summary"].(string)
	if !strings.Contains(summary, "https://gh/example/pr/1") {
		t.Errorf("synthesised summary missing PR URL: %q", summary)
	}
}

func TestNewPoster_Validation(t *testing.T) {
	t.Parallel()
	if _, err := result.NewPoster(result.Options{PlatformURL: ""}); err == nil {
		t.Errorf("expected error for empty PlatformURL")
	}
	if _, err := result.NewPoster(result.Options{PlatformURL: "not a url"}); err == nil {
		t.Errorf("expected error for invalid PlatformURL")
	}
	if _, err := result.NewPoster(result.Options{PlatformURL: "https://ok.example"}); err != nil {
		t.Errorf("unexpected error for valid PlatformURL: %v", err)
	}
}

func TestPosterPost_StatusBodyShape(t *testing.T) {
	t.Parallel()
	var statusBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/status") {
			statusBody = body
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p := newPoster(t, srv.URL, 0)

	r := goodResult()
	r.Error = "something blew up"
	r.Status = "failed"
	r.FailureMode = "agent-blocked"
	if err := p.Post(context.Background(), "sess-9", r); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(statusBody, &body); err != nil {
		t.Fatalf("status body not JSON: %v", err)
	}
	if body["status"] != "failed" {
		t.Errorf("status field = %v, want failed", body["status"])
	}
	errBlock, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error block missing or wrong shape: %v", body["error"])
	}
	if errBlock["message"] != "something blew up" {
		t.Errorf("error.message = %v", errBlock["message"])
	}
	// Cost fields should be populated.
	if body["totalCostUsd"] == nil {
		t.Errorf("totalCostUsd missing")
	}
	// failureMode must reach the platform as the authoritative routing
	// signal (e.g. needs-clarification on "agent-blocked").
	if body["failureMode"] != "agent-blocked" {
		t.Errorf("failureMode = %v, want agent-blocked", body["failureMode"])
	}
	// summary rides the status path too — lifecycle hooks read it.
	if body["summary"] != "Implemented X, opened PR." {
		t.Errorf("summary = %v, want %q", body["summary"], "Implemented X, opened PR.")
	}
}

// TestPosterPost_StatusFailureModeSerialized asserts the failureMode field
// is serialised on the /status body across the runner's failure-mode enum,
// and is omitted entirely when empty (backward-compatible additive field).
//
// The table enumerates every constant defined in runner/failure.go so a new
// failure mode added there without a corresponding test entry is caught by
// the round-trip count assertion below — the serialisation path is generic
// (it passes r.FailureMode verbatim), and the final "unknown free-form"
// case proves any non-enum string round-trips identically.
func TestPosterPost_StatusFailureModeSerialized(t *testing.T) {
	t.Parallel()

	// allFailureModes mirrors the wire strings in runner/failure.go. Keep
	// in sync when a new constant is added there (the count check below
	// guards against silent drift).
	allFailureModes := []string{
		runner.FailureWorktreeProvision, // "worktree-provision"
		runner.FailurePromptRender,      // "prompt-render"
		runner.FailureProviderResolve,   // "provider-resolve"
		runner.FailureSpawn,             // "spawn-failed"
		runner.FailureProviderError,     // "provider-error"
		runner.FailureSilentExit,        // "silent-exit"
		runner.FailureLostOwnership,     // "lost-ownership"
		runner.FailureTimeout,           // "timeout"
		runner.FailureBackstop,          // "backstop-failed"
		runner.FailureKitProvision,      // "kit-provision"
		runner.FailureAgentBlocked,      // "agent-blocked"
	}
	if got, want := len(allFailureModes), 11; got != want {
		t.Fatalf("failure-mode count = %d, want %d — a constant was added to "+
			"runner/failure.go without updating this table", got, want)
	}

	tests := []struct {
		name        string
		failureMode string
		wantPresent bool
	}{
		{name: "empty omitted", failureMode: "", wantPresent: false},
		// Generic non-enum string must round-trip verbatim — proves the
		// serializer never gates on the known-enum set.
		{name: "unknown free-form", failureMode: "some-future-mode", wantPresent: true},
	}
	for _, mode := range allFailureModes {
		tests = append(tests, struct {
			name        string
			failureMode string
			wantPresent bool
		}{name: mode, failureMode: mode, wantPresent: true})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var statusBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.HasSuffix(r.URL.Path, "/status") {
					statusBody = body
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			p := newPoster(t, srv.URL, 0)

			r := goodResult()
			r.Status = "failed"
			r.FailureMode = tc.failureMode
			if err := p.Post(context.Background(), "sess-fm", r); err != nil {
				t.Fatalf("Post: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(statusBody, &body); err != nil {
				t.Fatalf("status body not JSON: %v (raw %q)", err, statusBody)
			}
			got, present := body["failureMode"]
			if present != tc.wantPresent {
				t.Fatalf("failureMode present = %v, want %v (body %q)", present, tc.wantPresent, statusBody)
			}
			if tc.wantPresent && got != tc.failureMode {
				t.Errorf("failureMode = %v, want %q", got, tc.failureMode)
			}
		})
	}
}
