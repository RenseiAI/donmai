package ptycli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

type seedTestSession struct {
	agent.InteractiveSession
	mu       sync.Mutex
	bytes    []byte
	maxWrite int
	err      error
	started  chan struct{}
	release  chan struct{}
}

func (s *seedTestSession) WriteInput(p []byte) (int, error) {
	if s.started != nil {
		select {
		case <-s.started:
		default:
			close(s.started)
		}
		<-s.release
	}
	limit := len(p)
	if s.maxWrite > 0 && limit > s.maxWrite {
		limit = s.maxWrite
	}
	s.mu.Lock()
	s.bytes = append(s.bytes, p[:limit]...)
	s.mu.Unlock()
	return limit, s.err
}

type seedTestHandle struct {
	events  chan agent.Event
	stops   atomic.Int64
	release chan struct{}
}

func (h *seedTestHandle) SessionID() string                    { return "" }
func (h *seedTestHandle) Events() <-chan agent.Event           { return h.events }
func (h *seedTestHandle) Inject(context.Context, string) error { return agent.ErrUnsupported }
func (h *seedTestHandle) Stop(context.Context) error {
	h.stops.Add(1)
	if h.release != nil {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	}
	return nil
}

func TestDeliverSeed_ExactBytesAndShortWrites(t *testing.T) {
	t.Parallel()
	session := &seedTestSession{maxWrite: 2}
	handle := &seedTestHandle{events: make(chan agent.Event)}
	if err := DeliverSeed(context.Background(), handle, session, "  雪\nnext  "); err != nil {
		t.Fatalf("DeliverSeed: %v", err)
	}
	session.mu.Lock()
	got := string(session.bytes)
	session.mu.Unlock()
	if want := "  雪\nnext  \n"; got != want {
		t.Fatalf("PTY seed bytes = %q, want %q", got, want)
	}
	if got := handle.stops.Load(); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
}

func TestDeliverSeed_PartialErrorStopsAndFails(t *testing.T) {
	t.Parallel()
	session := &seedTestSession{maxWrite: 2, err: errors.New("closed")}
	handle := &seedTestHandle{events: make(chan agent.Event)}
	err := DeliverSeed(context.Background(), handle, session, "seed")
	if err == nil || !errors.Is(err, session.err) {
		t.Fatalf("DeliverSeed error = %v, want closed", err)
	}
	if got := handle.stops.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestDeliverSeed_CancellationStopsBlockedWrite(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	session := &seedTestSession{started: started, release: release}
	handle := &seedTestHandle{events: make(chan agent.Event), release: release}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- DeliverSeed(ctx, handle, session, "seed") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("seed write did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DeliverSeed error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock seed delivery")
	}
	if got := handle.stops.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}
