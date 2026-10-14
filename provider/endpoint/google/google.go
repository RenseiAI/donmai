// Package google declares the Google model-endpoint family (Family B) for the
// two-axis provider model. Gemini (direct/vertex, gemini-generate) and the
// opencode×google cell (local /v1, openai-chat) share one company with three
// distinct host cells. Resolve is pure (no network); no vendor SDK is imported.
package google

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

// Endpoint is the Google ModelEndpointProvider implementation.
type Endpoint struct{}

// Compile-time assertion: Endpoint satisfies ModelEndpointProvider.
var _ agent.ModelEndpointProvider = (*Endpoint)(nil)

// New constructs a zero-state Google endpoint provider. Never fails.
func New() *Endpoint { return &Endpoint{} }

// Resolve constructs the resolved EndpointBinding for the requested host
// without dialing.
func (e *Endpoint) Resolve(_ context.Context, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	return resolve.FromManifest(e.Manifest(), req)
}
