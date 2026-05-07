// Package daemon handle_workarea.go — HTTP handlers for the
// /api/daemon/workareas* operator surface.
//
// Wave 9 / Track A3 / ADR-2026-05-07-daemon-http-control-api.md §D4a.
//
// Routes:
//
//	GET    /api/daemon/workareas                            list active + archived
//	GET    /api/daemon/workareas/<id>                       inspect (active or archived)
//	POST   /api/daemon/workareas/<archiveID>/restore        201 on success
//	GET    /api/daemon/workareas/<idA>/diff/<idB>           JSON or NDJSON
//
// The streaming-NDJSON variant on /diff/ kicks in when the entry count
// exceeds the daemon's configured workarea.diffStreamingThreshold
// (default 1000 per ADR D4a). Below that, the response is a single
// WorkareaDiffEnvelope JSON object.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// defaultDiffStreamingThreshold is the cutoff used when no daemon.yaml
// override is present. Mirrors the ADR D4a default.
const defaultDiffStreamingThreshold = 1000

// workareaRoutePrefix is the route base every handler dispatches off of.
// The handler does its own path parsing because the stdlib mux only
// supports prefix matching pre-Go 1.22.
const workareaRoutePrefix = "/api/daemon/workareas"

// handleWorkareasRoot dispatches the LIST endpoint.
//
//	GET /api/daemon/workareas → 200 ListWorkareasResponse
func (s *Server) handleWorkareasRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := s.daemon.workareaArchiveRegistry()
	if reg == nil {
		writeJSON(w, http.StatusOK, &afclient.ListWorkareasResponse{
			Active:   []afclient.WorkareaSummary{},
			Archived: []afclient.WorkareaSummary{},
		})
		return
	}
	active, archived, err := reg.List()
	if err != nil {
		http.Error(w, "list workareas: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if active == nil {
		active = []afclient.WorkareaSummary{}
	}
	if archived == nil {
		archived = []afclient.WorkareaSummary{}
	}
	writeJSON(w, http.StatusOK, &afclient.ListWorkareasResponse{
		Active:   active,
		Archived: archived,
	})
}

