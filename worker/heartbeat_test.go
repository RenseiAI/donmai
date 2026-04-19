package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newHeartbeatTestClient returns a Client pointed at srv with WorkerID and
// RuntimeJWT pre-populated so Heartbeat can be exercised directly.
func newHeartbeatTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.URL, "rsp_live_test")
	c.RuntimeJWT = "rjwt_test"
	c.WorkerID = "w_hb1"
	return c
}

func TestHeartbeat_Success(t *testing.T) {
	var (
		gotAuth, gotMethod, gotPath, gotCT string
		gotBody                            HeartbeatRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("server decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ack":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)
	req := HeartbeatRequest{ActiveAgentCount: 7, Status: "busy"}

	resp, err := c.Heartbeat(context.Background(), req)
	if err != nil {
		t.Fatalf("Heartbeat error: %v", err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	if !resp.Ack {
		t.Errorf("resp.Ack = false, want true")
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	wantPath := "/api/workers/" + c.WorkerID + "/heartbeat"
	if gotPath != wantPath {
		t.Errorf("path = %s, want %s", gotPath, wantPath)
	}
	if !strings.Contains(gotPath, c.WorkerID) {
		t.Errorf("path = %s, want to contain worker id %s", gotPath, c.WorkerID)
	}
	if gotAuth != "Bearer rjwt_test" {
		t.Errorf("Authorization = %q, want Bearer rjwt_test", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody.ActiveAgentCount != 7 {
		t.Errorf("body.ActiveAgentCount = %d, want 7", gotBody.ActiveAgentCount)
	}
	if gotBody.Status != "busy" {
		t.Errorf("body.Status = %q, want busy", gotBody.Status)
	}
}

func TestHeartbeat_StatusErrorsTable(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"401 runtime expired", http.StatusUnauthorized, ErrRuntimeJWTExpired},
		{"429 rate limited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, ErrHeartbeatFailed},
		{"503 service unavailable", http.StatusServiceUnavailable, ErrHeartbeatFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)

			c := newHeartbeatTestClient(srv)
			beforeID := c.WorkerID
			beforeJWT := c.RuntimeJWT

			resp, err := c.Heartbeat(context.Background(), HeartbeatRequest{ActiveAgentCount: 1})
			if err == nil {
				t.Fatalf("expected error, got nil resp=%+v", resp)
			}
			if resp != nil {
				t.Errorf("resp = %+v, want nil", resp)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false (err = %v)", tc.wantErr, err)
			}
			// 5xx must NOT leak ErrServerError to callers.
			if tc.wantErr == ErrHeartbeatFailed && errors.Is(err, ErrServerError) {
				t.Errorf("5xx error should be remapped away from ErrServerError, got %v", err)
			}
			// State must be unchanged even on failure.
			if c.WorkerID != beforeID {
				t.Errorf("WorkerID mutated: before=%q after=%q", beforeID, c.WorkerID)
			}
			if c.RuntimeJWT != beforeJWT {
				t.Errorf("RuntimeJWT mutated: before=%q after=%q", beforeJWT, c.RuntimeJWT)
			}
		})
	}
}

func TestHeartbeat_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ack":tru`)) // truncated, invalid JSON
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)
	resp, err := c.Heartbeat(context.Background(), HeartbeatRequest{ActiveAgentCount: 0})
	if err == nil {
		t.Fatalf("expected decode error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want to mention decode", err)
	}
}

func TestHeartbeat_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Do

	resp, err := c.Heartbeat(ctx, HeartbeatRequest{ActiveAgentCount: 0})
	if err == nil {
		t.Fatalf("expected context error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false (err = %v)", err)
	}
}

func TestHeartbeat_EmptyWorkerID(t *testing.T) {
	// Server must not be hit. Track via atomic counter.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "rsp_live_test")
	c.RuntimeJWT = "rjwt_test"
	// Intentionally leave c.WorkerID empty.

	resp, err := c.Heartbeat(context.Background(), HeartbeatRequest{ActiveAgentCount: 0})
	if err == nil {
		t.Fatalf("expected error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	if !strings.Contains(err.Error(), "worker not registered") {
		t.Errorf("err = %v, want to contain 'worker not registered'", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("server hit %d times, want 0", hits)
	}
}

func TestHeartbeatLoop_FiresOnInterval(t *testing.T) {
	var hbCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hbCount, 1)
		_, _ = w.Write([]byte(`{"ack":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)

	var counterCalls int32
	counter := func() int {
		atomic.AddInt32(&counterCalls, 1)
		return 3
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- c.HeartbeatLoop(ctx, 10*time.Millisecond, counter)
	}()

	// Let 2-3 ticks fire then cancel.
	time.Sleep(45 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HeartbeatLoop returned err = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("HeartbeatLoop did not return after cancel")
	}

	hbs := atomic.LoadInt32(&hbCount)
	if hbs < 2 {
		t.Errorf("hbCount = %d, want >= 2", hbs)
	}
	calls := atomic.LoadInt32(&counterCalls)
	if calls < hbs {
		t.Errorf("counterCalls = %d, want >= hbCount = %d", calls, hbs)
	}
}

func TestHeartbeatLoop_CtxCancelledReturnsNilWithinOneTick(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ack":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)

	interval := 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- c.HeartbeatLoop(ctx, interval, func() int { return 0 })
	}()

	// Cancel immediately.
	cancel()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("HeartbeatLoop returned err = %v, want nil", err)
		}
		// Must return within roughly one tick (allow generous slack).
		if elapsed > 2*interval {
			t.Errorf("HeartbeatLoop took %v to return, want <= %v", elapsed, 2*interval)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("HeartbeatLoop did not return within 1s of cancel")
	}
}

func TestHeartbeatLoop_401ReturnsRuntimeJWTExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- c.HeartbeatLoop(ctx, 10*time.Millisecond, func() int { return 0 })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("HeartbeatLoop returned nil, want ErrRuntimeJWTExpired")
		}
		if !errors.Is(err, ErrRuntimeJWTExpired) {
			t.Errorf("errors.Is(err, ErrRuntimeJWTExpired) = false (err = %v)", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("HeartbeatLoop did not exit on 401 within 1s")
	}
}

func TestHeartbeatLoop_TransientServerErrorsKeepGoing(t *testing.T) {
	// First two responses are 500s, subsequent are successful. Loop must
	// survive the errors and keep ticking.
	var reqCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&reqCount, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ack":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newHeartbeatTestClient(srv)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- c.HeartbeatLoop(ctx, 10*time.Millisecond, func() int { return 0 })
	}()

	// Let several ticks fire so we can observe survival past the initial
	// transient errors.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HeartbeatLoop returned err = %v, want nil after transient 500s", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("HeartbeatLoop did not return after cancel")
	}

	got := atomic.LoadInt32(&reqCount)
	// Must have ticked well past the initial two failing requests.
	if got < 4 {
		t.Errorf("reqCount = %d, want >= 4 (survives past transient 500s)", got)
	}
}
