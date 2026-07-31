package codeintelhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// ToolCaller is the subset of *mcpserver.Server a pool workarea exposes. It
// exists as an interface so Pool tests can substitute a fake without
// standing up a real Git checkout.
type ToolCaller interface {
	Call(ctx context.Context, name string, args json.RawMessage) (mcpserver.ToolResult, error)
	WaitReady(ctx context.Context) error
}

var _ ToolCaller = (*mcpserver.Server)(nil)

// Factory provisions the resources backing one resident workarea for a
// binding: it must produce a ToolCaller that is fully warm (WaitReady
// already safe to call and expected to succeed promptly) and an io.Closer
// that releases the workarea's on-disk resources on eviction. Create's ctx
// bounds ONLY the provisioning/warm-up work, never a caller's request
// lifetime — see Pool.Acquire.
type Factory interface {
	Create(ctx context.Context, binding Binding) (ToolCaller, io.Closer, error)
}

// PoolConfig configures a Pool.
type PoolConfig struct {
	// MaxWorkareas bounds the number of resident (warm or warming) entries.
	// Must be positive.
	MaxWorkareas int
	// IdleTTL is the minimum idle duration (time since last release) before
	// an unleased entry becomes eligible for the idle reaper. Zero disables
	// TTL-based reaping (LRU-at-capacity eviction still applies).
	IdleTTL time.Duration
	// WarmTimeout bounds each Factory.Create + WaitReady call. Zero means no
	// timeout (not recommended in production; the CLI supplies a default).
	WarmTimeout time.Duration
}

// entry is one resident (or warming) workarea slot.
type entry struct {
	binding  Binding
	refs     int
	lastUsed time.Time
	warmDone chan struct{}
	warmErr  error
	caller   ToolCaller
	closer   io.Closer
	evicting bool
}

func (e *entry) isWarm() bool {
	select {
	case <-e.warmDone:
		return true
	default:
		return false
	}
}

// evictable reports whether e may be chosen as an LRU/TTL victim: it must be
// fully warm (not mid-warm), unleased, and not already marked for eviction.
func (e *entry) evictable() bool {
	return e.isWarm() && e.refs == 0 && !e.evicting
}

// Pool is a bounded, ref-counted, single-flight-warmed set of resident
// codeintel workareas keyed by exact Binding. It is safe for concurrent use.
type Pool struct {
	factory Factory
	cfg     PoolConfig

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
}

// NewPool constructs a Pool. It returns an error if cfg is invalid.
func NewPool(factory Factory, cfg PoolConfig) (*Pool, error) {
	if factory == nil {
		return nil, errors.New("code intel host: pool requires a factory")
	}
	if cfg.MaxWorkareas <= 0 {
		return nil, errors.New("code intel host: pool requires a positive max workarea count")
	}
	return &Pool{
		factory: factory,
		cfg:     cfg,
		entries: make(map[string]*entry),
	}, nil
}

// Lease is a reference-counted hold on one resident workarea. Release is
// idempotent (via releaseOnce): callers should still call it exactly once,
// but a second call — e.g. from both an explicit call and a deferred
// cleanup on an error path — is safe and does not double-decrement the
// entry's reference count, which would otherwise wrongly expose a
// still-active second lease on the same binding to eviction/backpressure.
type Lease struct {
	pool    *Pool
	key     string
	binding Binding
	caller  ToolCaller

	releaseOnce sync.Once
}

// Binding returns the exact binding this lease's workarea was warmed for —
// used for the handler's held-binding equality recheck.
func (l *Lease) Binding() Binding { return l.binding }

// Call dispatches a tool call against the leased workarea.
func (l *Lease) Call(ctx context.Context, name string, args json.RawMessage) (mcpserver.ToolResult, error) {
	return l.caller.Call(ctx, name, args)
}

// Release returns the lease's reference. It is idempotent-safe to call from
// a defer even after the pool has been closed, and safe to call more than
// once on the same Lease: only the first call has any effect.
func (l *Lease) Release() {
	l.releaseOnce.Do(func() {
		l.pool.release(l.key)
	})
}

// Acquire returns a Lease for binding, warming a new workarea (subject to
// single-flight de-duplication against concurrent Acquire calls for the same
// binding) or evicting an idle LRU entry to make room, as needed. ctx bounds
// this call's wait for admission and warm-up; it does NOT bound the warm-up
// operation itself, which continues on behalf of any other concurrent waiter
// once started.
func (p *Pool) Acquire(ctx context.Context, binding Binding) (*Lease, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	key := binding.Key()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := p.entries[key]; ok && !e.evicting {
		e.refs++
		p.mu.Unlock()
		return p.waitWarm(ctx, key, e, binding)
	}

	var victim *entry
	if len(p.entries) >= p.cfg.MaxWorkareas {
		victim = p.pickVictimLocked()
		if victim == nil {
			p.mu.Unlock()
			return nil, ErrAtCapacity
		}
		delete(p.entries, victim.binding.Key())
	}

	e := &entry{
		binding:  binding,
		refs:     1,
		lastUsed: time.Now(),
		warmDone: make(chan struct{}),
	}
	p.entries[key] = e
	p.mu.Unlock()

	// Physically release the evicted victim's resources AFTER dropping the
	// pool lock and BEFORE warming the new entry, matching the design's
	// "evict, then warm" ordering (E: "Before warming a new key at capacity,
	// evict the least-recently-used entry...").
	if victim != nil && victim.closer != nil {
		_ = victim.closer.Close()
	}

	// The warm operation runs on its own bounded context, decoupled from
	// this caller's ctx: if this request is cancelled while waiting, other
	// concurrent Acquire calls for the same binding must still see the warm
	// operation complete rather than being aborted by an unrelated caller.
	// ctx is still threaded through (rather than starting fresh from
	// context.Background) so runWarm can derive its own detached context via
	// context.WithoutCancel, preserving whatever request-scoped values ctx
	// carries while shedding its cancellation/deadline.
	go p.runWarm(ctx, e, binding)

	return p.waitWarm(ctx, key, e, binding)
}