// handleWorkareaItem dispatches the per-id endpoints. The path shape is
// one of:
//
//	/api/daemon/workareas/<id>
//	/api/daemon/workareas/<id>/restore        (POST)
//	/api/daemon/workareas/<idA>/diff/<idB>    (GET)
func (s *Server) handleWorkareaItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, workareaRoutePrefix+"/")
	if rest == "" || rest == "/" {
		// Trailing slash is the list endpoint, fall through to root.
		s.handleWorkareasRoot(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 1:
		s.serveWorkareaInspect(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "restore":
		s.serveWorkareaRestore(w, r, parts[0])
	case len(parts) == 3 && parts[1] == "diff":
		s.serveWorkareaDiff(w, r, parts[0], parts[2])
	default:
		http.NotFound(w, r)
	}
}

// serveWorkareaInspect handles GET /api/daemon/workareas/<id>. The id
// may resolve to either an active pool member or an archived entry.
func (s *Server) serveWorkareaInspect(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	reg := s.daemon.workareaArchiveRegistry()
	if reg == nil {
		http.NotFound(w, r)
		return
	}
	// Active pool member lookup first — if the daemon has a
	// PoolStatsProvider we walk its members for an id match.
	if active, ok := s.lookupActiveByID(id); ok {
		writeJSON(w, http.StatusOK, &afclient.WorkareaEnvelope{Workarea: active})
		return
	}
	wa, err := reg.Get(id)
	if err != nil {
		if errors.Is(err, ErrArchiveNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, ErrArchiveCorrupted) {
			http.Error(w, "archive corrupted: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "inspect workarea: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, &afclient.WorkareaEnvelope{Workarea: *wa})
}

// serveWorkareaRestore handles POST /api/daemon/workareas/<archiveID>/restore.
// On success returns 201 with the new active pool member.
func (s *Server) serveWorkareaRestore(w http.ResponseWriter, r *http.Request, archiveID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := s.daemon.workareaArchiveRegistry()
	if reg == nil {
		http.NotFound(w, r)
		return
	}
	var req afclient.WorkareaRestoreRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	wa, retryAfter, err := reg.Restore(archiveID, req)
	if err != nil {
		switch {
		case errors.Is(err, afclient.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, afclient.ErrUnavailable):
			if retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrArchiveNotFound):
			http.NotFound(w, r)
		case errors.Is(err, ErrArchiveCorrupted):
			http.Error(w, "archive corrupted: "+err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "restore failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusCreated, &afclient.WorkareaRestoreResult{Workarea: *wa})
}

// serveWorkareaDiff handles GET /api/daemon/workareas/<idA>/diff/<idB>.
// The response shape switches between a single JSON envelope and NDJSON
// streaming based on the entry count vs the configured threshold.
func (s *Server) serveWorkareaDiff(w http.ResponseWriter, r *http.Request, idA, idB string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := s.daemon.workareaArchiveRegistry()
	if reg == nil {
		http.NotFound(w, r)
		return
	}
	threshold := s.diffStreamingThreshold()

	// Self-diff is the common short-circuit; bypass the walker entirely
	// for an empty result. Both ids still need to resolve so we don't
	// silently mask "missing archive" cases.
	if idA == idB {
		if _, err := reg.Get(idA); err != nil {
			if errors.Is(err, ErrArchiveNotFound) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, ErrArchiveCorrupted) {
				http.Error(w, "archive corrupted: "+err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "diff: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, &afclient.WorkareaDiffEnvelope{
			Diff: afclient.WorkareaDiffResult{
				Summary: afclient.WorkareaDiffSummary{WorkareaA: idA, WorkareaB: idB},
				Entries: []afclient.WorkareaDiffEntry{},
			},
		})
		return
	}

	count, err := reg.CountDiff(idA, idB)
	if err != nil {
		if errors.Is(err, ErrArchiveNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, ErrArchiveCorrupted) {
			http.Error(w, "archive corrupted: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "diff: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if count <= threshold {
		s.serveDiffJSON(w, reg, idA, idB)
		return
	}
	s.serveDiffNDJSON(w, reg, idA, idB)
}

// serveDiffJSON emits a single JSON envelope. Used when entry count is
// at or below the threshold.
func (s *Server) serveDiffJSON(w http.ResponseWriter, reg *WorkareaArchiveRegistry, idA, idB string) {
	result, err := reg.Diff(idA, idB)
	if err != nil {
		// Re-classify so the response code matches the failure mode.
		switch {
		case errors.Is(err, ErrArchiveNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrArchiveCorrupted):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, &afclient.WorkareaDiffEnvelope{Diff: *result})
}

// serveDiffNDJSON emits NDJSON one entry per line, ending with a
// {"summary":{...}} line carrying counts. Headers are flushed eagerly
// so consumers can switch their parser before the body arrives.
func (s *Server) serveDiffNDJSON(w http.ResponseWriter, reg *WorkareaArchiveRegistry, idA, idB string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	emit := func(entry afclient.WorkareaDiffEntry) error {
		if err := enc.Encode(&entry); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	summary, err := reg.DiffStream(idA, idB, emit)
	if err != nil {
		// We've already written 200 + Content-Type. The best we can do
		// is emit a final error line; clients will detect the missing
		// summary line.
		_ = enc.Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = enc.Encode(struct {
		Summary afclient.WorkareaDiffSummary `json:"summary"`
	}{Summary: *summary})
	if flusher != nil {
		flusher.Flush()
	}
}

// lookupActiveByID returns the active pool member matching id, if the
// daemon has a registered ActiveWorkareaProvider that exposes one.
// (The registry's List uses the same provider.)
func (s *Server) lookupActiveByID(id string) (afclient.Workarea, bool) {
	reg := s.daemon.workareaArchiveRegistry()
	if reg == nil || reg.activeProvider == nil {
		return afclient.Workarea{}, false
	}
	for _, member := range reg.activeProvider.ActiveWorkareas() {
		if member.ID != id {
			continue
		}
		return afclient.Workarea{
			ID:         member.ID,
			Kind:       afclient.WorkareaKindActive,
			ProviderID: member.ProviderID,
			SessionID:  member.SessionID,
			ProjectID:  member.ProjectID,
			Status:     member.Status,
			Ref:        member.Ref,
			Repository: member.Repository,
			AcquiredAt: member.AcquiredAt,
			ReleasedAt: member.ReleasedAt,
		}, true
	}
	return afclient.Workarea{}, false
}

// diffStreamingThreshold reads daemon.yaml's workarea.diffStreamingThreshold
// or falls back to defaultDiffStreamingThreshold.
func (s *Server) diffStreamingThreshold() int {
	cfg := s.daemon.Config()
	if cfg == nil || cfg.Workarea.DiffStreamingThreshold <= 0 {
		return defaultDiffStreamingThreshold
	}
	return cfg.Workarea.DiffStreamingThreshold
}

// workareaArchiveRegistry returns the daemon's archive registry,
// constructing one on first use from the configured archive root.
// A daemon with no Workarea config gets a registry pointed at the
// default ~/.rensei/workareas.
func (d *Daemon) workareaArchiveRegistry() *WorkareaArchiveRegistry {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workareaArchive != nil {
		return d.workareaArchive
	}
	root := ""
	if d.config != nil {
		root = d.config.Workarea.ArchiveRoot
	}
	d.workareaArchive = NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
		Root: root,
		// Active provider/pool guard wiring is the responsibility of
		// the runtime; tests inject directly via SetWorkareaArchiveRegistry.
	})
	return d.workareaArchive
}

// SetWorkareaArchiveRegistry replaces the daemon's archive registry
// with the provided one. Used by tests + by the future pool wire-up
// (REN-1280) to inject an ActiveWorkareaProvider that sees the live
// pool.
func (d *Daemon) SetWorkareaArchiveRegistry(reg *WorkareaArchiveRegistry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.workareaArchive = reg
}

// computeArchiveSize sums the byte counts of every regular file under a
// workarea's tree directory. Exposed for archive producers + tests
// asserting manifest fidelity.
func computeArchiveSize(treeDir string) (int64, error) {
	entries, err := walkArchiveTree(treeDir)
	if err != nil {
		return 0, fmt.Errorf("walk %q: %w", treeDir, err)
	}
	var total int64
	for _, e := range entries {
		if e.IsDir || e.IsSymlink {
			continue
		}
		total += e.Size
	}
	return total, nil
}
