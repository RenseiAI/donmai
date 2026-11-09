package daemon

// Launch-path tests for WHERE a seat's scratch directory comes from, driven
// through the production entry points (AcceptWork, StopSession,
// SweepSessionTmpDirs) with real worker processes:
//
//   - a value the embedder's OnPreSpawn hook declares — the env the worker
//     actually execs with — binds TMPDIR/TMP/TEMP, is created 0700 and
//     recorded before exec, and is removed when the session completes,
//     fails or is stopped;
//   - a session that declares nothing gets a daemon-derived directory; a
//     declared value always wins;
//   - a path that would push a unix socket under it past the socket path
//     cap refuses the launch loudly;
//   - the daemon's own inherited advisory key never binds a session;
//   - Node's os.tmpdir() in the seat resolves to the session directory, so
//     the compile cache (and tsx's cache, which derives its root from
//     os.tmpdir()) lands there and is deleted with it;
//   - the startup sweep reclaims the directory of a session whose daemon
//     died mid-session, and leaves this process's own live entries alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// derivedSessionTmpPattern is the documented shape of a derived directory
// under the daemon's default prefix.
var derivedSessionTmpPattern = regexp.MustCompile(`^/tmp/donmai-[0-9a-f]{12}$`)

// heldSeatWorker builds a worker that records its temp bindings to
// <root>/<session>/env.txt, then holds until the test writes
// <root>/<session>/release, whose content is the exit code (or until ~20 s
// pass). Holding lets a test inspect the live directory, record and claim.
func heldSeatWorker(root string) []string {
	quoted := "'" + strings.ReplaceAll(root, "'", "'\\''") + "'"
	script := `root=` + quoted + `/"$DONMAI_SESSION_ID"; mkdir -p "$root"; ` +
		`printf '%s\n' "TMPDIR=$TMPDIR" "TMP=$TMP" "TEMP=$TEMP" "ADVISORY=$DONMAI_SESSION_TMPDIR" > "$root/env.partial" && mv "$root/env.partial" "$root/env.txt"; ` +
		`i=0; while [ ! -s "$root/release" ] && [ "$i" -lt 400 ]; do sleep 0.05; i=$((i+1)); done; ` +
		`exit "$(cat "$root/release" 2>/dev/null || echo 0)"`
	return []string{"/bin/sh", "-c", script}
}

// declaringHook is an OnPreSpawn that delivers DONMAI_SESSION_TMPDIR the way
// an embedding product does: merged into the env it returns, never into the
// work item's env.
func declaringHook(dir string) func(SessionSpec, []string) ([]string, error) {
	return func(_ SessionSpec, env []string) ([]string, error) {
		return append(append([]string(nil), env...), SessionTmpDirEnv+"="+dir), nil
	}
}

// cleanupReportedScratch removes, at test end, a scratch directory a worker
// REPORTED — but only one of the derived shape. A reported path is whatever
// TMPDIR the worker saw; when the behaviour under test is broken that is
// the host's shared per-user temp, which a test must never delete.
func cleanupReportedScratch(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		if !derivedSessionTmpPattern.MatchString(dir) {
			return
		}
		if _, err := validateSessionTmpDir(dir); err != nil {
			return
		}
		_ = os.RemoveAll(dir)
	})
}

func waitForSeatFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(spawnerWaitTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", spawnerWaitTimeout, path)
}

// heldSeatEnv waits for a held worker's recorded bindings and parses them.
// It registers the guarded cleanup for the TMPDIR the worker saw before any
// caller asserts on it, so a failing assertion never leaks a derived
// directory whose session outlives the test binary.
func heldSeatEnv(t *testing.T, root, sessionID string) map[string]string {
	t.Helper()
	path := filepath.Join(root, sessionID, "env.txt")
	waitForSeatFile(t, path)
	env := envToMapSessionTmpTest(strings.Split(strings.TrimSpace(readSeatFile(t, path)), "\n"))
	cleanupReportedScratch(t, env["TMPDIR"])
	return env
}

func releaseHeldSeat(t *testing.T, root, sessionID, exitCode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, sessionID, "release"), []byte(exitCode), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertSeatBound checks that the worker ran with all four keys on dir.
func assertSeatBound(t *testing.T, env map[string]string, dir string) {
	t.Helper()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "ADVISORY"} {
		if env[key] != dir {
			t.Fatalf("worker %s = %q, want %q (worker env %v)", key, env[key], dir, env)
		}
	}
}

