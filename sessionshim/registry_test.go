package sessionshim

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

func testIdentity() Identity { return Identity{OrgID: "org-a", SessionID: "sess-a"} }

func testRecord(t *testing.T, id Identity, reg *Registry) Record {
	t.Helper()
	return Record{
		SchemaVersion:     RecordSchemaVersion,
		OrgID:             id.OrgID,
		SessionID:         id.SessionID,
		ShimID:            "shim-1",
		ProcessEpoch:      1,
		PID:               os.Getpid(),
		ProcessStartedAt:  1700000000,
		SocketPath:        reg.SocketPath(id),
		SocketDevice:      1,
		SocketInode:       2,
		ProtocolMin:       shimwire.ProtocolMin,
		ProtocolMax:       shimwire.ProtocolMax,
		Phase:             shimwire.PhaseRunning,
		WorkareaPath:      "/w/a",
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := NewRegistry(shortTempDir(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func TestRegistryPutGetRoundTrip(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	want := testRecord(t, id, reg)
	if err := reg.Put(want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := reg.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ShimID != want.ShimID || got.Identity() != id || got.WorkareaPath != want.WorkareaPath {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
}

func TestRegistryPublishesWithBoundedPermissions(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// §D6: records are 0600 under a 0700 directory. A widened record would put
	// every live session's socket path in reach of another local user.
	dirInfo, err := os.Stat(reg.Dir())
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != RegistryDirMode {
		t.Fatalf("registry dir mode = %#o, want %#o", perm, RegistryDirMode)
	}
	fileInfo, err := os.Stat(filepath.Join(reg.Dir(), id.RecordName()))
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != RecordFileMode {
		t.Fatalf("record mode = %#o, want %#o", perm, RecordFileMode)
	}
}

func TestRegistryLeavesNoTemporaryFilesBehind(t *testing.T) {
	t.Parallel()

	// A publish that leaked its temp file would grow the directory without bound
	// and would make a scan see half-written records.
	reg := newTestRegistry(t)
	id := testIdentity()
	for i := 0; i < 5; i++ {
		if err := reg.Put(testRecord(t, id, reg)); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	ents, err := os.ReadDir(reg.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 1 {
		names := make([]string, len(ents))
		for i, e := range ents {
			names[i] = e.Name()
		}
		t.Fatalf("registry holds %d entries after 5 republishes: %v", len(ents), names)
	}
}

func TestRegistryRefusesWidenedRecordMode(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := filepath.Join(reg.Dir(), id.RecordName())
	// The widened mode is the FIXTURE, not an oversight: this test exists to
	// prove the reader refuses a record another local user could have written.
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: deliberately widened to prove ErrRegistryUnsafe fires
		t.Fatalf("chmod: %v", err)
	}
	// Widened ownership is quarantine EVIDENCE, not something to silently repair:
	// a record another user could have written is not a record to trust.
	if _, err := reg.Get(id); !errors.Is(err, ErrRegistryUnsafe) {
		t.Fatalf("Get widened record = %v, want ErrRegistryUnsafe", err)
	}
}

func TestRegistryRefusesRecordWithUnknownField(t *testing.T) {
	t.Parallel()

	// This is the mechanical guard behind "the discovery record contains no
	// secrets": the schema is CLOSED, so a writer that adds a token field
	// produces a record this reader refuses rather than silently carries.
	reg := newTestRegistry(t)
	id := testIdentity()
	rec := testRecord(t, id, reg)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	asMap["carrierBearerToken"] = "a-secret-that-must-not-be-carried"
	tampered, err := json.Marshal(asMap)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reg.Dir(), id.RecordName()), tampered, RecordFileMode); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	_, err = reg.Get(id)
	if !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("Get record with unknown field = %v, want ErrRecordInvalid", err)
	}
}

func TestRegistryRefusesOversizedEntry(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	huge := make([]byte, MaxRecordBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(reg.Dir(), id.RecordName()), huge, RecordFileMode); err != nil {
		t.Fatalf("write oversized: %v", err)
	}
	if _, err := reg.Get(id); !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("Get oversized record = %v, want ErrRecordInvalid", err)
	}
}

func TestRegistryScanReportsBadEntriesInsteadOfAborting(t *testing.T) {
	t.Parallel()

	// A single corrupt file must not blind a starting daemon to every other live
	// session — and the corrupt entry is itself something the daemon must account
	// for, so it is reported rather than skipped.
	reg := newTestRegistry(t)
	good := Identity{OrgID: "org-a", SessionID: "good"}
	if err := reg.Put(testRecord(t, good, reg)); err != nil {
		t.Fatalf("Put good: %v", err)
	}
	bad := Identity{OrgID: "org-a", SessionID: "bad"}
	if err := os.WriteFile(filepath.Join(reg.Dir(), bad.RecordName()), []byte("{not json"), RecordFileMode); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	entries, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Scan returned %d entries, want 2", len(entries))
	}
	var sawGood, sawBad bool
	for _, e := range entries {
		switch {
		case e.Err == nil && e.Record.Identity() == good:
			sawGood = true
		case e.Err != nil:
			sawBad = true
		}
	}
	if !sawGood {
		t.Error("Scan lost the healthy record because a sibling was corrupt")
	}
	if !sawBad {
		t.Error("Scan silently dropped the corrupt entry instead of reporting it")
	}
}

func TestScanIsDeterministicallyOrdered(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	for _, s := range []string{"s3", "s1", "s2"} {
		id := Identity{OrgID: "org-a", SessionID: s}
		if err := reg.Put(testRecord(t, id, reg)); err != nil {
			t.Fatalf("Put %s: %v", s, err)
		}
	}
	first, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	second, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan again: %v", err)
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Fatalf("scan order is unstable at %d: %q then %q", i, first[i].Name, second[i].Name)
		}
	}
}

