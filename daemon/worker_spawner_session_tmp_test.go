package daemon

// Launch-path tests for the executor-owned per-session scratch directory,
// driven through the production entry point (AcceptWork/StopSession):
// validation + creation before the worker starts with TMPDIR/TMP/TEMP
// bound (AC-1), two concurrent seats isolated (AC-2), removal on every
// terminal path (AC-3), byte-identical env without the key (AC-5), and the
// shim-path binding.
//
// RED: remove the prepare call from spawn, and the binding assertions fail;
// remove the reaper cleanup, and the gone-after-end assertions fail.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// seatTmpWorker builds a worker command that observes its scratch binding
// and proves isolation: it records its temp variables, checks the directory
// exists, and round-trips session-distinct bytes through $TMPDIR/prbody.md.
// Capture paths key off DONMAI_SESSION_ID under root, so one fixed command
// serves every session in a test. The root is embedded in the script (not
// passed via the process env) so parallel tests never share it.
func seatTmpWorker(root string) []string {
	quoted := "'" + strings.ReplaceAll(root, "'", "'\\''") + "'"
	script := `root=` + quoted + `/"$DONMAI_SESSION_ID"; mkdir -p "$root"; printf '%s\n' "TMPDIR=$TMPDIR" "TMP=$TMP" "TEMP=$TEMP" "ADVISORY=$DONMAI_SESSION_TMPDIR" > "$root/env.txt"; test -d "$TMPDIR" && printf isdir > "$root/isdir.txt"; printf '%s-bytes' "$DONMAI_SESSION_ID" > "$TMPDIR/prbody.md"; cat "$TMPDIR/prbody.md" > "$root/roundtrip.txt"`
	return []string{"/bin/sh", "-c", script}
}

func seatTmpSpawner(t *testing.T, command ...string) (*WorkerSpawner, string, string) {
	t.Helper()
	root := t.TempDir()
	recordDir := t.TempDir()
	cmd := command
	if len(cmd) == 0 {
		cmd = seatTmpWorker(root)
	}
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         cmd,
		SessionTmpRecordDir:   recordDir,
	})
	return s, recordDir, root
}

func seatTmpSpec(sessionID, dir string) SessionSpec {
	spec := SessionSpec{SessionID: sessionID, Repository: "github.com/a/b", Ref: "main"}
	if dir != "" {
		spec.Env = map[string]string{SessionTmpDirEnv: dir}
	}
	return spec
}

func readSeatFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestSpawner_SessionTmp_BoundBeforeWorkerStarts pins AC-1 through a real
// launch: the directory exists 0700 when the worker runs, the three temp
// variables bind it, the advisory key rides along, and the worker's
// round-trip through $TMPDIR/prbody.md reads back its own bytes.
func TestSpawner_SessionTmp_BoundBeforeWorkerStarts(t *testing.T) {
	t.Parallel()
	s, _, captureRoot := seatTmpSpawner(t)
	ended := sessionEnds(s)
	dir := "/tmp/seat-bound-before-start"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-ac1", dir)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)

	capture := filepath.Join(captureRoot, "seat-ac1")
	for _, line := range []string{"TMPDIR=" + dir, "TMP=" + dir, "TEMP=" + dir, "ADVISORY=" + dir} {
		if got := readSeatFile(t, filepath.Join(capture, "env.txt")); !strings.Contains(got, line) {
			t.Fatalf("worker env lacks %q:\n%s", line, got)
		}
	}
	if got := readSeatFile(t, filepath.Join(capture, "isdir.txt")); got != "isdir" {
		t.Fatalf("scratch directory did not exist when the worker ran: %q", got)
	}
	if got := readSeatFile(t, filepath.Join(capture, "roundtrip.txt")); got != "seat-ac1-bytes" {
		t.Fatalf("round trip = %q, want seat-ac1-bytes", got)
	}
}

// TestSpawner_SessionTmp_TwoSeatsIsolated pins AC-2: two concurrent seats
// on one host each write $TMPDIR/prbody.md with different content and each
// reads back its own bytes, under -race.
func TestSpawner_SessionTmp_TwoSeatsIsolated(t *testing.T) {
	t.Parallel()
	s, _, captureRoot := seatTmpSpawner(t)
	ended := sessionEnds(s)
	dirs := map[string]string{"seat-one": "/tmp/seat-isolated-one", "seat-two": "/tmp/seat-isolated-two"}
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(dirs))
	for id, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AcceptWork(seatTmpSpec(id, dir))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
	}
	waitSessionEnd(t, ended)
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)

	for id, dir := range dirs {
		capture := filepath.Join(captureRoot, id)
		if got := readSeatFile(t, filepath.Join(capture, "roundtrip.txt")); got != id+"-bytes" {
			t.Fatalf("%s round trip = %q, want %s-bytes: seats share temp files", id, got, id)
		}
		env := readSeatFile(t, filepath.Join(capture, "env.txt"))
		if !strings.Contains(env, "TMPDIR="+dir) {
			t.Fatalf("%s TMPDIR is not its own directory:\n%s", id, env)
		}
	}
}

// TestSpawner_SessionTmp_RemovedAfterEnd pins AC-3 on the success path: the
// directory and its record entry are gone once the seat ends.
func TestSpawner_SessionTmp_RemovedAfterEnd(t *testing.T) {
	t.Parallel()
	s, recordDir, _ := seatTmpSpawner(t)
	ended := sessionEnds(s)
	dir := "/tmp/seat-removed-after-end"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-gone", dir)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("scratch directory missing while the seat runs: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch directory survives the seat end: %v", err)
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Entries) != 0 {
		t.Fatalf("record entries survive the seat end: %+v", record.Entries)
	}
}

