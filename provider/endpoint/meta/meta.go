// Package meta declares the Meta model-endpoint family (Family B) for the
// two-axis provider model. It SPEAKS the muse-msp wire protocol served by
// the `muse serve` MSP session host: the oauth-cli host carries the
// subscriber login (host-session auth, brings its own auth), and the
// direct host carries an explicit API key. Resolve() is pure (no network).
// No vendor SDK is imported. PURELY ADDITIVE; no Phase-1 consumer.
package meta

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

// Endpoint is the Meta ModelEndpointProvider implementation.
type Endpoint struct{}

// Compile-time assertion: Endpoint satisfies the ModelEndpointProvider
// interface (Manifest() + Resolve()). State-free, so the matrix generator can
// harvest Manifest() from a zero-value instance.
var _ agent.ModelEndpointProvider = (*Endpoint)(nil)

// New constructs a zero-state Meta endpoint provider. Never fails.
func New() *Endpoint { return &Endpoint{} }

// Resolve constructs the resolved EndpointBinding for the requested host
// without dialing.
func (e *Endpoint) Resolve(_ context.Context, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	return resolve.FromManifest(e.Manifest(), req)
}
