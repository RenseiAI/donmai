// Package daemon handle_capabilities.go — HTTP handler for the
// GET /api/daemon/capabilities endpoint.
//
// Exposes the local daemon's detected substrate capabilities to clients
// (rensei-tui, debugging tools, CI smoke tests). The response shape mirrors
// the provides[] array sent to POST /api/workers/register so consumers can
// verify what was advertised without re-detecting.
//
// Architecture reference:
//
//	rensei-architecture/ADR-2026-05-12-capacity-pools-and-substrate-resolution.md
//	§ Stream H sub-lane — agentfactory-tui daemon pool awareness
package daemon

import (
	"net/http"
	"time"
)

// CapabilitiesResponse is the JSON response from GET /api/daemon/capabilities.
type CapabilitiesResponse struct {
	// Provides is the substrate capability set detected at daemon startup.
	// Each entry corresponds to a SubstrateCapabilityDeclaration.runtimeKinds
	// value (e.g. "native", "npm", "python-pip"). This matches the provides[]
	// array sent to POST /api/workers/register.
	Provides []ProvideCapability `json:"provides"`
	// Timestamp is the RFC3339 UTC time when this response was generated.
	Timestamp string `json:"timestamp"`
}

// handleCapabilities implements GET /api/daemon/capabilities.
//
// Returns the substrate capabilities the daemon detected at startup and
// advertised to the platform during registration. The endpoint is read-only
// and always returns 200 — even before Start() is called it returns an empty
// provides list so clients can poll safely during daemon init.
func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	resp := CapabilitiesResponse{
		Provides:  []ProvideCapability{},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	caps := s.daemon.SubstrateCapabilities()
	if len(caps) > 0 {
		provides := make([]ProvideCapability, len(caps))
		for i, c := range caps {
			provides[i] = ProvideCapability{Kind: string(c.Kind)}
		}
		resp.Provides = provides
	}
	writeJSON(w, http.StatusOK, &resp)
}
