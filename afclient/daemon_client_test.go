package afclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDaemonClientDrainContextAllowsLongerDrain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/drain" {
			http.NotFound(w, r)
			return
		}
		var req DaemonDrainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode drain request: %v", err)
			return
		}
		if req.TimeoutSeconds != 1 {
			t.Errorf("timeoutSeconds = %d, want 1", req.TimeoutSeconds)
			return
		}
		time.Sleep(75 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(DaemonActionResponse{OK: true, Message: "drained"})
	}))
	t.Cleanup(srv.Close)

	client := NewDaemonClientFromURL(srv.URL)
	client.httpClient.Timeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	resp, err := client.DrainContext(ctx, 1)
	if err != nil {
		t.Fatalf("DrainContext: %v", err)
	}
	if resp == nil || !resp.OK {
		t.Fatalf("DrainContext response = %#v, want successful response", resp)
	}
}

func TestDaemonClientDrainContextCancelsPromptly(t *testing.T) {
	started := make(chan struct{})
	handlerCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(handlerCanceled)
	}))
	t.Cleanup(srv.Close)

	client := NewDaemonClientFromURL(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.DrainContext(ctx, 1)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain handler did not start")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DrainContext error = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("DrainContext did not return after cancellation")
	}
	select {
	case <-handlerCanceled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain handler did not observe cancellation")
	}
}
