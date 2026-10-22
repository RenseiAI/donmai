package heartbeat_test

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

// threadSafeBuffer wraps bytes.Buffer with a mutex so a test goroutine can
// read while the pulser's logger goroutine writes. Mirrors
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

// TestStopSignalClosesLostOwnershipImmediately covers the R8 fast in-band
// cancel leg: a lock-refresh response carrying {"stop": true} must close
// LostOwnership on the FIRST tick (the synchronous Start tick), well before
// the 3-strike fuse, and StopRequested must report the operator-cancel
// origin so the runner forks to FailureOperatorCancelled.
func TestStopSignalClosesLostOwnershipImmediately(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// refreshed:true so the only reason to lose ownership is the
		// explicit stop flag — this isolates the stop path from the
		// refreshed=false path.
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true, "stop": true})
	})

	cfg := heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour, // suppress further ticks — first tick must suffice
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   3, // prove we did NOT wait out the fuse
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
		// good — fired immediately
	case <-time.After(2 * time.Second):
		t.Fatalf("LostOwnership did not fire on stop=true (hits=%d, strikes=%d)",
			hits.Load(), p.Strikes())
	}
	if !p.StopRequested() {
		t.Error("StopRequested = false after stop=true; runner cannot distinguish operator cancel")
	}
	// The immediate path must NOT have waited for 3 strikes. With a 24h
	// interval only the synchronous first tick has run, so a single hit
	// proves the immediate close.
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want exactly 1 (immediate close, no fuse wait)", got)
	}
	if got := p.Strikes(); got >= cfg.StrikesUntilLost {
		t.Errorf("strikes = %d; immediate stop should not have tripped the 3-strike fuse", got)
	}
}

// TestRefreshedFalseDoesNotSetStopRequested guards the disambiguation: a
// hand-off (refreshed=false) closes LostOwnership but is NOT an operator
// cancel, so StopRequested must stay false (runner → FailureLostOwnership,
// not FailureOperatorCancelled).
func TestRefreshedFalseDoesNotSetStopRequested(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": false})
	})

	cfg := heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour,
		MaxAttemptsPerTick: 1,
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
	case <-time.After(2 * time.Second):
		t.Fatal("LostOwnership did not fire on refreshed=false")
	}
	if p.StopRequested() {
		t.Error("StopRequested = true on refreshed=false; should be false (hand-off, not operator cancel)")
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
	// A Config with no SessionClass must NOT emit the field (omitempty keeps
	// the wire byte-identical for every headless / interview session — W4).
	if strings.Contains(*got, "sessionClass") {
		t.Fatalf("unset SessionClass leaked onto the wire: %s", *got)
	}
}