// TestSpawner_SessionTmp_RemovedAfterStop pins AC-3 on the stop/cancel path.
func TestSpawner_SessionTmp_RemovedAfterStop(t *testing.T) {
	t.Parallel()
	s, _, _ := seatTmpSpawner(t, "/bin/sh", "-c", "sleep 30")
	ended := sessionEnds(s)
	dir := "/tmp/seat-removed-after-stop"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-stopped", dir)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("scratch directory missing while the seat runs: %v", err)
	}
	if !s.StopSession("seat-stopped") {
		t.Fatal("StopSession refused a live session")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch directory survives stop: %v", err)
	}
}

// TestSpawner_SessionTmp_RemovedAfterSpawnFailure pins AC-3 on the failure
// path: a worker that never starts still releases the prepared directory.
func TestSpawner_SessionTmp_RemovedAfterSpawnFailure(t *testing.T) {
	t.Parallel()
	s, _, _ := seatTmpSpawner(t, "/nonexistent-worker-binary", "--x")
	dir := "/tmp/seat-removed-after-failure"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-failed", dir)); err == nil {
		t.Fatal("expected a spawn failure, got an accept")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch directory survives the spawn failure: %v", err)
	}
}

// TestSpawner_SessionTmp_MalformedRefusesLaunch pins AC-4 at the launch
// gate: a malformed advisory value refuses the launch with a clear error,
// and no worker starts.
func TestSpawner_SessionTmp_MalformedRefusesLaunch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{name: "relative", dir: "seat-relative", want: "absolute"},
		{name: "outside tmp", dir: "/var/tmp/seat-x", want: "direct child"},
		{name: "unstable clean", dir: "/tmp/../seat-x", want: "stable under Clean"},
		{name: "bad charset", dir: "/tmp/has space", want: "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var started bool
			s := NewWorkerSpawner(SpawnerOptions{
				Projects:      []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
				WorkerCommand: []string{"/bin/sh", "-c", "exit 0"},
				OnPreSpawn: func(_ SessionSpec, _ []string) ([]string, error) {
					started = true
					return nil, nil
				},
			})
			_, err := s.AcceptWork(seatTmpSpec("seat-malformed", tc.dir))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("accept = %v, want a refusal containing %q", err, tc.want)
			}
			if started {
				t.Fatal("a worker started despite the malformed scratch value")
			}
		})
	}
}

// TestSpawner_SessionTmp_SquatRefusesLaunch pins the shared-/tmp attacks at
// the launch gate: a planted symlink or loose directory at the advisory
// path refuses the launch.
func TestSpawner_SessionTmp_SquatRefusesLaunch(t *testing.T) {
	t.Parallel()
	dir := "/tmp/seat-squat-refused"
	_ = os.RemoveAll(dir)
	_ = os.Remove(dir)
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(dir)
	})
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	s, _, _ := seatTmpSpawner(t)
	if _, err := s.AcceptWork(seatTmpSpec("seat-squat", dir)); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink squat accepted: %v", err)
	}
	_ = os.Remove(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a deliberately loose squat fixture the launch gate must refuse.
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(seatTmpSpec("seat-squat", dir)); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("loose-mode squat accepted: %v", err)
	}
}

// TestSpawner_SessionTmp_ContinuedTurnRecreates pins that a continued turn
// of the same session may recreate the directory: relaunching the same id
// on the same path works, and each generation still cleans up after itself.
func TestSpawner_SessionTmp_ContinuedTurnRecreates(t *testing.T) {
	t.Parallel()
	s, _, _ := seatTmpSpawner(t)
	ended := sessionEnds(s)
	dir := "/tmp/seat-continued-turn"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	for _, id := range []string{"seat-again", "seat-again"} {
		if _, err := s.AcceptWork(seatTmpSpec(id, dir)); err != nil {
			t.Fatalf("accept %s: %v", id, err)
		}
		if _, err := os.Lstat(dir); err != nil {
			t.Fatalf("continued turn of %s has no scratch directory: %v", id, err)
		}
		waitSessionEnd(t, ended)
		waitForActiveCount(t, s, 0)
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("generation of %s leaked its directory: %v", id, err)
		}
	}
}

// TestSpawner_SessionTmp_NoKeyLeavesEnvAlone pins AC-5: with no advisory
// key the worker env is byte-identical to today's — the composition adds
// nothing of its own. The temp variables and the advisory key read exactly
// as inherited from the daemon's own environment (which on a seat host
// already carries them for the daemon itself).
func TestSpawner_SessionTmp_NoKeyLeavesEnvAlone(t *testing.T) {
	t.Parallel()
	parent := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			parent[kv[:i]] = kv[i+1:]
		}
	}
	s := NewWorkerSpawner(SpawnerOptions{})
	got := envToMapSessionTmpTest(s.sessionEnv(SessionSpec{SessionID: "plain", Repository: "github.com/a/b"}, nil))
	for _, key := range append([]string{SessionTmpDirEnv}, sessionTmpBindingKeys...) {
		if got[key] != parent[key] {
			t.Fatalf("%s = %q, want the inherited %q: the composition must add nothing without a stamped key", key, got[key], parent[key])
		}
	}
}

