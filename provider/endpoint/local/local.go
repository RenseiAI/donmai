// Package local declares the Local (Ollama bare surface) model-endpoint family
// (Family B) for the two-axis provider model. It SPEAKS the ollama wire
// protocol on a single local host. Resolve() is pure (no network). No vendor
// SDK is imported. PURELY ADDITIVE; no Phase-1 consumer.
package local

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

// Endpoint is the Local ModelEndpointProvider implementation.
type Endpoint struct{}

// Compile-time assertion: Endpoint satisfies ModelEndpointProvider.
var _ agent.ModelEndpointProvider = (*Endpoint)(nil)

// New constructs a zero-state Local endpoint provider. Never fails.
func New() *Endpoint { return &Endpoint{} }

// Resolve constructs the resolved EndpointBinding for the requested host
// without dialing.
func (e *Endpoint) Resolve(_ context.Context, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	return resolve.FromManifest(e.Manifest(), req)
}