// TestSessionClassStampedOnLockRefresh covers the W4 amendment-4 stamp: when
// Config.SessionClass is set the lock-refresh body carries
// `"sessionClass":"interactive"` so the platform's activity-stall reaper can
// exempt an interactive session during human think-time. This is the named
// cross-repo rail W3 reads.
func TestSessionClassStampedOnLockRefresh(t *testing.T) {
	t.Parallel()

	var captured atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		captured.Store(&s)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:    "s1",
		WorkerID:     "w1",
		IssueID:      "i1",
		SessionClass: "interactive",
		BaseURL:      srv.URL,
		HTTPClient:   srv.Client(),
		Interval:     24 * time.Hour,
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
	if !strings.Contains(*got, `"sessionClass":"interactive"`) {
		t.Fatalf("lock-refresh body missing sessionClass stamp: %s", *got)
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

// TestInjectWithNoConsumerIsNeverAcked covers the worst ack-and-drop case: a
// pulser configured WITHOUT an OnInject consumer.
//
// It used to ack unconditionally in that state, so the payload was destroyed
// with no log line while the producer recorded a successful delivery. A
// transport whose entire contract is ack-or-requeue must never ack what it
// cannot deliver: with no consumer the inject stays unacked and the producer
// requeues it for a session that can actually take it.
func TestInjectWithNoConsumerIsNeverAcked(t *testing.T) {
	t.Parallel()

	var (
		offers atomic.Int64
		sawAck atomic.Bool
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			AckedInject string `json:"ackedInject"`
		}
		_ = json.Unmarshal(body, &req)
		if req.AckedInject != "" {
			sawAck.Store(true)
		}
		offers.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"refreshed": true,
			"inject": map[string]any{
				"deliveryId": "dlv-no-consumer",
				"text":       "must not be destroyed",
			},
		})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
		// OnInject deliberately nil: no consumer is wired.
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && offers.Load() < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := offers.Load(); got < 3 {
		t.Fatalf("only %d refreshes observed; the test needs the inject re-offered", got)
	}
	if sawAck.Load() {
		t.Fatal("an inject with no consumer was acked — the payload is destroyed and never requeued")
	}

	// Stop must not sneak the ack out through the final flush either.
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sawAck.Load() {
		t.Fatal("the Stop-time ack flush acked an inject that was never delivered")
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

// ── Interactive non-fatal heartbeat-loss posture ─────────────────────────────
//
// For SessionClass=interactive, heartbeat loss must DEGRADE, never kill: the
// pulser never closes LostOwnership on failed ticks or a refused refresh
// (refreshed=false); it keeps beating and resumes on its own when the control
// plane is reachable again. Only the explicit {"stop": true} operator cancel
// stays fatal. Every other class keeps the fail-fast fuse. See
// heartbeat.SessionClassInteractive.

// waitFor polls cond every 2ms until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestHeartbeatLossFatalityBySessionClass contrasts the two loss postures on
// identical failing control planes: non-interactive classes trip the fatal
// fuse (HTTP failure) or lose ownership immediately (refreshed=false), while
// the interactive class survives BOTH — including strike counts well past
// the threshold, the compressed-time equivalent of a >65s partition.
func TestHeartbeatLossFatalityBySessionClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sessionClass string
		handler      http.HandlerFunc
		wantLost     bool
	}{
		{
			name:         "default class trips fuse on http failure",
			sessionClass: "",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unreachable", http.StatusInternalServerError)
			},
			wantLost: true,
		},
		{
			name:         "default class dies immediately on refreshed=false",
			sessionClass: "",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": false})
			},
			wantLost: true,
		},
		{
			name:         "interactive survives http failure past the fuse",
			sessionClass: heartbeat.SessionClassInteractive,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unreachable", http.StatusInternalServerError)
			},
			wantLost: false,
		},
		{
			name:         "interactive survives refreshed=false past the fuse",
			sessionClass: heartbeat.SessionClassInteractive,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": false})
			},
			wantLost: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer(t, tt.handler)
			p, err := heartbeat.New(heartbeat.Config{
				SessionID:          "s1",
				BaseURL:            srv.URL,
				HTTPClient:         srv.Client(),
				Interval:           5 * time.Millisecond,
				MaxAttemptsPerTick: 1,
				StrikesUntilLost:   3,
				Sleep:              func(time.Duration) {},
				SessionClass:       tt.sessionClass,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := p.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Stop() })

			if tt.wantLost {
				select {
				case <-p.LostOwnership():
					// fail-fast preserved
				case <-time.After(2 * time.Second):
					t.Fatalf("LostOwnership did not fire (strikes=%d)", p.Strikes())
				}
				return
			}

			// Drive the failure well past the fuse threshold — the
			// compressed-time equivalent of a >65s partition (>= 2x the
			// 3-strike fuse) — and require ownership intact throughout.
			if !waitFor(t, 2*time.Second, func() bool { return p.Strikes() >= 6 }) {
				t.Fatalf("strikes did not accumulate past the fuse, got %d", p.Strikes())
			}
			select {
			case <-p.LostOwnership():
				t.Fatalf("LostOwnership fired for interactive class at %d strikes — heartbeat loss must be non-fatal", p.Strikes())
			default:
			}
			if !p.Degraded() {
				t.Error("Degraded() = false past the strike threshold; interactive loss state must be marked")
			}
			if p.StopRequested() {
				t.Error("StopRequested = true; heartbeat loss is not an operator cancel")
			}
		})
	}
}

