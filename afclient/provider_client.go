// Package afclient provider_client.go — DaemonClient methods for the
// /api/daemon/providers* surface. Wave-9 Phase 2 ships canonical method
// signatures stubbed with ErrUnimplemented; Track A1 fills in the HTTP
// implementation. Sub-agents must replace ErrUnimplemented with the real
// c.get/c.post call to the route specified in the doc comment.
package afclient

import "fmt"

// ListProviders fetches the daemon's known provider registry from
// GET /api/daemon/providers.
func (c *DaemonClient) ListProviders() (*ListProvidersResponse, error) {
	return nil, fmt.Errorf("ListProviders: %w", ErrUnimplemented)
}

// GetProvider fetches a single provider by id from
// GET /api/daemon/providers/<id>. Returns ErrNotFound when the id is not
// registered.
func (c *DaemonClient) GetProvider(_ string) (*ProviderEnvelope, error) {
	return nil, fmt.Errorf("GetProvider: %w", ErrUnimplemented)
}
