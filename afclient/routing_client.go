// Package afclient routing_client.go — DaemonClient methods for the
// /api/daemon/routing/* surface. Wave-9 Phase 2 ships canonical method
// signatures stubbed with ErrUnimplemented; Track A4 fills in the HTTP
// implementation. Sub-agents must replace ErrUnimplemented with the real
// c.get call to the route specified in each method's doc comment.
package afclient

import "fmt"

// GetRoutingConfig fetches the daemon's current routing config snapshot
// from GET /api/daemon/routing/config. The response carries
// Thompson-Sampling state for both LLM and sandbox dimensions plus the
// last N decisions.
func (c *DaemonClient) GetRoutingConfig() (*RoutingConfigResponse, error) {
	return nil, fmt.Errorf("GetRoutingConfig: %w", ErrUnimplemented)
}

// ExplainRouting fetches the full decision trace for a session from
// GET /api/daemon/routing/explain/<sessionID>. Returns ErrNotFound when
// the session id has no recorded routing decision.
func (c *DaemonClient) ExplainRouting(_ string) (*RoutingExplainResponse, error) {
	return nil, fmt.Errorf("ExplainRouting: %w", ErrUnimplemented)
}
