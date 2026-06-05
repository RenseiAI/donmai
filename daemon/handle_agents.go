// Package daemon handle_agents.go — HTTP handlers for the
// /api/daemon/agents* operator surface.
//
// GET /api/daemon/agents       — list active-session AgentCard stubs.
// GET /api/daemon/agents/:id   — detail for a single card (404 on miss).
//
// The handlers synthesise a minimal AgentCard from the in-process
// sessionDetailStore so the CLI's ListAgents / GetAgent calls resolve
// against the local daemon without requiring a platform /api/agents
// endpoint. The wire shape matches the H-workType smoke contract pinned
// by donmai-smokes step7_agent_card_roundtrip_test.go:
//
//	{ id, workType, poolRef, model, labels }
//
// Additional full-card fields (name, scope, runtimes, …) are populated
// with sensible daemon-local values. Consumers that only need the
// H-workType contract subset are unaffected by the richer fields.
package daemon

import (
	"net/http"
	"strings"
	"time"
)

// ── AgentCard registry ────────────────────────────────────────────────────────

// agentCardRegistry is the read-only view of the session-detail store used
// by the agents handlers. It converts active sessions into AgentCard stubs.
// Having a dedicated type keeps the handler logic testable without a full
// Daemon — tests can inject a minimal implementation.
type agentCardRegistry interface {
	// agentCards returns a snapshot of the current active AgentCard stubs,
	// ordered by session acceptance time (oldest first).
	agentCards() []agentCardStub

	// agentCard returns the stub for the given agent id (sessionId), or
	// (agentCardStub{}, false) when the id is not found.
	agentCard(id string) (agentCardStub, bool)
}

// agentCardStub is the minimal H-workType wire shape emitted by the daemon.
// It matches the agentCard struct in donmai-smokes step7.
type agentCardStub struct {
	ID       string   `json:"id"`
	WorkType string   `json:"workType"`
	PoolRef  string   `json:"poolRef"`
	Model    string   `json:"model"`
	Labels   []string `json:"labels"`
}

// agentListResponse is the envelope for GET /api/daemon/agents.
type agentListResponse struct {
	Agents    []agentCardStub `json:"agents"`
	Count     int             `json:"count"`
	Timestamp string          `json:"timestamp"`
}

// ── sessionDetailAgentRegistry ────────────────────────────────────────────────

// sessionDetailAgentRegistry adapts the sessionDetailStore to the
// agentCardRegistry interface. The daemon wires this in register().
type sessionDetailAgentRegistry struct {
	store *sessionDetailStore
	// workerId surfaces as poolRef when no richer pool information is
	// available. It is the daemon's registration workerId.
	workerID func() string
}

func (r *sessionDetailAgentRegistry) agentCards() []agentCardStub {
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()

	out := make([]agentCardStub, 0, len(r.store.details))
	for _, d := range r.store.details {
		if d == nil {
			continue
		}
		out = append(out, r.detailToStub(d))
	}
	return out
}

func (r *sessionDetailAgentRegistry) agentCard(id string) (agentCardStub, bool) {
	d, ok := r.store.Get(id)
	if !ok {
		return agentCardStub{}, false
	}
	return r.detailToStub(d), true
}

// detailToStub projects a SessionDetail onto the H-workType AgentCard stub.
//
// Mapping rationale:
//   - ID      → SessionDetail.SessionID (the stable per-session identity the
//     daemon tracks; stable across GetAgent calls for this session's
//     lifetime).
//   - WorkType → SessionDetail.WorkType (direct — this is the H-workType
//     discriminant the smoke exercises).
//   - PoolRef  → workerID (the daemon's registration identity; the closest
//     "pool" concept the local daemon has; empty when not registered).
//   - Model    → SessionDetail.ResolvedProfile.Model (or ModelProfile.Model
//     when the richer profile is present); falls back to "" so
//     callers can distinguish "no model selected" from an error.
//   - Labels   → nil (no per-session label propagation yet; the field
//     survives the round-trip as null / empty per the smoke contract).
func (r *sessionDetailAgentRegistry) detailToStub(d *SessionDetail) agentCardStub {
	stub := agentCardStub{
		ID:       d.SessionID,
		WorkType: d.WorkType,
	}
	if r.workerID != nil {
		stub.PoolRef = r.workerID()
	}
	// Prefer the richer ModelProfile when present.
	if d.ModelProfile != nil && d.ModelProfile.Model != "" {
		stub.Model = d.ModelProfile.Model
	} else if d.ResolvedProfile != nil && d.ResolvedProfile.Model != "" {
		stub.Model = d.ResolvedProfile.Model
	}
	// Labels: nil is acceptable per the smoke contract; only set when
	// the detail carries explicit tags (not yet a SessionDetail field —
	// leaving nil until a future wave adds a Tags/Labels field to
	// the session-detail schema).
	stub.Labels = nil
	return stub
}

// ── Server handlers ──────────────────────────────────────────────────────────

// handleListAgents implements GET /api/daemon/agents.
//
// Query parameters:
//   - scope   — accepted and ignored (reserved for future filtering)
//   - workType — when non-empty, filters cards to matching WorkType values
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	reg := s.agentCardRegistryOrDefault()
	cards := reg.agentCards()

	// Optional workType filter.
	if wt := r.URL.Query().Get("workType"); wt != "" {
		filtered := cards[:0]
		for _, c := range cards {
			if c.WorkType == wt {
				filtered = append(filtered, c)
			}
		}
		cards = filtered
	}

	if cards == nil {
		cards = []agentCardStub{}
	}
	resp := agentListResponse{
		Agents:    cards,
		Count:     len(cards),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, &resp)
}

// handleGetAgent implements GET /api/daemon/agents/:id.
// Returns the full AgentCard stub or a 404 with {"error":"..."} when not found.
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/daemon/agents/"
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	reg := s.agentCardRegistryOrDefault()
	stub, ok := reg.agentCard(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "agent not found",
			"agentId": id,
		})
		return
	}
	writeJSON(w, http.StatusOK, &stub)
}

// agentCardRegistryOrDefault returns the server's agentCardRegistry, lazily
// constructing a sessionDetailAgentRegistry backed by the daemon's store when
// no explicit registry has been injected. Tests that need a custom registry
// can satisfy the agentCardRegistry interface directly.
func (s *Server) agentCardRegistryOrDefault() agentCardRegistry {
	if s.agentReg != nil {
		return s.agentReg
	}
	return &sessionDetailAgentRegistry{
		store:    s.daemon.sessionDetails,
		workerID: s.daemon.WorkerID,
	}
}