// TestInteractivePartitionRecovery proves the resume half of the posture: a
// simulated partition longer than the fuse window (>= 5 failed ticks vs the
// 3-strike fuse — compressed-time stand-in for a >65s outage) neither kills
// the session nor stops the loop, and once connectivity returns the pulser
// resumes successful heartbeating on its own (strikes reset, degraded state
// clears, LastTick advances).
func TestInteractivePartitionRecovery(t *testing.T) {
	t.Parallel()

	const failedTicks = 5
	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= failedTicks {
			http.Error(w, "partition", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   3,
		Sleep:              func(time.Duration) {},
		SessionClass:       heartbeat.SessionClassInteractive,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// The partition heals only after the fuse would have tripped; the
	// pulser must still get there and recover.
	if !waitFor(t, 2*time.Second, func() bool { return p.LastTick() != 0 && p.Strikes() == 0 }) {
		t.Fatalf("heartbeat did not resume after the partition healed (hits=%d strikes=%d lastTick=%d)",
			hits.Load(), p.Strikes(), p.LastTick())
	}
	if p.Degraded() {
		t.Error("Degraded() = true after recovery; must clear on the first successful tick")
	}
	select {
	case <-p.LostOwnership():
		t.Fatal("LostOwnership fired across a recoverable partition")
	default:
	}

	// Beating continues at cadence after recovery — the loop did not exit.
	after := hits.Load()
	if !waitFor(t, 2*time.Second, func() bool { return hits.Load() > after }) {
		t.Fatalf("heartbeat loop stopped ticking after recovery (hits stuck at %d)", after)
	}
}

// TestInteractiveRefusedRefreshFirstTickRecovery pins the post-wake shape:
// the FIRST tick (Start's synchronous one — the first tick after a host
// wakes) answering refreshed=false must not kill an interactive session,
// and a later accepted refresh restores normal heartbeating.
func TestInteractiveRefusedRefreshFirstTickRecovery(t *testing.T) {
	t.Parallel()

	const refusedTicks = 4
	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		refreshed := hits.Add(1) > refusedTicks
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": refreshed})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   3,
		Sleep:              func(time.Duration) {},
		SessionClass:       heartbeat.SessionClassInteractive,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// The synchronous first tick has already run and was refused: the old
	// behavior called loseOwnershipNow right here (the first-post-wake-tick
	// killer). Ownership must be intact.
	select {
	case <-p.LostOwnership():
		t.Fatal("LostOwnership fired on first-tick refreshed=false for interactive class")
	default:
	}
	if p.StopRequested() {
		t.Error("StopRequested = true on refreshed=false; only stop:true may set it")
	}

	if !waitFor(t, 2*time.Second, func() bool { return p.LastTick() != 0 && p.Strikes() == 0 }) {
		t.Fatalf("heartbeat did not resume after refresh was accepted again (hits=%d strikes=%d)",
			hits.Load(), p.Strikes())
	}
	select {
	case <-p.LostOwnership():
		t.Fatal("LostOwnership fired despite recovery")
	default:
	}
}

// TestInteractiveStopTrueStillFatal guards the deliberate exception: the
// platform's deterministic operator cancel ({"stop": true}) is honored for
// interactive sessions exactly as for every other class — non-fatality
// covers heartbeat LOSS, not explicit stops.
func TestInteractiveStopTrueStillFatal(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true, "stop": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour, // the synchronous first tick must suffice
		MaxAttemptsPerTick: 1,
		Sleep:              func(time.Duration) {},
		SessionClass:       heartbeat.SessionClassInteractive,
	})
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
		// good — explicit stop remains fatal for interactive
	case <-time.After(2 * time.Second):
		t.Fatal("LostOwnership did not fire on stop=true for interactive class")
	}
	if !p.StopRequested() {
		t.Error("StopRequested = false; runner cannot classify the operator cancel")
	}
}

