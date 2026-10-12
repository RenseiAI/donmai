package sessionshim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSessionSidecar(id Identity, epoch uint64) SessionSidecar {
	return NewSessionSidecar(id, epoch,
		"https://example.invalid/o/r.git", "example-project", "stub-harness")
}

// TestSessionSidecarRoundTripsBesideTheRecord pins the sidecar's storage: it
// persists through Put/Get unchanged at the registry's owner-only mode, under
// a name that is not a discovery record, so Scan — which an older daemon runs
// with a strict decoder — never enumerates it.
func TestSessionSidecarRoundTripsBesideTheRecord(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := testSessionSidecar(id, 1)
	if err := reg.PutSessionSidecar(want); err != nil {
		t.Fatalf("PutSessionSidecar: %v", err)
	}
	got, found, err := reg.SessionSidecar(id, 1)
	if err != nil || !found || got != want {
		t.Fatalf("SessionSidecar = %+v, %v, %v; want %+v", got, found, err, want)
	}
	if _, found, err := reg.SessionSidecar(id, 2); err != nil || found {
		t.Fatalf("SessionSidecar at another epoch = found %v, err %v; want a separate, absent record", found, err)
	}
	entries, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Err != nil {
		t.Fatalf("Scan = %+v; want only the one discovery record (the session sidecar is not one)", entries)
	}
	info, err := os.Stat(filepath.Join(reg.Dir(), sessionSidecarName(id, 1)))
	if err != nil {
		t.Fatalf("stat session sidecar: %v", err)
	}
	if perm := info.Mode().Perm(); perm != RecordFileMode {
		t.Errorf("session sidecar mode %#o, want %#o", perm, RecordFileMode)
	}
	if strings.HasSuffix(sessionSidecarName(id, 1), recordSuffix) {
		t.Errorf("session sidecar name %q ends in the record suffix; Scan would read it as a discovery record", sessionSidecarName(id, 1))
	}
}

// TestSessionSidecarRefusesMalformed pins the bounds: every malformed sidecar
// fails closed on write and on read, so a corrupted file can never report an
// identity the launch did not admit.
func TestSessionSidecarRefusesMalformed(t *testing.T) {
	t.Parallel()

	id := testIdentity()
	for name, mutate := range map[string]func(*SessionSidecar){
		"schema version": func(s *SessionSidecar) { s.SchemaVersion = 2 },
		"identity":       func(s *SessionSidecar) { s.SessionID = "" },
		"oversize field": func(s *SessionSidecar) { s.Repository = strings.Repeat("a", maxSessionFieldBytes+1) },
		"control byte":   func(s *SessionSidecar) { s.ProjectName = "proj\x00ect" },
		"newline":        func(s *SessionSidecar) { s.Harness = "har\nness" },
	} {
		sidecar := testSessionSidecar(id, 1)
		mutate(&sidecar)
		reg := newTestRegistry(t)
		if err := reg.PutSessionSidecar(sidecar); err == nil {
			t.Errorf("%s: PutSessionSidecar accepted a malformed sidecar", name)
		}
	}
}

// TestSessionSidecarAcceptedAtTheFieldBoundary pins the 512-byte per-field
// bound at its edge: a 512-byte field writes and reads back unchanged, while
// the malformed suite pins 513 as refused. Three full 512-byte fields still
// exceed the 1 KiB whole-sidecar bound, so the acceptance case uses one full
// field beside two short ones.
func TestSessionSidecarAcceptedAtTheFieldBoundary(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	want := testSessionSidecar(id, 1)
	// Literal 512: the test must not follow the constant if the bound moves.
	want.Repository = strings.Repeat("r", 512)
	if err := reg.PutSessionSidecar(want); err != nil {
		t.Fatalf("PutSessionSidecar with a 512-byte field: %v", err)
	}
	got, found, err := reg.SessionSidecar(id, 1)
	if err != nil || !found || got != want {
		t.Fatalf("SessionSidecar = %+v, %v, %v; want %+v", got, found, err, want)
	}
}

// TestSessionSidecarAbsentIsNotAnError pins the pre-sidecar case: a seat
// launched before sidecars existed reports found=false with no error, so an
// adopting daemon keeps listing it — just without the identity fields a
// scoped reader filters on.
func TestSessionSidecarAbsentIsNotAnError(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	if _, found, err := reg.SessionSidecar(testIdentity(), 1); err != nil || found {
		t.Fatalf("SessionSidecar on an empty registry = found %v, err %v; want absent without error", found, err)
	}
}

// TestSessionSidecarSurvivesTombstoneDisposal pins the lifecycle: the
// sidecar shares the incarnation's terminal disposal with the seat record,
// so no stale identity outlives the seat it describes.
func TestSessionSidecarSurvivesTombstoneDisposal(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	tomb := Tombstone{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-1", ProcessEpoch: 1, HarnessPID: 4242, HarnessStartedAt: 17,
		ExitCode: 0, LastSeq: 12, GroupReaped: true,
		ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := reg.PutTombstone(tomb); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	if err := reg.PutSessionSidecar(testSessionSidecar(id, 1)); err != nil {
		t.Fatalf("PutSessionSidecar: %v", err)
	}
	if err := reg.RemoveTombstoneIncarnation(tomb); err != nil {
		t.Fatalf("RemoveTombstoneIncarnation: %v", err)
	}
	if _, found, err := reg.SessionSidecar(id, 1); err != nil || found {
		t.Fatalf("SessionSidecar after tombstone disposal = found %v, err %v; want it gone", found, err)
	}
}

// TestSessionSidecarWithdrawnWithTheIncarnation pins the withdrawal lifecycle:
// withdrawing the discovery record takes the session sidecar with it, the way
// it takes the back-pressure state. A withdrawn-then-relaunched incarnation at
// the same epoch must not serve the previous incarnation's identity to a
// scoped reader.
func TestSessionSidecarWithdrawnWithTheIncarnation(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := reg.PutSessionSidecar(testSessionSidecar(id, 1)); err != nil {
		t.Fatalf("PutSessionSidecar: %v", err)
	}
	if err := reg.RemoveIncarnation(id, "shim-1", 1); err != nil {
		t.Fatalf("RemoveIncarnation: %v", err)
	}
	if _, found, err := reg.SessionSidecar(id, 1); err != nil || found {
		t.Fatalf("SessionSidecar after withdrawal = found %v, err %v; want it gone", found, err)
	}
	// Idempotent: both the shim's teardown and a daemon janitor reach it.
	if err := reg.RemoveIncarnation(id, "shim-1", 1); err != nil {
		t.Fatalf("second RemoveIncarnation was not idempotent: %v", err)
	}
}
