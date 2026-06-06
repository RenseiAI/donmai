// Package openai declares the OpenAI model-endpoint family (Family B) for the
// two-axis provider model. It SPEAKS openai-responses (codex app-server host
// login) and openai-chat (keyed direct/azure). Resolve() is pure (no network).
// No vendor SDK is imported. PURELY ADDITIVE; no Phase-1 consumer.
package openai

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

// Endpoint is the OpenAI ModelEndpointProvider implementation.
type Endpoint struct{}

// Compile-time assertion: Endpoint satisfies ModelEndpointProvider.
var _ agent.ModelEndpointProvider = (*Endpoint)(nil)

// New constructs a zero-state OpenAI endpoint provider. Never fails.
func New() *Endpoint { return &Endpoint{} }

// Resolve constructs the resolved EndpointBinding for the requested host
// without dialing.
func (e *Endpoint) Resolve(_ context.Context, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	return resolve.FromManifest(e.Manifest(), req)
}