// assertLiveScratch checks a running session's directory on disk and in
// the spawner's bookkeeping: a real 0700 directory owned by this uid,
// marked for the session, recorded by this process, and claimed.
func assertLiveScratch(t *testing.T, s *WorkerSpawner, recordDir, sessionID, dir string) {
	t.Helper()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("scratch directory %s missing while the seat runs: %v", dir, err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o700 {
		t.Fatalf("scratch directory %s is %v, want a real 0700 directory", dir, fi.Mode())
	}
	if uid, err := sessionTmpOwnerUID(fi); err != nil || uid != os.Getuid() {
		t.Fatalf("scratch directory %s owned by uid %d (%v), want %d", dir, uid, err, os.Getuid())
	}
	owner, ok, err := readSessionTmpOwnerRecord(dir)
	if err != nil || !ok || owner.SessionID != sessionID {
		t.Fatalf("ownership marker = %+v, %v, %v; want session %q", owner, ok, err, sessionID)
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if entry := record.Entries[sessionID]; entry.Path != dir || entry.PID != os.Getpid() {
		t.Fatalf("record entry for %s = %+v, want path %q recorded by pid %d", sessionID, entry, dir, os.Getpid())
	}
	if claim, ok := s.sessionTmpClaimFor(sessionID); !ok || claim.Dir != dir {
		t.Fatalf("live claim for %s = %+v, %v; want %q", sessionID, claim, ok, dir)
	}
}

// assertScratchGone checks a finished session left nothing behind: no
// directory, no record entry, no claim.
func assertScratchGone(t *testing.T, s *WorkerSpawner, recordDir, sessionID, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch directory %s survives the session: %v", dir, err)
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := record.Entries[sessionID]; ok {
		t.Fatalf("record entry for %s survives the session: %+v", sessionID, entry)
	}
	if claim, ok := s.sessionTmpClaimFor(sessionID); ok {
		t.Fatalf("claim for %s survives the session: %+v", sessionID, claim)
	}
}

// TestSpawner_SessionTmp_HookDeclaredValueBinds pins the defect this suite
// exists for: an embedding product delivers DONMAI_SESSION_TMPDIR through
// the env its OnPreSpawn hook returns, not the work item's env. The worker
// must run with TMPDIR, TMP and TEMP on that directory, the directory must
// exist 0700 and be recorded before exec, and it must be gone once the
// session completes, fails or is stopped.
//
// RED: resolve the directory from the work item's env alone (the seam
// before this change) and the worker inherits the shared host temp.
func TestSpawner_SessionTmp_HookDeclaredValueBinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  string // release exit code, or "stop"
	}{
		{name: "completes", end: "0"},
		{name: "fails", end: "3"},
		{name: "stopped", end: "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, recordDir := t.TempDir(), t.TempDir()
			dir := fmt.Sprintf("/tmp/seat-hook-%s-%d", tc.name, os.Getpid())
			_ = os.RemoveAll(dir)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			s := NewWorkerSpawner(SpawnerOptions{
				Projects:            []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
				WorkerCommand:       heldSeatWorker(root),
				SessionTmpRecordDir: recordDir,
				OnPreSpawn:          declaringHook(dir),
			})
			ended := sessionEnds(s)
			sessionID := "seat-hook-" + tc.name
			if _, err := s.AcceptWork(seatTmpSpec(sessionID, "")); err != nil {
				t.Fatalf("accept: %v", err)
			}
			assertSeatBound(t, heldSeatEnv(t, root, sessionID), dir)
			assertLiveScratch(t, s, recordDir, sessionID, dir)
			if tc.end == "stop" {
				if !s.StopSession(sessionID) {
					t.Fatal("StopSession refused a live session")
				}
			} else {
				releaseHeldSeat(t, root, sessionID, tc.end)
			}
			waitSessionEnd(t, ended)
			waitForActiveCount(t, s, 0)
			assertScratchGone(t, s, recordDir, sessionID, dir)
		})
	}
}

