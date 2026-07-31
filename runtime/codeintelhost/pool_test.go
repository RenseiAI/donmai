package codeintelhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// fakeToolCaller is a minimal ToolCaller stand-in: no real Git checkout or
// mcpserver.Server is needed to exercise Pool's admission, single-flight,
// eviction, and drain behavior.
type fakeToolCaller struct {
	key string
}

func (f *fakeToolCaller) Call(_ context.Context, name string, _ json.RawMessage) (mcpserver.ToolResult, error) {
	return mcpserver.ToolResult{Content: []mcpserver.ContentItem{{Type: "text", Text: f.key + ":" + name}}}, nil
}

func (f *fakeToolCaller) WaitReady(context.Context) error { return nil }

// fakeCloser records close order on the owning fakeFactory.
type fakeCloser struct {
	factory *fakeFactory
	key     string
}

func (c *fakeCloser) Close() error {
	c.factory.mu.Lock()
	c.factory.closedKeys = append(c.factory.closedKeys, c.key)
	c.factory.mu.Unlock()
	return nil
}

// fakeFactory is a controllable Factory: it counts Create calls, can inject
// a one-shot failure for a given binding key, can add an artificial delay to
// Create so tests can exercise cancellation and single-flight overlap
// deterministically, and can substitute a custom ToolCaller (e.g. a
// handler-level blocking caller) in place of the default fakeToolCaller.
type fakeFactory struct {
	mu         sync.Mutex
	closedKeys []string
	failOnce   map[string]error
	delay      time.Duration
	caller     ToolCaller

	createCount int32
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{failOnce: map[string]error{}}
}

func (f *fakeFactory) Create(ctx context.Context, binding Binding) (ToolCaller, io.Closer, error) {
	atomic.AddInt32(&f.createCount, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	f.mu.Lock()
	err, ok := f.failOnce[binding.Key()]
	if ok {
		delete(f.failOnce, binding.Key())
	}
	caller := f.caller
	f.mu.Unlock()
	if ok {
		return nil, nil, err
	}
	if caller == nil {
		caller = &fakeToolCaller{key: binding.Key()}
	}
	return caller, &fakeCloser{factory: f, key: binding.Key()}, nil
}

func (f *fakeFactory) creates() int {
	return int(atomic.LoadInt32(&f.createCount))
}

func (f *fakeFactory) wasClosed(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.closedKeys {
		if k == key {
			return true
		}
	}
	return false
}

func bindingWithRevision(label string) Binding {
	b := validBinding()
	b.Revision = fullObjectID(label)
	return b
}

func TestPoolSingleFlightWarmReuse(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 4})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	binding := validBinding()

	const n = 8
	var wg sync.WaitGroup
	leases := make([]*Lease, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = pool.Acquire(context.Background(), binding)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Acquire()[%d] error = %v", i, err)
		}
	}
	if got := factory.creates(); got != 1 {
		t.Errorf("factory.Create() called %d times, want 1 (single-flight)", got)
	}
	for _, l := range leases {
		l.Release()
	}
}

func TestPoolRefRotationCreatesDistinctEntries(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 4})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}

	l1, err := pool.Acquire(context.Background(), bindingWithRevision("rev-1"))
	if err != nil {
		t.Fatalf("Acquire(rev-1) error = %v", err)
	}
	l1.Release()

	l2, err := pool.Acquire(context.Background(), bindingWithRevision("rev-2"))
	if err != nil {
		t.Fatalf("Acquire(rev-2) error = %v", err)
	}
	defer l2.Release()

	if got := factory.creates(); got != 2 {
		t.Errorf("factory.Create() called %d times, want 2 (ref rotation must not reuse the prior entry)", got)
	}
	if pool.Len() != 2 {
		t.Errorf("pool.Len() = %d, want 2", pool.Len())
	}
}

func TestPoolLRUEvictsIdleEntry(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	bindingA := bindingWithRevision("rev-a")
	bindingB := bindingWithRevision("rev-b")

	la, err := pool.Acquire(context.Background(), bindingA)
	if err != nil {
		t.Fatalf("Acquire(A) error = %v", err)
	}
	la.Release() // idle, evictable

	lb, err := pool.Acquire(context.Background(), bindingB)
	if err != nil {
		t.Fatalf("Acquire(B) error = %v", err)
	}
	defer lb.Release()

	if pool.Len() != 1 {
		t.Errorf("pool.Len() = %d, want 1 (A evicted to admit B under maxWorkareas=1)", pool.Len())
	}
	if !factory.wasClosed(bindingA.Key()) {
		t.Error("evicted binding A's workarea was never closed")
	}
}

func TestPoolNeverEvictsLeasedEntry(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	bindingA := bindingWithRevision("rev-a")
	bindingB := bindingWithRevision("rev-b")

	la, err := pool.Acquire(context.Background(), bindingA)
	if err != nil {
		t.Fatalf("Acquire(A) error = %v", err)
	}
	defer la.Release() // held for the whole test: A must never be evicted

	_, err = pool.Acquire(context.Background(), bindingB)
	if !errors.Is(err, ErrAtCapacity) {
		t.Errorf("Acquire(B) error = %v, want ErrAtCapacity (A is leased and must not be evicted)", err)
	}
}

func TestPoolAllLeasedBackpressure(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 2})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	la, err := pool.Acquire(context.Background(), bindingWithRevision("rev-a"))
	if err != nil {
		t.Fatalf("Acquire(A) error = %v", err)
	}
	defer la.Release()
	lb, err := pool.Acquire(context.Background(), bindingWithRevision("rev-b"))
	if err != nil {
		t.Fatalf("Acquire(B) error = %v", err)
	}
	defer lb.Release()

	_, err = pool.Acquire(context.Background(), bindingWithRevision("rev-c"))
	if !errors.Is(err, ErrAtCapacity) {
		t.Errorf("Acquire(C) error = %v, want ErrAtCapacity (both resident entries are leased)", err)
	}
}

