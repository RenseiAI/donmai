package runner

import (
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/runtime/state"
)

// observerRunner returns a Runner with only the store wired — the narrow
// seam heartbeatAckObserver needs.
func observerRunner() *Runner {
	return &Runner{store: state.NewStore()}
}

// seedState writes a state.json carrying the given session id and startedAt
// under dir, returning the read-back document.
func seedState(t *testing.T, dir, sessionID string, startedAt int64) *state.State {
	t.Helper()
	store := state.NewStore()
	st, err := store.Update(dir, func(s *state.State) error {
		s.SessionID = sessionID
		s.StartedAt = startedAt
		return nil
	})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return st
}

// readHeartbeat returns the persisted LastHeartbeat for dir.
func readHeartbeat(t *testing.T, dir string) int64 {
	t.Helper()
	st, err := state.NewStore().Read(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return st.LastHeartbeat
}

// TestHeartbeatAckObserverPersistsExactTick covers the producer contract:
// the exact acknowledged tick is persisted to state.State.LastHeartbeat,
// fenced to this session and run.
func TestHeartbeatAckObserverPersistsExactTick(t *testing.T) {
	t.Parallel()

	r := observerRunner()
	dir := t.TempDir()
	seedState(t, dir, "sess-1", 1000)

	obs := r.heartbeatAckObserver(dir, "sess-1", 1000)
	if obs == nil {
		t.Fatal("observer is nil for a wired store, state path and session id")
	}
	obs(1_786_000_000_123)

	if got := readHeartbeat(t, dir); got != 1_786_000_000_123 {
		t.Fatalf("LastHeartbeat = %d; want the exact acknowledged tick", got)
	}
}

// TestHeartbeatAckObserverSessionMismatchLeavesStateUntouched covers the
// reused-path fence: a tick acked by another session must not advance this
// path's LastHeartbeat.
func TestHeartbeatAckObserverSessionMismatchLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	r := observerRunner()
	dir := t.TempDir()
	seedState(t, dir, "sess-1", 1000)

	obs := r.heartbeatAckObserver(dir, "sess-2", 1000)
	obs(1_786_000_000_123)

	if got := readHeartbeat(t, dir); got != 0 {
		t.Fatalf("LastHeartbeat = %d; want 0 (session mismatch must not fabricate freshness)", got)
	}
}

// TestHeartbeatAckObserverRunMismatchLeavesStateUntouched covers the stale
// run fence: a tick from a different run of the same session must not
// advance LastHeartbeat.
func TestHeartbeatAckObserverRunMismatchLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	r := observerRunner()
	dir := t.TempDir()
	seedState(t, dir, "sess-1", 1000)

	obs := r.heartbeatAckObserver(dir, "sess-1", 2000)
	obs(1_786_000_000_123)

	if got := readHeartbeat(t, dir); got != 0 {
		t.Fatalf("LastHeartbeat = %d; want 0 (stale run must not fabricate freshness)", got)
	}
}

// TestHeartbeatAckObserverNeverMovesBackwards covers monotonicity: a
// terminal session's last actual heartbeat is retained — an older tick
// (e.g. a late-arriving duplicate) must not rewind it.
func TestHeartbeatAckObserverNeverMovesBackwards(t *testing.T) {
	t.Parallel()

	r := observerRunner()
	dir := t.TempDir()
	seedState(t, dir, "sess-1", 1000)

	obs := r.heartbeatAckObserver(dir, "sess-1", 1000)
	obs(2000)
	obs(1000)

	if got := readHeartbeat(t, dir); got != 2000 {
		t.Fatalf("LastHeartbeat = %d; want 2000 (must never move backwards)", got)
	}
}

// TestHeartbeatAckObserverNilStoreDisablesObservation pins the additive
// wiring: without a store the observer is nil, so pulser construction is
// unchanged for callers that never set it.
func TestHeartbeatAckObserverNilStoreDisablesObservation(t *testing.T) {
	t.Parallel()

	r := &Runner{}
	if obs := r.heartbeatAckObserver(t.TempDir(), "sess-1", 1000); obs != nil {
		t.Fatal("observer must be nil when the store is nil")
	}
	if obs := observerRunner().heartbeatAckObserver("", "sess-1", 1000); obs != nil {
		t.Fatal("observer must be nil when the state path is empty")
	}
	if obs := observerRunner().heartbeatAckObserver(t.TempDir(), "", 1000); obs != nil {
		t.Fatal("observer must be nil when the session id is empty")
	}
}

// TestHeartbeatAckObserverFailsClosedOnMissingState pins the fence against
// a missing state file: Update materialises a zero-value State whose
// SessionID ("") mismatches, so the write is refused rather than adopted.
func TestHeartbeatAckObserverFailsClosedOnMissingState(t *testing.T) {
	t.Parallel()

	r := observerRunner()
	dir := t.TempDir() // no state.json seeded

	obs := r.heartbeatAckObserver(dir, "sess-1", 1000)
	obs(1_786_000_000_123) // must not panic; the fence trips

	st, err := state.NewStore().Read(dir)
	if err == nil && st.LastHeartbeat != 0 {
		t.Fatalf("LastHeartbeat = %d; want 0 (missing state must stay unknown)", st.LastHeartbeat)
	}
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("read state: %v", err)
	}
}

// TestHeartbeatAckObserverMismatchErrorIsFenceNotFailure documents that the
// fence sentinel is swallowed, not surfaced: the observer logs and drops it.
func TestHeartbeatAckObserverMismatchErrorIsFenceNotFailure(t *testing.T) {
	t.Parallel()

	if !errors.Is(errHeartbeatSessionMismatch, errHeartbeatSessionMismatch) {
		t.Fatal("fence sentinel must be comparable with errors.Is")
	}
}

func TestHeartbeatAckObserverUnknownRunLeavesStateUntouched(t *testing.T) {
	t.Parallel()
	for _, start := range []int64{0, -1} {
		dir := t.TempDir()
		seedState(t, dir, "sess-1", start)
		observerRunner().heartbeatAckObserver(dir, "sess-1", 1000)(2000)
		if got := readHeartbeat(t, dir); got != 0 {
			t.Fatalf("unknown run accepted heartbeat: %d", got)
		}
	}
}
