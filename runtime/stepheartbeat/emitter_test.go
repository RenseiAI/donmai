package stepheartbeat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
)

// threadSafeBuffer wraps bytes.Buffer with a mutex so a test goroutine can
// read while the emitter's logger goroutine writes. Mirrors
// runtime/activity/poster_test.go's helper.
type threadSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *threadSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogger returns an slog.Logger writing JSON lines to buf for later
// assertion. Mirrors runtime/activity/poster_test.go's helper.
func captureLogger(buf *threadSafeBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newServer returns an httptest.Server whose handler is provided by the
// caller. Mirrors runtime/heartbeat/pulser_test.go's helper.
func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestNewValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	if _, err := stepheartbeat.New(stepheartbeat.Config{BaseURL: "x"}); err == nil {
		t.Fatal("expected error for missing SessionID")
	}
	if _, err := stepheartbeat.New(stepheartbeat.Config{SessionID: "s"}); err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

func TestStartFiresFirstBeatSynchronously(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		Interval:   24 * time.Hour, // suppress further beats
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	if hits.Load() != 1 {
		t.Fatalf("expected synchronous first beat, got %d hits", hits.Load())
	}
}

// TestRequestShape asserts the POST path, auth header, and JSON body — the
// exact wire contract the platform companion route must accept.
func TestRequestShape(t *testing.T) {
	t.Parallel()

	var (
		capturedPath   atomic.Pointer[string]
		capturedAuth   atomic.Pointer[string]
		capturedMethod atomic.Pointer[string]
		capturedBody   atomic.Pointer[string]
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		capturedPath.Store(&path)
		auth := r.Header.Get("Authorization")
		capturedAuth.Store(&auth)
		method := r.Method
		capturedMethod.Store(&method)
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		capturedBody.Store(&s)
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		AuthToken:  "tok",
		BaseURL:    srv.URL,
		Interval:   24 * time.Hour,
		HTTPClient: srv.Client(),
		Now:        func() time.Time { return time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	if got := capturedMethod.Load(); got == nil || *got != http.MethodPost {
		t.Fatalf("method = %v, want POST", got)
	}
	if got := capturedPath.Load(); got == nil || *got != "/api/sessions/s1/step-heartbeat" {
		t.Fatalf("path = %v, want /api/sessions/s1/step-heartbeat", got)
	}
	if got := capturedAuth.Load(); got == nil || *got != "Bearer tok" {
		t.Fatalf("Authorization = %v, want Bearer tok", got)
	}
	got := capturedBody.Load()
	if got == nil {
		t.Fatal("no body captured")
	}
	var body struct {
		WorkerID  string `json:"workerId"`
		EmittedAt string `json:"emittedAt"`
	}
	if err := json.Unmarshal([]byte(*got), &body); err != nil {
		t.Fatalf("body not JSON: %q (%v)", *got, err)
	}
	if body.WorkerID != "w1" {
		t.Errorf("workerId = %q, want w1", body.WorkerID)
	}
	if body.EmittedAt != "2026-07-01T09:00:00Z" {
		t.Errorf("emittedAt = %q, want RFC3339 2026-07-01T09:00:00Z", body.EmittedAt)
	}
	// EmittedAt must be a valid RFC3339 timestamp the platform can parse.
	if _, err := time.Parse(time.RFC3339, body.EmittedAt); err != nil {
		t.Errorf("emittedAt %q not RFC3339: %v", body.EmittedAt, err)
	}
}

// TestBestEffortDoesNotCrashOnError covers the core contract: a non-2xx
// (e.g. 404 from a platform build without the companion route) or a network
// failure must be swallowed — the emitter keeps beating and Stop still
// returns cleanly.
func TestBestEffortDoesNotCrashOnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"not found", http.StatusNotFound},
		{"server error", http.StatusInternalServerError},
		{"bad request", http.StatusBadRequest},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int64
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				http.Error(w, "no", tc.status)
			})

			e, err := stepheartbeat.New(stepheartbeat.Config{
				SessionID:  "s1",
				WorkerID:   "w1",
				BaseURL:    srv.URL,
				Interval:   5 * time.Millisecond,
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}

			// Loop must keep beating past the first failure.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && hits.Load() < 2 {
				time.Sleep(2 * time.Millisecond)
			}
			if hits.Load() < 2 {
				t.Fatalf("expected the loop to keep beating past a %d, got %d hits", tc.status, hits.Load())
			}
			if err := e.Stop(); err != nil {
				t.Fatalf("Stop after error responses: %v", err)
			}
		})
	}
}

