package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// Registry resolves [agent.ProviderName] values to their corresponding
// [agent.Provider] instances. The runner builds one [Registry] at
// daemon startup (per F.2.8 wire-up) and consults it on every Run.
//
// The zero value of Registry is unusable — callers must build one via
// [NewRegistry] which seeds the map.
//
// Concurrency: Registry is safe for concurrent reads. Registration is
// only safe before the runner starts dispatching Runs; treat it as
// build-once, read-many.
type Registry struct {
	mu        sync.RWMutex
	providers map[agent.ProviderName]agent.Provider
}

// NewRegistry constructs an empty Registry. Use [Registry.Register]
// to add providers.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[agent.ProviderName]agent.Provider)}
}

// Register adds p under its declared Name. Calling Register with a
// different instance under an existing name overwrites the earlier
// entry — daemon startup decides whether duplicate registration is
// fatal.
//
// Returns an error when p is nil or its Name is empty (a programmer
// error: every Provider implementation must declare a name).
func (r *Registry) Register(p agent.Provider) error {
	if p == nil {
		return fmt.Errorf("runner: cannot register nil provider")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("runner: provider has empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
	return nil
}

// Resolve returns the registered Provider for name, or
// [agent.ErrNoProvider] when no provider is registered. The error is
// wrapped with the requested name so callers can log forensics.
func (r *Registry) Resolve(name agent.ProviderName) (agent.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", agent.ErrNoProvider, name)
	}
	return p, nil
}

// Names returns the sorted list of registered provider names. Useful
// for daemon-startup logging and the `af agent providers` admin
// command.
func (r *Registry) Names() []agent.ProviderName {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]agent.ProviderName, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Shutdown calls Provider.Shutdown on every registered provider and
// returns the joined error if any failed. Daemon drain calls this
// once on graceful exit so long-lived child processes (codex
// app-server) terminate cleanly.
//
// A nil error means every provider's Shutdown returned nil. When
// multiple providers fail, the returned error is errors.Join'd so
// callers see all failures.
func (r *Registry) Shutdown(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var errs []error
	for name, p := range r.providers {
		if err := p.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown %s: %w", name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
