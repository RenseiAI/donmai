// Package google declares the Google model-endpoint family (Family B) for the
// two-axis provider model. This is where gemini (direct/vertex,
// gemini-generate), agy-cli (oauth-cli, antigravity-oauth), and the north-star
// opencode×google cell (local /v1, openai-chat) COLLAPSE into one company with
// four distinct host cells. Resolve() is pure (no network). No vendor SDK is
// imported. PURELY ADDITIVE; no Phase-1 consumer.
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