// TestSpawner_SessionTmp_NoKeyDirectSpawnAddsNoBindings pins AC-5 on the
// direct spawn path at the launch level: with no advisory key the executor
// adds no binding of its own — the spawn env carries no TMPDIR/TMP/TEMP
// entry whose value is the (empty) claim dir. The direct-path owned() gate
// decides this; deleting it (an unconditional applySessionTmpBindings with
// an empty claim dir) binds three empty variables, and this test goes RED.
// Inherited host values pass through untouched: the assertion is on empty
// executor bindings, not on absence. The captured env is what startCommand
// receives — the seam immediately before the real exec.
func TestSpawner_SessionTmp_NoKeyDirectSpawnAddsNoBindings(t *testing.T) {
	t.Parallel()
	var gotEnv []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
	})
	s.startCommand = func(cmd *exec.Cmd) error {
		gotEnv = append([]string(nil), cmd.Env...)
		return errors.New("controlled capture: never start a real worker")
	}
	if _, err := s.AcceptWork(seatTmpSpec("seat-no-key-direct", "")); err == nil {
		t.Fatal("expected the controlled capture failure, got an accept")
	}
	if gotEnv == nil {
		t.Fatal("startCommand never ran: no spawn env to assert on")
	}
	bound := envToMapSessionTmpTest(gotEnv)
	for _, key := range sessionTmpBindingKeys {
		if value, present := bound[key]; present && value == "" {
			t.Fatalf("direct spawn env carries an empty executor %s binding with no advisory key: the executor must add nothing", key)
		}
	}
}

// TestSpawner_SessionTmp_NoKeyShimSpawnAddsNoBindings pins AC-5 on the shim
// spawn path at the launch level: with no advisory key the executor adds no
// binding of its own — the env handed to the shim launcher carries no
// TMPDIR/TMP/TEMP entry whose value is the (empty) claim dir. The shim-path
// owned() gate decides this; deleting it binds three empty variables, and
// this test goes RED. Inherited host values pass through untouched.
func TestSpawner_SessionTmp_NoKeyShimSpawnAddsNoBindings(t *testing.T) {
	t.Parallel()
	var gotEnv []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		ShimOwns: func(SessionSpec) bool { return true },
		ShimSpawn: func(_ SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
			gotEnv = append([]string(nil), env...)
			return &SessionHandle{SessionID: "seat-no-key-shim", State: SessionRunning}, nil
		},
	})
	if _, err := s.AcceptWork(seatTmpSpec("seat-no-key-shim", "")); err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { s.CleanupSessionTmpDir("seat-no-key-shim") })
	if gotEnv == nil {
		t.Fatal("shim launcher never ran: no spawn env to assert on")
	}
	bound := envToMapSessionTmpTest(gotEnv)
	for _, key := range sessionTmpBindingKeys {
		if value, present := bound[key]; present && value == "" {
			t.Fatalf("shim spawn env carries an empty executor %s binding with no advisory key: the executor must add nothing", key)
		}
	}
}

// TestSpawner_SessionTmp_ShimPathBound pins the shim spawn path: the env
// handed to the shim launcher carries the scratch binding, the claim is
// recorded for terminal cleanup, and CleanupSessionTmpDir (the daemon's
// session-end listener entry point) removes the directory.
func TestSpawner_SessionTmp_ShimPathBound(t *testing.T) {
	t.Parallel()
	var gotEnv []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		ShimOwns: func(SessionSpec) bool { return true },
		ShimSpawn: func(_ SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
			gotEnv = append([]string(nil), env...)
			return &SessionHandle{SessionID: "seat-shim", State: SessionRunning}, nil
		},
	})
	dir := "/tmp/seat-shim-path"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-shim", dir)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	bound := map[string]string{}
	for _, kv := range gotEnv {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			bound[kv[:i]] = kv[i+1:]
		}
	}
	for _, key := range sessionTmpBindingKeys {
		if bound[key] != dir {
			t.Fatalf("shim env %s = %q, want %q", key, bound[key], dir)
		}
	}
	if _, ok := s.sessionTmpClaimFor("seat-shim"); !ok {
		t.Fatal("shim session has no recorded scratch claim for terminal cleanup")
	}
	s.CleanupSessionTmpDir("seat-shim")
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("shim scratch directory survives listener cleanup: %v", err)
	}
	if _, ok := s.sessionTmpClaimFor("seat-shim"); ok {
		t.Fatal("shim scratch claim survives listener cleanup")
	}
}

// TestSpawner_SessionTmp_BookkeepingFailureKeepsReusedDir pins that a
// record failure fails the launch closed without wiping a reused
// directory: the advisory path already holds the prior turn's files, the
// record is corrupt, so prepare refuses the launch — and the prior
// contents must survive. Fail-closed must not mean wipe-on-failure.
// Driven through AcceptWork — the production launch entry point — so
// unconditionally removing the directory on the record-failure path goes
// RED.
func TestSpawner_SessionTmp_BookkeepingFailureKeepsReusedDir(t *testing.T) {
	t.Parallel()
	dir := "/tmp/seat-bookkeeping-keeps-reused"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if _, err := ensureSessionTmpDir(dir); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "prior-turn-data.txt")
	if err := os.WriteFile(sentinel, []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(recordDir, sessionTmpRecordFileName), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var started bool
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:            []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		WorkerCommand:       []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir: recordDir,
		OnPreSpawn: func(_ SessionSpec, _ []string) ([]string, error) {
			started = true
			return nil, nil
		},
	})
	if _, err := s.AcceptWork(seatTmpSpec("seat-bookkeeping", dir)); err == nil {
		t.Fatal("expected a record-failure refusal, got an accept")
	}
	if started {
		t.Fatal("a worker started despite the record failure")
	}
	raw, err := os.ReadFile(sentinel) //nolint:gosec // G304: the path is a constant fixture under /tmp.
	if err != nil || string(raw) != "prior" {
		t.Fatalf("reused directory's prior contents deleted on a record failure the launch already reports: %v %q", err, raw)
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("reused directory disturbed by the refused launch: %v %v", fi, err)
	}
}

