package activity_test

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

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runtime/activity"
)

// captureLogger returns an slog.Logger writing JSON lines to buf for
// later assertion. Concurrent-safe; the JSONHandler serializes writes.
func captureLogger(buf *threadSafeBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// threadSafeBuffer wraps bytes.Buffer with a mutex so multiple goroutines
// can write without racing. slog's JSONHandler serializes records but
// nothing prevents tests from reading buf concurrently with handler writes.
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

// newServer is a tiny helper that mirrors heartbeat's test pattern.
func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// TestNewValidatesRequiredFields asserts the constructor rejects
// missing SessionID / BaseURL.
func TestNewValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	if _, err := activity.New(activity.Config{BaseURL: "x"}); err == nil {
		t.Fatal("expected error for missing SessionID")
	}
	if _, err := activity.New(activity.Config{SessionID: "s"}); err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
	if _, err := activity.New(activity.Config{SessionID: "s", BaseURL: "x"}); err != nil {
		t.Fatalf("unexpected error for valid Config: %v", err)
	}
}

// TestSendBeforeStartIsNoOp confirms Send drops events when the worker
// hasn't been started — never blocks, never sends.
func TestSendBeforeStartIsNoOp(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	p, err := activity.New(activity.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Send(context.Background(), agent.AssistantTextEvent{Text: "hi"})
	// No goroutine has been started — give the runtime a beat to be sure.
	time.Sleep(10 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("expected zero requests before Start; got %d", hits.Load())
	}
}

// TestSuccessfulSendPostsActivityThenStatus confirms:
//   - the first successful activity POST triggers a follow-up status=running POST
//   - subsequent activities do NOT re-fire the status nudge
func TestSuccessfulSendPostsActivityThenStatus(t *testing.T) {
	t.Parallel()
	var actHits, statusHits atomic.Int64
	var lastBody atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/activity"):
			actHits.Add(1)
			lastBody.Store(&s)
		case strings.HasSuffix(r.URL.Path, "/status"):
			statusHits.Add(1)
			if !strings.Contains(s, `"status":"running"`) {
				t.Errorf("status body missing running: %s", s)
			}
			if !strings.Contains(s, `"workerId":"w1"`) {
				t.Errorf("status body missing workerId: %s", s)
			}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})

	p, err := activity.New(activity.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		AuthToken:  "tok",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Send(context.Background(), agent.AssistantTextEvent{Text: "thinking"})
	p.Send(context.Background(), agent.AssistantTextEvent{Text: "more thinking"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if actHits.Load() >= 2 && statusHits.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if actHits.Load() < 2 {
		t.Errorf("expected >=2 activity posts; got %d", actHits.Load())
	}
	if statusHits.Load() != 1 {
		t.Errorf("expected exactly 1 status post (running nudge); got %d", statusHits.Load())
	}
	got := lastBody.Load()
	if got == nil || !strings.Contains(*got, `"type":"thought"`) {
		t.Errorf("body missing type=thought: %v", got)
	}
}

// TestUnauthorizedTriggersCredentialRefresh confirms that a 401 response
// triggers the next attempt to re-resolve credentials.
func TestUnauthorizedTriggersCredentialRefresh(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	var capturedAuth atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/activity") {
			// Status nudge — accept after the activity succeeds.
			w.WriteHeader(http.StatusOK)
			return
		}
		capturedAuth.Store(&auth)
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	var providerCalls atomic.Int64
	p, err := activity.New(activity.Config{
		SessionID: "s1",
		WorkerID:  "w_old",
		AuthToken: "tok-old",
		BaseURL:   srv.URL,
		CredentialProvider: func(context.Context) (activity.RuntimeCredentials, error) {
			providerCalls.Add(1)
			return activity.RuntimeCredentials{
				WorkerID:  "w_new",
				AuthToken: "tok-new",
			}, nil
		},
		HTTPClient:     srv.Client(),
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
		Sleep:          func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Send(context.Background(), agent.AssistantTextEvent{Text: "hi"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if attempts.Load() < 2 {
		t.Fatalf("expected retry after 401; got %d attempts", attempts.Load())
	}
	if got := capturedAuth.Load(); got == nil || *got != "Bearer tok-new" {
		t.Fatalf("expected Bearer tok-new on retry; got %v", got)
	}
}

// TestServerErrorsRetryThenDrop confirms 5xx triggers retries up to
// MaxRetries+1 attempts before dropping.
func TestServerErrorsRetryThenDrop(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	logBuf := &threadSafeBuffer{}
	p, err := activity.New(activity.Config{
		SessionID:      "s1",
		WorkerID:       "w1",
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
		MaxRetries:     2, // 3 attempts total
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
		Sleep:          func(time.Duration) {},
		Logger:         captureLogger(logBuf),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Send(context.Background(), agent.AssistantTextEvent{Text: "test"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Allow a beat for the warn log to flush after the final attempt.
	time.Sleep(50 * time.Millisecond)
	if got := hits.Load(); got != 3 {
		t.Fatalf("expected exactly 3 attempts (2 retries); got %d", got)
	}
	if !strings.Contains(logBuf.String(), "failed after retries") {
		t.Fatalf("expected drop log; got %s", logBuf.String())
	}
}

// TestNonRetryable4xxDropsImmediately confirms a 400/403/404 short-
// circuits the retry loop on the first attempt.
func TestNonRetryable4xxDropsImmediately(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	logBuf := &threadSafeBuffer{}
	p, err := activity.New(activity.Config{
		SessionID:      "s1",
		WorkerID:       "w1",
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
		MaxRetries:     5,
		InitialBackoff: 1 * time.Millisecond,
		Sleep:          func(time.Duration) {},
		Logger:         captureLogger(logBuf),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Send(context.Background(), agent.AssistantTextEvent{Text: "test"})

	time.Sleep(150 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt on 400; got %d", got)
	}
	if !strings.Contains(logBuf.String(), "non-retryable") {
		t.Fatalf("expected non-retryable log; got %s", logBuf.String())
	}
}

// TestQueueFullDropsWithWarn confirms Send is non-blocking under load.
func TestQueueFullDropsWithWarn(t *testing.T) {
	t.Parallel()
	// Hold every request open until release closes; this lets us pin
	// the worker on one in-flight job and then overflow the queue.
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	logBuf := &threadSafeBuffer{}
	p, err := activity.New(activity.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		QueueSize:  2,
		Logger:     captureLogger(logBuf),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// First Send is picked up by the worker (blocks on <-release in the
	// handler). Next two fill the queue. The fourth must be dropped.
	for i := 0; i < 10; i++ {
		p.Send(context.Background(), agent.AssistantTextEvent{Text: "x"})
	}
	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(logBuf.String(), "queue full") {
		t.Fatalf("expected queue-full warn log; got %s", logBuf.String())
	}
}

// TestStopDrainsQueue confirms Stop waits for in-flight jobs.
func TestStopDrainsQueue(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	p, err := activity.New(activity.Config{
		SessionID:        "s1",
		WorkerID:         "w1",
		BaseURL:          srv.URL,
		HTTPClient:       srv.Client(),
		StopDrainTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		p.Send(context.Background(), agent.AssistantTextEvent{Text: "x"})
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 5 activity posts + 1 status nudge after the first success.
	if got := hits.Load(); got < 5 {
		t.Fatalf("expected >=5 hits after drain; got %d", got)
	}
	// Second Stop is a no-op.
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestSendAfterStopIsNoOp confirms Send drops events post-Stop.
func TestSendAfterStopIsNoOp(t *testing.T) {
	t.Parallel()
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p, err := activity.New(activity.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Should not panic; should not deadlock.
	p.Send(context.Background(), agent.AssistantTextEvent{Text: "post-stop"})
}

// TestEventMappingTable covers each agent.Event variant. Uses the
// public API only — Send → captured-server JSON.
func TestEventMappingTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		event       agent.Event
		wantSent    bool
		wantType    string
		wantContent string // substring assertion
		wantTool    string
		hasInput    bool
		hasOutput   bool
	}{
		{
			name:        "AssistantText -> thought",
			event:       agent.AssistantTextEvent{Text: "thinking out loud"},
			wantSent:    true,
			wantType:    "thought",
			wantContent: "thinking out loud",
		},
		{
			name: "ToolUse Bash -> action with summary",
			event: agent.ToolUseEvent{
				ToolName: "Bash",
				Input:    map[string]any{"command": "git status"},
			},
			wantSent:    true,
			wantType:    "action",
			wantContent: "Bash: git status",
			wantTool:    "Bash",
			hasInput:    true,
		},
		{
			name: "ToolUse Read -> action with file path",
			event: agent.ToolUseEvent{
				ToolName: "Read",
				Input:    map[string]any{"file_path": "/tmp/foo.go"},
			},
			wantSent:    true,
			wantType:    "action",
			wantContent: "Read: /tmp/foo.go",
			wantTool:    "Read",
			hasInput:    true,
		},
		{
			name: "ToolUse Agent -> action with description",
			event: agent.ToolUseEvent{
				ToolName: "Agent",
				Input:    map[string]any{"description": "research caching"},
			},
			wantSent:    true,
			wantType:    "action",
			wantContent: "Agent: research caching",
			wantTool:    "Agent",
			hasInput:    true,
		},
		{
			name: "ToolUse unknown -> action with bare name",
			event: agent.ToolUseEvent{
				ToolName: "WeirdTool",
				Input:    map[string]any{"foo": "bar"},
			},
			wantSent:    true,
			wantType:    "action",
			wantContent: "WeirdTool",
			wantTool:    "WeirdTool",
			hasInput:    true,
		},
		{
			name: "ToolResult -> action with output",
			event: agent.ToolResultEvent{
				ToolName: "Bash",
				Content:  "branch is clean",
			},
			wantSent:    true,
			wantType:    "action",
			wantContent: "Bash result",
			wantTool:    "Bash",
			hasOutput:   true,
		},
		{
			name: "Result success -> response",
			event: agent.ResultEvent{
				Success: true,
				Message: "all done",
			},
			wantSent:    true,
			wantType:    "response",
			wantContent: "all done",
		},
		{
			name: "Result success no message -> response default",
			event: agent.ResultEvent{
				Success: true,
			},
			wantSent:    true,
			wantType:    "response",
			wantContent: "Session completed",
		},
		{
			name: "Error -> error",
			event: agent.ErrorEvent{
				Message: "boom",
			},
			wantSent:    true,
			wantType:    "error",
			wantContent: "boom",
		},
		{
			name:     "Init -> skipped",
			event:    agent.InitEvent{SessionID: "x"},
			wantSent: false,
		},
		{
			name:     "System -> skipped",
			event:    agent.SystemEvent{Subtype: "compaction"},
			wantSent: false,
		},
		{
			name:     "ToolProgress -> skipped",
			event:    agent.ToolProgressEvent{ToolName: "Bash", ElapsedSeconds: 5},
			wantSent: false,
		},
		{
			name:     "AssistantText empty -> skipped",
			event:    agent.AssistantTextEvent{Text: "  \n"},
			wantSent: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var captured atomic.Pointer[string]
			var hits atomic.Int64
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/activity") {
					w.WriteHeader(http.StatusOK)
					return
				}
				body, _ := io.ReadAll(r.Body)
				s := string(body)
				captured.Store(&s)
				hits.Add(1)
				w.WriteHeader(http.StatusOK)
			})

			p, err := activity.New(activity.Config{
				SessionID:  "s1",
				WorkerID:   "w1",
				BaseURL:    srv.URL,
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Stop() })

			p.Send(context.Background(), tc.event)

			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) && hits.Load() == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			if !tc.wantSent {
				if hits.Load() != 0 {
					t.Fatalf("expected event %q to be filtered; got %d hits", tc.name, hits.Load())
				}
				return
			}
			if hits.Load() == 0 {
				t.Fatalf("expected event %q to be sent", tc.name)
			}
			body := captured.Load()
			if body == nil {
				t.Fatal("no body captured")
			}
			// Decode and verify the activity inner shape.
			var wire struct {
				WorkerID string `json:"workerId"`
				Activity struct {
					Type       string         `json:"type"`
					Content    string         `json:"content"`
					ToolName   string         `json:"toolName"`
					ToolInput  map[string]any `json:"toolInput"`
					ToolOutput string         `json:"toolOutput"`
					Timestamp  string         `json:"timestamp"`
				} `json:"activity"`
			}
			if err := json.Unmarshal([]byte(*body), &wire); err != nil {
				t.Fatalf("decode body: %v\n%s", err, *body)
			}
			if wire.WorkerID != "w1" {
				t.Errorf("workerId = %q; want w1", wire.WorkerID)
			}
			if wire.Activity.Type != tc.wantType {
				t.Errorf("type = %q; want %q", wire.Activity.Type, tc.wantType)
			}
			if !strings.Contains(wire.Activity.Content, tc.wantContent) {
				t.Errorf("content = %q; want substring %q", wire.Activity.Content, tc.wantContent)
			}
			if tc.wantTool != "" && wire.Activity.ToolName != tc.wantTool {
				t.Errorf("toolName = %q; want %q", wire.Activity.ToolName, tc.wantTool)
			}
			if tc.hasInput && wire.Activity.ToolInput == nil {
				t.Error("expected toolInput populated")
			}
			if tc.hasOutput && wire.Activity.ToolOutput == "" {
				t.Error("expected toolOutput populated")
			}
			if wire.Activity.Timestamp == "" {
				t.Error("expected timestamp populated")
			}
		})
	}
}

// TestToolOutputTruncation confirms ToolResult content > MaxToolOutputChars
// is truncated.
func TestToolOutputTruncation(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", activity.MaxToolOutputChars+200)
	var captured atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/activity") {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		captured.Store(&s)
		w.WriteHeader(http.StatusOK)
	})
	p, err := activity.New(activity.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Send(context.Background(), agent.ToolResultEvent{
		ToolName: "Bash",
		Content:  long,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && captured.Load() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	body := captured.Load()
	if body == nil {
		t.Fatal("no body captured")
	}
	var wire struct {
		Activity struct {
			ToolOutput string `json:"toolOutput"`
		} `json:"activity"`
	}
	if err := json.Unmarshal([]byte(*body), &wire); err != nil {
		t.Fatal(err)
	}
	if got := len(wire.Activity.ToolOutput); got > activity.MaxToolOutputChars {
		t.Fatalf("toolOutput length %d exceeds cap %d", got, activity.MaxToolOutputChars)
	}
}

// TestProviderNameAndToolUseFieldsRoundTrip pins the wire-format extension
// from ADR-2026-05-12-cross-process-hook-bus-bridge: a paired ToolUseEvent
// + ToolResultEvent must carry providerName, toolUseId, isError, and a
// non-zero durationMs through the wire payload. The platform-side hook
// bridge depends on this contract; a regression here breaks Layer 6 for
// every daemon-driven session.
func TestProviderNameAndToolUseFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	type wirePayload struct {
		Activity struct {
			Type         string `json:"type"`
			ToolName     string `json:"toolName"`
			ToolUseID    string `json:"toolUseId"`
			IsError      bool   `json:"isError"`
			DurationMs   int64  `json:"durationMs"`
			ProviderName string `json:"providerName"`
			ToolOutput   string `json:"toolOutput"`
		} `json:"activity"`
	}

	var mu sync.Mutex
	var bodies []wirePayload
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/activity") {
			w.WriteHeader(http.StatusOK)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var wire wirePayload
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Errorf("unmarshal: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, wire)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// Make time deterministic so we can assert exact durationMs.
	var clock atomic.Int64
	clock.Store(time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC).UnixMilli())
	nowFn := func() time.Time {
		return time.UnixMilli(clock.Load()).UTC()
	}

	p, err := activity.New(activity.Config{
		SessionID:    "s1",
		WorkerID:     "w1",
		BaseURL:      srv.URL,
		HTTPClient:   srv.Client(),
		ProviderName: "claude",
		Now:          nowFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// Send a ToolUseEvent at t0, then advance 75ms and send the matching
	// ToolResultEvent.
	p.Send(context.Background(), agent.ToolUseEvent{
		ToolName:  "Bash",
		ToolUseID: "tu_abc",
		Input:     map[string]any{"command": "ls -la"},
	})
	clock.Add(75)
	p.Send(context.Background(), agent.ToolResultEvent{
		ToolName:  "Bash",
		ToolUseID: "tu_abc",
		Content:   "total 0\n",
		IsError:   false,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 activity posts; got %d", len(bodies))
	}

	use := bodies[0].Activity
	if use.ProviderName != "claude" {
		t.Errorf("ToolUse providerName: want claude, got %q", use.ProviderName)
	}
	if use.ToolUseID != "tu_abc" {
		t.Errorf("ToolUse toolUseId: want tu_abc, got %q", use.ToolUseID)
	}
	if use.IsError {
		t.Errorf("ToolUse isError: want false, got true")
	}
	if use.DurationMs != 0 {
		t.Errorf("ToolUse durationMs: want 0 (only ToolResult carries it), got %d", use.DurationMs)
	}

	res := bodies[1].Activity
	if res.ProviderName != "claude" {
		t.Errorf("ToolResult providerName: want claude, got %q", res.ProviderName)
	}
	if res.ToolUseID != "tu_abc" {
		t.Errorf("ToolResult toolUseId: want tu_abc, got %q", res.ToolUseID)
	}
	if res.IsError {
		t.Errorf("ToolResult isError: want false, got true")
	}
	if res.DurationMs != 75 {
		t.Errorf("ToolResult durationMs: want 75, got %d", res.DurationMs)
	}
	if res.ToolOutput != "total 0\n" {
		t.Errorf("ToolResult toolOutput: want %q, got %q", "total 0\n", res.ToolOutput)
	}
}

// TestToolResultWithoutPairedUseHasZeroDuration confirms an orphan
// ToolResultEvent (no preceding ToolUseEvent in this Poster's lifetime)
// emits with durationMs=0 rather than crashing or fabricating a duration.
func TestToolResultWithoutPairedUseHasZeroDuration(t *testing.T) {
	t.Parallel()

	var captured atomic.Pointer[int64]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/activity") {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var wire struct {
			Activity struct {
				DurationMs int64 `json:"durationMs"`
			} `json:"activity"`
		}
		_ = json.Unmarshal(body, &wire)
		captured.Store(&wire.Activity.DurationMs)
		w.WriteHeader(http.StatusOK)
	})

	p, err := activity.New(activity.Config{
		SessionID:    "s1",
		WorkerID:     "w1",
		BaseURL:      srv.URL,
		HTTPClient:   srv.Client(),
		ProviderName: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Send(context.Background(), agent.ToolResultEvent{
		ToolName:  "Bash",
		ToolUseID: "tu_orphan",
		Content:   "oops",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && captured.Load() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	got := captured.Load()
	if got == nil {
		t.Fatal("no body captured")
	}
	if *got != 0 {
		t.Fatalf("orphan ToolResult durationMs: want 0, got %d", *got)
	}
}
