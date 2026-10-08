package daemon

// Tests for the executor-owned per-session scratch directory: advisory-value
// validation (every refusal), creation/reuse semantics, worker-env bindings,
// the executor-side record, and the startup sweep.
//
// RED: weaken any check in session_tmpdir.go (accept a relative path, skip
// the symlink refusal, stop replacing TMPDIR) and the named test below fails.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestValidateSessionTmpDir pins every acceptance and every refusal. Each
// refusal is its own case: a malformed advisory value fails the launch
// closed, never falls back to the shared host temp.
func TestValidateSessionTmpDir(t *testing.T) {
	t.Parallel()
	longName := strings.Repeat("a", sessionTmpMaxNameLen+1)
	for _, tc := range []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "direct child of tmp", raw: "/tmp/seat-abc123", want: "/tmp/seat-abc123"},
		{name: "macOS tmp spelling", raw: "/private/tmp/seat-abc123", want: "/private/tmp/seat-abc123"},
		{name: "dotted dashed underscored", raw: "/tmp/s.e_a-t9", want: "/tmp/s.e_a-t9"},
		{name: "empty", raw: "", wantErr: "empty"},
		{name: "relative", raw: "tmp/seat-1", wantErr: "absolute"},
		{name: "bare name", raw: "seat-1", wantErr: "absolute"},
		{name: "parent is not tmp", raw: "/var/tmp/seat-1", wantErr: "direct child"},
		{name: "filesystem root", raw: "/", wantErr: "direct child"},
		{name: "tmp itself", raw: "/tmp", wantErr: "direct child"},
		{name: "nested under tmp", raw: "/tmp/a/b", wantErr: "direct child"},
		{name: "dotdot segment", raw: "/tmp/../evil", wantErr: "stable under Clean"},
		{name: "dotdot name", raw: "/tmp/..", wantErr: "stable under Clean"},
		{name: "trailing slash", raw: "/tmp/seat-1/", wantErr: "stable under Clean"},
		{name: "double slash", raw: "/tmp//seat-1", wantErr: "stable under Clean"},
		{name: "space in name", raw: "/tmp/has space", wantErr: "outside"},
		{name: "slash in name", raw: "/tmp/a/b", wantErr: "direct child"},
		{name: "glob in name", raw: "/tmp/*.log", wantErr: "outside"},
		{name: "shell metachar", raw: "/tmp/a;b", wantErr: "outside"},
		{name: "unicode", raw: "/tmp/séat", wantErr: "outside"},
		{name: "name too long", raw: "/tmp/" + longName, wantErr: "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := validateSessionTmpDir(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("validateSessionTmpDir(%q) = %q, %v; want an error containing %q", tc.raw, got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSessionTmpDir(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("validateSessionTmpDir(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestEnsureSessionTmpDir_Creates0700 pins creation: the directory appears
// with mode exactly 0700, owned by this user, before any worker starts.
func TestEnsureSessionTmpDir_Creates0700(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("/tmp", "ensure-creates-0700")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.RemoveAll(dir)
	got, err := ensureSessionTmpDir(dir)
	if err != nil {
		t.Fatalf("ensureSessionTmpDir: %v", err)
	}
	if got != dir {
		t.Fatalf("ensureSessionTmpDir = %q, want %q", got, dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = %v, want a directory with 0700", fi.Mode())
	}
	if err := verifySessionTmpOwner(fi, os.Getuid()); err != nil {
		t.Fatalf("created dir ownership: %v", err)
	}
}

// TestEnsureSessionTmpDir_ReusesOurs pins reuse: a directory this user
// created at 0700 is adopted, not refused, so a continued turn of the same
// session recreates its scratch in place.
func TestEnsureSessionTmpDir_ReusesOurs(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("/tmp", "ensure-reuses-ours")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.RemoveAll(dir)
	if _, err := ensureSessionTmpDir(dir); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	sentinel := filepath.Join(dir, "survives.txt")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSessionTmpDir(dir); err != nil {
		t.Fatalf("reuse of our 0700 directory refused: %v", err)
	}
}

// TestEnsureSessionTmpDir_RefusesSquats pins every existing-path refusal:
// a symlink, a non-directory, and a wrongly-moded directory are all
// refused, closing the squat and symlink attacks in the shared temp.
func TestEnsureSessionTmpDir_RefusesSquats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	linkTarget := filepath.Join(root, "target")
	if err := os.MkdirAll(linkTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join("/tmp", "ensure-refuses-symlink")
	t.Cleanup(func() { _ = os.Remove(linkPath) })
	_ = os.Remove(linkPath)
	if err := os.Symlink(linkTarget, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSessionTmpDir(linkPath); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink squat: err = %v, want a symlink refusal", err)
	}

	filePath := filepath.Join("/tmp", "ensure-refuses-file")
	t.Cleanup(func() { _ = os.Remove(filePath) })
	if err := os.WriteFile(filePath, []byte("squat"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSessionTmpDir(filePath); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file squat: err = %v, want a not-a-directory refusal", err)
	}

	loosePath := filepath.Join("/tmp", "ensure-refuses-mode")
	t.Cleanup(func() { _ = os.RemoveAll(loosePath) })
	_ = os.RemoveAll(loosePath)
	if err := os.MkdirAll(loosePath, 0o755); err != nil { //nolint:gosec // G301: a deliberately loose squat fixture the ensure must refuse.
		t.Fatal(err)
	}
	if _, err := ensureSessionTmpDir(loosePath); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("loose-mode squat: err = %v, want a mode refusal", err)
	}
}

// TestVerifySessionTmpOwner_ComparesUID pins the ownership comparison
// without needing root: our own files pass for our uid and fail for any
// other, so the check provably fires.
func TestVerifySessionTmpOwner_ComparesUID(t *testing.T) {
	t.Parallel()
	fi, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySessionTmpOwner(fi, os.Getuid()); err != nil {
		t.Fatalf("own directory with own uid refused: %v", err)
	}
	if err := verifySessionTmpOwner(fi, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("foreign uid: err = %v, want an ownership refusal", err)
	}
}

// TestApplySessionTmpBindings pins the worker-env binding: the three temp
// variables land on the directory exactly once, replacing whatever the
// worker was otherwise configured with, while the advisory key and every
// other entry pass through untouched.
func TestApplySessionTmpBindings(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"TMPDIR=/shared/host-tmp",
		"TMP=/shared/host-tmp",
		"TEMP=/shared/host-tmp",
		"TMPDIR_EXTRA=keep",
		SessionTmpDirEnv + "=/tmp/seat-9",
		"BARE_TMPDIR_LIKE=1",
	}
	got := applySessionTmpBindings(in, "/tmp/seat-9")
	counts := map[string]int{}
	values := map[string]string{}
	for _, kv := range got {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
			values[key] = kv[i+1:]
		}
		counts[key]++
	}
	for _, key := range sessionTmpBindingKeys {
		if counts[key] != 1 || values[key] != "/tmp/seat-9" {
			t.Fatalf("%s bound %d times to %q (env %v), want exactly once to /tmp/seat-9", key, counts[key], values[key], got)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "TMPDIR_EXTRA=keep", SessionTmpDirEnv + "=/tmp/seat-9", "BARE_TMPDIR_LIKE=1"} {
		found := false
		for _, kv := range got {
			found = found || kv == want
		}
		if !found {
			t.Fatalf("passthrough entry %q lost (env %v)", want, got)
		}
	}
}

// TestSessionTmpRecord_RoundTrip pins the executor-side record: creations
// persist across a (simulated) restart, and forgetting drops them.
func TestSessionTmpRecord_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	record, err := loadSessionTmpRecord(dir)
	if err != nil {
		t.Fatalf("load missing record: %v", err)
	}
	if len(record.Entries) != 0 {
		t.Fatalf("missing record is not empty: %+v", record)
	}
	record.Entries["s1"] = sessionTmpRecordEntry{Path: "/tmp/seat-s1", CreatedAt: "2026-10-06T00:00:00Z"}
	if err := saveSessionTmpRecord(dir, record); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, sessionTmpRecordFileName))
	if err != nil {
		t.Fatal(err)
	}
	var decoded sessionTmpRecord
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Entries["s1"].Path != "/tmp/seat-s1" {
		t.Fatalf("record round trip = %+v", decoded)
	}
	fi, err := os.Stat(filepath.Join(dir, sessionTmpRecordFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %04o, want 0600", fi.Mode().Perm())
	}
	if _, err := loadSessionTmpRecord(filepath.Join(dir, "missing-subdir")); err != nil {
		t.Fatalf("load from missing dir: %v", err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(broken, filepath.Join(dir, sessionTmpRecordFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionTmpRecord(dir); err == nil {
		t.Fatal("corrupt record loads cleanly; the executor cannot safely update what it cannot decode")
	}
}

// TestSweepSessionTmpDirs pins crash recovery: recorded directories of dead
// sessions are removed, live sessions' are kept, and entries that are
// already gone, malformed, or no longer provably ours are dropped without
// deleting. Nothing outside the record is touched.
func TestSweepSessionTmpDirs(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{SessionTmpRecordDir: recordDir})

	dead := filepath.Join("/tmp", "sweep-removes-dead")
	_ = os.RemoveAll(dead)
	if _, err := ensureSessionTmpDir(dead); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join("/tmp", "sweep-keeps-live")
	_ = os.RemoveAll(live)
	if _, err := ensureSessionTmpDir(live); err != nil {
		t.Fatal(err)
	}
	// A recorded path outside /tmp must be dropped without deleting, on
	// every platform. os.MkdirTemp("", ...) cannot pin that: on Linux it
	// lands inside /tmp, where it passes validation and is removed. A
	// scratch entry built under recordDir (t.TempDir()) is nested at least
	// two levels under the platform temp dir on every platform, so it
	// always fails validation as "not a direct child" and is dropped.
	squat := filepath.Join(recordDir, "outside-tmp", "sweep-squat-outside-tmp")
	if err := os.MkdirAll(squat, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(squat) })
	loose := filepath.Join("/tmp", "sweep-drops-loose-mode")
	_ = os.RemoveAll(loose)
	t.Cleanup(func() { _ = os.RemoveAll(loose) })
	if err := os.MkdirAll(loose, 0o755); err != nil { //nolint:gosec // G301: a deliberately loose sweep fixture the ownership check must drop.
		t.Fatal(err)
	}
	innocent := filepath.Join("/tmp", "sweep-ignores-unrecorded")
	_ = os.RemoveAll(innocent)
	if _, err := ensureSessionTmpDir(innocent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(innocent) })

	record := sessionTmpRecord{Version: 1, Entries: map[string]sessionTmpRecordEntry{
		"dead":    {Path: dead},
		"live":    {Path: live},
		"gone":    {Path: filepath.Join("/tmp", "sweep-already-gone")},
		"bad":     {Path: "/var/tmp/sweep-malformed"},
		"outside": {Path: squat},
		"loose":   {Path: loose},
	}}
	_ = os.RemoveAll(filepath.Join("/tmp", "sweep-already-gone"))
	if err := saveSessionTmpRecord(recordDir, record); err != nil {
		t.Fatal(err)
	}

	report := s.SweepSessionTmpDirs(map[string]struct{}{"live": {}})
	if report.Examined != 6 {
		t.Fatalf("examined = %d, want 6", report.Examined)
	}
	if report.Removed != 1 || report.KeptLive != 1 || report.Dropped != 4 {
		t.Fatalf("report = %+v, want {Removed:1 KeptLive:1 Dropped:4}", report)
	}
	if _, err := os.Lstat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead session directory survives the sweep: %v", err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Fatalf("live session directory swept: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(live) })
	if _, err := os.Lstat(squat); err != nil {
		t.Fatalf("unrecorded-parent entry deleted: %v", err)
	}
	if _, err := os.Lstat(loose); err != nil {
		t.Fatalf("loose-mode entry deleted instead of dropped: %v", err)
	}
	if _, err := os.Lstat(innocent); err != nil {
		t.Fatalf("unrecorded directory touched: %v", err)
	}
	after, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Entries) != 1 || after.Entries["live"].Path != live {
		t.Fatalf("record after sweep = %+v, want only the live entry", after.Entries)
	}
}

