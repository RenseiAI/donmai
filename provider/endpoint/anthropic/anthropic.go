// Package anthropic declares the Anthropic model-endpoint family (Family B)
// for the two-axis provider model. It SPEAKS the anthropic-messages wire
// protocol across the oauth-cli, direct, bedrock, and vertex serving hosts.
//
// This package is PURELY ADDITIVE and has no existing consumer in Phase 1:
// Resolve() is pure (no network) — it templates the host's BaseURLTmpl with
// the requested region and copies the host's declared env-var values out of
// EndpointRequest.EnvProvided into the binding. No vendor SDK is imported.
package anthropic

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

// Endpoint is the Anthropic ModelEndpointProvider implementation.
type Endpoint struct{}

// Compile-time assertion: Endpoint satisfies the ModelEndpointProvider
// interface (Manifest() + Resolve()). State-free, so the matrix generator can
// harvest Manifest() from a zero-value instance.
var _ agent.ModelEndpointProvider = (*Endpoint)(nil)

// New constructs a zero-state Anthropic endpoint provider. Never fails.
func New() *Endpoint { return &Endpoint{} }

// Resolve constructs the resolved EndpointBinding for the requested host
// without dialing.
func (e *Endpoint) Resolve(_ context.Context, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	return resolve.FromManifest(e.Manifest(), req)
}
