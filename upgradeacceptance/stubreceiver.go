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
//     caller presents the current worker id AND its bearer. A
//     re-registration (POST /api/workers/register with the previous worker
//     id) rotates the pair; the old pair lapses at once and the refresh
//     fails closed until the caller presents the rotated pair.
//     rotateWithOverlap rotates without the immediate lapse: the old pair
//     stays valid until retired, the window in which a returning daemon
//     pushes the rotated pair before the old bearer expires.
//   - /api/sessions/{id}/step-heartbeat — step liveness. Recorded; succeeds
//     while the bearer is current. A lapsed bearer is refused: a stale or
//     rotated-out worker cannot keep writing step beats after its bearer
//     lapsed, so beats/stepBeats only grow under authority.
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
	// prevWorkerID/prevBearer stay valid during a rotation overlap, until
	// the overlap is retired.
	prevWorkerID string
	prevBearer   string
	// refreshesBy counts successful lease refreshes per worker id.
	refreshesBy map[string]int
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
		workerID:    "worker-1",
		bearer:      "bearer-1",
		terminals:   map[string]storedTerminal{},
		refreshesBy: map[string]int{},
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

// refreshesFor reports the successful lease refreshes presented by workerID.
func (r *stubReceiver) refreshesFor(workerID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshesBy[workerID]
}

// rotateWithOverlap mints a fresh worker pair while the current pair stays
// valid, and returns the fresh pair plus the func that retires the old
// one. It models re-registration by a daemon that returns before the old
// bearer expires: the rotated pair is pushed to the runner while both
// pairs are accepted, then the old pair lapses.
func (r *stubReceiver) rotateWithOverlap() (workerID, bearer string, retire func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prevWorkerID, r.prevBearer = r.workerID, r.bearer
	r.rotateLocked()
	return r.workerID, r.bearer, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.prevWorkerID, r.prevBearer = "", ""
	}
}

// rotateLocked mints the next worker pair. Caller holds r.mu.
func (r *stubReceiver) rotateLocked() {
	r.registrations++
	r.workerID = "worker-" + strings.TrimPrefix(r.workerID, "worker-") + "-r"
	r.bearer = "bearer-" + strings.TrimPrefix(r.bearer, "bearer-") + "-r"
}

// authorizedLocked reports whether the presented pair is the current
// registration, or the previous one during a rotation overlap. Both halves
// must match: a rotated worker id with a lapsed bearer is not the rotated
// pair. Caller holds r.mu.
func (r *stubReceiver) authorizedLocked(workerID, bearer string) bool {
	if workerID == r.workerID && bearer == r.bearer {
		return true
	}
	return r.prevBearer != "" && workerID == r.prevWorkerID && bearer == r.prevBearer
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
	r.prevWorkerID, r.prevBearer = "", ""
	r.rotateLocked()
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
	authorized := r.authorizedLocked(identity.WorkerID, bearerOf(req))
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
		r.refreshesBy[identity.WorkerID]++
		writeJSON(w, map[string]any{"refreshed": true})
	case "step-heartbeat":
		if !authorized {
			http.Error(w, "stale registration", http.StatusUnauthorized)
			return
		}
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
