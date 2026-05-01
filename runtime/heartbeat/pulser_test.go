package heartbeat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/runtime/heartbeat"
)

// newServer returns an httptest.Server whose handler is provided by
// the caller. Helps each test set up an explicit script.
func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestStartFiresFirstTickSynchronously(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	cfg := heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		Interval:   24 * time.Hour, // suppress further ticks
		HTTPClient: srv.Client(),
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if hits.Load() != 1 {
		t.Fatalf("expected synchronous first tick, got %d hits", hits.Load())
	}
	if p.LastTick() == 0 {
		t.Fatalf("LastTick should be set after success")
	}
}

func TestThreeStrikeTrips(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "no", http.StatusInternalServerError)
	})

	cfg := heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1, // one attempt per tick → predictable 3-tick trip
		StrikesUntilLost:   3,
		Sleep:              func(time.Duration) {},
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case <-p.LostOwnership():
		// good
	case <-time.After(2 * time.Second):
		t.Fatalf("LostOwnership did not fire after 3 failures (strikes=%d, hits=%d)",
			p.Strikes(), hits.Load())
	}
	if got := p.Strikes(); got < 3 {
		t.Fatalf("expected at least 3 strikes, got %d", got)
	}
}

func TestStrikeResetsOnSuccess(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n <= 2 {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	cfg := heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   5,
		Sleep:              func(time.Duration) {},
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.LastTick() != 0 && p.Strikes() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.LastTick() == 0 {
		t.Fatalf("expected eventual success; hits=%d strikes=%d", hits.Load(), p.Strikes())
	}
	if p.Strikes() != 0 {
		t.Fatalf("expected strikes reset on success, got %d", p.Strikes())
	}
}

func TestRefreshedFalseCountsAsFailure(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": false})
	})

	cfg := heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   2,
		Sleep:              func(time.Duration) {},
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case <-p.LostOwnership():
	case <-time.After(2 * time.Second):
		t.Fatal("LostOwnership did not fire on refreshed=false")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestContextCancelStops(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Stop should return promptly since ctx-cancel triggered the loop
	// to exit.
	done := make(chan struct{})
	go func() {
		_ = p.Stop()
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
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("expected second Start to return error")
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	if _, err := heartbeat.New(heartbeat.Config{BaseURL: "x"}); err == nil {
		t.Fatal("expected error for missing SessionID")
	}
	if _, err := heartbeat.New(heartbeat.Config{SessionID: "s"}); err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

func TestRequestBodyShape(t *testing.T) {
	t.Parallel()

	var captured atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		captured.Store(&s)
		// also assert path
		if !strings.HasSuffix(r.URL.Path, "/api/sessions/s1/lock-refresh") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("expected Bearer tok, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		AuthToken:  "tok",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	got := captured.Load()
	if got == nil {
		t.Fatal("no body captured")
	}
	if !strings.Contains(*got, `"workerId":"w1"`) || !strings.Contains(*got, `"issueId":"i1"`) {
		t.Fatalf("body missing workerId/issueId: %s", *got)
	}
}
