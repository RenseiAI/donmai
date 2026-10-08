package upgradeacceptance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubReceiver is the in-process stand-in for the container's stub hosted
// receiver: a small HTTP server implementing lease refresh, step heartbeat
// and terminal status with exact-replay semantics, rotating the worker id
// on re-registration.
//
// Routes (all POST, JSON bodies):
//
//   - /api/sessions/{id}/lock-refresh — lease refresh. Succeeds while the
//     bearer is current. A re-registration (POST /api/workers/register with
//     the previous worker id) rotates the worker id; the old bearer lapses
//     and the refresh fails closed until the caller presents the rotated
//     pair.
//   - /api/sessions/{id}/step-heartbeat — step liveness. Recorded; always
//     succeeds while the session is not terminal.
//   - /api/sessions/{id}/status — terminal status. The first commit for a
//     session+attempt+body digest wins and its receipt (revision) is
//     returned; an exact replay returns the original receipt unchanged; a
//     changed body for the same session+attempt is a conflict.
//
// The receiver never releases a session without terminal evidence: reads
// report the stored terminal receipt once committed.
type stubReceiver struct {
	server *httptest.Server

	mu sync.Mutex
	// outage, when true, fails terminal posts with 503 without committing,
	// modelling the harness finishing while the daemon is down.
	outage bool
	// workerID is the currently valid registration; bearer is its bearer.
	workerID string
	bearer   string
	// registrations counts re-registrations (rotations).
	registrations int
	// refreshes counts successful lease refreshes.
	refreshes int
	// beats counts step heartbeats.
	beats int
	// terminals maps session id → stored terminal commit.
	terminals map[string]storedTerminal
	// stepBeats stores raw step-heartbeat bodies in order.
	stepBeats [][]byte
}

type storedTerminal struct {
	sessionID  string
	attempt    string
	bodyDigest string
	body       []byte
	revision   string
	workerID   string
}

func newStubReceiver(t *testing.T) *stubReceiver {
	t.Helper()
	r := &stubReceiver{
		workerID:  "worker-1",
		bearer:    "bearer-1",
		terminals: map[string]storedTerminal{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workers/register", r.handleRegister)
	mux.HandleFunc("/api/sessions/", r.handleSession)
	r.server = httptest.NewServer(mux)
	t.Cleanup(r.server.Close)
	return r
}

func (r *stubReceiver) url() string { return r.server.URL }

func (r *stubReceiver) client() *http.Client { return r.server.Client() }

// currentWorker reports the live worker id and bearer.
func (r *stubReceiver) currentWorker() (workerID, bearer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workerID, r.bearer
}

func (r *stubReceiver) counts() (refreshes, beats, registrations int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshes, r.beats, r.registrations
}

func (r *stubReceiver) terminal(sessionID string) (storedTerminal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.terminals[sessionID]
	return st, ok
}

func (r *stubReceiver) stepBeatBodies() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.stepBeats...)
}

// setOutage toggles the simulated receiver outage for terminal posts.
func (r *stubReceiver) setOutage(down bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outage = down
}

func (r *stubReceiver) handleRegister(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	var body struct {
		WorkerID string `json:"workerId"`
	}
	_ = json.Unmarshal(raw, &body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if body.WorkerID != "" && body.WorkerID != r.workerID {
		http.Error(w, "unknown registration", http.StatusNotFound)
		return
	}
	// Rotate: the replacement registration mints a fresh worker id and
	// bearer; the previous pair lapses immediately.
	r.registrations++
	r.workerID = "worker-" + strings.TrimPrefix(r.workerID, "worker-") + "-r"
	r.bearer = "bearer-" + strings.TrimPrefix(r.bearer, "bearer-") + "-r"
	writeJSON(w, map[string]any{"workerId": r.workerID, "bearer": r.bearer})
}

func (r *stubReceiver) handleSession(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, "/api/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, req)
		return
	}
	sessionID, route := parts[0], parts[1]
	raw, _ := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	var identity struct {
		WorkerID string `json:"workerId"`
	}
	_ = json.Unmarshal(raw, &identity)

	r.mu.Lock()
	defer r.mu.Unlock()
	authorized := identity.WorkerID == r.workerID || bearerOf(req) == r.bearer
	switch route {
	case "lock-refresh":
		if !authorized {
			http.Error(w, "stale registration", http.StatusUnauthorized)
			return
		}
		if _, done := r.terminals[sessionID]; done {
			writeJSON(w, map[string]any{"refreshed": false, "stop": true})
			return
		}
		r.refreshes++
		writeJSON(w, map[string]any{"refreshed": true})
	case "step-heartbeat":
		r.beats++
		r.stepBeats = append(r.stepBeats, bytes.Clone(raw))
		if _, done := r.terminals[sessionID]; done {
			writeJSON(w, map[string]any{"stored": true, "terminal": true})
			return
		}
		writeJSON(w, map[string]any{"stored": true})
	case "status":
		var status struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &status); err != nil || status.Status == "" {
			http.Error(w, "status is required", http.StatusBadRequest)
			return
		}
		if r.outage {
			http.Error(w, "receiver unavailable", http.StatusServiceUnavailable)
			return
		}
		if status.Status == "running" {
			writeJSON(w, map[string]any{"observed": true})
			return
		}
		if !authorized {
			http.Error(w, "stale registration", http.StatusUnauthorized)
			return
		}
		digest := sha256.Sum256(raw)
		digestHex := hex.EncodeToString(digest[:])
		attempt := attemptOf(raw)
		if prev, done := r.terminals[sessionID]; done {
			if prev.attempt == attempt && prev.bodyDigest == digestHex {
				// Exact replay: the original receipt, revision unchanged.
				writeJSON(w, map[string]any{"committed": true, "revision": prev.revision, "replay": true})
				return
			}
			http.Error(w, "terminal body conflict", http.StatusConflict)
			return
		}
		rev := "rev-" + digestHex[:12]
		r.terminals[sessionID] = storedTerminal{
			sessionID: sessionID, attempt: attempt, bodyDigest: digestHex,
			body: bytes.Clone(raw), revision: rev, workerID: identity.WorkerID,
		}
		writeJSON(w, map[string]any{"committed": true, "revision": rev})
	default:
		http.NotFound(w, req)
	}
}

func attemptOf(raw []byte) string {
	var body struct {
		Attempt string `json:"attempt"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return body.Attempt
}

func bearerOf(req *http.Request) string {
	auth := req.Header.Get("Authorization")
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