// TestSpawner_SessionTmp_HookDeclaredValueBindsShimPath pins the same seam
// on the shim launch path: the env handed to the shim launcher carries the
// hook-declared directory, which already exists 0700 when the launcher
// runs, and the session-end cleanup removes it.
//
// RED: resolve from the work item's env alone and the launcher env keeps
// the shared host temp.
func TestSpawner_SessionTmp_HookDeclaredValueBindsShimPath(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	dir := fmt.Sprintf("/tmp/seat-hook-shim-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var (
		gotEnv  []string
		modeErr error
	)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:            []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		SessionTmpRecordDir: recordDir,
		OnPreSpawn:          declaringHook(dir),
		ShimOwns:            func(SessionSpec) bool { return true },
		ShimSpawn: func(spec SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
			gotEnv = append([]string(nil), env...)
			if fi, err := os.Lstat(dir); err != nil {
				modeErr = err
			} else if fi.Mode().Perm() != 0o700 {
				modeErr = fmt.Errorf("mode %04o at launch", fi.Mode().Perm())
			}
			return &SessionHandle{SessionID: spec.SessionID, State: SessionRunning}, nil
		},
	})
	const sessionID = "seat-hook-shim"
	if _, err := s.AcceptWork(seatTmpSpec(sessionID, "")); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if modeErr != nil {
		t.Fatalf("scratch directory not ready 0700 when the shim launcher ran: %v", modeErr)
	}
	bound := envToMapSessionTmpTest(gotEnv)
	for _, key := range append([]string{SessionTmpDirEnv}, sessionTmpBindingKeys...) {
		if bound[key] != dir {
			t.Fatalf("shim launcher env %s = %q, want %q", key, bound[key], dir)
		}
	}
	assertLiveScratch(t, s, recordDir, sessionID, dir)
	s.CleanupSessionTmpDir(sessionID)
	assertScratchGone(t, s, recordDir, sessionID, dir)
}

// TestSpawner_SessionTmp_DeclaredValueWins pins precedence: the hook runs
// last, so its value beats the work item's, and any declared value beats
// the derived default. The losing work-item path is never created.
//
// RED: let the work item's value win over the hook's (or derive whenever
// derivation is configured) and the worker's TMPDIR is the wrong path.
func TestSpawner_SessionTmp_DeclaredValueWins(t *testing.T) {
	t.Parallel()
	pid := os.Getpid()
	for _, tc := range []struct {
		name     string
		workItem string
		hook     string
		want     string
	}{
		{name: "hook over work item", workItem: fmt.Sprintf("/tmp/seat-wins-item-%d", pid), hook: fmt.Sprintf("/tmp/seat-wins-hook-%d", pid), want: fmt.Sprintf("/tmp/seat-wins-hook-%d", pid)},
		{name: "work item over derived", workItem: fmt.Sprintf("/tmp/seat-wins-only-item-%d", pid), want: fmt.Sprintf("/tmp/seat-wins-only-item-%d", pid)},
		{name: "hook over derived", hook: fmt.Sprintf("/tmp/seat-wins-only-hook-%d", pid), want: fmt.Sprintf("/tmp/seat-wins-only-hook-%d", pid)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, dir := range []string{tc.workItem, tc.hook} {
				if dir != "" {
					_ = os.RemoveAll(dir)
					t.Cleanup(func() { _ = os.RemoveAll(dir) })
				}
			}
			root, recordDir := t.TempDir(), t.TempDir()
			opts := SpawnerOptions{
				Projects:                []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
				WorkerCommand:           heldSeatWorker(root),
				SessionTmpRecordDir:     recordDir,
				SessionTmpDerivedPrefix: DefaultSessionTmpDerivedPrefix,
			}
			if tc.hook != "" {
				opts.OnPreSpawn = declaringHook(tc.hook)
			}
			s := NewWorkerSpawner(opts)
			ended := sessionEnds(s)
			sessionID := "seat-wins"
			if _, err := s.AcceptWork(seatTmpSpec(sessionID, tc.workItem)); err != nil {
				t.Fatalf("accept: %v", err)
			}
			assertSeatBound(t, heldSeatEnv(t, root, sessionID), tc.want)
			assertLiveScratch(t, s, recordDir, sessionID, tc.want)
			if tc.workItem != "" && tc.workItem != tc.want {
				if _, err := os.Lstat(tc.workItem); !os.IsNotExist(err) {
					t.Fatalf("overridden work-item path %s was created: %v", tc.workItem, err)
				}
			}
			releaseHeldSeat(t, root, sessionID, "0")
			waitSessionEnd(t, ended)
			waitForActiveCount(t, s, 0)
			assertScratchGone(t, s, recordDir, sessionID, tc.want)
		})
	}
}

