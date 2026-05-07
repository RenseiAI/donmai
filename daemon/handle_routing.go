// Package daemon handle_routing.go — HTTP handlers for the
// /api/daemon/routing/* operator surface. Wave 9 / A4.
//
// The handlers expose the daemon's RoutingTraceStore as JSON. The wire
// shape is locked in
// rensei-architecture/ADR-2026-05-07-daemon-http-control-api.md §D4 and
// matches the surfaces the SaaS dashboard's Routing Intelligence panel
// (REN-205) consumes, so the same renderer composes both.
//
// Read-only this wave. The /config endpoint surfaces the static
// scheduler configuration (weights, capability filters, sandbox/LLM
// provider state) plus the rolling tail of recent decisions; the
// /explain/<sessionID> endpoint returns the full per-session decision
// trace.
package daemon

import (
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// handleGetRoutingConfig implements GET /api/daemon/routing/config. It
// builds a snapshot from the daemon's RoutingTraceStore and the
// configured ProviderRegistry (which surfaces the AgentRuntime names that
// project into the LLM provider state). The endpoint is always 200; an
// empty store produces an empty RecentDecisions list with default
// weights and the `local` sandbox row at Thompson priors.
func (s *Server) handleGetRoutingConfig(w http.ResponseWriter, _ *http.Request) {
	store := s.daemon.routingTraces
	if store == nil {
		// Should be unreachable — New() always seeds the store — but
		// keep the read robust to mis-constructed daemons (test code).
		store = NewRoutingTraceStore(DefaultRoutingRingBufferSize)
	}

	var providerNames []string
	if reg := s.daemon.opts.ProviderRegistry; reg != nil {
		providerNames = reg.Names()
	}

	cfg := store.GetConfig(providerNames, time.Now().UTC())
	writeJSON(w, http.StatusOK, &afclient.RoutingConfigResponse{Config: cfg})
}

// handleExplainRouting implements GET /api/daemon/routing/explain/<id>.
// The session id segment is the path tail after the route prefix; an
// empty id or one containing additional slashes 404s. Unknown ids 404
// with the canonical {"error":"...","sessionId":"..."} envelope.
func (s *Server) handleExplainRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/daemon/routing/explain/"
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":     "routing decision not found",
			"sessionId": id,
		})
		return
	}
	store := s.daemon.routingTraces
	if store == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":     "routing decision not found",
			"sessionId": id,
		})
		return
	}
	decision, trace, ok := store.Explain(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":     "routing decision not found",
			"sessionId": id,
		})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.RoutingExplainResponse{
		SessionID: id,
		Decision:  decision,
		Trace:     trace,
	})
}
