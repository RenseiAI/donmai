// Package daemon handle_kit.go — HTTP handlers for the
// /api/daemon/kits* and /api/daemon/kit-sources* surfaces.
//
// Wave-9 A2 — see ADR-2026-05-07-daemon-http-control-api.md § D1 for the
// canonical route list. Path-prefix dispatch follows the same pattern
// used by handleSessionDetail in server.go.
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// kitRegistry is the subset of *KitRegistry the handlers need.
// Defined as an interface so handler tests can inject a fake without
// scanning a real filesystem.
type kitRegistryDoer interface {
	List() []afclient.Kit
	Get(id string) (afclient.KitManifest, error)
	Enable(id string) (afclient.Kit, error)
	Disable(id string) (afclient.Kit, error)
	VerifySignature(id string) (afclient.KitSignatureResult, error)
	Install(id string, req afclient.KitInstallRequest) (afclient.KitInstallResult, error)
	ListSources() []afclient.KitRegistrySource
	EnableSource(name string) (afclient.KitRegistrySource, error)
	DisableSource(name string) (afclient.KitRegistrySource, error)
}

// kitRoutePrefix is the canonical URL prefix for the per-kit surface.
// Exposed for the server-side route registration in server.go.
const kitRoutePrefix = "/api/daemon/kits/"

// kitSourceRoutePrefix is the canonical URL prefix for the per-source
// kit registry surface.
const kitSourceRoutePrefix = "/api/daemon/kit-sources/"

// handleKitsCollection serves GET /api/daemon/kits — the kit list
// endpoint. Other methods return 405.
func (s *Server) handleKitsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := s.kitRegistryOrEmpty()
	resp := afclient.ListKitsResponse{Kits: reg.List()}
	if resp.Kits == nil {
		resp.Kits = []afclient.Kit{}
	}
	writeJSON(w, http.StatusOK, &resp)
}

// handleKitDetail dispatches the per-kit routes:
//
//	GET    /api/daemon/kits/<id>
//	GET    /api/daemon/kits/<id>/verify-signature
//	POST   /api/daemon/kits/<id>/install
//	POST   /api/daemon/kits/<id>/enable
//	POST   /api/daemon/kits/<id>/disable
func (s *Server) handleKitDetail(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, kitRoutePrefix)
	if suffix == "" {
		http.NotFound(w, r)
		return
	}
	id, action := splitKitPath(suffix)
	if id == "" {
		http.NotFound(w, r)
		return
	}
	reg := s.kitRegistryOrEmpty()

	switch action {
	case "":
		// GET /api/daemon/kits/<id>
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		manifest, err := reg.Get(id)
		if writeKitNotFound(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, &afclient.KitManifestEnvelope{Kit: manifest})
	case "verify-signature":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		res, err := reg.VerifySignature(id)
		if writeKitNotFound(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, &res)
	case "install":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req afclient.KitInstallRequest
		// Empty body is valid (optional version pin).
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
				return
			}
		}
		res, err := reg.Install(id, req)
		if errors.Is(err, ErrKitInstallUnimplemented) {
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"error":   err.Error(),
				"kitId":   id,
				"details": "Remote-registry fetch lands in a follow-up wave (see ADR-2026-05-07 § D4 caveat).",
			})
			return
		}
		if writeKitNotFound(w, err) {
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, &res)
	case "enable":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		k, err := reg.Enable(id)
		if writeKitNotFound(w, err) {
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, &k)
	case "disable":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		k, err := reg.Disable(id)
		if writeKitNotFound(w, err) {
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, &k)
	default:
		http.NotFound(w, r)
	}
}

// handleKitSourcesCollection serves GET /api/daemon/kit-sources.
func (s *Server) handleKitSourcesCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := s.kitRegistryOrEmpty()
	resp := afclient.ListKitSourcesResponse{Sources: reg.ListSources()}
	if resp.Sources == nil {
		resp.Sources = []afclient.KitRegistrySource{}
	}
	writeJSON(w, http.StatusOK, &resp)
}

// handleKitSourceDetail dispatches the per-source toggle routes:
//
//	POST /api/daemon/kit-sources/<name>/enable
//	POST /api/daemon/kit-sources/<name>/disable
func (s *Server) handleKitSourceDetail(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, kitSourceRoutePrefix)
	if suffix == "" {
		http.NotFound(w, r)
		return
	}
	name, action := splitKitPath(suffix)
	if name == "" || action == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := s.kitRegistryOrEmpty()

	var (
		src afclient.KitRegistrySource
		err error
	)
	switch action {
	case "enable":
		src, err = reg.EnableSource(name)
	case "disable":
		src, err = reg.DisableSource(name)
	default:
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, ErrKitSourceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":  err.Error(),
			"source": name,
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, &afclient.KitSourceToggleResult{
		Source:  src,
		Message: "source " + name + " " + action + "d",
	})
}

// splitKitPath splits a path suffix like "spring/java" or
// "spring/java/verify-signature" into the (id, action) pair. Kit ids may
// contain slashes (per the spec, e.g. "spring/java"), so we treat the
// trailing path segment as the action only when it matches a known verb.
//
// Known suffix verbs: install, enable, disable, verify-signature.
func splitKitPath(suffix string) (id, action string) {
	suffix = strings.Trim(suffix, "/")
	if suffix == "" {
		return "", ""
	}
	// Check each known verb to see if the suffix ends with `/<verb>`.
	for _, verb := range knownKitVerbs {
		if strings.HasSuffix(suffix, "/"+verb) {
			return strings.TrimSuffix(suffix, "/"+verb), verb
		}
	}
	return suffix, ""
}

// knownKitVerbs is the closed set of action suffixes accepted on the
// /api/daemon/kits/<id>/<action> route.
var knownKitVerbs = []string{
	"verify-signature",
	"install",
	"enable",
	"disable",
}

// writeKitNotFound writes a 404 envelope when err wraps ErrKitNotFound.
// Returns true when the response was written so the caller can return.
func writeKitNotFound(w http.ResponseWriter, err error) bool {
	if errors.Is(err, ErrKitNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return true
	}
	return false
}

// kitRegistryOrEmpty returns the daemon's kit registry, lazily
// constructing one with the configured scan paths if needed. Defining
// the registry on Server (rather than Daemon) keeps the registry's
// lifecycle bound to the HTTP surface — the registry has no background
// goroutines, so there's nothing to start/stop.
func (s *Server) kitRegistryOrEmpty() kitRegistryDoer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kitReg != nil {
		return s.kitReg
	}
	scanPaths := []string{DefaultKitScanPath()}
	// Future: read scanPaths from daemon.yaml's `kit.scanPaths`. The
	// Config struct does not yet declare that field; once it does,
	// passing cfg.Kit.ScanPaths through here keeps the registry pluggable
	// without a server change.
	s.kitReg = NewKitRegistry(scanPaths)
	return s.kitReg
}
