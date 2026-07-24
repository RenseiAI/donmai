// Package daemon handle_gateway.go — HTTP handler for the
// /api/daemon/gateway operator surface (ADR-2026-07-24 / 08 §3).
//
// It reports the translating-gateway loopback host's status: whether it is
// enabled, its bound loopback address, the number of live per-session routes,
// the inbound surfaces it presents, and the local cost-ledger path. The
// gateway is opt-in (daemon.Options.EnableGateway); when disabled the surface
// still answers, reporting enabled:false — consumers render "gateway off"
// without sniffing for a missing route (the same honesty-marker posture as
// handle_provider.go's PartialCoverage).
package daemon

import "net/http"

// handleGateway implements GET /api/daemon/gateway. It returns the current
// gateway.Status snapshot. Localhost-only like every /api/daemon/* route.
func (s *Server) handleGateway(w http.ResponseWriter, _ *http.Request) {
	st := s.daemon.GatewayStatus()
	writeJSON(w, http.StatusOK, &st)
}