// TestSpawner_SessionTmp_DerivedWhenUndeclared pins the undeclared default:
// with the daemon's prefix configured, a session that declares nothing runs
// in its own /tmp/donmai-<12 hex> directory — stated to it as
// DONMAI_SESSION_TMPDIR, created 0700, recorded — never in the shared host
// temp. Two concurrent seats get distinct directories, and each is removed
// when its session ends.
//
// RED: drop derivation and both workers inherit the shared host temp; drop
// the advisory binding and ADVISORY reads empty.
func TestSpawner_SessionTmp_DerivedWhenUndeclared(t *testing.T) {
	t.Parallel()
	root, recordDir := t.TempDir(), t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:                []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions:   4,
		WorkerCommand:           heldSeatWorker(root),
		SessionTmpRecordDir:     recordDir,
		SessionTmpDerivedPrefix: DefaultSessionTmpDerivedPrefix,
	})
	ended := sessionEnds(s)
	ids := []string{"seat-derived-one", "seat-derived-two"}
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AcceptWork(seatTmpSpec(id, ""))
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
	// Read every seat's bindings before asserting on any, so each observed
	// directory has its cleanup registered even when an assertion fails.
	envs := map[string]map[string]string{}
	for _, id := range ids {
		envs[id] = heldSeatEnv(t, root, id)
	}
	dirs := map[string]string{}
	for _, id := range ids {
		env := envs[id]
		dir := env["TMPDIR"]
		if !derivedSessionTmpPattern.MatchString(dir) {
			t.Fatalf("%s TMPDIR = %q, want a derived %s", id, dir, derivedSessionTmpPattern)
		}
		assertSeatBound(t, env, dir)
		assertLiveScratch(t, s, recordDir, id, dir)
		dirs[id] = dir
	}
	if dirs[ids[0]] == dirs[ids[1]] {
		t.Fatalf("two seats share the derived directory %q", dirs[ids[0]])
	}
	releaseHeldSeat(t, root, ids[0], "0")
	if !s.StopSession(ids[1]) {
		t.Fatal("StopSession refused a live session")
	}
	waitSessionEnd(t, ended)
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	for _, id := range ids {
		assertScratchGone(t, s, recordDir, id, dirs[id])
	}
}

// TestSpawner_SessionTmp_InheritedAdvisoryIgnoredAtLaunch pins, at the
// launch level, that the daemon's own inherited DONMAI_SESSION_TMPDIR (a
// daemon started inside another seat) never binds a session it launches:
// the session gets its own derived directory, and the outer path is never
// created or reused.
//
// Not parallel: it sets the process environment the spawner inherits.
//
// RED: let the inherited key through the parent env filter and the worker
// runs in the outer session's directory.
func TestSpawner_SessionTmp_InheritedAdvisoryIgnoredAtLaunch(t *testing.T) {
	outer := fmt.Sprintf("/tmp/seat-outer-session-%d", os.Getpid())
	_ = os.RemoveAll(outer)
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	t.Setenv(SessionTmpDirEnv, outer)
	root, recordDir := t.TempDir(), t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:                []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		WorkerCommand:           heldSeatWorker(root),
		SessionTmpRecordDir:     recordDir,
		SessionTmpDerivedPrefix: DefaultSessionTmpDerivedPrefix,
	})
	ended := sessionEnds(s)
	const sessionID = "seat-inner"
	if _, err := s.AcceptWork(seatTmpSpec(sessionID, "")); err != nil {
		t.Fatalf("accept: %v", err)
	}
	env := heldSeatEnv(t, root, sessionID)
	dir := env["TMPDIR"]
	if !derivedSessionTmpPattern.MatchString(dir) {
		t.Fatalf("worker TMPDIR = %q, want its own derived directory, not the daemon's inherited %q", dir, outer)
	}
	assertSeatBound(t, env, dir)
	if _, err := os.Lstat(outer); !os.IsNotExist(err) {
		t.Fatalf("the daemon's inherited scratch path %s was created for another session: %v", outer, err)
	}
	releaseHeldSeat(t, root, sessionID, "0")
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	assertScratchGone(t, s, recordDir, sessionID, dir)
}

