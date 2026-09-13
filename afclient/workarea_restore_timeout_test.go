package afclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDaemonClientRestoreWorkareaOutlivesOrdinaryTransportTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path != "/api/daemon/workareas/wa-slow/restore" &&
			r.URL.Path != "/api/daemon/workareas/wa-slow-v1/restore") || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		time.Sleep(75 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(WorkareaRestoreResult{Workarea: Workarea{ID: "wa-restored"}})
	}))
	t.Cleanup(server.Close)

	client := NewDaemonClientFromURL(server.URL)
	client.httpClient.Timeout = 20 * time.Millisecond
	result, err := client.RestoreWorkarea("wa-slow", WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("valid restore inherited ordinary transport timeout: %v", err)
	}
	if result.Workarea.ID != "wa-restored" {
		t.Fatalf("restored workarea = %+v", result.Workarea)
	}
	versioned, err := client.RestoreWorkareaV1("wa-slow-v1", WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("valid v1 restore inherited ordinary transport timeout: %v", err)
	}
	if versioned.Workarea.ID != "wa-restored" {
		t.Fatalf("restored v1 workarea = %+v", versioned.Workarea)
	}
	if client.httpClient.Timeout != 20*time.Millisecond {
		t.Fatalf("restore mutated shared client timeout to %s", client.httpClient.Timeout)
	}
}

func TestDaemonClientRestoreWorkareaContextPreservesCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(server.Close)

	client := NewDaemonClientFromURL(server.URL)
	client.httpClient.Timeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.RestoreWorkareaContext(ctx, "wa-cancel", WorkareaRestoreRequest{})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restore cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restore ignored caller cancellation")
	}
	close(release)
}
