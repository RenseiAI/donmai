package result_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/result"
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
	bodies     []string
},
) {
	t.Helper()
	state := &struct {
		completion atomic.Int32
		status     atomic.Int32
		bodies     []string
	}{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		state.bodies = append(state.bodies, string(body))
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
}