// TestDeadLetterInjectReachesTheProducerAndNeverAcks is the observability half
// of ack-or-requeue.
//
// A consumer that gives up on a payload used to have exactly two ways to say
// so, and both were bad: ack it (destroying it while reporting success), or say
// nothing (leaving the sender waiting on a delivery that will never happen,
// with the single in-flight slot held forever behind it). The dead letter is
// the third: it rides back on the lock-refresh so the producer learns the push
// died AND why, while the delivery stays UNACKED so a durable producer can
// route it somewhere that can take it.
//
// Both halves are asserted, because either one alone is the old bug wearing a
// new name.
func TestDeadLetterInjectReachesTheProducerAndNeverAcks(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		reported []struct {
			DeliveryID string `json:"deliveryId"`
			Reason     string `json:"reason"`
		}
		sawAck atomic.Bool
		hits   atomic.Int64
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			AckedInject         string `json:"ackedInject"`
			DeadLetteredInjects []struct {
				DeliveryID string `json:"deliveryId"`
				Reason     string `json:"reason"`
			} `json:"deadLetteredInjects"`
		}
		_ = json.Unmarshal(body, &req)
		if req.AckedInject != "" {
			sawAck.Store(true)
		}
		if len(req.DeadLetteredInjects) > 0 {
			mu.Lock()
			reported = append(reported, req.DeadLetteredInjects...)
			mu.Unlock()
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
		OnInject:   func(heartbeat.InjectPayload) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	p.DeadLetterInject("dlv-undeliverable", "channel-not-driven")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(reported)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 {
		t.Fatal("the dead letter never reached the producer — a sender told 'queued' cannot learn it died")
	}
	if reported[0].DeliveryID != "dlv-undeliverable" || reported[0].Reason != "channel-not-driven" {
		t.Fatalf("dead letter = %+v; want the delivery id and reason verbatim", reported[0])
	}
	if sawAck.Load() {
		t.Fatal("a dead-lettered delivery was also acked — a dead letter reports a FAILURE, " +
			"so acking it destroys exactly the message it was meant to rescue")
	}
	// Once echoed on a 2xx it stops riding every subsequent request.
	if got := len(reported); got > int(hits.Load()) {
		t.Fatalf("dead letter reported %d times across %d requests — it must clear once echoed", got, hits.Load())
	}
}