// waitWarm blocks until e finishes warming (success or failure) or ctx is
// done, releasing the reference bumped by the caller if ctx wins the race.
func (p *Pool) waitWarm(ctx context.Context, key string, e *entry, binding Binding) (*Lease, error) {
	select {
	case <-e.warmDone:
	case <-ctx.Done():
		p.release(key)
		return nil, fmt.Errorf("acquire workarea: %w", ctx.Err())
	}
	if e.warmErr != nil {
		p.release(key)
		return nil, e.warmErr
	}
	return &Lease{pool: p, key: key, binding: binding, caller: e.caller}, nil
}

// runWarm provisions e via the factory and publishes the result. A failed
// warm removes e from the map so it never occupies a resident slot (E:
// "failed warms do not consume a resident slot") and so a subsequent
// Acquire for the same binding retries rather than replaying the failure.
//
// requestCtx is the ctx of whichever Acquire call triggered this warm; it is
// deliberately NOT used to bound the warm operation directly (see Acquire's
// decoupling comment above the `go p.runWarm(...)` call). context.WithoutCancel
// strips requestCtx's cancellation/deadline while keeping its values, giving
// runWarm a context that survives its triggering caller going away without
// resorting to context.Background/TODO, which would silently drop any
// request-scoped values (and trips gosec G118).
func (p *Pool) runWarm(requestCtx context.Context, e *entry, binding Binding) {
	defer close(e.warmDone)

	ctx := context.WithoutCancel(requestCtx)
	if p.cfg.WarmTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.WarmTimeout)
		defer cancel()
	}

	caller, closer, err := p.factory.Create(ctx, binding)
	if err != nil {
		e.warmErr = fmt.Errorf("warm workarea: %w", err)
		p.dropFailed(binding.Key())
		return
	}
	if err := caller.WaitReady(ctx); err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		e.warmErr = fmt.Errorf("warm workarea: %w", err)
		p.dropFailed(binding.Key())
		return
	}
	e.caller = caller
	e.closer = closer
}

func (p *Pool) dropFailed(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, key)
}

// release decrements the reference count for key and refreshes its
// last-used time so idle/LRU eviction measures from the most recent
// release, not from admission.
func (p *Pool) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok {
		return
	}
	if e.refs > 0 {
		e.refs--
	}
	e.lastUsed = time.Now()
}

// pickVictimLocked returns the least-recently-used evictable entry, or nil
// if none exists. Callers MUST hold p.mu and MUST delete the returned
// victim from p.entries themselves (see Acquire) before releasing the lock,
// so it can never be handed out to a concurrent Acquire.
func (p *Pool) pickVictimLocked() *entry {
	var victim *entry
	for _, e := range p.entries {
		if !e.evictable() {
			continue
		}
		if victim == nil || e.lastUsed.Before(victim.lastUsed) {
			victim = e
		}
	}
	return victim
}

// ReapIdle evicts every currently-evictable entry whose last-used time is
// at least IdleTTL in the past (relative to now), releasing each one's
// on-disk resources. It returns the bindings removed, for logging. A zero
// IdleTTL disables TTL reaping (ReapIdle always returns nil).
func (p *Pool) ReapIdle(now time.Time) []Binding {
	if p.cfg.IdleTTL <= 0 {
		return nil
	}
	p.mu.Lock()
	var victims []*entry
	for key, e := range p.entries {
		if !e.evictable() {
			continue
		}
		if now.Sub(e.lastUsed) < p.cfg.IdleTTL {
			continue
		}
		e.evicting = true
		victims = append(victims, e)
		delete(p.entries, key)
	}
	p.mu.Unlock()

	removed := make([]Binding, 0, len(victims))
	for _, e := range victims {
		if e.closer != nil {
			_ = e.closer.Close()
		}
		removed = append(removed, e.binding)
	}
	return removed
}

// RunIdleReaper runs ReapIdle every interval until ctx is cancelled. It is
// intended to run in its own goroutine for the lifetime of the host process.
func (p *Pool) RunIdleReaper(ctx context.Context, interval time.Duration, onReap func(removed []Binding)) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			removed := p.ReapIdle(now)
			if len(removed) > 0 && onReap != nil {
				onReap(removed)
			}
		}
	}
}

// Closed reports whether the pool has begun (or finished) a graceful
// shutdown drain.
func (p *Pool) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close stops admitting new Acquire calls and waits for every currently
// leased entry's reference count to reach zero, bounded by ctx. It
// deliberately does NOT close/remove any resident workarea's on-disk
// resources — those survive a graceful shutdown so the persistent volume's
// warm cache remains valid across a process restart (design F). Only
// LRU/idle-TTL eviction ever removes a workarea's resources.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if p.allReleased() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain pool: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (p *Pool) allReleased() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.refs > 0 {
			return false
		}
	}
	return true
}

// Len reports the number of currently resident (warm or warming) entries.
// Test/diagnostic helper.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}
