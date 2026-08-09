package kgextract

import (
	"context"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/worker"
)

// Lane is the kg-extraction worker lane: the capability tag a worker advertises
// at registration TOGETHER WITH the handler that executes a claimed item.
//
// The two halves ship as ONE value on purpose. The coordinator gates the claim
// on the advertised capability and pops the item off the org queue when it hands
// it over, so a worker that advertises the tag WITHOUT running the executor is
// strictly worse than one that advertises nothing: it takes the item off the
// queue and drops it, and no capable worker ever sees it again. Because the tag
// can only be obtained by constructing the lane — which always builds the
// handler — the advertisement and the execution cannot drift apart, no matter
// how many poll paths wire it (the standalone worker process and the resident
// daemon both go through NewLane).
type Lane struct {
	// Capability is the tag to advertise at registration
	// (== WorkTypeKGExtraction). Never advertise it without wiring Handler.
	Capability string
	// Handler routes a claimed kgExtractWork[] item to Executor. Safe for
	// concurrent use; best-effort by contract (an error is for logging only and
	// never stops a poll loop).
	Handler worker.BatchHandler
	// Executor is the underlying executor, exposed for callers that need to
	// compose it (e.g. into a multi-work-type mux).
	Executor *Executor
}

// NewLane builds the kg-extraction lane. A nil Options.EmitterFactory is filled
// with DefaultEmitterFactory, so the returned lane can always execute an item:
// a host without the provider CLI reports a status:"error" result the platform
// can see, rather than silently swallowing the work.
func NewLane(opts Options) Lane {
	if opts.EmitterFactory == nil {
		opts.EmitterFactory = DefaultEmitterFactory
	}
	exec := NewExecutor(opts)
	return Lane{
		Capability: WorkTypeKGExtraction,
		Handler:    BatchHandler(exec),
		Executor:   exec,
	}
}

// DefaultEmitterFactory is the production EmitterFactory. It builds a
// constrained, single-shot Emitter per work item from a best-effort claude
// provider (the provider named in the kg-extraction contract example). The
// provider probes the host `claude` CLI at construction; when it is missing the
// factory returns an error and the executor reports a status:"error" result for
// the item (the platform learns the host could not run the emit) rather than
// crashing the poll loop.
//
// host-session vs local: both authModes flow through the same provider-emit seam
// today — the claude CLI invocation IS the host-session transport, and a future
// local-completion provider plugs in here without changing the executor. The
// item's provider/model selection is honored via the per-item factory signature.
func DefaultEmitterFactory(_ context.Context, item KgExtractWorkItem) (Emitter, error) {
	// Only "claude" is wired today; other providers are a follow-up. An unknown
	// provider surfaces as an emitter error → status:"error" for the item.
	switch item.Provider {
	case "", string(agent.ProviderClaude):
		prov, err := providerclaude.New(providerclaude.Options{})
		if err != nil {
			return nil, fmt.Errorf("kgextract: claude provider unavailable: %w", err)
		}
		return NewProviderEmitter(ProviderEmitterConfig{
			Provider: prov,
			Model:    item.Model,
		})
	default:
		return nil, fmt.Errorf("kgextract: provider %q not wired on this worker", item.Provider)
	}
}
