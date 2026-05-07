// Package afclient routing_client.go — DaemonClient methods for the
// /api/daemon/routing/* surface. The contract is locked in
// rensei-architecture/ADR-2026-05-07-daemon-http-control-api.md (D1, D2,
// D4) and the wire types live in routing_types.go.
package afclient

import (
	"fmt"
	"net/url"
)

// GetRoutingConfig fetches the daemon's current routing config snapshot
// from GET /api/daemon/routing/config. The response carries
// Thompson-Sampling state for both LLM and sandbox dimensions plus the
// last N decisions.
func (c *DaemonClient) GetRoutingConfig() (*RoutingConfigResponse, error) {
	var resp RoutingConfigResponse
	if err := c.get("/api/daemon/routing/config", &resp); err != nil {
		return nil, fmt.Errorf("daemon routing config: %w", err)
	}
	return &resp, nil
}

// ExplainRouting fetches the full decision trace for a session from
// GET /api/daemon/routing/explain/<sessionID>. Returns ErrNotFound when
// the session id has no recorded routing decision.
func (c *DaemonClient) ExplainRouting(sessionID string) (*RoutingExplainResponse, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("daemon routing explain: %w", ErrNotFound)
	}
	path := "/api/daemon/routing/explain/" + url.PathEscape(sessionID)
	var resp RoutingExplainResponse
	if err := c.get(path, &resp); err != nil {
		return nil, fmt.Errorf("daemon routing explain %q: %w", sessionID, err)
	}
	return &resp, nil
}