// TestDeadLetterInjectIsFlushedAtStop covers the short-session gap: a session
// that gives up on a payload and then exits before the next heartbeat interval
// must still tell the producer, or the report dies with the process — the
// silent case all over again.
func TestDeadLetterInjectIsFlushedAtStop(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		reported []string
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			DeadLetteredInjects []struct {
				DeliveryID string `json:"deliveryId"`
			} `json:"deadLetteredInjects"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		for _, d := range req.DeadLetteredInjects {
			reported = append(reported, d.DeliveryID)
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		IssueID:    "i1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		// An hour: nothing but the synchronous first tick and the Stop flush
		// can carry the report, which is the case under test.
		Interval: time.Hour,
		OnInject: func(heartbeat.InjectPayload) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.DeadLetterInject("dlv-short-session", "attempt-cap-exceeded")
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 || reported[len(reported)-1] != "dlv-short-session" {
		t.Fatalf("dead letters reported = %v; want the Stop-time flush to carry dlv-short-session", reported)
	}
}

// ── Session-mortality: credentials must stay live across the pulser's whole
// lifetime, and an auth-shaped rejection must be loud ─────────────────────

// TestCredentialProviderReflectsRotationAcrossTicks proves the pulser calls
// CredentialProvider fresh on EVERY tick rather than caching the value from
// construction: the first tick must carry the pre-rotation token and a LATER
// tick — after the daemon's proactive/reactive refresh has rotated the
// worker's runtime JWT mid-session — must carry the rotated one. This is the
// ownership-pulser half of the seam that keeps a session from dying when it
// crosses an hourly token-rotation boundary.
func TestCredentialProviderReflectsRotationAcrossTicks(t *testing.T) {
	t.Parallel()

	var auths []string
	var mu sync.Mutex
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	var rotated atomic.Bool
	p, err := heartbeat.New(heartbeat.Config{
		SessionID: "s1",
		WorkerID:  "wkr_1",
		IssueID:   "issue-1",
		BaseURL:   srv.URL,
		CredentialProvider: func(context.Context) (heartbeat.RuntimeCredentials, error) {
			if rotated.Load() {
				return heartbeat.RuntimeCredentials{WorkerID: "wkr_1", AuthToken: "post-rotation-token"}, nil
			}
			return heartbeat.RuntimeCredentials{WorkerID: "wkr_1", AuthToken: "pre-rotation-token"}, nil
		},
		HTTPClient: srv.Client(),
		Interval:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// The first (synchronous) tick must have carried the PRE-rotation
	// token — proving the assertion below is a genuine before/after, not
	// an artifact of the provider always returning the same value.
	mu.Lock()
	firstAuth := auths[0]
	mu.Unlock()
	if firstAuth != "Bearer pre-rotation-token" {
		t.Fatalf("first tick Authorization = %q, want Bearer pre-rotation-token", firstAuth)
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
	t.Fatal("no tick after rotation carried the post-rotation token — a session crossing the rotation boundary would keep refusing on the stale snapshot")
}

// TestTickLogsAuthRejectionDistinctlyOnStatus401Or403 asserts that a 401/403
// on the lock-refresh POST — the wire-shape of a rotated-out worker token —
// produces a WARN log line an operator can distinguish from a generic
// outage at a glance. The pulser's normal failure handling (strike counting,
// eventual LostOwnership) is unchanged; only the log message's specificity
// is under test here.
func TestTickLogsAuthRejectionDistinctlyOnStatus401Or403(t *testing.T) {
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
				http.Error(w, "no", tc.status)
			})

			buf := &threadSafeBuffer{}
			p, err := heartbeat.New(heartbeat.Config{
				SessionID:          "s1",
				BaseURL:            srv.URL,
				HTTPClient:         srv.Client(),
				Interval:           24 * time.Hour, // one tick only
				MaxAttemptsPerTick: 1,              // no retry backoff to wait out
				Logger:             captureLogger(buf),
				Sleep:              func(time.Duration) {},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = p.Stop() })

			logs := buf.String()
			if !strings.Contains(logs, "auth rejected") {
				t.Fatalf("expected a distinct auth-rejected log line for status %d, got: %s", tc.status, logs)
			}
			if !strings.Contains(logs, `"level":"WARN"`) {
				t.Fatalf("expected the auth-rejected line at WARN level, got: %s", logs)
			}
		})
	}
}

// TestTickLogsGenericStatusForNonAuthFailures is the control: a non-auth
// failure (500) keeps its generic status-coded message, unchanged by the
// 401/403 special-case above.
func TestTickLogsGenericStatusForNonAuthFailures(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})

	buf := &threadSafeBuffer{}
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour,
		MaxAttemptsPerTick: 1,
		Logger:             captureLogger(buf),
		Sleep:              func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	logs := buf.String()
	if !strings.Contains(logs, "status 500") {
		t.Fatalf("expected the generic status-coded line for a 500, got: %s", logs)
	}
	if strings.Contains(logs, "auth rejected") {
		t.Fatalf("a 500 must not be classified as an auth rejection: %s", logs)
	}
}

// TestPulser_SetAckObserverRebinds covers the pre-provision liveness seam:
// a pulser started with no observer reports ticks nowhere; rebinding an
// observer mid-run delivers subsequent ticks to it. Nil clears the binding.
// Each phase uses a short interval plus a poll deadline instead of calling
// the unexported tick, so the test only exercises the public surface.
func TestPulser_SetAckObserverRebinds(t *testing.T) {
	t.Parallel()

	var nilPulser *heartbeat.Pulser
	nilPulser.SetAckObserver(func(int64) {}) // must not panic

	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true}`))
	}))
	t.Cleanup(srv.Close)

	p, err := heartbeat.NewWithAckObserver(heartbeat.Config{
		SessionID:  "sess-rebind",
		BaseURL:    srv.URL,
		Interval:   20 * time.Millisecond,
		HTTPClient: srv.Client(),
	}, nil)
	if err != nil {
		t.Fatalf("NewWithAckObserver: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	// Phase 1: ticks fire with no observer — posts accumulate, nothing observed.
	if !waitFor(t, 5*time.Second, func() bool { return posts.Load() >= 2 }) {
		t.Fatal("no lock-refresh posts with nil observer")
	}

	// Phase 2: rebind — the next ticks reach the new observer. A superseded
	// observer bound then immediately replaced must never fire.
	firstFired := make(chan int64, 8)
	secondFired := make(chan int64, 8)
	p.SetAckObserver(func(tick int64) { firstFired <- tick })
	p.SetAckObserver(func(tick int64) { secondFired <- tick })
	select {
	case tick := <-secondFired:
		if tick <= 0 {
			t.Fatalf("rebound observer got non-positive tick %d", tick)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rebound observer never fired")
	}
	select {
	case tick := <-firstFired:
		t.Fatalf("superseded observer fired with tick %d; want no call", tick)
	default:
	}

	// Phase 3: nil clears the binding — ticks continue (posts accumulate)
	// but no further observation fires.
	before := posts.Load()
	p.SetAckObserver(nil)
	if !waitFor(t, 5*time.Second, func() bool { return posts.Load() > before }) {
		t.Fatal("no lock-refresh posts after clearing observer")
	}
	select {
	case tick := <-secondFired:
		t.Fatalf("cleared observer fired with tick %d; want no call", tick)
	default:
	}
}

func TestDispatchTraceHeaders(t *testing.T) {
	t.Parallel()
	const parent = "00-11111111111111111111111111111111-2222222222222222-01"
	for _, tc := range []struct{ name, parent, state string }{
		{name: "both", parent: parent, state: "vendor=value"},
		{name: "parent-only", parent: parent},
		{name: "state-only", state: "vendor=value"},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var hits atomic.Int64
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				for name, want := range map[string]string{"traceparent": tc.parent, "tracestate": tc.state} {
					if got := r.Header.Values(name); (want == "" && len(got) != 0) || (want != "" && (len(got) != 1 || got[0] != want)) {
						t.Errorf("%s = %q, want %q (absent when empty)", name, got, want)
					}
				}
				_, _ = w.Write([]byte(`{"refreshed":true}`))
			})
			p, err := heartbeat.New(heartbeat.Config{
				SessionID: "session", WorkerID: "worker", BaseURL: srv.URL,
				Traceparent: tc.parent, Tracestate: tc.state,
				Interval: time.Hour, HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Stop() })
			if err := p.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

// TestLastFailureStatusRetainsTheTrippingStatus drives the production tick
// path (Start → tick → LostOwnership): three 401 ticks trip the fuse and
// the retained status is 401; a 500 server retains 500; a server that never
// answers (no HTTP response) retains 0.
func TestLastFailureStatusRetainsTheTrippingStatus(t *testing.T) {
	t.Parallel()

	t.Run("auth rejection trips with 401 retained", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusUnauthorized)
		})
		p, err := heartbeat.New(heartbeat.Config{
			SessionID:          "s1",
			BaseURL:            srv.URL,
			HTTPClient:         srv.Client(),
			Interval:           5 * time.Millisecond,
			MaxAttemptsPerTick: 1, // one attempt per tick → predictable 3-tick trip
			StrikesUntilLost:   3,
			Sleep:              func(time.Duration) {},
		})
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
			t.Fatalf("LostOwnership did not fire (strikes=%d)", p.Strikes())
		}
		if got := p.LastFailureStatus(); got != http.StatusUnauthorized {
			t.Fatalf("LastFailureStatus() = %d; want %d", got, http.StatusUnauthorized)
		}
	})

	t.Run("server error retains 500", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusInternalServerError)
		})
		p, err := heartbeat.New(heartbeat.Config{
			SessionID:          "s1",
			BaseURL:            srv.URL,
			HTTPClient:         srv.Client(),
			Interval:           5 * time.Millisecond,
			MaxAttemptsPerTick: 1,
			StrikesUntilLost:   3,
			Sleep:              func(time.Duration) {},
		})
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
			t.Fatalf("LostOwnership did not fire (strikes=%d)", p.Strikes())
		}
		if got := p.LastFailureStatus(); got != http.StatusInternalServerError {
			t.Fatalf("LastFailureStatus() = %d; want %d", got, http.StatusInternalServerError)
		}
	})

	t.Run("transport error retains 0", func(t *testing.T) {
		t.Parallel()

		// An unroutable address fails before any HTTP response exists.
		p, err := heartbeat.New(heartbeat.Config{
			SessionID:          "s1",
			BaseURL:            "http://127.0.0.1:1",
			Interval:           5 * time.Millisecond,
			MaxAttemptsPerTick: 1,
			StrikesUntilLost:   3,
			Sleep:              func(time.Duration) {},
		})
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
		case <-time.After(5 * time.Second):
			t.Fatalf("LostOwnership did not fire (strikes=%d)", p.Strikes())
		}
		if got := p.LastFailureStatus(); got != 0 {
			t.Fatalf("LastFailureStatus() = %d; want 0 (no response)", got)
		}
	})
}

