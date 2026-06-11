package heartbeat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/heartbeat"
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

// TestInjectPayloadFiresOnInjectAndAcks covers the Wave 3 runtime
// memory-inject transport: a successful lock-refresh that piggybacks an
// `inject` object must (a) decode the payload and fire Config.OnInject,
// and (b) cause the NEXT request body to carry ackedInject == the prior
// DeliveryID so the platform stops re-sending it.
func TestInjectPayloadFiresOnInjectAndAcks(t *testing.T) {
	t.Parallel()

	var (
		hits          atomic.Int64
		capturedAcked atomic.Pointer[string]
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			AckedInject string `json:"ackedInject"`
		}
		_ = json.Unmarshal(body, &req)
		n := hits.Add(1)
		if n == 1 {
			// First tick: extend the lock + piggyback an inject.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"refreshed": true,
				"inject": map[string]any{
					"deliveryId": "dlv-1",
					"text":       "remember: prefer the existing helper",
				},
			})
			return
		}
		// Subsequent ticks: record the ack the worker echoed back; no
		// further inject.
		acked := req.AckedInject
		capturedAcked.Store(&acked)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	var (
		mu       sync.Mutex
		received []heartbeat.InjectPayload
	)
	cfg := heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond, // drive a 2nd tick promptly
		OnInject: func(p heartbeat.InjectPayload) bool {
			mu.Lock()
			received = append(received, p)
			mu.Unlock()
			return true
		},
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// (a) OnInject fired with the decoded payload.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	gotCount := len(received)
	var first heartbeat.InjectPayload
	if gotCount > 0 {
		first = received[0]
	}
	mu.Unlock()
	if gotCount == 0 {
		t.Fatal("OnInject never fired for the piggybacked inject")
	}
	if first.DeliveryID != "dlv-1" || first.Text != "remember: prefer the existing helper" {
		t.Fatalf("OnInject payload = %+v; want {dlv-1, remember...}", first)
	}

	// (b) the NEXT request carries ackedInject == the prior DeliveryID.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := capturedAcked.Load(); got != nil && *got != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := capturedAcked.Load(); got == nil || *got != "dlv-1" {
		t.Fatalf("subsequent request ackedInject = %v; want dlv-1", got)
	}

	// OnInject must fire exactly once — the inject is only sent on tick 1
	// and not re-delivered after the ack.
	mu.Lock()
	finalCount := len(received)
	mu.Unlock()
	if finalCount != 1 {
		t.Fatalf("OnInject fired %d times; want exactly 1 (no re-delivery)", finalCount)
	}
}

// TestInjectNotAppliedOnRefreshedFalse verifies the ownership-lost guard:
// when the platform refuses the refresh (refreshed=false) any piggybacked
// inject is ignored — OnInject must NOT fire (the platform only routes
// injects to the current lock holder, and a refused refresh means we no
// longer are it).
func TestInjectNotAppliedOnRefreshedFalse(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Refuse the lock but (perversely) include an inject — the pulser
		// must not apply it.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"refreshed": false,
			"inject": map[string]any{
				"deliveryId": "dlv-x",
				"text":       "this must never be delivered",
			},
		})
	})

	var injectFired atomic.Bool
	cfg := heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   2,
		Sleep:              func(time.Duration) {},
		OnInject: func(heartbeat.InjectPayload) bool {
			injectFired.Store(true)
			return true
		},
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// Wait for ownership-lost (refreshed=false counts as a strike). Once
	// it trips we know at least two refresh attempts ran — ample chance
	// for a buggy implementation to have fired OnInject.
	select {
	case <-p.LostOwnership():
	case <-time.After(2 * time.Second):
		t.Fatal("LostOwnership did not fire on refreshed=false")
	}
	if injectFired.Load() {
		t.Fatal("OnInject fired on refreshed=false; inject must be ignored when ownership is refused")
	}
}

// TestInjectRejectedByConsumerStaysUnacked covers the ack-or-requeue
// contract: when OnInject reports the consumer did NOT accept the payload
// (e.g. the runner's buffer was full), the pulser must not ack it — the
// platform keeps re-delivering until a delivery is accepted, and only then
// does a request carry the ack echo.
func TestInjectRejectedByConsumerStaysUnacked(t *testing.T) {
	t.Parallel()

	var sawAck atomic.Bool
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			AckedInject string `json:"ackedInject"`
		}
		_ = json.Unmarshal(body, &req)
		if req.AckedInject == "dlv-1" {
			sawAck.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
			return
		}
		// No ack yet — keep re-delivering the same inject, exactly like
		// the platform's delivered-but-unacked requeue.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"refreshed": true,
			"inject": map[string]any{
				"deliveryId": "dlv-1",
				"text":       "must not be lost",
			},
		})
	})

	var calls atomic.Int64
	cfg := heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
		OnInject: func(heartbeat.InjectPayload) bool {
			// First delivery is rejected (full buffer); the re-delivery
			// is accepted.
			return calls.Add(1) > 1
		},
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sawAck.Load() {
		time.Sleep(2 * time.Millisecond)
	}
	if !sawAck.Load() {
		t.Fatal("ack for dlv-1 never arrived after the consumer accepted the re-delivery")
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("OnInject called %d times; want >= 2 (rejected delivery must be re-delivered)", got)
	}
}

// TestStopFlushesPendingInjectAck covers the short-session ack gap (the
// delivered=true/acked=false strand): an inject accepted on the synchronous
// first tick whose ack would only ride the NEXT tick must be flushed by
// Stop with one final ack-only request — short sessions exit long before
// the next heartbeat interval elapses.
func TestStopFlushesPendingInjectAck(t *testing.T) {
	t.Parallel()

	var (
		hits     atomic.Int64
		flushAck atomic.Pointer[string]
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			AckedInject string `json:"ackedInject"`
		}
		_ = json.Unmarshal(body, &req)
		if hits.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"refreshed": true,
				"inject": map[string]any{
					"deliveryId": "dlv-9",
					"text":       "applied on the only tick",
				},
			})
			return
		}
		acked := req.AckedInject
		flushAck.Store(&acked)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	cfg := heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour, // no natural second tick
		OnInject:   func(heartbeat.InjectPayload) bool { return true },
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := hits.Load(); got != 2 {
		t.Fatalf("expected exactly 2 requests (tick + Stop ack flush), got %d", got)
	}
	if got := flushAck.Load(); got == nil || *got != "dlv-9" {
		t.Fatalf("Stop flush ackedInject = %v; want dlv-9", got)
	}
}

// TestStopSkipsFlushWithoutPendingAck pins the no-op guard: a session that
// never received an inject must not pay an extra request on Stop.
func TestStopSkipsFlushWithoutPendingAck(t *testing.T) {
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
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour,
		OnInject:   func(heartbeat.InjectPayload) bool { return true },
	}
	p, err := heartbeat.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (no ack to flush), got %d", got)
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
		bodyText := string(body)
		capturedBody.Store(&bodyText)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID: "s1",
		WorkerID:  "wkr_old",
		IssueID:   "issue-1",
		AuthToken: "old-token",
		BaseURL:   srv.URL,
		CredentialProvider: func(context.Context) (heartbeat.RuntimeCredentials, error) {
			return heartbeat.RuntimeCredentials{
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
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if got := capturedAuth.Load(); got == nil || *got != "Bearer fresh-token" {
		t.Fatalf("Authorization = %v, want Bearer fresh-token", got)
	}
	if got := capturedBody.Load(); got == nil || !strings.Contains(*got, `"workerId":"wkr_fresh"`) {
		t.Fatalf("body = %v, want fresh worker id", got)
	}
}