func TestPutTombstoneWithdrawsTheLiveRecord(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
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

	if _, err := reg.Get(id); err == nil {
		t.Fatal("live record survived tombstone publication; the session would look live and terminal at once")
	}
	got, err := reg.GetTombstone(id)
	if err != nil {
		t.Fatalf("GetTombstone: %v", err)
	}
	if !got.GroupReaped || got.HarnessPID != 4242 {
		t.Fatalf("GetTombstone = %+v", got)
	}

	// A tombstone must NOT be reported as a live record by a scan.
	entries, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Scan saw %d live records after tombstoning, want 0", len(entries))
	}
	tombs, err := reg.ScanTombstones()
	if err != nil {
		t.Fatalf("ScanTombstones: %v", err)
	}
	if len(tombs) != 1 {
		t.Fatalf("ScanTombstones returned %d, want 1", len(tombs))
	}
}

func TestRegistryResumeKeySurvivesTerminalPublication(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	key := ResumeKey{CodexHome: "/tmp/donmai-codex-home-test", ThreadID: "thread-live"}
	if err := reg.PutResumeKey(id, key); err != nil {
		t.Fatalf("PutResumeKey: %v", err)
	}
	live, err := reg.Get(id)
	if err != nil || live.ResumeKey == nil || *live.ResumeKey != key {
		t.Fatalf("live resume key = %+v, err=%v", live.ResumeKey, err)
	}
	if err := reg.PutTombstone(Tombstone{
		SchemaVersion: RecordSchemaVersion, OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-1", ProcessEpoch: 1, GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	tombstone, err := reg.GetTombstone(id)
	if err != nil || tombstone.ResumeKey == nil || *tombstone.ResumeKey != key {
		t.Fatalf("terminal resume key = %+v, err=%v", tombstone.ResumeKey, err)
	}
}

func TestTombstonesPersistPerIncarnationAndLegacyAPIsRefuseAmbiguity(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	first := Tombstone{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-a", ProcessEpoch: 1, GroupReaped: true,
		ObservedAtUnixNano: time.Now().UnixNano(),
	}
	second := first
	second.ShimID = "shim-b"
	second.ProcessEpoch = 2
	second.ObservedAtUnixNano++
	if err := reg.PutTombstone(first); err != nil {
		t.Fatalf("PutTombstone first: %v", err)
	}
	if err := reg.PutTombstone(second); err != nil {
		t.Fatalf("PutTombstone second: %v", err)
	}
	tombstones, err := reg.ScanTombstones()
	if err != nil {
		t.Fatalf("ScanTombstones: %v", err)
	}
	if len(tombstones) != 2 || tombstones[0].ShimID != "shim-a" || tombstones[1].ShimID != "shim-b" {
		t.Fatalf("per-incarnation tombstones = %+v, want both exact proofs", tombstones)
	}
	if _, err := reg.GetTombstone(id); !errors.Is(err, ErrTombstoneAmbiguous) {
		t.Fatalf("legacy GetTombstone ambiguity = %v, want ErrTombstoneAmbiguous", err)
	}
	if err := reg.RemoveTombstone(id); !errors.Is(err, ErrTombstoneAmbiguous) {
		t.Fatalf("legacy RemoveTombstone ambiguity = %v, want ErrTombstoneAmbiguous", err)
	}
	if got, err := reg.GetTombstoneIncarnation(id, second.ShimID, second.ProcessEpoch); err != nil || got.ShimID != second.ShimID {
		t.Fatalf("GetTombstoneIncarnation second = %+v, %v", got, err)
	}
	if err := reg.RemoveTombstoneIncarnation(first); err != nil {
		t.Fatalf("RemoveTombstoneIncarnation first: %v", err)
	}
	if got, err := reg.GetTombstone(id); err != nil || got.ShimID != second.ShimID {
		t.Fatalf("legacy GetTombstone after exact removal = %+v, %v", got, err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	t.Parallel()

	// Both the shim's own teardown and a daemon janitor can reach removal, so a
	// missing entry is a normal outcome rather than an error.
	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Remove(id); err != nil {
		t.Fatalf("Remove absent record: %v", err)
	}
	if err := reg.RemoveTombstone(id); err != nil {
		t.Fatalf("RemoveTombstone absent: %v", err)
	}
}

func TestRecordValidationRejectsMissingStartIdentity(t *testing.T) {
	t.Parallel()

	// §D2: a bare PID is never evidence, so a record without start identity is
	// structurally invalid — it would force a reader to trust one.
	reg := newTestRegistry(t)
	rec := testRecord(t, testIdentity(), reg)
	rec.ProcessStartedAt = 0
	err := rec.Validate()
	if !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("Validate without start identity = %v, want ErrRecordInvalid", err)
	}
	if !strings.Contains(err.Error(), "processStartedAt") {
		t.Fatalf("error should name the missing field, got %v", err)
	}
}

func TestRecordValidationRejectsBadSchemaAndRange(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	base := testRecord(t, testIdentity(), reg)

	cases := []struct {
		name   string
		mutate func(*Record)
	}{
		{"wrong schema version", func(r *Record) { r.SchemaVersion = 99 }},
		{"empty shim id", func(r *Record) { r.ShimID = "" }},
		{"non-positive pid", func(r *Record) { r.PID = 0 }},
		{"empty socket path", func(r *Record) { r.SocketPath = "" }},
		{"inverted protocol range", func(r *Record) { r.ProtocolMin, r.ProtocolMax = 5, 1 }},
		{"unknown phase", func(r *Record) { r.Phase = "recovering" }},
		{"missing createdAt", func(r *Record) { r.CreatedAtUnixNano = 0 }},
		{"empty org", func(r *Record) { r.OrgID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := base
			tc.mutate(&rec)
			if err := rec.Validate(); !errors.Is(err, ErrRecordInvalid) {
				t.Fatalf("Validate = %v, want ErrRecordInvalid", err)
			}
		})
	}
}

func TestNewRegistryTightensAnExistingLooseDirectory(t *testing.T) {
	t.Parallel()

	// MkdirAll leaves an existing directory's mode alone, so a registry created
	// under a pre-existing 0755 path would be world-readable without this.
	dir := filepath.Join(shortTempDir(t), "loose")
	// A pre-existing loose directory is the FIXTURE: MkdirAll would leave it
	// alone, so this proves NewRegistry tightens it rather than inheriting it.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: deliberately loose to prove NewRegistry chmods it to 0700
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := NewRegistry(dir); err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != RegistryDirMode {
		t.Fatalf("registry dir mode = %#o after NewRegistry, want %#o", perm, RegistryDirMode)
	}
}

func TestNewRegistryRequiresADirectory(t *testing.T) {
	t.Parallel()

	if _, err := NewRegistry(""); err == nil {
		t.Fatal("NewRegistry(\"\") succeeded; the state-directory seam is required")
	}
}

// TestTerminalWithdrawalCannotBeUndoneByAnOrphanRepublish pins the invariant a
// full parallel -race run caught being violated.
//
// The reachable sequence is ordinary: a Stop reaps the harness while the
// controller connection is dropping, and that connection's teardown arms the
// orphan clock — which republishes the discovery record. If that republish can
// land after the tombstone withdrew the record, the session reads as live and
// terminal at once, and the resurrected record arms an orphan deadline for a
// harness that is already gone.
func TestTerminalWithdrawalCannotBeUndoneByAnOrphanRepublish(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-resurrect", SessionID: "sess-resurrect"}

	sh, err := Start(Options{
		Identity:     id,
		Registry:     reg,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}},
		WorkareaPath: dir,
		Orphan: OrphanPolicy{
			Deadline:          time.Hour, // never fires on its own during this test
			TerminationGrace:  500 * time.Millisecond,
			PropagationMargin: 0,
		},
		ProcessEpoch: 1,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	if _, err := reg.Get(id); err != nil {
		t.Fatalf("no record published at start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sh.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := reg.GetTombstone(id); err != nil {
		t.Fatalf("no tombstone after Terminate: %v", err)
	}
	if _, err := reg.Get(id); err == nil {
		t.Fatal("the live discovery record survived Terminate")
	}

	// This is what a dropping controller connection does. It must be a no-op now,
	// not a resurrection — and it must not be a panic or an error either, because
	// the teardown path has no way to know it lost the race.
	sh.armOrphan()
	if _, err := reg.Get(id); err == nil {
		t.Fatal("arming the orphan clock after termination republished the discovery record")
	}
	// disarmOrphan republishes too, and reaches the same guard.
	sh.disarmOrphan()
	if _, err := reg.Get(id); err == nil {
		t.Fatal("disarming the orphan clock after termination republished the discovery record")
	}
	if _, err := reg.GetTombstone(id); err != nil {
		t.Fatalf("the tombstone was disturbed by a post-terminal republish: %v", err)
	}
}

// TestLegacyTombstoneAliasIsRefusedWhileASiblingRecordIsLive pins the one
// assertion the identity-only alias makes: "this SESSION's harness group was
// reaped".
//
// A v1 reader can only read the alias. §D7's duplicate-identity case makes a
// sibling lineage tombstoning beside a running session real — the acceptance
// seam creates exactly that — and writing the alias there tells such a reader
// the LIVE session's group is gone. Worse, the alias is deliberately never
// overwritten, so the real lineage's later tombstone could never replace it.
func TestLegacyTombstoneAliasIsRefusedWhileASiblingRecordIsLive(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-alias", SessionID: "session-alias"}
	live := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-live", ProcessEpoch: 1,
		PID: os.Getpid(), ProcessStartedAt: 1,
		SocketPath:  dir + "/live.sock",
		ProtocolMin: 1, ProtocolMax: 3,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.Put(live); err != nil {
		t.Fatal(err)
	}
	sibling := Tombstone{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-sibling", ProcessEpoch: 2,
		HarnessPID: os.Getpid(), HarnessStartedAt: 1,
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(sibling); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}

	// The exact incarnation is readable, as always.
	if _, err := registry.GetTombstoneIncarnation(id, "shim-sibling", 2); err != nil {
		t.Fatalf("per-incarnation tombstone missing: %v", err)
	}
	// The identity-only alias is NOT written while the identity has another
	// live record.
	if _, err := registry.readEntry(id.TombstoneName()); err == nil {
		t.Fatal("the identity-only alias was written while a sibling lineage is still live; a v1 reader " +
			"would conclude the live session's harness group was reaped")
	}
	// The live lineage is untouched by its sibling's terminal publication.
	if got, err := registry.Get(id); err != nil || got.ShimID != "shim-live" {
		t.Fatalf("live discovery record after a sibling tombstone = %+v, %v", got, err)
	}

	// Control: an identity whose only incarnation is the one that ended still
	// gets the alias, so v1 readers keep working for the ordinary case.
	soleID := Identity{OrgID: "org-alias", SessionID: "session-alias-sole"}
	sole := sibling
	sole.SessionID = soleID.SessionID
	sole.ShimID = "shim-sole"
	sole.ProcessEpoch = 1
	if err := registry.PutTombstone(sole); err != nil {
		t.Fatalf("PutTombstone(sole): %v", err)
	}
	if _, err := registry.readEntry(soleID.TombstoneName()); err != nil {
		t.Fatalf("the identity-only alias is missing for an unambiguous identity: %v", err)
	}
}

// TestLegacyTombstoneAliasScopeIsTheIdentity pins what the identity-only alias
// is allowed to depend on.
//
// The alias asserts something about ONE identity, and it is written on the
// terminal path under the shim's own record lock. Deciding it from a full
// registry scan made two things true that must not be: an unreadable file
// belonging to some unrelated session suppressed the alias for everyone, and a
// DEAD sibling's tombstone counted as evidence of a live sibling. Liveness is
// what a `.json` discovery record claims; a tombstone is the opposite claim.
func TestLegacyTombstoneAliasScopeIsTheIdentity(t *testing.T) {
	t.Parallel()
	id := Identity{OrgID: "org-alias", SessionID: "session-alias"}
	terminal := func(shimID string, epoch uint64) Tombstone {
		return Tombstone{
			SchemaVersion: RecordSchemaVersion,
			OrgID:         id.OrgID, SessionID: id.SessionID,
			ShimID: shimID, ProcessEpoch: epoch,
			HarnessPID: os.Getpid(), HarnessStartedAt: 1,
			ExitCode: 0, GroupReaped: true, ObservedAtUnixNano: 1700000000,
		}
	}
	liveRecord := func(shimID string, epoch uint64, reg *Registry) Record {
		return Record{
			SchemaVersion: RecordSchemaVersion,
			OrgID:         id.OrgID, SessionID: id.SessionID,
			ShimID:            shimID,
			ProcessEpoch:      epoch,
			PID:               os.Getpid(),
			ProcessStartedAt:  1700000000,
			SocketPath:        reg.SocketPath(id),
			SocketDevice:      1,
			SocketInode:       2,
			ProtocolMin:       shimwire.V1,
			ProtocolMax:       shimwire.V3,
			Phase:             shimwire.PhaseRunning,
			CreatedAtUnixNano: 1700000000,
		}
	}

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, reg *Registry, dir string)
		want  bool
	}{
		{
			name:  "no other entry: the alias is written",
			stage: func(*testing.T, *Registry, string) {},
			want:  true,
		},
		{
			name: "an unrelated unreadable entry is not this identity's business",
			stage: func(t *testing.T, _ *Registry, dir string) {
				t.Helper()
				other := Identity{OrgID: "org-alias", SessionID: "someone-else"}
				if err := os.WriteFile(filepath.Join(dir, other.RecordName()), []byte("{not json"), RecordFileMode); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name: "a dead sibling that never owned the alias does not block it",
			stage: func(t *testing.T, _ *Registry, dir string) {
				t.Helper()
				sibling := terminal("shim-sibling", 9)
				raw, err := sibling.encode()
				if err != nil {
					t.Fatal(err)
				}
				// The exact-incarnation proof only: the sibling's tombstone is a
				// statement that IT is gone, not that anything is alive.
				if err := os.WriteFile(filepath.Join(dir, tombstoneIncarnationName(sibling)), raw, RecordFileMode); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name: "a sibling that already owns the alias keeps it",
			stage: func(t *testing.T, reg *Registry, _ string) {
				t.Helper()
				if err := reg.PutTombstone(terminal("shim-sibling", 9)); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name: "a LIVE sibling record refuses the alias",
			stage: func(t *testing.T, reg *Registry, _ string) {
				t.Helper()
				if err := reg.Put(liveRecord("shim-sibling", 9, reg)); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := shortTempDir(t)
			reg, err := NewRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			tc.stage(t, reg, dir)
			mine := terminal("shim-mine", 3)
			safe, err := reg.legacyTombstoneAliasSafe(mine)
			if err != nil {
				t.Fatalf("legacyTombstoneAliasSafe: %v", err)
			}
			if safe != tc.want {
				t.Fatalf("legacyTombstoneAliasSafe = %t, want %t", safe, tc.want)
			}
			if err := reg.PutTombstone(mine); err != nil {
				t.Fatalf("PutTombstone: %v", err)
			}
			// The exact-incarnation proof is unconditional; only the alias is
			// scoped, so read it back to prove the decision above is the one
			// that reached disk.
			if _, err := reg.GetTombstoneIncarnation(id, mine.ShimID, mine.ProcessEpoch); err != nil {
				t.Fatalf("exact tombstone: %v", err)
			}
			raw, statErr := os.ReadFile(filepath.Join(dir, id.TombstoneName()))
			switch {
			case tc.want && statErr != nil:
				t.Fatalf("the legacy alias was not written: %v", statErr)
			case tc.want:
				alias, decErr := decodeTombstone(raw)
				if decErr != nil || alias.ShimID != mine.ShimID {
					t.Fatalf("alias carries %+v, %v, want this incarnation's proof", alias, decErr)
				}
			case !tc.want && statErr == nil:
				alias, decErr := decodeTombstone(raw)
				if decErr == nil && alias.ShimID == mine.ShimID {
					t.Fatal("the alias was overwritten with this incarnation's proof")
				}
			}
		})
	}
}
