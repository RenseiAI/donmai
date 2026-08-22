package sessionshim

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		ShimID: "shim-1", HarnessPID: 4242, HarnessStartedAt: 17,
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