// TestSpawner_SessionTmp_BookkeepingFailureRemovesCreatedDir pins the
// counterpart: when this launch created the directory and record
// bookkeeping then fails, the launch still refuses — and removes exactly
// what it created, so a failed launch leaks nothing. Driven through
// AcceptWork, so skipping the created-new rollback goes RED.
func TestSpawner_SessionTmp_BookkeepingFailureRemovesCreatedDir(t *testing.T) {
	t.Parallel()
	dir := "/tmp/seat-bookkeeping-removes-created"
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	recordDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(recordDir, sessionTmpRecordFileName), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:            []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		WorkerCommand:       []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir: recordDir,
	})
	if _, err := s.AcceptWork(seatTmpSpec("seat-bookkeeping-new", dir)); err == nil {
		t.Fatal("expected a record-failure refusal, got an accept")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("created directory leaks after the refused launch: %v", err)
	}
}

// envToMapSessionTmpTest parses KEY=VALUE entries into a map for
// assertions. Bare keys (no '=') are skipped: they cannot name a binding.
func envToMapSessionTmpTest(entries []string) map[string]string {
	out := map[string]string{}
	for _, kv := range entries {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// TestSpawner_SessionTmp_ConcurrentRecordKeepsExactEntries pins the
// crash-recovery record against concurrent lost updates: 24 concurrent
// launches each record exactly one entry, and concurrent terminal cleanups
// drop exactly their own. Without the record mutex, concurrent
// load-modify-save cycles overwrite each other and entries leak past a
// restart. Driven through the production prepare/cleanup the launch and
// reaper paths use.
//
// RED: drop the record lock and the recorded set is short (the gate's own
// probe saw 3 of 24) or a cleanup erases a live entry.
func TestSpawner_SessionTmp_ConcurrentRecordKeepsExactEntries(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 32,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	const n = 24
	pid := os.Getpid()
	type prepared struct {
		id    string
		dir   string
		claim sessionTmpClaim
	}
	got := make([]prepared, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%d-%d", pid, i)
			dir := fmt.Sprintf("/tmp/concurrent-record-%d-%d", pid, i)
			_ = os.RemoveAll(dir)
			claim, err := s.prepareSessionTmpDir(seatTmpSpec(id, dir))
			got[i] = prepared{id: id, dir: dir, claim: claim}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepare %d: %v", i, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(got[i].dir) })
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if len(record.Entries) != n {
		t.Fatalf("record entries = %d, want %d: concurrent creations lost each other's entries", len(record.Entries), n)
	}
	for _, p := range got {
		entry, ok := record.Entries[p.id]
		if !ok || entry.Path != p.dir {
			t.Fatalf("record entry for %s = %+v, want path %q", p.id, entry, p.dir)
		}
	}
	// Concurrent terminal cleanups drop exactly their own entries.
	var cwg sync.WaitGroup
	for i := 0; i < n/2; i++ {
		cwg.Add(1)
		go func(i int) {
			defer cwg.Done()
			s.cleanupSessionTmpClaim(got[i].id, got[i].claim)
		}(i)
	}
	cwg.Wait()
	after, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatalf("load record after cleanup: %v", err)
	}
	if len(after.Entries) != n-n/2 {
		t.Fatalf("record entries after concurrent cleanup = %d, want %d", len(after.Entries), n-n/2)
	}
	for i := n / 2; i < n; i++ {
		if _, ok := after.Entries[got[i].id]; !ok {
			t.Fatalf("live entry %s lost to a concurrent cleanup", got[i].id)
		}
		if _, err := os.Lstat(got[i].dir); err != nil {
			t.Fatalf("live directory %s removed by another session's cleanup: %v", got[i].dir, err)
		}
	}
	for i := 0; i < n/2; i++ {
		if _, ok := after.Entries[got[i].id]; ok {
			t.Fatalf("cleaned entry %s survives its terminal cleanup", got[i].id)
		}
		if _, err := os.Lstat(got[i].dir); !os.IsNotExist(err) {
			t.Fatalf("cleaned directory %s survives its terminal cleanup", got[i].dir)
		}
	}
	// Release the survivors so the test leaves no scratch behind.
	for i := n / 2; i < n; i++ {
		s.cleanupSessionTmpClaim(got[i].id, got[i].claim)
	}
}

// TestSpawner_SessionTmp_DuplicatePathRefusesSecondLaunch pins that a
// launch stamped with a live session's path fails closed: the second seat
// never reuses the directory, so ending it can never delete the live
// seat's files. Driven through AcceptWork — the production launch entry —
// with the first seat kept live by a sleeping worker.
//
// RED: drop the live-claim scan and the second accept succeeds, rewriting
// the ownership marker.
func TestSpawner_SessionTmp_DuplicatePathRefusesSecondLaunch(t *testing.T) {
	t.Parallel()
	s, _, _ := seatTmpSpawner(t, "/bin/sh", "-c", "sleep 30")
	ended := sessionEnds(s)
	dir := fmt.Sprintf("/tmp/duplicate-path-refused-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-live", dir)); err != nil {
		t.Fatalf("accept live: %v", err)
	}
	sentinel := filepath.Join(dir, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(seatTmpSpec("seat-second", dir)); err == nil {
		t.Fatal("second launch sharing a live session's path accepted; it must fail closed")
	} else if !strings.Contains(err.Error(), "seat-live") || !strings.Contains(err.Error(), "seat-second") {
		t.Fatalf("duplicate-path refusal = %v, want an error naming both sessions", err)
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "live bytes" {
		t.Fatalf("live seat's file disturbed by the refused launch: %v %q", err, raw)
	}
	if _, ok := s.sessionTmpClaimFor("seat-live"); !ok {
		t.Fatal("live seat's claim lost to the refused launch")
	}
	if _, ok := s.sessionTmpClaimFor("seat-second"); ok {
		t.Fatal("refused launch left a live claim behind")
	}
	if !s.StopSession("seat-live") {
		t.Fatal("StopSession refused a live session")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its terminal cleanup: %v", err)
	}
	// A continued turn on the freed path works: the refusal held only
	// while another live session claimed it.
	s2, _, _ := seatTmpSpawner(t)
	ended2 := sessionEnds(s2)
	if _, err := s2.AcceptWork(seatTmpSpec("seat-after", dir)); err != nil {
		t.Fatalf("accept after the live session ended: %v", err)
	}
	waitSessionEnd(t, ended2)
	waitForActiveCount(t, s2, 0)
}

// TestSpawner_SessionTmp_DuplicatePathAliasRefusesSecondLaunch pins the
// marker-identity refusal under an aliased spelling: on macOS /tmp is the
// /private/tmp directory, so two cleaned strings name one directory and
// the live-claim scan cannot match them. The ownership marker lives in
// the directory itself, so both spellings read the same owner and the
// second launch fails closed before overwriting the stamp. Driven through
// AcceptWork with the first seat kept live by a sleeping worker.
//
// RED: drop the marker-identity check and the second accept succeeds,
// rewriting the live session's ownership marker.
func TestSpawner_SessionTmp_DuplicatePathAliasRefusesSecondLaunch(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("the /tmp alias exists only on macOS")
	}
	s, _, _ := seatTmpSpawner(t, "/bin/sh", "-c", "sleep 30")
	ended := sessionEnds(s)
	name := fmt.Sprintf("duplicate-path-alias-%d", os.Getpid())
	a := filepath.Join("/tmp", name)
	b := filepath.Join("/private/tmp", name)
	_ = os.RemoveAll(a)
	t.Cleanup(func() { _ = os.RemoveAll(a) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-alias-live", a)); err != nil {
		t.Fatalf("accept live: %v", err)
	}
	sentinel := filepath.Join(a, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(seatTmpSpec("seat-alias-second", b)); err == nil {
		t.Fatal("second launch on the aliased path accepted; it must fail closed")
	} else if !strings.Contains(err.Error(), "seat-alias-live") || !strings.Contains(err.Error(), "seat-alias-second") {
		t.Fatalf("aliased-path refusal = %v, want an error naming both sessions", err)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live bytes" {
		t.Fatalf("live seat's file disturbed by the refused launch: %v %q", err, raw)
	}
	if _, ok := s.sessionTmpClaimFor("seat-alias-second"); ok {
		t.Fatal("refused launch left a live claim behind")
	}
	if !s.StopSession("seat-alias-live") {
		t.Fatal("StopSession refused a live session")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its terminal cleanup: %v", err)
	}
}

// TestSpawner_SessionTmp_DuplicatePathCaseVariantRefusesSecondLaunch pins
// the marker-identity refusal under a case variant: on the default
// case-insensitive macOS volume two cleaned strings differing only in
// case name one directory, which the live-claim scan cannot match. The
// marker read refuses the second launch before the stamp is overwritten.
//
// RED: drop the marker-identity check and the second accept succeeds.
func TestSpawner_SessionTmp_DuplicatePathCaseVariantRefusesSecondLaunch(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("case-insensitive tmp spellings only collide on macOS default volumes")
	}
	probe := fmt.Sprintf("/tmp/case-probe-%d", os.Getpid())
	probeUpper := "/tmp/" + strings.ToUpper(fmt.Sprintf("case-probe-%d", os.Getpid()))
	_ = os.RemoveAll(probe)
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(probeUpper); os.IsNotExist(err) {
		_ = os.RemoveAll(probe)
		t.Skip("volume is case-sensitive: the variant names a different directory")
	}
	_ = os.RemoveAll(probe)

	s, _, _ := seatTmpSpawner(t, "/bin/sh", "-c", "sleep 30")
	ended := sessionEnds(s)
	lower := fmt.Sprintf("/tmp/duplicate-path-case-%d", os.Getpid())
	upper := "/tmp/" + strings.ToUpper(fmt.Sprintf("duplicate-path-case-%d", os.Getpid()))
	_ = os.RemoveAll(lower)
	t.Cleanup(func() {
		_ = os.RemoveAll(lower)
		_ = os.RemoveAll(upper)
	})

	if _, err := s.AcceptWork(seatTmpSpec("seat-case-live", lower)); err != nil {
		t.Fatalf("accept live: %v", err)
	}
	sentinel := filepath.Join(lower, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(seatTmpSpec("seat-case-second", upper)); err == nil {
		t.Fatal("second launch on the case-variant path accepted; it must fail closed")
	} else if !strings.Contains(err.Error(), "seat-case-live") || !strings.Contains(err.Error(), "seat-case-second") {
		t.Fatalf("case-variant refusal = %v, want an error naming both sessions", err)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live bytes" {
		t.Fatalf("live seat's file disturbed by the refused launch: %v %q", err, raw)
	}
	if !s.StopSession("seat-case-live") {
		t.Fatal("StopSession refused a live session")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	if _, err := os.Lstat(lower); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its terminal cleanup: %v", err)
	}
}

// TestSpawner_SessionTmp_DuplicatePathAfterRestartRefusesSecondLaunch pins
// the marker-identity refusal across a daemon restart: a live adopted
// session never inserts an in-memory claim in the new process, so the
// cleaned-string scan cannot see it — but its ownership marker still names
// it, and a second session stamped with the same path fails closed before
// the stamp is overwritten. The restart is simulated by a fresh spawner
// sharing the record directory.
//
// RED: drop the marker-identity check and the second prepare succeeds,
// rewriting the live session's ownership marker.
func TestSpawner_SessionTmp_DuplicatePathAfterRestartRefusesSecondLaunch(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	s1 := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	dir := fmt.Sprintf("/tmp/duplicate-path-restart-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	claimA, err := s1.prepareSessionTmpDir(seatTmpSpec("seat-restart-live", dir))
	if err != nil {
		t.Fatalf("prepare live: %v", err)
	}
	sentinel := filepath.Join(dir, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fresh spawner shares the record directory but holds no in-memory
	// claim, as an adopted live session would after a restart.
	s2 := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	if _, err := s2.prepareSessionTmpDir(seatTmpSpec("seat-restart-second", dir)); err == nil {
		t.Fatal("second prepare on an adopted session's path accepted; it must fail closed")
	} else if !strings.Contains(err.Error(), "seat-restart-live") || !strings.Contains(err.Error(), "seat-restart-second") {
		t.Fatalf("restart duplicate refusal = %v, want an error naming both sessions", err)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live bytes" {
		t.Fatalf("live session's file disturbed by the refused prepare: %v %q", err, raw)
	}
	if _, ok := s2.sessionTmpClaimFor("seat-restart-second"); ok {
		t.Fatal("refused prepare left a live claim behind")
	}
	s1.cleanupSessionTmpClaim("seat-restart-live", claimA)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its terminal cleanup: %v", err)
	}
}

// TestSpawner_SessionTmp_StaleClaimCleanupKeepsLive pins the generation
// fence in terminal cleanup: a stale generation's cleanup must not forget
// the live generation's claim, delete its directory, or drop its record
// entry. Two generations of one session share one path (a continued turn
// shape); the older generation's delayed cleanup runs after the newer
// owns it.
//
// RED: drop the (current != claim) fence and the live claim, directory
// and record entry are gone.
func TestSpawner_SessionTmp_StaleClaimCleanupKeepsLive(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	dir := fmt.Sprintf("/tmp/stale-claim-keeps-live-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	old, err := s.prepareSessionTmpDir(seatTmpSpec("seat-stale", dir))
	if err != nil {
		t.Fatalf("prepare old generation: %v", err)
	}
	live, err := s.prepareSessionTmpDir(seatTmpSpec("seat-stale", dir))
	if err != nil {
		t.Fatalf("prepare live generation: %v", err)
	}
	if old == live {
		t.Fatal("two prepares returned the same generation; the fence has nothing to judge")
	}
	sentinel := filepath.Join(dir, "live.txt")
	if err := os.WriteFile(sentinel, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cleanupSessionTmpClaim("seat-stale", old)
	if _, ok := s.sessionTmpClaimFor("seat-stale"); !ok {
		t.Fatal("stale cleanup forgot the live generation's claim")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live" {
		t.Fatalf("stale cleanup deleted the live generation's directory: %v %q", err, raw)
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := record.Entries["seat-stale"]; !ok {
		t.Fatal("stale cleanup dropped the live generation's record entry")
	}
	s.cleanupSessionTmpClaim("seat-stale", live)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its own terminal cleanup: %v", err)
	}
}

// TestSpawner_SessionTmp_MarkerWriteFailureRefusesLaunch pins that a
// launch whose ownership marker cannot be written fails closed before any
// worker starts: without the marker, end-of-session removal would refuse
// the directory and leak it. The fixture plants a directory at the marker
// path inside a reused 0700 directory, so the marker write deterministically
// fails. Driven through AcceptWork — the production launch entry.
//
// RED: ignore the marker-write error and the launch is accepted unmarked.
func TestSpawner_SessionTmp_MarkerWriteFailureRefusesLaunch(t *testing.T) {
	t.Parallel()
	dir := fmt.Sprintf("/tmp/marker-write-refused-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, sessionTmpOwnerFile), 0o700); err != nil {
		t.Fatal(err)
	}
	var started bool
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:      []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		WorkerCommand: []string{"/bin/sh", "-c", "exit 0"},
		OnPreSpawn: func(_ SessionSpec, _ []string) ([]string, error) {
			started = true
			return nil, nil
		},
	})
	if _, err := s.AcceptWork(seatTmpSpec("seat-marker-fails", dir)); err == nil {
		t.Fatal("launch with an unwritable ownership marker accepted; it must fail closed")
	}
	if started {
		t.Fatal("a worker started despite the marker failure")
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.Mode().IsDir() {
		t.Fatalf("reused directory disturbed by the refused launch: %v %v", fi, err)
	}
	if _, ok := s.sessionTmpClaimFor("seat-marker-fails"); ok {
		t.Fatal("refused launch left a live claim behind")
	}
}

// TestSpawner_SessionTmp_CreatedDirFailingVerifyIsRemoved pins that a
// directory this launch created but which fails its own checks is removed
// again: a failed launch leaks nothing. The hook sabotages the fresh
// directory off 0700 between creation and verification, so the production
// verification fails on a real filesystem observation and the production
// created-rollback beneath it runs.
//
// RED: drop the production created-rollback (if created) and the
// directory survives the refused launch; disable the hook and the
// creation is accepted, failing the refusal assertion.
func TestSpawner_SessionTmp_CreatedDirFailingVerifyIsRemoved(t *testing.T) {
	// Not parallel: it sets the process-wide after-create hook the
	// production ensure reads, so it must not run alongside other
	// scratch-directory tests.
	dir := fmt.Sprintf("/tmp/created-verify-rollback-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sessionTmpAfterCreateHookForTest = func(target string) {
		if target == dir {
			_ = os.Chmod(target, 0o755) //nolint:gosec // G302: a deliberately loosened sabotage fixture the production verification must refuse.
		}
	}
	defer func() { sessionTmpAfterCreateHookForTest = nil }()
	if _, _, err := ensureSessionTmpDirCreated(dir); err == nil {
		t.Fatal("sabotaged verification accepted; the launch must fail closed")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("created directory survives its failed verification: %v", err)
	}
}

// TestSpawner_SessionTmp_ShimFinishRemovesDir pins the shim terminal path:
// a shim-launched session that holds a scratch claim and ends through
// finishAdoptedShim removes its directory, claim and record entry. The
// daemon carries no Ended listener here, so only the finish cleanup can
// remove it — deleting that line leaves the directory behind.
//
// RED: delete the finishAdoptedShim session-tmp cleanup and the directory,
// claim and record entry survive.
func TestSpawner_SessionTmp_ShimFinishRemovesDir(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	sessionID := fmt.Sprintf("shim-finish-%d", os.Getpid())
	dir := fmt.Sprintf("/tmp/shim-finish-removes-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	claim, err := s.prepareSessionTmpDir(seatTmpSpec(sessionID, dir))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	sentinel := filepath.Join(dir, "prbody.md")
	if err := os.WriteFile(sentinel, []byte("seat bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(Options{SkipRegistration: true})
	d.spawner = s
	id := sessionshim.Identity{OrgID: "o", SessionID: sessionID}
	d.shims.adopted[id] = adoptedShim{
		launched:   true,
		handle:     SessionHandle{SessionID: sessionID, State: SessionRunning},
		spec:       SessionSpec{SessionID: sessionID, Repository: "github.com/a/b"},
		sessionTmp: claim,
		shimID:     "shim-finish",
		adoption:   SessionShimAdoptionEvidence{Identity: id, ShimID: "shim-finish", ProcessEpoch: 1},
	}
	d.finishAdoptedShim(id, shimwire.ExitMsg{ExitCode: 0})
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("shim scratch directory survives shim finish: %v", err)
	}
	if _, ok := s.sessionTmpClaimFor(sessionID); ok {
		t.Fatal("shim scratch claim survives shim finish")
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := record.Entries[sessionID]; ok {
		t.Fatalf("shim record entry survives shim finish: %+v", record.Entries)
	}
}

// TestSpawner_SessionTmp_UnreadableMarkerRefusesSecondPrepare pins
// fail-closed on a marker read error: a second session stamped with a live
// adopted session's path is refused when the ownership marker is present
// but unreadable, instead of taking over the directory. The restart is
// simulated by a fresh spawner sharing the record directory but holding
// no in-memory claim, so the cleaned-string scan cannot see the live
// session — only the marker read stands between the second launch and the
// live seat's files. Driven through prepareSessionTmpDir, the production
// launch gate, on the exact path (the scan-blind shape is the missing
// in-memory claim, not the spelling).
//
// RED: treat an unreadable marker as "no owner" and the second prepare
// succeeds, overwriting the live session's marker.
func TestSpawner_SessionTmp_UnreadableMarkerRefusesSecondPrepare(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permission bits: the unreadable fixture cannot fail")
	}
	recordDir := t.TempDir()
	s1 := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	dir := fmt.Sprintf("/tmp/unreadable-marker-refused-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	claimA, err := s1.prepareSessionTmpDir(seatTmpSpec("seat-unreadable-live", dir))
	if err != nil {
		t.Fatalf("prepare live: %v", err)
	}
	sentinel := filepath.Join(dir, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, sessionTmpOwnerFile)
	if err := os.Chmod(marker, 0o200); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(marker); err == nil {
		_ = os.Chmod(marker, 0o600)
		t.Skip("ownership marker still readable after chmod 0200: cannot prove the read-error leg")
	}
	// A fresh spawner holds no in-memory claim, as an adopted live
	// session would after a restart.
	s2 := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	if _, err := s2.prepareSessionTmpDir(seatTmpSpec("seat-unreadable-second", dir)); err == nil {
		_ = os.Chmod(marker, 0o600)
		t.Fatal("second prepare with an unreadable ownership marker accepted; it must fail closed")
	} else if !strings.Contains(err.Error(), "seat-unreadable-second") || !strings.Contains(err.Error(), dir) {
		_ = os.Chmod(marker, 0o600)
		t.Fatalf("unreadable-marker refusal = %v, want an error naming the directory and the refused session", err)
	}
	if _, ok := s2.sessionTmpClaimFor("seat-unreadable-second"); ok {
		_ = os.Chmod(marker, 0o600)
		t.Fatal("refused prepare left a live claim behind")
	}
	if err := os.Chmod(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live bytes" {
		t.Fatalf("live session's file disturbed by the refused prepare: %v %q", err, raw)
	}
	s1.cleanupSessionTmpClaim("seat-unreadable-live", claimA)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its terminal cleanup: %v", err)
	}
}

// TestSpawner_SessionTmp_CorruptMarkerRefusesSecondPrepare pins fail-closed
// on a marker decode error: a marker that is present but does not decode
// to a named session refuses the second launch, instead of being treated
// as "no owner". Unlike the unreadable-marker cell, this leg fails
// deterministically for every uid, including root. Driven through
// prepareSessionTmpDir on a fresh spawner, as an adopted live session
// would appear after a restart.
//
// RED: treat an undecodable marker as "no owner" and the second prepare
// succeeds, overwriting the live session's marker.
func TestSpawner_SessionTmp_CorruptMarkerRefusesSecondPrepare(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	s1 := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	dir := fmt.Sprintf("/tmp/corrupt-marker-refused-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	claimA, err := s1.prepareSessionTmpDir(seatTmpSpec("seat-corrupt-live", dir))
	if err != nil {
		t.Fatalf("prepare live: %v", err)
	}
	sentinel := filepath.Join(dir, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionTmpOwnerFile), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		SessionTmpRecordDir:   recordDir,
	})
	if _, err := s2.prepareSessionTmpDir(seatTmpSpec("seat-corrupt-second", dir)); err == nil {
		t.Fatal("second prepare with a corrupt ownership marker accepted; it must fail closed")
	} else if !strings.Contains(err.Error(), "seat-corrupt-second") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("corrupt-marker refusal = %v, want an error naming the directory and the refused session", err)
	}
	if _, ok := s2.sessionTmpClaimFor("seat-corrupt-second"); ok {
		t.Fatal("refused prepare left a live claim behind")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live bytes" {
		t.Fatalf("live session's file disturbed by the refused prepare: %v %q", err, raw)
	}
	// Restore a valid marker so the live generation's terminal cleanup
	// can prove ownership and remove the directory.
	if err := writeSessionTmpOwner(dir, "seat-corrupt-live", claimA.Gen); err != nil {
		t.Fatal(err)
	}
	s1.cleanupSessionTmpClaim("seat-corrupt-live", claimA)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("live directory survives its terminal cleanup: %v", err)
	}
}

// TestSpawner_SessionTmp_MissingMarkerStillRefusesExactPath pins the
// cleaned-string live-claim scan: a live claim with its ownership marker
// removed still refuses an exact-path second launch. The marker check
// alone cannot catch this cell (there is no marker to read), so only the
// scan stands between the second launch and the live seat's files.
// Driven through AcceptWork — the production launch entry — with the
// first seat kept live by a sleeping worker.
//
// RED: delete the live-claim scan and the second accept succeeds, taking
// over the live session's directory.
func TestSpawner_SessionTmp_MissingMarkerStillRefusesExactPath(t *testing.T) {
	t.Parallel()
	s, _, _ := seatTmpSpawner(t, "/bin/sh", "-c", "sleep 30")
	ended := sessionEnds(s)
	dir := fmt.Sprintf("/tmp/missing-marker-scan-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := s.AcceptWork(seatTmpSpec("seat-scan-live", dir)); err != nil {
		t.Fatalf("accept live: %v", err)
	}
	sentinel := filepath.Join(dir, "a-live.txt")
	if err := os.WriteFile(sentinel, []byte("live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The session can delete dotfiles inside its own scratch directory;
	// the scan is the only guard left once the marker is gone.
	if err := os.Remove(filepath.Join(dir, sessionTmpOwnerFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(seatTmpSpec("seat-scan-second", dir)); err == nil {
		t.Fatal("second launch sharing a live session's path with no marker accepted; the scan must fail closed")
	} else if !strings.Contains(err.Error(), "seat-scan-live") || !strings.Contains(err.Error(), "seat-scan-second") {
		t.Fatalf("missing-marker refusal = %v, want an error naming both sessions", err)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "live bytes" {
		t.Fatalf("live seat's file disturbed by the refused launch: %v %q", err, raw)
	}
	if _, ok := s.sessionTmpClaimFor("seat-scan-live"); !ok {
		t.Fatal("live seat's claim lost to the refused launch")
	}
	if _, ok := s.sessionTmpClaimFor("seat-scan-second"); ok {
		t.Fatal("refused launch left a live claim behind")
	}
	if !s.StopSession("seat-scan-live") {
		t.Fatal("StopSession refused a live session")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	// Without its marker the live generation cannot prove ownership for
	// removal, so the directory correctly survives its own cleanup; the
	// test owns the fixture from here.
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("live directory missing before test cleanup: %v", err)
	}
}