// TestPeriodicBeats confirms the loop fires more than one beat over time.
func TestPeriodicBeats(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		Interval:   5 * time.Millisecond,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	if hits.Load() < 3 {
		t.Fatalf("expected >= 3 beats, got %d", hits.Load())
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestContextCancelStops(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		_ = e.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop after ctx-cancel did not return")
	}
}

func TestStartTwiceRejected(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	if err := e.Start(context.Background()); err == nil {
		t.Fatal("expected second Start to return error")
	}
}

func TestCredentialProviderOverridesCachedCredentials(t *testing.T) {
	t.Parallel()

	var capturedAuth atomic.Pointer[string]
	var capturedBody atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		capturedAuth.Store(&auth)
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		capturedBody.Store(&s)
		w.WriteHeader(http.StatusOK)
	})

	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID: "s1",
		WorkerID:  "wkr_old",
		AuthToken: "old-token",
		BaseURL:   srv.URL,
		CredentialProvider: func(context.Context) (stepheartbeat.RuntimeCredentials, error) {
			return stepheartbeat.RuntimeCredentials{
				WorkerID:  "wkr_fresh",
				AuthToken: "fresh-token",
			}, nil
		},
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	if got := capturedAuth.Load(); got == nil || *got != "Bearer fresh-token" {
		t.Fatalf("Authorization = %v, want Bearer fresh-token", got)
	}
	if got := capturedBody.Load(); got == nil || !strings.Contains(*got, `"workerId":"wkr_fresh"`) {
		t.Fatalf("body = %v, want fresh worker id", got)
	}
}

// TestCredentialProviderReflectsRotationAcrossTicks proves the emitter calls
// CredentialProvider fresh on EVERY beat rather than caching the value from
// construction: the first beat must carry the pre-rotation token and a LATER
// beat — after the daemon's proactive/reactive refresh has rotated the
// worker's runtime JWT mid-session — must carry the rotated one. This is the
// exact seam that keeps a step-heartbeat from silently 401ing across an
// hourly token-rotation boundary.
func TestCredentialProviderReflectsRotationAcrossTicks(t *testing.T) {
	t.Parallel()

	var auths []string
	var mu sync.Mutex
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	var rotated atomic.Bool
	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID: "s1",
		WorkerID:  "wkr_1",
		BaseURL:   srv.URL,
		CredentialProvider: func(context.Context) (stepheartbeat.RuntimeCredentials, error) {
			if rotated.Load() {
				return stepheartbeat.RuntimeCredentials{WorkerID: "wkr_1", AuthToken: "post-rotation-token"}, nil
			}
			return stepheartbeat.RuntimeCredentials{WorkerID: "wkr_1", AuthToken: "pre-rotation-token"}, nil
		},
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	// First beat (the synchronous one Start fires) must have carried the
	// PRE-rotation token — proving the assertion below is a genuine
	// before/after, not an artifact of the provider always returning the
	// same value.
	mu.Lock()
	firstAuth := auths[0]
	mu.Unlock()
	if firstAuth != "Bearer pre-rotation-token" {
		t.Fatalf("first beat Authorization = %q, want Bearer pre-rotation-token", firstAuth)
	}

	// Simulate the daemon-side rotation landing mid-session.
	rotated.Store(true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		latest := auths[len(auths)-1]
		mu.Unlock()
		if latest == "Bearer post-rotation-token" {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no beat after rotation carried the post-rotation token — a session crossing the rotation boundary would keep 401ing on the stale snapshot")
}

// TestBeatLogsAuthRejectionDistinctlyOnStatus401Or403 asserts that a 401/403
// on the step-heartbeat POST — the wire-shape of a rotated-out worker
// token — is logged at WARN with an auth-shaped, distinct message, not
// folded into the routine best-effort "non-2xx" Debug line every other
// failure (404, 500, network error) gets. The step-heartbeat outage stays
// swallowed either way (the session must never fail because of it) but an
// operator grepping daemon logs must be able to tell "credentials expired"
// from "platform build lacks the route" / "transient 500" at a glance.
func TestBeatLogsAuthRejectionDistinctlyOnStatus401Or403(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})

			buf := &threadSafeBuffer{}
			e, err := stepheartbeat.New(stepheartbeat.Config{
				SessionID:  "s1",
				WorkerID:   "w1",
				BaseURL:    srv.URL,
				Interval:   24 * time.Hour, // one beat only
				HTTPClient: srv.Client(),
				Logger:     captureLogger(buf),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = e.Stop() })

			logs := buf.String()
			if !strings.Contains(logs, "auth rejected") {
				t.Fatalf("expected a distinct auth-rejected log line for status %d, got: %s", tc.status, logs)
			}
			if !strings.Contains(logs, `"level":"WARN"`) {
				t.Fatalf("expected the auth-rejected line at WARN level, got: %s", logs)
			}
			if strings.Contains(logs, "non-2xx") {
				t.Fatalf("auth-shaped failure must not also log the generic non-2xx line: %s", logs)
			}
		})
	}
}

// TestBeatLogsGenericNon2xxAtDebugForNonAuthFailures is the control: a
// non-auth failure (404, 500, ...) keeps logging at the routine Debug level
// with the generic message, unchanged by the 401/403 special-case above.
func TestBeatLogsGenericNon2xxAtDebugForNonAuthFailures(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	buf := &threadSafeBuffer{}
	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		Interval:   24 * time.Hour,
		HTTPClient: srv.Client(),
		Logger:     captureLogger(buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	logs := buf.String()
	if !strings.Contains(logs, "non-2xx") {
		t.Fatalf("expected the generic non-2xx line for a 404, got: %s", logs)
	}
	if strings.Contains(logs, "auth rejected") {
		t.Fatalf("a 404 must not be classified as an auth rejection: %s", logs)
	}
}