// TestLostOwnershipErrorText pins the terminal-result rendering: a recorded
// status is suffixed, a zero status (or a nil pulser) keeps the bare generic
// string.
func TestLostOwnershipErrorText(t *testing.T) {
	t.Parallel()

	var nilPulser *heartbeat.Pulser
	if got := heartbeat.LostOwnershipError(nilPulser); got != heartbeat.ErrLostOwnership.Error() {
		t.Fatalf("nil pulser: got %q; want bare %q", got, heartbeat.ErrLostOwnership.Error())
	}

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour, // one tick only
		MaxAttemptsPerTick: 1,
		Sleep:              func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if got, want := p.LastFailureStatus(), http.StatusUnauthorized; got != want {
		t.Fatalf("LastFailureStatus() = %d; want %d", got, want)
	}
	errText := heartbeat.LostOwnershipError(p)
	if !strings.Contains(errText, "401") || !strings.Contains(errText, "last lock-refresh status") {
		t.Fatalf("LostOwnershipError = %q; want the 401 suffix", errText)
	}
	if !strings.Contains(errText, heartbeat.ErrLostOwnership.Error()) {
		t.Fatalf("LostOwnershipError = %q; want the generic text as prefix", errText)
	}
}

// TestFailureStatusClearedOnRecovery pins the success-path clear: a 401
// tick retains 401, then a 2xx refreshed=true tick recovers and the
// retained status returns to 0. Deleting the Store(0) next to
// strikes.Swap(0) in tick() leaves 401 behind and turns this red.
func TestFailureStatusClearedOnRecovery(t *testing.T) {
	t.Parallel()

	var phase atomic.Int64 // 0 = 401, 1 = refreshed=true
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if phase.Load() == 0 {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"refreshed":true}`)
	})
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   5, // no trip on the single failed tick
		Sleep:              func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	deadline := time.Now().Add(5 * time.Second)
	for p.LastFailureStatus() != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatalf("LastFailureStatus() = %d; want 401 after the failed tick", p.LastFailureStatus())
		}
		time.Sleep(2 * time.Millisecond)
	}

	phase.Store(1)
	deadline = time.Now().Add(5 * time.Second)
	for p.Strikes() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Strikes() = %d; want 0 after recovery", p.Strikes())
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The recovery tick ran (strikes back to 0); the retained status
	// must be cleared with it.
	if got := p.LastFailureStatus(); got != 0 {
		t.Fatalf("LastFailureStatus() = %d after recovery; want 0", got)
	}
	if got := heartbeat.LostOwnershipError(p); got != heartbeat.ErrLostOwnership.Error() {
		t.Fatalf("LostOwnershipError = %q after recovery; want bare %q", got, heartbeat.ErrLostOwnership.Error())
	}
}

// TestRefusedAfterFailureReportsBareText pins the refused-path clear: the
// hand-off closes LostOwnership from inside doRefresh, before tick()'s
// trailing store runs, so without a Store(0) before loseOwnershipNow() a
// reader racing the close stamps the stale 401 onto the hand-off result.
// The inner-retry sleep widens that window so the at-close read reliably
// observes it: deleting the refused-path Store(0) turns this red.
func TestRefusedAfterFailureReportsBareText(t *testing.T) {
	t.Parallel()

	t.Run("direct 401 then refused", func(t *testing.T) {
		t.Parallel()
		refusedAfterFailureReportsBare(t, false)
	})
	t.Run("401, recovered, then refused", func(t *testing.T) {
		t.Parallel()
		refusedAfterFailureReportsBare(t, true)
	})
}

func refusedAfterFailureReportsBare(t *testing.T, recoverFirst bool) {
	t.Helper()

	var phase atomic.Int64 // 0 = 401, 1 = refreshed=true, 2 = refused
	phase.Store(0)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		switch phase.Load() {
		case 0:
			http.Error(w, "no", http.StatusUnauthorized)
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"refreshed":true}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"refreshed":false}`)
		}
	})
	// Two attempts per tick with a real inner-retry pause: the first
	// refused attempt closes LostOwnership while the tick is still
	// parked in its retry sleep, so a reader woken by the close
	// observes the pre-store value.
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 2,
		StrikesUntilLost:   10, // never the fuse: the refused close wins
		Sleep:              func(time.Duration) { time.Sleep(50 * time.Millisecond) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	deadline := time.Now().Add(5 * time.Second)
	for p.LastFailureStatus() != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatalf("LastFailureStatus() = %d; want 401 after the failed tick", p.LastFailureStatus())
		}
		time.Sleep(2 * time.Millisecond)
	}

	if recoverFirst {
		phase.Store(1)
		deadline = time.Now().Add(5 * time.Second)
		for p.Strikes() != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("Strikes() = %d; want 0 after recovery", p.Strikes())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Record the status and text at the instant LostOwnership closes,
	// racing the tick's trailing store on purpose.
	var atClose atomic.Int64
	atClose.Store(-1)
	var closeText atomic.Value
	closeText.Store("")
	go func() {
		<-p.LostOwnership()
		atClose.Store(int64(p.LastFailureStatus()))
		closeText.Store(heartbeat.LostOwnershipError(p))
	}()

	phase.Store(2)
	select {
	case <-p.LostOwnership():
	case <-time.After(10 * time.Second):
		t.Fatalf("LostOwnership did not fire on refused refresh (strikes=%d status=%d)", p.Strikes(), p.LastFailureStatus())
	}
	deadline = time.Now().Add(5 * time.Second)
	for atClose.Load() == -1 {
		if time.Now().After(deadline) {
			t.Fatal("at-close recorder never ran")
		}
		time.Sleep(time.Millisecond)
	}
	if got := int(atClose.Load()); got != 0 {
		t.Fatalf("at-close LastFailureStatus() = %d; want 0 (bare hand-off)", got)
	}
	if got, _ := closeText.Load().(string); got != heartbeat.ErrLostOwnership.Error() {
		t.Fatalf("at-close LostOwnershipError = %q; want bare %q", got, heartbeat.ErrLostOwnership.Error())
	}
}

// TestTransportAfterAuthFailureRetainsZero pins the non-HTTP reset: one 401
// tick retains 401, then transport-error ticks trip the fuse and the
// retained status returns to 0 with the bare text. Deleting the else
// Store(0) in tick() leaves the stale 401 behind and turns this red.
func TestTransportAfterAuthFailureRetainsZero(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		// No HTTP response: hijack and close so the client sees a
		// transport error rather than a status.
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("httptest server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic(err.Error())
		}
		_ = conn.Close()
	})
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           5 * time.Millisecond,
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   3,
		Sleep:              func(time.Duration) {},
	})
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
	case <-time.After(10 * time.Second):
		t.Fatalf("LostOwnership did not fire (strikes=%d status=%d hits=%d)", p.Strikes(), p.LastFailureStatus(), hits.Load())
	}
	if got := p.LastFailureStatus(); got != 0 {
		t.Fatalf("LastFailureStatus() = %d after transport-error trip; want 0", got)
	}
	if got := heartbeat.LostOwnershipError(p); got != heartbeat.ErrLostOwnership.Error() {
		t.Fatalf("LostOwnershipError = %q; want bare %q", got, heartbeat.ErrLostOwnership.Error())
	}
}