func TestPoolIdleTTLReap(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 4, IdleTTL: time.Millisecond})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	binding := validBinding()
	l, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	l.Release()

	removed := pool.ReapIdle(time.Now().Add(time.Hour))
	if len(removed) != 1 || !removed[0].Equal(binding) {
		t.Fatalf("ReapIdle() = %+v, want exactly [binding]", removed)
	}
	if pool.Len() != 0 {
		t.Errorf("pool.Len() = %d, want 0 after idle reap", pool.Len())
	}
	if !factory.wasClosed(binding.Key()) {
		t.Error("idle-reaped workarea was never closed")
	}
}

func TestPoolIdleTTLZeroDisablesReap(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 4}) // IdleTTL: 0
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	l, err := pool.Acquire(context.Background(), validBinding())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	l.Release()

	if removed := pool.ReapIdle(time.Now().Add(time.Hour)); removed != nil {
		t.Errorf("ReapIdle() with IdleTTL=0 = %v, want nil (disabled)", removed)
	}
	if pool.Len() != 1 {
		t.Errorf("pool.Len() = %d, want 1 (entry must survive with TTL reaping disabled)", pool.Len())
	}
}

func TestPoolWarmFailureDoesNotConsumeSlot(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	binding := validBinding()
	boom := errors.New("boom")
	factory.failOnce[binding.Key()] = boom

	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}

	if _, err := pool.Acquire(context.Background(), binding); err == nil {
		t.Fatal("Acquire() error = nil, want the injected failure")
	}
	if pool.Len() != 0 {
		t.Errorf("pool.Len() = %d, want 0 (a failed warm must not occupy a resident slot)", pool.Len())
	}

	// Retrying (now that failOnce has been consumed) must succeed.
	l, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("Acquire() retry error = %v", err)
	}
	l.Release()
}

func TestPoolAcquireCancelDoesNotAbortOtherWaiters(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	factory.delay = 100 * time.Millisecond
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	binding := validBinding()

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	var slowLease *Lease
	var slowErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowLease, slowErr = pool.Acquire(context.Background(), binding)
	}()

	_, err = pool.Acquire(shortCtx, binding)
	if err == nil {
		t.Fatal("Acquire() with short timeout error = nil, want context deadline error")
	}

	wg.Wait()
	if slowErr != nil {
		t.Fatalf("second (background-ctx) Acquire() error = %v, want nil (warm must not be aborted by the first caller's cancellation)", slowErr)
	}
	if got := factory.creates(); got != 1 {
		t.Errorf("factory.Create() called %d times, want 1 (one shared warm operation)", got)
	}
	slowLease.Release()
}

func TestPoolCloseDrainsThenRejectsNewAcquire(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	binding := validBinding()
	l, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- pool.Close(context.Background())
	}()

	// Give Close a moment to observe the still-held lease before releasing.
	time.Sleep(20 * time.Millisecond)
	l.Release()

	if err := <-closeErr; err != nil {
		t.Errorf("Close() error = %v, want nil once the held lease is released", err)
	}
	if !pool.Closed() {
		t.Error("Closed() = false after Close() returned")
	}

	if _, err := pool.Acquire(context.Background(), binding); !errors.Is(err, ErrClosed) {
		t.Errorf("Acquire() after Close() error = %v, want ErrClosed", err)
	}
	// A graceful Close must not have physically removed the still-resident
	// workarea's on-disk resources (design F: warm cache survives restart).
	if factory.wasClosed(binding.Key()) {
		t.Error("Close() must not evict/close resident workareas — only LRU/idle-TTL eviction may")
	}
}

func TestPoolCloseTimesOutIfLeaseNeverReleased(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	l, err := pool.Acquire(context.Background(), validBinding())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer l.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := pool.Close(ctx); err == nil {
		t.Error("Close() error = nil, want a deadline error when the lease is never released")
	}
}

// TestPoolLeaseReleaseIsIdempotent proves a double Release() on one lease
// does not double-decrement the shared entry's reference count. Without
// idempotency, releasing l1 twice would drop refs from 2 (l1+l2) straight to
// 0, wrongly exposing l2 — still logically active — to eviction/backpressure:
// this test detects that by asserting a competing Acquire is still refused
// (ErrAtCapacity) for as long as l2 is held.
func TestPoolLeaseReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	pool, err := NewPool(factory, PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	binding := validBinding()

	l1, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("Acquire() [l1] error = %v", err)
	}
	l2, err := pool.Acquire(context.Background(), binding)
	if err != nil {
		t.Fatalf("Acquire() [l2] error = %v", err)
	}

	l1.Release()
	l1.Release() // double release: must be a no-op, not a second decrement

	if _, err := pool.Acquire(context.Background(), bindingWithRevision("other")); !errors.Is(err, ErrAtCapacity) {
		t.Errorf("Acquire(other) error = %v, want ErrAtCapacity (l2 must still protect its entry from eviction)", err)
	}

	l2.Release()
}

func TestNewPoolValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewPool(nil, PoolConfig{MaxWorkareas: 1}); err == nil {
		t.Error("NewPool(nil factory) error = nil, want error")
	}
	if _, err := NewPool(newFakeFactory(), PoolConfig{MaxWorkareas: 0}); err == nil {
		t.Error("NewPool(MaxWorkareas: 0) error = nil, want error")
	}
}
