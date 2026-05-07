// Package afclient provider_client.go — DaemonClient methods for the
// /api/daemon/providers* surface. The wire types live in
// provider_types.go; this file holds the HTTP method bodies. The contract
// is locked in
// rensei-architecture/ADR-2026-05-07-daemon-http-control-api.md (D1).
package afclient

import "fmt"

// ListProviders fetches the daemon's known provider registry from
// GET /api/daemon/providers.
func (c *DaemonClient) ListProviders() (*ListProvidersResponse, error) {
	var resp ListProvidersResponse
	if err := c.get("/api/daemon/providers", &resp); err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	return &resp, nil
}

// GetProvider fetches a single provider by id from
// GET /api/daemon/providers/<id>. Returns ErrNotFound when the id is not
// registered.
func (c *DaemonClient) GetProvider(id string) (*ProviderEnvelope, error) {
	var resp ProviderEnvelope
	if err := c.get("/api/daemon/providers/"+id, &resp); err != nil {
		return nil, fmt.Errorf("get provider %q: %w", id, err)
	}
	return &resp, nil
}