// TestSpawner_SessionTmp_PathBudget pins the socket-path budget: a scratch
// path longer than sessionTmpMaxPathLen refuses the launch loudly — from
// the work item (before the hook runs), from the hook (rolled back through
// OnSpawnAborted), or derived from an over-long prefix — and no worker
// starts. A path of exactly the limit launches.
//
// RED: drop the length check and the over-long launches are accepted.
func TestSpawner_SessionTmp_PathBudget(t *testing.T) {
	t.Parallel()
	pathOfLen := func(tag string, n int) string {
		head := fmt.Sprintf("/tmp/budget-%s-%d-", tag, os.Getpid())
		return head + strings.Repeat("x", n-len(head))
	}
	if sessionTmpMaxPathLen != 63 {
		t.Fatalf("sessionTmpMaxPathLen = %d, want 63 (104-byte macOS sun_path, less the NUL and 40 bytes of socket-name headroom)", sessionTmpMaxPathLen)
	}
	atLimit, overLimit := pathOfLen("at", sessionTmpMaxPathLen), pathOfLen("over", sessionTmpMaxPathLen+1)
	if _, err := validateSessionTmpLaunchValue(atLimit); err != nil {
		t.Fatalf("a %d-byte path refused: %v", len(atLimit), err)
	}
	if _, err := validateSessionTmpLaunchValue(overLimit); err == nil || !strings.Contains(err.Error(), "64 bytes") || !strings.Contains(err.Error(), "63-byte limit") {
		t.Fatalf("a %d-byte path = %v, want a refusal naming both lengths", len(overLimit), err)
	}

	for _, tc := range []struct {
		name       string
		workItem   string
		hook       string
		prefix     string
		want       string
		hookRuns   bool
		wantAborts int
	}{
		{name: "work item over limit", workItem: pathOfLen("item", sessionTmpMaxPathLen+1), want: "64 bytes", hookRuns: false},
		{name: "hook over limit", hook: pathOfLen("hook", sessionTmpMaxPathLen+1), want: "64 bytes", hookRuns: true, wantAborts: 1},
		{name: "derived over limit", prefix: strings.Repeat("p", 50), want: "derived scratch directory", hookRuns: true, wantAborts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var hookRan bool
			var aborted int
			s := NewWorkerSpawner(SpawnerOptions{
				Projects:                []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
				WorkerCommand:           []string{"/bin/sh", "-c", "exit 0"},
				SessionTmpDerivedPrefix: tc.prefix,
				OnPreSpawn: func(spec SessionSpec, env []string) ([]string, error) {
					hookRan = true
					if tc.hook != "" {
						return declaringHook(tc.hook)(spec, env)
					}
					return nil, nil
				},
				OnSpawnAborted: func(SessionSpec, error) { aborted++ },
			})
			started := false
			s.startCommand = func(*exec.Cmd) error {
				started = true
				return errors.New("a worker must never start on an over-long scratch path")
			}
			_, err := s.AcceptWork(seatTmpSpec("seat-budget", tc.workItem))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("accept = %v, want a loud refusal containing %q", err, tc.want)
			}
			if started {
				t.Fatal("a worker started on an over-long scratch path")
			}
			if hookRan != tc.hookRuns {
				t.Fatalf("hook ran = %v, want %v", hookRan, tc.hookRuns)
			}
			if aborted != tc.wantAborts {
				t.Fatalf("OnSpawnAborted ran %d times, want %d", aborted, tc.wantAborts)
			}
			for _, dir := range []string{tc.workItem, tc.hook} {
				if dir == "" {
					continue
				}
				if _, statErr := os.Lstat(dir); !os.IsNotExist(statErr) {
					_ = os.RemoveAll(dir)
					t.Fatalf("over-long path %s was created: %v", dir, statErr)
				}
			}
			if _, ok := s.sessionTmpClaimFor("seat-budget"); ok {
				t.Fatal("refused launch left a claim behind")
			}
		})
	}

	t.Run("at the limit launches", func(t *testing.T) {
		t.Parallel()
		_ = os.RemoveAll(atLimit)
		t.Cleanup(func() { _ = os.RemoveAll(atLimit) })
		root, recordDir := t.TempDir(), t.TempDir()
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:            []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
			WorkerCommand:       heldSeatWorker(root),
			SessionTmpRecordDir: recordDir,
			OnPreSpawn:          declaringHook(atLimit),
		})
		ended := sessionEnds(s)
		if _, err := s.AcceptWork(seatTmpSpec("seat-at-limit", "")); err != nil {
			t.Fatalf("a %d-byte scratch path refused: %v", len(atLimit), err)
		}
		assertSeatBound(t, heldSeatEnv(t, root, "seat-at-limit"), atLimit)
		releaseHeldSeat(t, root, "seat-at-limit", "0")
		waitSessionEnd(t, ended)
		waitForActiveCount(t, s, 0)
		assertScratchGone(t, s, recordDir, "seat-at-limit", atLimit)
	})
}

