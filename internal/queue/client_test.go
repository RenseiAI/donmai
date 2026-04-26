package queue_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/RenseiAI/agentfactory-tui/internal/queue"
)

// newTestClient starts a miniredis server and returns a connected Client plus
// a stop function. The stop function closes the server but NOT the client —
// use it to simulate server-unavailable scenarios.
func newTestClient(t *testing.T) (*queue.Client, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := queue.NewClient("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("NewClient: unexpected error: %v", err)
	}
	return c, mr.Close
}

func TestNewClient_InvalidURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{name: "empty URL", url: ""},
		{name: "malformed URL", url: "not-a-valid-redis-url://???"},
		{name: "unsupported scheme", url: "http://localhost:6379"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := queue.NewClient(tc.url)
			if c != nil {
				_ = c.Close()
				t.Fatal("expected nil client")
			}
			if !errors.Is(err, queue.ErrInvalidRedisURL) {
				t.Fatalf("expected ErrInvalidRedisURL, got %v", err)
			}
		})
	}
}

func TestPing_Success(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: unexpected error: %v", err)
	}
}

func TestPing_ServerStopped(t *testing.T) {
	t.Parallel()
	c, stop := newTestClient(t)
	defer func() { _ = c.Close() }()

	// Stop the server before pinging.
	stop()

	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error after server stopped, got nil")
	}
	if !errors.Is(err, queue.ErrRedisUnavailable) {
		t.Fatalf("expected ErrRedisUnavailable, got %v", err)
	}
}

func TestEnqueuePeek_Roundtrip(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payload := []byte(`{"task":"dispatch","id":42}`)

	if err := c.Enqueue(ctx, payload); err != nil {
		t.Fatalf("Enqueue: unexpected error: %v", err)
	}

	got, err := c.Peek(ctx)
	if err != nil {
		t.Fatalf("Peek: unexpected error: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("Peek: got %q, want %q", got, payload)
	}
}

func TestPeek_EmptyQueue(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	_, err := c.Peek(context.Background())
	if !errors.Is(err, queue.ErrEmptyQueue) {
		t.Fatalf("expected ErrEmptyQueue, got %v", err)
	}
}

func TestEnqueuePeek_MultipleItems(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	items := [][]byte{
		[]byte("first"),
		[]byte("second"),
		[]byte("third"),
	}
	for _, item := range items {
		if err := c.Enqueue(ctx, item); err != nil {
			t.Fatalf("Enqueue(%q): unexpected error: %v", item, err)
		}
	}

	// Peek must return the head (first inserted) without removal.
	got, err := c.Peek(ctx)
	if err != nil {
		t.Fatalf("Peek: unexpected error: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("Peek: got %q, want %q", got, "first")
	}

	// Second Peek must still return the same item.
	got2, err := c.Peek(ctx)
	if err != nil {
		t.Fatalf("Peek (second): unexpected error: %v", err)
	}
	if string(got2) != "first" {
		t.Fatalf("Peek (second): got %q, want %q", got2, "first")
	}
}

func TestIncrDispatchCounter(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	key := "test:counter"

	for i := int64(1); i <= 3; i++ {
		val, err := c.IncrDispatchCounter(ctx, key)
		if err != nil {
			t.Fatalf("IncrDispatchCounter: unexpected error: %v", err)
		}
		if val != i {
			t.Fatalf("IncrDispatchCounter: got %d, want %d", val, i)
		}
	}
}

func TestGetDispatchCounter_MissingKey(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	val, err := c.GetDispatchCounter(context.Background(), "nonexistent:key")
	if err != nil {
		t.Fatalf("GetDispatchCounter: unexpected error: %v", err)
	}
	if val != 0 {
		t.Fatalf("GetDispatchCounter: got %d, want 0 for missing key", val)
	}
}

func TestGetDispatchCounter_AfterIncr(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	key := "test:get:counter"

	if _, err := c.IncrDispatchCounter(ctx, key); err != nil {
		t.Fatalf("IncrDispatchCounter: %v", err)
	}
	if _, err := c.IncrDispatchCounter(ctx, key); err != nil {
		t.Fatalf("IncrDispatchCounter: %v", err)
	}

	val, err := c.GetDispatchCounter(ctx, key)
	if err != nil {
		t.Fatalf("GetDispatchCounter: %v", err)
	}
	if val != 2 {
		t.Fatalf("GetDispatchCounter: got %d, want 2", val)
	}
}
