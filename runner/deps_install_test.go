package runner

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// recordingDepsExecer is a DepsExecer fake: it records every call and
// returns a scripted exit code / error without running anything.
type recordingDepsExecer struct {
	mu       sync.Mutex
	calls    []depsCall
	exitCode int
	err      error
}

type depsCall struct {
	dir     string
	command string
}

func (f *recordingDepsExecer) Exec(_ context.Context, dir, command string, _ map[string]string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, depsCall{dir: dir, command: command})
	return f.exitCode, f.err
}

func (f *recordingDepsExecer) recorded() []depsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]depsCall(nil), f.calls...)
}

func depsTestRunner(t *testing.T, execer DepsExecer) *Runner {
	t.Helper()
	h := newRunnerHarness(t)
	h.runner.depsExecer = execer
	return h.runner
}

func TestInstallSessionDependencies_PnpmBeforeGo(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-ORDER")}, wpath, false)

	calls := execer.recorded()
	if len(calls) != 2 {
		t.Fatalf("install calls = %d; want 2 (pnpm then go)", len(calls))
	}
	if calls[0].command != pnpmDepsInstallCommand {
		t.Errorf("first call = %q; want %q", calls[0].command, pnpmDepsInstallCommand)
	}
	if calls[1].command != goDepsInstallCommand {
		t.Errorf("second call = %q; want %q", calls[1].command, goDepsInstallCommand)
	}
	for i, call := range calls {
		if call.dir != wpath {
			t.Errorf("call %d dir = %q; want worktree %q", i, call.dir, wpath)
		}
	}
}

func TestInstallSessionDependencies_SkipsPnpmWhenNodeModulesPresent(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, filepath.Join("node_modules", ".bin", "keep"), "")

	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-SKIP")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none (kit hook already installed)", calls)
	}
}

func TestInstallSessionDependencies_SkipsOnlyInstalledEcosystem(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, filepath.Join("node_modules", ".bin", "keep"), "")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-PARTIAL")}, wpath, false)

	calls := execer.recorded()
	if len(calls) != 1 {
		t.Fatalf("install calls = %v; want only the go install", calls)
	}
	if calls[0].command != goDepsInstallCommand || calls[0].dir != wpath {
		t.Errorf("call = %+v; want go install in %q", calls[0], wpath)
	}
}

func TestInstallSessionDependencies_NoManifestsRunsNothing(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "README.md", "# no manifests here\n")

	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-NONE")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none", calls)
	}
}

func TestInstallSessionDependencies_ReadOnlyCheckoutSkips(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-RO")}, wpath, true)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none on a read-only checkout", calls)
	}
}

func TestInstallSessionDependencies_FailureContinuesToNextEcosystem(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{exitCode: 1}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	// Must not panic, abort, or return an error: a failed install is
	// logged and the run continues with whatever is on disk.
	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-FAIL")}, wpath, false)

	calls := execer.recorded()
	if len(calls) != 2 {
		t.Fatalf("install calls = %d; want 2 (failure must not skip go)", len(calls))
	}
}

func TestInstallSessionDependencies_ExecErrorContinues(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{err: errors.New("binary missing")}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-ERR")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 1 {
		t.Fatalf("install calls = %d; want 1", len(calls))
	}
}

// orderLog is a mutex-guarded event log shared between the fake
// dependency execer and the spawn-recording provider wrapper so a
// Run-level test can assert the install ran before spawn.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type orderRecordingDepsExecer struct {
	log *orderLog
}

func (f *orderRecordingDepsExecer) Exec(_ context.Context, dir, command string, _ map[string]string) (int, error) {
	f.log.add("exec:" + command + " dir=" + dir)
	return 0, nil
}

// spawnRecordingProvider delegates to the stub provider while
// recording the Spawn call on the shared order log.
type spawnRecordingProvider struct {
	agent.Provider
	log *orderLog
}

func (p *spawnRecordingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.log.add("spawn")
	return p.Provider.Spawn(ctx, spec)
}

// TestRun_InstallsDependenciesBeforeSpawn is the revert-RED test for
// the post-acquire dependency install step: the fake execer records
// the exact install command and working directory, and the shared log
// proves the install ran before the provider spawned. Removing the
// installSessionDependencies call from runLoop leaves the log at
// ["spawn"] and this test fails.
func TestRun_InstallsDependenciesBeforeSpawn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	log := &orderLog{}
	h.runner.depsExecer = &orderRecordingDepsExecer{log: log}

	stubProvider, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := h.runner.registry.Register(&spawnRecordingProvider{Provider: stubProvider, log: log}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	qw := h.queuedWork("DEPS-BEFORE-SPAWN")
	seedManifestsInBareRepo(t, h.bareRepo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}

	events := log.snapshot()
	wantPnpm := "exec:" + pnpmDepsInstallCommand + " dir=" + res.WorktreePath
	wantGo := "exec:" + goDepsInstallCommand + " dir=" + res.WorktreePath
	switch {
	case len(events) != 3:
		t.Fatalf("order log = %v; want [%q %q spawn]", events, wantPnpm, wantGo)
	case events[0] != wantPnpm:
		t.Errorf("first event = %q; want %q (pnpm runs before go)", events[0], wantPnpm)
	case events[1] != wantGo:
		t.Errorf("second event = %q; want %q", events[1], wantGo)
	case events[2] != "spawn":
		t.Errorf("third event = %q; want spawn (install runs before spawn)", events[2])
	}
}

// TestRun_DependencyInstallFailureStillSpawns proves the install step
// never fails the session: a non-zero install exit still leads to a
// completed run.
func TestRun_DependencyInstallFailureStillSpawns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	h.runner.depsExecer = &recordingDepsExecer{exitCode: 1}

	qw := h.queuedWork("DEPS-FAIL-SPAWNS")
	seedManifestsInBareRepo(t, h.bareRepo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("Status = %q; want completed (install failure must not fail the session)", res.Status)
	}
}

// seedManifestsInBareRepo commits a pnpm lockfile and a go.mod into
// the harness's bare source repo so clones carry both manifests.
func seedManifestsInBareRepo(t *testing.T, bareRepo string) {
	t.Helper()
	work := t.TempDir()
	runGitFixture(t, "", "clone", "-q", bareRepo, work)
	writeFile(t, work, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, work, "go.mod", "module example.test/seed\n\ngo 1.24\n")
	for _, args := range [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "test"},
		{"-C", work, "add", "pnpm-lock.yaml", "go.mod"},
		{"-C", work, "commit", "-q", "-m", "seed dependency manifests"},
		{"-C", work, "push", "-q", "origin", "HEAD:main"},
	} {
		runGitFixture(t, "", args...)
	}
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test fixture with caller-supplied args
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
