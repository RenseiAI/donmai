package sessionshim

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

func absenceTestRecord(dir string, id Identity, shimID string, epoch uint64) Record {
	return Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: epoch,
		PID: os.Getpid(), ProcessStartedAt: 1,
		SocketPath:  filepath.Join(dir, "shim.sock"),
		ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
}

// TestWithdrawnAbsenceIsInvisibleToDiscoveryButReadableAsEvidence is the whole
// point of the sidecar in one test.
//
// Composing a shim-absent attestation requires the discovery record for the
// incarnation to be GONE. Unlinking it satisfies that and destroys the only
// copy of the other half of the proof — the recorded (pid, start time) — so a
// daemon whose report is refused and which then restarts can never compose the
// attestation again, and the composer keeps an active obligation for a
// correlation nothing on the host can name.
//
// A rename satisfies both: Scan does not enumerate the sidecar, so Adopt cannot
// classify it stale and no daemon can adopt it, while the bytes survive for the
// next daemon to re-prove from.
func TestWithdrawnAbsenceIsInvisibleToDiscoveryButReadableAsEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-sidecar", SessionID: "session-sidecar"}
	const shimID = "shim-sidecar"
	const epoch = uint64(3)
	record := absenceTestRecord(dir, id, shimID, epoch)
	if err := registry.Put(record); err != nil {
		t.Fatalf("Put: %v", err)
	}

	withdrawn, err := registry.WithdrawIncarnationForAbsence(id, shimID, epoch)
	if err != nil || !withdrawn {
		t.Fatalf("WithdrawIncarnationForAbsence = %v, %v", withdrawn, err)
	}

	// GONE as a discovery record: neither the scan nor the incarnation check
	// can see it, which is what makes the attestation's second fact true.
	entries, err := registry.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, entry := range entries {
		if entry.Err == nil && entry.Record.Identity() == id {
			t.Fatalf("Scan still enumerates a withdrawn record: %+v", entry.Record)
		}
	}
	present, err := registry.HasIncarnation(id, shimID, epoch)
	if err != nil || present {
		t.Fatalf("HasIncarnation = %v, %v after withdrawal", present, err)
	}

	// READABLE as evidence: the identity the next daemon has to re-prove from.
	sidecar, ok, err := registry.GetWithdrawnAbsence(id, shimID, epoch)
	if err != nil || !ok {
		t.Fatalf("GetWithdrawnAbsence = %v, %v", ok, err)
	}
	if sidecar.PID != record.PID || sidecar.ProcessStartedAt != record.ProcessStartedAt ||
		sidecar.SocketPath != record.SocketPath {
		t.Fatalf("sidecar lost the evidence it exists to keep: %+v", sidecar)
	}
	all, err := registry.ScanWithdrawnAbsences()
	if err != nil || len(all) != 1 || all[0].ShimID != shimID {
		t.Fatalf("ScanWithdrawnAbsences = %+v, %v; want exactly the withdrawn incarnation", all, err)
	}

	// Disposed only after acceptance, and idempotently.
	if err := registry.DisposeWithdrawnAbsence(id, shimID, epoch); err != nil {
		t.Fatalf("DisposeWithdrawnAbsence: %v", err)
	}
	if _, ok, err := registry.GetWithdrawnAbsence(id, shimID, epoch); err != nil || ok {
		t.Fatalf("sidecar survived disposal: ok=%v err=%v", ok, err)
	}
	if err := registry.DisposeWithdrawnAbsence(id, shimID, epoch); err != nil {
		t.Fatalf("second DisposeWithdrawnAbsence was not idempotent: %v", err)
	}
}

// TestWithdrawalIsPerIncarnation keeps §D7's duplicate-identity case intact:
// two live incarnations of one lifecycle identity are two separate survivors,
// and withdrawing one must not touch the other or collapse them into one file.
func TestWithdrawalIsPerIncarnation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-dup", SessionID: "session-dup"}
	if err := registry.Put(absenceTestRecord(dir, id, "shim-a", 1)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := registry.WithdrawIncarnationForAbsence(id, "shim-a", 1); err != nil {
		t.Fatalf("withdraw shim-a: %v", err)
	}
	if err := registry.Put(absenceTestRecord(dir, id, "shim-b", 2)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, ok, err := registry.GetWithdrawnAbsence(id, "shim-a", 1); err != nil || !ok {
		t.Fatalf("the first incarnation's sidecar is gone: ok=%v err=%v", ok, err)
	}
	if _, ok, err := registry.GetWithdrawnAbsence(id, "shim-b", 2); err != nil || ok {
		t.Fatalf("the second incarnation was withdrawn by the first: ok=%v err=%v", ok, err)
	}
	present, err := registry.HasIncarnation(id, "shim-b", 2)
	if err != nil || !present {
		t.Fatalf("the surviving incarnation lost its discovery record: present=%v err=%v", present, err)
	}
}

// TestWithdrawingAnAbsentRecordReportsNothingWithdrawn keeps the caller honest.
// "There was no record here" is a different answer from "the record is now
// gone", and only the second one may be read as half of a proof.
func TestWithdrawingAnAbsentRecordReportsNothingWithdrawn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-none", SessionID: "session-none"}
	withdrawn, err := registry.WithdrawIncarnationForAbsence(id, "shim-none", 1)
	if err != nil {
		t.Fatalf("WithdrawIncarnationForAbsence: %v", err)
	}
	if withdrawn {
		t.Fatal("withdrawing a record that was never there reported a withdrawal")
	}
	if _, ok, err := registry.GetWithdrawnAbsence(id, "shim-none", 1); err != nil || ok {
		t.Fatalf("a sidecar was published for a record that did not exist: ok=%v err=%v", ok, err)
	}
}