// TestSweepSessionTmpDirs_QuarantinedKeptLive pins that the startup sweep
// protects quarantined lineages: the daemon-side entry point builds its
// live set from adopted identities plus quarantined session ids, because a
// quarantined lineage keeps its shim alive and its worker's live scratch
// must survive a daemon restart. Driven through (d *Daemon),
// sweepSessionTmpDirs — the production crash-recovery entry point — with a
// daemon carrying one adopted identity and one quarantined session, so
// narrowing the live set to adopted identities alone goes RED.
func TestSweepSessionTmpDirs_QuarantinedKeptLive(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	spawner := NewWorkerSpawner(SpawnerOptions{SessionTmpRecordDir: recordDir})

	adoptedDir := filepath.Join("/tmp", "sweep-quarantined-adopted")
	quarantinedDir := filepath.Join("/tmp", "sweep-quarantined-live")
	deadDir := filepath.Join("/tmp", "sweep-quarantined-dead")
	for _, dir := range []string{adoptedDir, quarantinedDir, deadDir} {
		_ = os.RemoveAll(dir)
		if _, err := ensureSessionTmpDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(adoptedDir)
		_ = os.RemoveAll(quarantinedDir)
		_ = os.RemoveAll(deadDir)
	})

	record := sessionTmpRecord{Version: 1, Entries: map[string]sessionTmpRecordEntry{
		"adopted":     {Path: adoptedDir},
		"quarantined": {Path: quarantinedDir},
		"dead":        {Path: deadDir},
	}}
	if err := saveSessionTmpRecord(recordDir, record); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		spawner: spawner,
		shims: &sessionShimState{
			adopted: map[sessionshim.Identity]adoptedShim{
				{OrgID: "org-quarantine", SessionID: "adopted"}: {},
			},
			quarantined: []sessionshim.QuarantinedSession{
				{OrgID: "org-quarantine", SessionID: "quarantined", ConsumesCapacity: true},
			},
		},
	}
	d.sweepSessionTmpDirs()

	if _, err := os.Lstat(adoptedDir); err != nil {
		t.Fatalf("adopted session directory swept: %v", err)
	}
	if _, err := os.Lstat(quarantinedDir); err != nil {
		t.Fatalf("quarantined session directory swept: the sweep deletes a live lineage's scratch: %v", err)
	}
	if _, err := os.Lstat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("dead session directory survives the sweep: %v", err)
	}
	after, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Entries) != 2 || after.Entries["adopted"].Path != adoptedDir || after.Entries["quarantined"].Path != quarantinedDir {
		t.Fatalf("record after sweep = %+v, want only the adopted and quarantined entries", after.Entries)
	}
}