// nodeSeatScript is a Node seat worker: it reports os.tmpdir(), enables
// Node's compile cache with no explicit directory (the default a tool such
// as tsc or eslint gets), compiles one module so the cache holds files,
// creates tsx's cache root the way tsx derives it (os.tmpdir()/tsx-<uid>),
// then holds until <root>/<session>/release appears.
const nodeSeatScript = `
const fs = require('fs'), os = require('os'), path = require('path'), mod = require('module');
const root = path.join(process.argv[1], process.env.DONMAI_SESSION_ID);
fs.mkdirSync(root, { recursive: true });
const out = { tmpdir: os.tmpdir(), compileCache: '', compileStatus: '' };
if (typeof mod.enableCompileCache === 'function') {
  const r = mod.enableCompileCache();
  out.compileCache = (r && r.directory) || '';
  out.compileStatus = String(r && r.status);
  const dep = path.join(root, 'dep.js');
  fs.writeFileSync(dep, 'module.exports = 42;\n');
  require(dep);
  if (typeof mod.flushCompileCache === 'function') mod.flushCompileCache();
}
out.tsxCache = path.join(os.tmpdir(), 'tsx-' + process.getuid());
fs.mkdirSync(out.tsxCache, { recursive: true });
fs.writeFileSync(path.join(out.tsxCache, 'probe.json'), '{}');
fs.writeFileSync(path.join(root, 'node.partial'), JSON.stringify(out));
fs.renameSync(path.join(root, 'node.partial'), path.join(root, 'node.json'));
const release = path.join(root, 'release'), deadline = Date.now() + 20000;
const tick = setInterval(() => { if (fs.existsSync(release) || Date.now() > deadline) clearInterval(tick); }, 50);
`

