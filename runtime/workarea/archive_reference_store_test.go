package workarea_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestRecordArchiveReferencePersistsAndReplays(t *testing.T) {
	t.Parallel()
	store := newArchiveReferenceStore(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	lease := acquireArchiveReferenceLease(t, store)
	reference := workarea.TerminalArchiveReference{
		SchemaVersion: workarea.TerminalArchiveReferenceSchemaV1, ArchiveID: lease.WorkareaID,
		ArchiveDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:     lease.SessionID, WorkareaID: lease.WorkareaID,
		TerminalResultID: lease.TerminalResultID,
		CommittedAt:      time.Date(2026, 7, 18, 12, 30, 0, 123_000_000, time.UTC),
	}
	recorded, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, reference)
	if err != nil {
		t.Fatalf("RecordArchiveReference: %v", err)
	}
	if recorded.ArchiveReference == nil || !recorded.ArchiveReference.IdentityEqual(reference) {
		t.Fatalf("recorded reference = %+v", recorded.ArchiveReference)
	}
	// Exact duplicate delivery is idempotent.
	again, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, reference)
	if err != nil || again.ArchiveReference == nil || !again.ArchiveReference.IdentityEqual(reference) {
		t.Fatalf("duplicate delivery = %+v err=%v", again, err)
	}
	// A re-read replays the exact reference.
	reloaded, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := workarea.CanonicalBytes(*reloaded.ArchiveReference)
	if err != nil {
		t.Fatal(err)
	}
	want, err := workarea.CanonicalBytes(reference)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("replayed reference = %s, want %s", raw, want)
	}
}

func TestRecordArchiveReferenceRefusesConflict(t *testing.T) {
	t.Parallel()
	store := newArchiveReferenceStore(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	lease := acquireArchiveReferenceLease(t, store)
	first := workarea.TerminalArchiveReference{
		SchemaVersion: workarea.TerminalArchiveReferenceSchemaV1, ArchiveID: lease.WorkareaID,
		ArchiveDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:     lease.SessionID, WorkareaID: lease.WorkareaID,
		TerminalResultID: lease.TerminalResultID,
		CommittedAt:      time.Date(2026, 7, 18, 12, 30, 0, 123_000_000, time.UTC),
	}
	if _, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, first); err != nil {
		t.Fatal(err)
	}
	conflicting := first
	conflicting.ArchiveDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, conflicting); err == nil {
		t.Fatal("conflicting digest accepted")
	} else if !strings.Contains(err.Error(), "archive reference conflict") {
		t.Fatalf("conflict error = %v", err)
	}
	// Identity mismatch against the lease is refused too.
	mismatched := first
	mismatched.WorkareaID = "wa_ffffffffffffffffffffffffffffffff"
	if _, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, mismatched); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	// The original reference survives the refused writes.
	reloaded, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ArchiveReference == nil || !reloaded.ArchiveReference.IdentityEqual(first) {
		t.Fatalf("stored reference changed: %+v", reloaded.ArchiveReference)
	}
}

// TestRecordArchiveReferenceRefusesForeignIdentity pins the identity fence:
// a reference naming another session, workarea or terminal result is never
// bound onto a lease that has no reference yet. Without the fence the first
// foreign reference would be written, because no recorded reference exists
// to conflict with.
func TestRecordArchiveReferenceRefusesForeignIdentity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*workarea.TerminalArchiveReference)
	}{
		{"session", func(r *workarea.TerminalArchiveReference) { r.SessionID = sessionB }},
		{"workarea", func(r *workarea.TerminalArchiveReference) { r.WorkareaID = workareaB; r.ArchiveID = workareaB }},
		{"terminal_result", func(r *workarea.TerminalArchiveReference) { r.TerminalResultID = resultB }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newArchiveReferenceStore(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
			lease := acquireArchiveReferenceLease(t, store)
			foreign := workarea.TerminalArchiveReference{
				SchemaVersion: workarea.TerminalArchiveReferenceSchemaV1, ArchiveID: lease.WorkareaID,
				ArchiveDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SessionID:     lease.SessionID, WorkareaID: lease.WorkareaID,
				TerminalResultID: lease.TerminalResultID,
				CommittedAt:      time.Date(2026, 7, 18, 12, 30, 0, 123_000_000, time.UTC),
			}
			tc.mutate(&foreign)
			if err := foreign.Validate(); err != nil {
				t.Fatalf("fixture must be a valid reference so only the fence can refuse it: %v", err)
			}
			if _, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, foreign); !errors.Is(err, workarea.ErrLeaseConflict) {
				t.Fatalf("foreign %s reference: err = %v, want ErrLeaseConflict", tc.name, err)
			}
			reloaded, err := store.Get(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.ArchiveReference != nil {
				t.Fatalf("refused write bound a reference: %+v", reloaded.ArchiveReference)
			}
		})
	}
}

func TestRecordArchiveReferenceFailsClosedOnUnknownSchema(t *testing.T) {
	t.Parallel()
	store := newArchiveReferenceStore(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	lease := acquireArchiveReferenceLease(t, store)
	reference := workarea.TerminalArchiveReference{
		SchemaVersion: "donmai.terminal-archive-reference.v0", ArchiveID: lease.WorkareaID,
		ArchiveDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:     lease.SessionID, WorkareaID: lease.WorkareaID,
		TerminalResultID: lease.TerminalResultID,
		CommittedAt:      time.Date(2026, 7, 18, 12, 30, 0, 123_000_000, time.UTC),
	}
	if _, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, reference); err == nil {
		t.Fatal("unknown schema accepted")
	}
	reloaded, err := store.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ArchiveReference != nil {
		t.Fatalf("failed write left a reference: %+v", reloaded.ArchiveReference)
	}
}

func TestAcquireReplayPreservesRecordedReference(t *testing.T) {
	t.Parallel()
	store := newArchiveReferenceStore(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	root := t.TempDir()
	spec := acquireSpec(root, sessionA, resultA, workareaA)
	lease, err := store.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	reference := workarea.TerminalArchiveReference{
		SchemaVersion: workarea.TerminalArchiveReferenceSchemaV1, ArchiveID: lease.WorkareaID,
		ArchiveDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:     lease.SessionID, WorkareaID: lease.WorkareaID,
		TerminalResultID: lease.TerminalResultID,
		CommittedAt:      time.Date(2026, 7, 18, 12, 30, 0, 123_000_000, time.UTC),
	}
	if _, err := store.RecordArchiveReference(context.Background(), lease.LeaseID, reference); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-acquire replays the recorded reference: this is the
	// durable precondition the runner's status-prepare carry observes.
	replayed, err := store.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ArchiveReference == nil || !replayed.ArchiveReference.IdentityEqual(reference) {
		t.Fatalf("re-acquired lease lost the reference: %+v", replayed.ArchiveReference)
	}
}

func newArchiveReferenceStore(t *testing.T, now time.Time) *workarea.LeaseStore {
	t.Helper()
	clock := newTestClock()
	clock.Set(now)
	root := t.TempDir()
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: root, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func acquireArchiveReferenceLease(t *testing.T, store *workarea.LeaseStore) *workarea.TerminalLease {
	t.Helper()
	lease, err := store.Acquire(context.Background(), acquireSpec(t.TempDir(), sessionA, resultA, workareaA))
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
