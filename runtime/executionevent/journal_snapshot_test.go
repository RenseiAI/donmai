package executionevent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func snapshotFixture(t *testing.T, session string, seq uint64, observed time.Time) Record {
	t.Helper()
	r, err := NewRecord(session, seq, observed, "tool.called", map[string]any{"toolName": "Read"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustUint(t *testing.T, v *uint64, want uint64, name string) {
	t.Helper()
	if v == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *v != want {
		t.Fatalf("%s = %d, want %d", name, *v, want)
	}
}

func TestJournalSnapshotReportsBacklogAndAckHealth(t *testing.T) {
	session := "snapshot-health"
	dir := t.TempDir()
	j, err := OpenJournal(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	first := snapshotFixture(t, session, 1, base)
	second := snapshotFixture(t, session, 2, base.Add(time.Minute))
	third := snapshotFixture(t, session, 3, base.Add(2*time.Minute))
	for _, r := range []Record{first, second, third} {
		if err := j.Append(session, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Ack(1); err != nil {
		t.Fatal(err)
	}
	if err := j.Quarantine([]Record{second}, 409, "platform returned HTTP 409"); err != nil {
		t.Fatal(err)
	}
	now := base.Add(10 * time.Minute)
	snap := j.Snapshot(now)
	if snap.Status != JournalSnapshotAvailable {
		t.Fatalf("status = %q, want available", snap.Status)
	}
	mustUint(t, snap.PendingRecords, 1, "pending records")
	raw, err := MarshalCompact(third)
	if err != nil {
		t.Fatal(err)
	}
	mustUint(t, snap.PendingBytes, uint64(len(raw)+1), "pending bytes")
	if snap.OldestUnacknowledgedAge == nil || *snap.OldestUnacknowledgedAge != 8*time.Minute {
		t.Fatalf("oldest age = %v, want 8m", snap.OldestUnacknowledgedAge)
	}
	mustUint(t, snap.AckedSequence, 2, "acked sequence")
	if snap.AckedAt == nil || snap.AckedAt.IsZero() {
		t.Fatal("acked time is unknown, want persisted UTC time")
	}
	mustUint(t, snap.QuarantineTotal, 1, "quarantine total")
	mustUint(t, snap.QuarantineConflicts, 1, "quarantine conflicts")
	before := *snap.AckedAt
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	again := reopened.Snapshot(now)
	mustUint(t, again.PendingRecords, 1, "reopened pending records")
	mustUint(t, again.PendingBytes, uint64(len(raw)+1), "reopened pending bytes")
	mustUint(t, again.AckedSequence, 2, "reopened acked sequence")
	if again.AckedAt == nil || !again.AckedAt.Equal(before) {
		t.Fatalf("reopened acked time = %v, want %v", again.AckedAt, before)
	}
	mustUint(t, again.QuarantineTotal, 1, "reopened quarantine total")
}

func TestJournalSnapshotLegacyAndUnavailableShapes(t *testing.T) {
	session := "snapshot-compat"
	dir := t.TempDir()
	j, err := OpenJournal(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	fresh := j.Snapshot(time.Now())
	if fresh.Status != JournalSnapshotAvailable {
		t.Fatalf("fresh status = %q", fresh.Status)
	}
	if fresh.AckedSequence != nil || fresh.AckedAt != nil {
		t.Fatalf("fresh ack = %v/%v, want nil/nil", fresh.AckedSequence, fresh.AckedAt)
	}
	mustUint(t, fresh.PendingRecords, 0, "fresh pending")
	mustUint(t, fresh.PendingBytes, 0, "fresh bytes")
	if fresh.OldestUnacknowledgedAge != nil {
		t.Fatalf("fresh age = %v, want nil", fresh.OldestUnacknowledgedAge)
	}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := j.Append(session, snapshotFixture(t, session, 1, base)); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(session, snapshotFixture(t, session, 2, base.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := j.Ack(1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ackFileName), []byte(`{"structuredSeq":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := j.Snapshot(base.Add(5 * time.Minute))
	if legacy.Status != JournalSnapshotAvailable {
		t.Fatalf("legacy status = %q", legacy.Status)
	}
	mustUint(t, legacy.AckedSequence, 1, "legacy acked sequence")
	if legacy.AckedAt != nil {
		t.Fatalf("legacy acked time = %v, want unknown", legacy.AckedAt)
	}
	mustUint(t, legacy.PendingRecords, 1, "legacy pending")
	before := map[string][]byte{}
	for _, name := range []string{journalFileName, ackFileName} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = b
	}
	if err := os.WriteFile(filepath.Join(dir, journalFileName), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformed := j.Snapshot(time.Now())
	if malformed.Status != JournalSnapshotUnavailable || malformed.UnavailableReason != JournalSnapshotInvalidData {
		t.Fatalf("malformed = %+v, want unavailable/invalid_data", malformed)
	}
	for _, v := range []*uint64{malformed.PendingRecords, malformed.PendingBytes, malformed.AckedSequence, malformed.QuarantineTotal, malformed.QuarantineConflicts} {
		if v != nil {
			t.Fatalf("malformed metric = %v, want nil", *v)
		}
	}
	if malformed.OldestUnacknowledgedAge != nil || malformed.AckedAt != nil {
		t.Fatal("malformed age/time must be nil")
	}
	after, err := os.ReadFile(filepath.Join(dir, journalFileName))
	if err != nil || string(after) != "{not-json}\n" {
		t.Fatalf("snapshot mutated journal: %q err=%v", after, err)
	}
	_ = j.Close()
	unreadDir := t.TempDir()
	u, err := OpenJournal(unreadDir, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Append(session, snapshotFixture(t, session, 1, base)); err != nil {
		t.Fatal(err)
	}
	if err := u.Ack(1); err != nil {
		t.Fatal(err)
	}
	ackPath := filepath.Join(unreadDir, ackFileName)
	ackBytes, err := os.ReadFile(ackPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ackPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ackPath, 0o700); err != nil {
		t.Fatal(err)
	}
	unread := u.Snapshot(time.Now())
	if unread.Status != JournalSnapshotUnavailable || unread.UnavailableReason != JournalSnapshotReadError {
		t.Fatalf("unreadable = %+v, want unavailable/read_error", unread)
	}
	if unread.PendingRecords != nil || unread.AckedSequence != nil || unread.QuarantineTotal != nil {
		t.Fatal("unreadable metrics must be nil")
	}
	if st, err := os.Stat(ackPath); err != nil || !st.IsDir() {
		t.Fatalf("snapshot repaired unreadable shape: %v %v", st, err)
	}
	_ = os.Remove(ackPath)
	_ = os.WriteFile(ackPath, ackBytes, 0o600) //nolint:gosec // test restores its own temp-dir fixture after the unreadable-shape check.
	_ = u.Close()
}

func TestJournalSnapshotMarshalsOnlyAggregates(t *testing.T) {
	session := "snapshot-disclosure"
	payloadMarker := "SENTINEL_PAYLOAD_MARKER_7Q2X"
	reasonMarker := "SENTINEL_REASON_MARKER_9K4M"
	dirMarker := "SENTINEL_DIR_MARKER_3F8H"
	dir := filepath.Join(t.TempDir(), dirMarker)
	j, err := OpenJournal(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r, err := NewRecord(session, 1, base, "tool.called", map[string]any{"toolName": payloadMarker})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(session, r); err != nil {
		t.Fatal(err)
	}
	if err := j.Quarantine([]Record{r}, 409, reasonMarker); err != nil {
		t.Fatal(err)
	}
	snap := j.Snapshot(base.Add(time.Minute))
	out, err := MarshalCompact(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{payloadMarker, reasonMarker, dirMarker, session, r.EventID, r.ObservedAt} {
		if strings.Contains(string(out), sentinel) {
			t.Fatalf("snapshot discloses %q in %s", sentinel, out)
		}
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	for key := range wire {
		switch key {
		case "status", "unavailableReason", "pendingRecords", "pendingBytes", "oldestUnacknowledgedAge", "ackedSequence", "ackedAt", "quarantineTotal", "quarantineConflicts":
		default:
			t.Fatalf("unexpected snapshot field %q in %s", key, out)
		}
	}
}
