// Package daemon handle_provider.go — HTTP handlers for the
// /api/daemon/providers* operator surface. Wave 9 / A1.
//
// The handlers expose the daemon's in-process AgentRuntime registry
// (claude/codex/ollama/opencode/gemini/amp/stub) as JSON. The remaining
// seven Provider Families (Sandbox, Workarea, VCS, IssueTracker,
// Deployment, AgentRegistry, Kit) return empty until per-family
// registries land in a future wave. The endpoint MUST emit
// PartialCoverage=true and CoveredFamilies=["agent-runtime"] so
// consumers render the "other families coming" caveat without sniffing
// for emptiness — see ADR-2026-05-07-daemon-http-control-api.md §D4.
package daemon

import (
	"net/http"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// handleListProviders implements GET /api/daemon/providers. It walks the
// configured ProviderRegistry, builds an afclient.Provider per registered
// runtime, and emits the canonical ListProvidersResponse. When no
// registry is wired the endpoint returns an empty providers list with
// PartialCoverage=true so consumers still see the honesty marker.
func (s *Server) handleListProviders(w http.ResponseWriter, _ *http.Request) {
	resp := afclient.ListProvidersResponse{
		Providers:       []afclient.Provider{},
		PartialCoverage: true,
		CoveredFamilies: []afclient.ProviderFamily{afclient.FamilyAgentRuntime},
	}
	reg := s.daemon.opts.ProviderRegistry
	if reg != nil {
		for _, name := range reg.Names() {
			resp.Providers = append(resp.Providers, buildProvider(name, reg))
		}
	}
	writeJSON(w, http.StatusOK, &resp)
}

// handleGetProvider implements GET /api/daemon/providers/<id>. The id
// must resolve to a registered AgentRuntime provider; unknown ids
// return a 404 with the canonical {"error":"..."} envelope used
// elsewhere in the daemon's HTTP API.
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/daemon/providers/"
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":      "provider not found",
			"providerId": id,
		})
		return
	}
	reg := s.daemon.opts.ProviderRegistry
	if reg == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":      "provider not found",
			"providerId": id,
		})
		return
	}
	if _, ok := reg.Capabilities(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":      "provider not found",
			"providerId": id,
		})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.ProviderEnvelope{
		Provider: buildProvider(id, reg),
	})
}

// buildProvider constructs the wire-shape afclient.Provider for the
// named runtime. The daemon's runtime registry only exposes names and
// capability flags today — the remaining metadata (version, scope,
// trust, source) will be sourced from the per-family registry when it
// lands. Until then we emit conservative defaults that match the
// "soldered-in OSS runtime" reality: bundled source, ready status,
// global scope, unsigned trust. Manifest signing is a Layer-1 plugin
// concern (002-provider-base-contract.md) which has not landed yet,
// so ManifestOK=true is honest only because there is no manifest to
// validate — every bundled runtime ships in-binary.
func buildProvider(name string, reg ProviderRegistry) afclient.Provider {
	caps, _ := reg.Capabilities(name)
	if caps == nil {
		caps = map[string]any{}
	}
	return afclient.Provider{
		ID:           name,
		Name:         name,
		Version:      Version,
		Family:       afclient.FamilyAgentRuntime,
		Scope:        afclient.ScopeGlobal,
		Status:       afclient.StatusReady,
		Source:       afclient.SourceBundled,
		Trust:        afclient.TrustUnsigned,
		ManifestOK:   true,
		Capabilities: caps,
	}
}
