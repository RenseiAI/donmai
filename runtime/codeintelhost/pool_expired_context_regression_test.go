package codeintelhost

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Both the existing workarea's readiness and the caller's deadline are settled
// before Acquire starts. No sleep or timer scheduling establishes the witness.
func TestPoolAcquireExpiredWarmContextReturnsDeadline(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatal(err)
	}
	binding := validBinding()
	healthyLease, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(healthyLease.Release)

	expired, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	t.Cleanup(cancel)
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatalf("witness context error=%v", expired.Err())
	}
	select {
	case <-expired.Done():
	default:
		t.Fatal("witness deadline channel not closed")
	}
	pool.mu.Lock()
	warmed := pool.entries[binding.Key()]
	pool.mu.Unlock()
	if warmed == nil {
		t.Fatal("witness warm entry missing")
	}
	select {
	case <-warmed.warmDone:
	default:
		t.Fatal("witness warm channel not closed")
	}

	// Exercise the real public Acquire seam against the settled two-ready state.
	// The repetition bounds the select tie witness; it adds no timing tolerance.
	for attempt := 0; attempt < 64; attempt++ {
		lease, acquireErr := pool.Acquire(expired, binding)
		if lease != nil {
			lease.Release()
		}
		if lease != nil || !errors.Is(acquireErr, context.DeadlineExceeded) {
			t.Fatalf("settled expired warm Acquire attempt=%d returned lease=%v error=%v, want nil lease and context deadline error", attempt, lease != nil, acquireErr)
		}
	}

	pool.mu.Lock()
	refs := pool.entries[binding.Key()].refs
	pool.mu.Unlock()
	if refs != 1 {
		t.Fatalf("expired waiter changed healthy lease refs=%d, want 1", refs)
	}
	survivor, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("healthy waiter after expired calls: %v", err)
	}
	survivor.Release()
	if got := factory.creates(); got != 1 {
		t.Fatalf("factory.Create called %d times, want original single warm", got)
	}
}

// The gate factory keeps actual shared warm completion behind a test-owned
// barrier. Caller cancellation is observed on the real provisioning context.
type acquireWarmGateFactory struct {
	base    *fakeFactory
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (f *acquireWarmGateFactory) Create(ctx context.Context, binding Binding) (ToolCaller, io.Closer, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
		return f.base.Create(ctx, binding)
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// Observing Done preserves the wrapped context's values/cancellation contract
// and signals that the surviving caller entered the real warm-wait select.
type acquireWaitEnteredContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *acquireWaitEnteredContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestPoolCanceledTriggerPreservesOtherWaiterWarm(t *testing.T) {
	t.Parallel()
	gate := &acquireWarmGateFactory{base: newFakeFactory(), entered: make(chan struct{}), release: make(chan struct{})}
	pool, err := NewPool(gate, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatal(err)
	}
	binding := validBinding()
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	t.Cleanup(cancel)
	type result struct {
		lease *Lease
		err   error
	}
	triggering := make(chan result, 1)
	go func() { lease, err := pool.Acquire(short, binding); triggering <- result{lease, err} }()
	<-gate.entered
	background := &acquireWaitEnteredContext{Context: context.Background(), entered: make(chan struct{})}
	surviving := make(chan result, 1)
	go func() { lease, err := pool.Acquire(background, binding); surviving <- result{lease, err} }()
	<-background.entered
	first := <-triggering
	if first.lease != nil {
		first.lease.Release()
	}
	if first.lease != nil || !errors.Is(first.err, context.DeadlineExceeded) {
		close(gate.release)
		t.Fatalf("triggering Acquire lease=%v error=%v, want deadline before warm completes", first.lease != nil, first.err)
	}
	close(gate.release)
	second := <-surviving
	if second.err != nil || second.lease == nil {
		t.Fatalf("surviving Acquire lease=%v error=%v, want the shared warm to complete", second.lease != nil, second.err)
	}
	second.lease.Release()
	if got := gate.calls.Load(); got != 1 {
		t.Fatalf("factory.Create called %d times, want one shared completed warm", got)
	}
}
