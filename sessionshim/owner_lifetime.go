package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/RenseiAI/donmai/ptyhost"
)

// OwnerLifetime keeps a one-shot process alive for its own shim listeners after
// the runner has reported the business result. It grants no lifecycle authority:
// terminal proof and serving completion remain owned by each actual Shim.
// Use WithOwnerLifetime before starting the run, then Wait after it returns.
type OwnerLifetime struct {
	mu           sync.Mutex
	draining     bool
	pending      int
	enrolled     chan struct{}
	drainStarted chan struct{}
	shims        []*Shim
}

type ownerLifetimeKey struct{}

// WithOwnerLifetime creates a per-run ownership scope. Child run/stage contexts
// inherit enrollment, but their cancellation does not cancel the process drain.
func WithOwnerLifetime(ctx context.Context) (context.Context, *OwnerLifetime) {
	owner := &OwnerLifetime{enrolled: make(chan struct{}), drainStarted: make(chan struct{})}
	return context.WithValue(ctx, ownerLifetimeKey{}, owner), owner
}

// begin reserves enrollment BEFORE a local shim can start. Draining seals new
// starts and joins reservations already in flight, so a late startup cannot be
// omitted from the lifetime snapshot.
func (o *OwnerLifetime) begin() (func(*Shim), error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.draining {
		return nil, errors.New("session shim owner lifetime is draining")
	}
	o.pending++
	return func(shim *Shim) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if shim != nil {
			o.shims = append(o.shims, shim)
		}
		o.pending--
		if o.draining && o.pending == 0 {
			close(o.enrolled)
		}
	}, nil
}

// StartOwnedFromEnvWithRoot is the context-aware process-owner composition of
// StartFromEnvWithRoot. With no owner scope it preserves the direct library path;
// that caller already owns the returned Shim and must manage its lifetime.
func StartOwnedFromEnvWithRoot(ctx context.Context, launch Launch, spec ptyhost.Spec, workareaPath, workareaRoot string) (*Shim, error) {
	return startOwned(ctx, func() (*Shim, error) {
		return StartFromEnvWithRoot(launch, spec, workareaPath, workareaRoot)
	})
}

func startOwned(ctx context.Context, start func() (*Shim, error)) (*Shim, error) {
	owner, _ := ctx.Value(ownerLifetimeKey{}).(*OwnerLifetime)
	if owner == nil {
		return start()
	}
	finish, err := owner.begin()
	if err != nil {
		return nil, err
	}
	shim, err := start()
	finish(shim)
	return shim, err
}

// Wait seals new starts, joins synchronous local startup already in flight, then
// drains every registered shim. Call it AFTER Runner.Run and result publication,
// with the command context rather than a stage-budget context. An explicit
// command cancellation requests bounded termination/serving teardown; it does
// not change an already reported business result.
func (o *OwnerLifetime) Wait(ctx context.Context) error {
	o.mu.Lock()
	if !o.draining {
		o.draining = true
		close(o.drainStarted)
		if o.pending == 0 {
			close(o.enrolled)
		}
	}
	o.mu.Unlock()
	// Never leave an in-flight startup unowned, including during cancellation.
	<-o.enrolled
	o.mu.Lock()
	shims := append([]*Shim(nil), o.shims...)
	o.mu.Unlock()
	var errs []error
	for _, shim := range shims {
		// Run's deferred Handle.Stop has already reaped normal children. Joining
		// Terminate also joins any concurrent terminal-publication attempt. Its
		// existing grace/courtesy bounds remain authoritative; no new wait constant.
		err := shim.Terminate(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		select {
		case <-shim.TerminalDone():
		default:
			errs = append(errs, fmt.Errorf("session shim %s has no durable terminal observation", shim.Identity().String()))
			_ = shim.Close()
			continue
		}
		select {
		case <-shim.Done():
		case <-ctx.Done():
			// Cancellation is explicit teardown, not a failed business outcome.
			if err := shim.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
