package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPollBeginStopCancelsWithoutJoiningAndReusesDone(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{{SessionID: "blocked"}}})
	}))
	t.Cleanup(srv.Close)

	p := NewPollService(PollOptions{
		WorkerID:        "wkr-begin-stop",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt",
		IntervalSeconds: 1,
		OnWork: func(PollWorkItem) error {
			close(entered)
			<-release // deliberately ignores the poll cancellation
			return nil
		},
	})
	p.Start()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = p.StopContext(context.Background())
	})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("poll callback did not enter")
	}

	beginReturned := make(chan (<-chan struct{}), 1)
	go func() { beginReturned <- p.beginStop() }()
	var done <-chan struct{}
	select {
	case done = <-beginReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("beginStop waited for blocked OnWork callback")
	}
	if done == nil {
		t.Fatal("beginStop returned nil completion channel for active poll loop")
	}
	if repeated := p.beginStop(); repeated != done {
		t.Fatal("repeated beginStop did not reuse the active poll completion channel")
	}
	select {
	case <-done:
		t.Fatal("poll completion closed while OnWork callback remained blocked")
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.StopContext(ctx); err != context.DeadlineExceeded {
		t.Fatalf("StopContext on shared completion channel = %v, want deadline exceeded", err)
	}

	releaseOnce.Do(func() { close(release) })
	if err := p.StopContext(context.Background()); err != nil {
		t.Fatalf("StopContext after callback release: %v", err)
	}
}
