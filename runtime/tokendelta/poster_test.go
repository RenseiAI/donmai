package tokendelta

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// captureServer records every token-delta POST body it receives.
type captureServer struct {
	mu     sync.Mutex
	bodies []requestBody
	auths  []string
}

func (c *captureServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body requestBody
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (c *captureServer) snapshot() ([]requestBody, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := make([]requestBody, len(c.bodies))
	copy(b, c.bodies)
	a := make([]string, len(c.auths))
	copy(a, c.auths)
	return b, a
}

func (c *captureServer) totalFrames() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.bodies {
		n += len(b.Frames)
	}
	return n
}

func newTestPoster(t *testing.T, url string, cfg Config) *Poster {
	t.Helper()
	cfg.SessionID = "sess-1"
	cfg.WorkerID = "worker-1"
	cfg.BaseURL = url
	cfg.AuthToken = "rsk_test"
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestPoster_FlushOn20Frames verifies the 20-frame batching threshold: 20
// frames sent in a tight burst force exactly one flush without waiting for
// the interval timer.
func TestPoster_FlushOn20Frames(t *testing.T) {
	srvCap := &captureServer{}
	srv := httptest.NewServer(srvCap.handler())
	defer srv.Close()

	// Long interval so only the frame-count threshold can trigger the flush.
	p := newTestPoster(t, srv.URL, Config{
		FlushInterval: time.Hour,
		FlushFrames:   20,
		HTTPClient:    srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = p.Start(ctx)
	p.SetTurn("turn-1")

	for i := 0; i < 20; i++ {
		p.Send(Frame{Index: i, Text: "tok"})
	}

	// The 20th frame triggers a flush; poll for it.
	waitForFrames(t, srvCap, 20, 2*time.Second)

	bodies, auths := srvCap.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly 1 batched POST, got %d", len(bodies))
	}
	if len(bodies[0].Frames) != 20 {
		t.Fatalf("expected 20 frames in the batch, got %d", len(bodies[0].Frames))
	}
	if bodies[0].TurnID != "turn-1" {
		t.Fatalf("turnId = %q; want turn-1", bodies[0].TurnID)
	}
	if auths[0] != "Bearer rsk_test" {
		t.Fatalf("auth header = %q; want Bearer rsk_test", auths[0])
	}
	_ = p.Stop()
}

// TestPoster_FlushOnInterval verifies the 100ms time-based flush: a single
// frame (below the 20-frame threshold) is flushed when the interval timer
// fires.
func TestPoster_FlushOnInterval(t *testing.T) {
	srvCap := &captureServer{}
	srv := httptest.NewServer(srvCap.handler())
	defer srv.Close()

	p := newTestPoster(t, srv.URL, Config{
		FlushInterval: 20 * time.Millisecond,
		FlushFrames:   1000, // unreachable — force the interval path
		HTTPClient:    srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = p.Start(ctx)
	p.SetTurn("turn-int")

	p.Send(Frame{Index: 0, Text: "single"})

	waitForFrames(t, srvCap, 1, 2*time.Second)
	bodies, _ := srvCap.snapshot()
	if len(bodies) == 0 || len(bodies[0].Frames) != 1 {
		t.Fatalf("expected one frame flushed on the interval, got %v", bodies)
	}
	_ = p.Stop()
}

// TestPoster_DoneFrameForcesFlush verifies a terminal (done) frame flushes
// immediately so the turn's final token reaches the browser promptly.
func TestPoster_DoneFrameForcesFlush(t *testing.T) {
	srvCap := &captureServer{}
	srv := httptest.NewServer(srvCap.handler())
	defer srv.Close()

	p := newTestPoster(t, srv.URL, Config{
		FlushInterval: time.Hour, // only the done-frame path can flush
		FlushFrames:   1000,
		HTTPClient:    srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = p.Start(ctx)
	p.SetTurn("turn-done")

	p.Send(Frame{Index: 0, Text: "hi"})
	p.Send(Frame{Index: 1, Text: "", Done: true})

	waitForFrames(t, srvCap, 2, 2*time.Second)
	bodies, _ := srvCap.snapshot()
	total := 0
	for _, b := range bodies {
		total += len(b.Frames)
	}
	if total != 2 {
		t.Fatalf("expected 2 frames flushed by the done frame, got %d", total)
	}
	_ = p.Stop()
}

// TestPoster_StopFlushesTail verifies Stop flushes any buffered frames that
// never reached a threshold.
func TestPoster_StopFlushesTail(t *testing.T) {
	srvCap := &captureServer{}
	srv := httptest.NewServer(srvCap.handler())
	defer srv.Close()

	p := newTestPoster(t, srv.URL, Config{
		FlushInterval: time.Hour, // never fires during the test
		FlushFrames:   1000,
		HTTPClient:    srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = p.Start(ctx)
	p.SetTurn("turn-tail")

	p.Send(Frame{Index: 0, Text: "leftover"})
	_ = p.Stop() // must flush the single buffered frame

	if got := srvCap.totalFrames(); got != 1 {
		t.Fatalf("expected Stop to flush 1 tail frame, got %d", got)
	}
}

// TestPoster_New_RequiresFields asserts the constructor validation.
func TestPoster_New_RequiresFields(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Fatal("expected error when SessionID is missing")
	}
	if _, err := New(Config{SessionID: "s"}); err == nil {
		t.Fatal("expected error when BaseURL is missing")
	}
}

// TestPoster_SendBeforeStartDropped verifies Send is a no-op before Start
// (the worker isn't draining yet, so a frame would otherwise leak into the
// buffered channel and surprise the first flush).
func TestPoster_SendBeforeStartDropped(t *testing.T) {
	srvCap := &captureServer{}
	srv := httptest.NewServer(srvCap.handler())
	defer srv.Close()

	p := newTestPoster(t, srv.URL, Config{HTTPClient: srv.Client()})
	// Send before Start — dropped.
	p.Send(Frame{Index: 0, Text: "early"})

	if got := len(p.queue); got != 0 {
		t.Fatalf("expected pre-Start Send to be dropped, queue len = %d", got)
	}
}

// waitForFrames polls srvCap until it has received at least n frames or the
// deadline elapses.
func waitForFrames(t *testing.T, srvCap *captureServer, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if srvCap.totalFrames() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frames; got %d", n, srvCap.totalFrames())
}