// TestSweepSessionTmpDirs_DisabledWithoutRecordDir pins that a spawner with
// no record directory sweeps nothing: memory-only mode opts out of restart
// recovery explicitly.
func TestSweepSessionTmpDirs_DisabledWithoutRecordDir(t *testing.T) {
	t.Parallel()
	s := NewWorkerSpawner(SpawnerOptions{})
	if report := s.SweepSessionTmpDirs(nil); report != (SessionTmpSweepReport{}) {
		t.Fatalf("sweep without a record dir = %+v, want zero", report)
	}
}

// TestSessionTmpOwnerMarker pins generation-exact cleanup: an older
// generation never removes a newer generation's directory sharing the same
// path (the continued-turn shape), and a missing marker refuses removal.
func TestSessionTmpOwnerMarker(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("/tmp", "owner-marker-exact")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.RemoveAll(dir)
	if _, err := ensureSessionTmpDir(dir); err != nil {
		t.Fatal(err)
	}
	old := sessionTmpClaim{Dir: dir, Gen: 7}
	newer := sessionTmpClaim{Dir: dir, Gen: 8}
	if err := writeSessionTmpOwner(dir, "s1", old.Gen); err != nil {
		t.Fatal(err)
	}
	if !sessionTmpOwnerMatches(dir, old) {
		t.Fatal("marker does not match the claim that wrote it")
	}
	if sessionTmpOwnerMatches(dir, newer) {
		t.Fatal("marker matches a generation that never owned the directory")
	}
	if removeSessionTmpClaimDir(newer) {
		t.Fatal("removed a directory on another generation's marker")
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("directory gone after refused removal: %v", err)
	}
	if err := writeSessionTmpOwner(dir, "s1", newer.Gen); err != nil {
		t.Fatal(err)
	}
	if !removeSessionTmpClaimDir(newer) {
		t.Fatal("refused to remove our own marked generation")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory survives its own removal: %v", err)
	}
	if removeSessionTmpClaimDir(newer) {
		t.Fatal("second removal of the same claim reports success")
	}
}