// TestSpawner_SessionTmp_NodeToolCachesFollowSessionDir pins the point of
// the whole feature through the real spawner path: in a Node seat whose
// directory the embedder's hook declared, os.tmpdir() is the session
// directory, so Node's default compile cache and tsx's cache root land
// inside it — and both are deleted with it when the session ends.
//
// RED: resolve from the work item's env alone and os.tmpdir() is the
// shared host temp, where both caches outlive the session.
func TestSpawner_SessionTmp_NodeToolCachesFollowSessionDir(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the Node tool-cache proof needs a Node runtime")
	}
	root, recordDir := t.TempDir(), t.TempDir()
	dir := fmt.Sprintf("/tmp/seat-node-%d", os.Getpid())
	_ = os.RemoveAll(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	declare := declaringHook(dir)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:            []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		WorkerCommand:       []string{node, "-e", nodeSeatScript, root},
		SessionTmpRecordDir: recordDir,
		// The hook also drops any cache relocation the developer's own
		// environment carries, so the seat sees Node's defaults.
		OnPreSpawn: func(spec SessionSpec, env []string) ([]string, error) {
			kept := make([]string, 0, len(env))
			for _, kv := range env {
				if isEnvEntryFor(kv, "NODE_COMPILE_CACHE") || isEnvEntryFor(kv, "NODE_DISABLE_COMPILE_CACHE") {
					continue
				}
				kept = append(kept, kv)
			}
			return declare(spec, kept)
		},
	})
	ended := sessionEnds(s)
	const sessionID = "seat-node"
	if _, err := s.AcceptWork(seatTmpSpec(sessionID, "")); err != nil {
		t.Fatalf("accept: %v", err)
	}
	report := filepath.Join(root, sessionID, "node.json")
	waitForSeatFile(t, report)
	var got struct {
		TmpDir        string `json:"tmpdir"`
		CompileCache  string `json:"compileCache"`
		CompileStatus string `json:"compileStatus"`
		TsxCache      string `json:"tsxCache"`
	}
	if err := json.Unmarshal([]byte(readSeatFile(t, report)), &got); err != nil {
		t.Fatalf("decode node report: %v", err)
	}
	t.Logf("node seat: tmpdir=%s compileCache=%s (status %s) tsxCache=%s", got.TmpDir, got.CompileCache, got.CompileStatus, got.TsxCache)
	if got.TmpDir != dir {
		t.Fatalf("os.tmpdir() in the seat = %q, want the session directory %q", got.TmpDir, dir)
	}
	if !strings.HasPrefix(got.TsxCache, dir+"/") {
		t.Fatalf("tsx cache root %q is outside the session directory %q", got.TsxCache, dir)
	}
	if got.CompileCache == "" {
		t.Fatalf("this Node (%s) reports no compile cache directory (status %q): the compile-cache half of the proof did not run", node, got.CompileStatus)
	}
	if !strings.HasPrefix(got.CompileCache, dir+"/") {
		t.Fatalf("Node compile cache %q is outside the session directory %q", got.CompileCache, dir)
	}
	cached := 0
	_ = filepath.WalkDir(got.CompileCache, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			cached++
		}
		return nil
	})
	if cached == 0 {
		t.Fatalf("Node compile cache %q holds no files: nothing was proven to land in the session directory", got.CompileCache)
	}
	assertLiveScratch(t, s, recordDir, sessionID, dir)
	releaseHeldSeat(t, root, sessionID, "0")
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	assertScratchGone(t, s, recordDir, sessionID, dir)
}

// The crash helper is this test binary re-executed (TestMain routes it):
// a daemon that launches one session with a derived scratch directory and
// dies mid-session, before any terminal cleanup can run.
const (
	sessionTmpCrashHelperEnv  = "DONMAI_TEST_SESSION_TMP_CRASH_RECORD"
	sessionTmpCrashCaptureEnv = "DONMAI_TEST_SESSION_TMP_CRASH_CAPTURE"
	sessionTmpCrashSessionID  = "seat-died-with-daemon"
)

func runSessionTmpCrashHelper() int {
	capture := os.Getenv(sessionTmpCrashCaptureEnv)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		// The worker reports its TMPDIR and lingers briefly, so the
		// daemon dies while its session is still running.
		WorkerCommand:           []string{"/bin/sh", "-c", `printf '%s' "$TMPDIR" > "$CRASH_CAPTURE.partial" && mv "$CRASH_CAPTURE.partial" "$CRASH_CAPTURE"; sleep 2`},
		BaseEnv:                 map[string]string{"CRASH_CAPTURE": capture},
		SessionTmpRecordDir:     os.Getenv(sessionTmpCrashHelperEnv),
		SessionTmpDerivedPrefix: DefaultSessionTmpDerivedPrefix,
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: sessionTmpCrashSessionID, Repository: "github.com/a/b", Ref: "main"}); err != nil {
		fmt.Fprintln(os.Stderr, "session tmp crash helper: accept:", err)
		return 2
	}
	for i := 0; i < 400; i++ {
		if _, err := os.Stat(capture); err == nil { //nolint:gosec // G703: capture is a t.TempDir path the parent test passes to its own re-executed binary.
			// Die mid-session: os.Exit runs no deferred or reaper cleanup.
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "session tmp crash helper: the worker never reported its TMPDIR")
	return 3
}

