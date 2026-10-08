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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
