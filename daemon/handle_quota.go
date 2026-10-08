package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// quotaUpdateRequest is the body a worker posts per observed quota
// update: the sparse windows its harness stream carried, with the
// harness that produced them. Windows are the only merge input; every
// other account field stays owned by the probe side.
type quotaUpdateRequest struct {
	Harness string                  `json:"harness"`
	Update  agent.UsageLimitsUpdate `json:"update"`
}

// handleSessionUsage handles POST /api/daemon/sessions/<id>/usage —
// the route a worker posts the sparse quota updates its harness
// stream carries (a codex `account/rateLimits/updated` notification,
// a claude `rate_limit_event`). The daemon folds the update into its
// quota cache by window id, so the next heartbeat carries the fresh
// rows beside the probe snapshot.
//
// The route is worker-reachable, not operator-gated: workers never
// hold the control token (it is minted into the state dir after they
// spawn and never enters their environment). Authentication is the
// same session-ownership proof as the detail fetch: localhost-only
// binding plus a session id the daemon itself admitted. Unknown ids
// 404; a malformed body or an unknown harness 400s. The update is
// bounded and merge-only — windows clamp to the display range and
// cannot touch the probe's login verdict — so a misbehaving worker
// can skew its own rows and nothing else.
func (s *Server) handleSessionUsage(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.daemon.SessionDetail(id); !ok {
		if !sessionOwnedBySpawner(s.daemon.spawner, id) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":     "session not found",
				"sessionId": id,
			})
			return
		}
	}
	var req quotaUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	at := time.Now()
	switch req.Harness {
	case agent.UsageHarnessCodex:
		s.daemon.quota.noteCodexUpdate(req.Update, at)
	case agent.UsageHarnessClaude:
		s.daemon.quota.noteClaudeUpdate(req.Update, at)
	default:
		http.Error(w, "unknown harness", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sessionOwnedBySpawner reports whether the spawner holds a live
// session for id. Sessions admitted without a stored detail (legacy
// dispatch paths) still own their harness streams.
func sessionOwnedBySpawner(spawner *WorkerSpawner, id string) bool {
	if spawner == nil {
		return false
	}
	for _, handle := range spawner.ActiveSessions() {
		if handle.SessionID == id {
			return true
		}
	}
	return false
}
