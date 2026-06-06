// Package stub declares a test-only model-endpoint family (Family B) for the
// two-axis provider model. It lets the matrix carry a stub × stub cell that
// satisfies the parity intersection rule with no special-casing. Resolve() is
// pure (no network). PURELY ADDITIVE; no Phase-1 production consumer.
package stub

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

// Endpoint is the stub ModelEndpointProvider implementation.
type Endpoint struct{}

// Compile-time assertion: Endpoint satisfies ModelEndpointProvider.
var _ agent.ModelEndpointProvider = (*Endpoint)(nil)

// New constructs a zero-state stub endpoint provider. Never fails.
func New() *Endpoint { return &Endpoint{} }

// Resolve constructs the resolved EndpointBinding for the requested host
// without dialing.
func (e *Endpoint) Resolve(_ context.Context, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	return resolve.FromManifest(e.Manifest(), req)
}
