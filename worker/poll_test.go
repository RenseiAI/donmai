package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newRegisteredClient returns a Client pointed at srv with WorkerID and
// RuntimeJWT pre-populated as if a Register call had already succeeded.
func newRegisteredClient(srv *httptest.Server) *Client {
	c := NewClient(srv.URL, "rsp_live_test")
	c.WorkerID = "w_abc123"
	c.RuntimeJWT = "rjwt_xyz"
	return c
}

func TestPoll_Success(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	payload := json.RawMessage(`{"k":"v"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		resp := PollResponse{
			WorkItems: []WorkItem{
				{ID: "wi_1", Type: "session.start", Payload: payload, CreatedAt: time.Unix(1700000000, 0).UTC()},
				{ID: "wi_2", Type: "session.stop", Payload: payload, CreatedAt: time.Unix(1700000001, 0).UTC()},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	resp, err := c.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll error: %v", err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	if len(resp.WorkItems) != 2 {
		t.Fatalf("len(WorkItems) = %d, want 2", len(resp.WorkItems))
	}
	if resp.WorkItems[0].ID != "wi_1" || resp.WorkItems[1].ID != "wi_2" {
		t.Errorf("item ids = %q, %q", resp.WorkItems[0].ID, resp.WorkItems[1].ID)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/api/workers/w_abc123/poll" {
		t.Errorf("path = %s, want /api/workers/w_abc123/poll", gotPath)
	}
	if gotAuth != "Bearer rjwt_xyz" {
		t.Errorf("Authorization = %q, want Bearer rjwt_xyz", gotAuth)
	}
}

func TestPoll_EmptyWorkItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work_items": []}`))
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	resp, err := c.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll error: %v", err)
	}
	if resp == nil {
		t.Fatal("resp is nil")
	}
	if len(resp.WorkItems) != 0 {
		t.Errorf("len(WorkItems) = %d, want 0", len(resp.WorkItems))
	}
}

func TestPoll_StatusErrorsTable(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"401 runtime jwt expired", http.StatusUnauthorized, ErrRuntimeJWTExpired},
		{"404 not found", http.StatusNotFound, ErrNotFound},
		{"429 rate limited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, ErrPollFailed},
		{"503 service unavailable", http.StatusServiceUnavailable, ErrPollFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)

			c := newRegisteredClient(srv)
			resp, err := c.Poll(context.Background())
			if err == nil {
				t.Fatalf("expected error, got nil resp=%+v", resp)
			}
			if resp != nil {
				t.Errorf("resp = %+v, want nil", resp)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false (err = %v)", tc.wantErr, err)
			}
			if tc.wantErr == ErrPollFailed && errors.Is(err, ErrServerError) {
				t.Errorf("5xx error should be remapped away from ErrServerError, got %v", err)
			}
		})
	}
}

func TestPoll_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work_items": [`)) // truncated JSON
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	resp, err := c.Poll(context.Background())
	if err == nil {
		t.Fatalf("expected decode error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	if !containsSubstr(err.Error(), "poll:") {
		t.Errorf("err = %v, want to contain 'poll:' prefix", err)
	}
}

func TestPoll_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Do

	resp, err := c.Poll(ctx)
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

func TestPoll_EmptyWorkerID(t *testing.T) {
	hit := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hit, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "rsp_live_test")
	// WorkerID intentionally left empty.
	resp, err := c.Poll(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
	if !containsSubstr(err.Error(), "worker not registered") {
		t.Errorf("err = %v, want to contain 'worker not registered'", err)
	}
	if n := atomic.LoadInt32(&hit); n != 0 {
		t.Errorf("server hit %d times, want 0", n)
	}
}

func TestPollLoop_FiresAtInterval(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&polls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work_items": []}`))
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- c.PollLoop(ctx, 10*time.Millisecond, func(_ WorkItem) error { return nil })
	}()

	// Let 2-3 ticks fire.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PollLoop err = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PollLoop did not return after ctx cancel")
	}

	if n := atomic.LoadInt32(&polls); n < 2 {
		t.Errorf("polls = %d, want >= 2", n)
	}
}

func TestPollLoop_CtxCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work_items": []}`))
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	interval := 20 * time.Millisecond
	go func() {
		done <- c.PollLoop(ctx, interval, func(_ WorkItem) error { return nil })
	}()

	cancel()

	// Must shut down within one tick.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PollLoop err = %v, want nil", err)
		}
	case <-time.After(interval + 200*time.Millisecond):
		t.Fatal("PollLoop did not return within one tick of ctx cancel")
	}
}

func TestPollLoop_RuntimeJWTExpiredExitsLoop(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&polls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- c.PollLoop(ctx, 10*time.Millisecond, func(_ WorkItem) error { return nil })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PollLoop returned nil, want error wrapping ErrRuntimeJWTExpired")
		}
		if !errors.Is(err, ErrRuntimeJWTExpired) {
			t.Errorf("errors.Is(err, ErrRuntimeJWTExpired) = false (err = %v)", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("PollLoop did not exit on 401")
	}

	if n := atomic.LoadInt32(&polls); n < 1 {
		t.Errorf("polls = %d, want >= 1", n)
	}
}

func TestPollLoop_HandlerErrorDoesNotStopLoop(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&polls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work_items":[{"id":"wi_1","type":"x","payload":{},"created_at":"2025-01-01T00:00:00Z"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var handlerCalls int32
	handler := func(_ WorkItem) error {
		atomic.AddInt32(&handlerCalls, 1)
		return errors.New("handler boom")
	}

	done := make(chan error, 1)
	go func() {
		done <- c.PollLoop(ctx, 10*time.Millisecond, handler)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PollLoop err = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PollLoop did not return after ctx cancel")
	}

	if n := atomic.LoadInt32(&handlerCalls); n < 2 {
		t.Errorf("handlerCalls = %d, want >= 2 (loop should have continued past handler errors)", n)
	}
	if n := atomic.LoadInt32(&polls); n < 2 {
		t.Errorf("polls = %d, want >= 2", n)
	}
}

func TestPollLoop_TransientServerErrorsKeepGoing(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&polls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- c.PollLoop(ctx, 10*time.Millisecond, func(_ WorkItem) error { return nil })
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PollLoop err = %v, want nil (5xx should not stop the loop)", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PollLoop did not return after ctx cancel")
	}

	if n := atomic.LoadInt32(&polls); n < 2 {
		t.Errorf("polls = %d, want >= 2 (loop should have continued past 5xx)", n)
	}
}