// TestSpawner_SessionTmp_SweepReclaimsAfterDaemonDies pins crash recovery
// end to end: a daemon process that dies mid-session leaves its session's
// derived directory and record entry behind; the restarted daemon's
// startup sweep removes both. An entry this very process recorded — a live
// session of another spawner sharing the state home — is kept.
//
// RED: drop the same-process guard and the sweep deletes the live
// session's directory.
func TestSpawner_SessionTmp_SweepReclaimsAfterDaemonDies(t *testing.T) {
	t.Parallel()
	recordDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "crashed-tmpdir")
	helper := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: re-executes this test binary as the crash helper.
	helper.Env = append(os.Environ(), sessionTmpCrashHelperEnv+"="+recordDir, sessionTmpCrashCaptureEnv+"="+capture)
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v\n%s", err, out)
	}
	crashed := readSeatFile(t, capture)
	cleanupReportedScratch(t, crashed)
	if !derivedSessionTmpPattern.MatchString(crashed) {
		t.Fatalf("crashed session TMPDIR = %q, want a derived directory", crashed)
	}
	if fi, err := os.Lstat(crashed); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("crashed session's directory should outlive its dead daemon: %v %v", fi, err)
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	entry := record.Entries[sessionTmpCrashSessionID]
	if entry.Path != crashed || entry.PID != helper.Process.Pid || entry.PID == os.Getpid() {
		t.Fatalf("crashed record entry = %+v, want path %q recorded by the helper pid %d", entry, crashed, helper.Process.Pid)
	}

	// A live session of another spawner in this process, sharing the record.
	live := NewWorkerSpawner(SpawnerOptions{SessionTmpRecordDir: recordDir})
	liveDir := fmt.Sprintf("/tmp/seat-sweep-own-process-%d", os.Getpid())
	_ = os.RemoveAll(liveDir)
	t.Cleanup(func() { _ = os.RemoveAll(liveDir) })
	liveClaim, err := live.prepareSessionTmpDir("seat-own-process-live", liveDir)
	if err != nil {
		t.Fatalf("prepare live: %v", err)
	}
	t.Cleanup(func() { live.cleanupSessionTmpClaim("seat-own-process-live", liveClaim) })

	restarted := NewWorkerSpawner(SpawnerOptions{SessionTmpRecordDir: recordDir})
	report := restarted.SweepSessionTmpDirs(nil)
	if report.Removed != 1 || report.KeptLive != 1 {
		t.Fatalf("sweep report = %+v, want {Removed:1 KeptLive:1}", report)
	}
	if _, err := os.Lstat(crashed); !os.IsNotExist(err) {
		t.Fatalf("crashed session's directory survives the restart sweep: %v", err)
	}
	if _, err := os.Lstat(liveDir); err != nil {
		t.Fatalf("the sweep deleted a live session's directory recorded by this process: %v", err)
	}
	after, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Entries[sessionTmpCrashSessionID]; ok || len(after.Entries) != 1 {
		t.Fatalf("record after the sweep = %+v, want only the live entry", after.Entries)
	}
}

// TestDaemonStart_DerivesSessionScratchByDefault pins the daemon contract
// for undeclared sessions: a started daemon configures its spawner to
// derive a per-session scratch directory under DefaultSessionTmpDerivedPrefix,
// and an embedder-pinned prefix keeps priority.
//
// RED: drop the daemon's default and the spawner derives nothing, so an
// undeclared session inherits the shared host temp.
func TestDaemonStart_DerivesSessionScratchByDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned string
		want   string
	}{
		{name: "default", want: DefaultSessionTmpDerivedPrefix},
		{name: "embedder pinned", pinned: "embedder-", want: "embedder-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.yaml")
			body := "machine:\n  id: isolated-fixture\norchestrator:\n  url: file:///" + dir + "/queue\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			d := New(Options{
				ConfigPath:       path,
				JWTPath:          filepath.Join(dir, "daemon.jwt"),
				SkipWizard:       true,
				SkipRegistration: true,
				HTTPHost:         "127.0.0.1",
				HTTPPort:         0,
				SpawnerOptions:   SpawnerOptions{WorkerCommand: []string{"/bin/false"}, SessionTmpDerivedPrefix: tc.pinned},
			})
			if err := d.Start(t.Context()); err != nil {
				t.Fatalf("foreground Start: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := d.Stop(ctx); err != nil {
					t.Errorf("foreground Stop: %v", err)
				}
			})
			d.spawner.mu.Lock()
			got := d.spawner.opts.SessionTmpDerivedPrefix
			d.spawner.mu.Unlock()
			if got != tc.want {
				t.Fatalf("spawner derived prefix = %q, want %q", got, tc.want)
			}
		})
	}
}
